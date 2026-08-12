package glass.relay.bridge

import android.content.Context
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.asSharedFlow

/**
 * The Android half of the seam.
 *
 * Mirrors `glasses/bridge/src/transport.ts` so both platforms and the mock speak
 * the same vocabulary. Keep the two in step: when a method is added there, it is
 * added here, and the protocol command it maps to is named in both.
 *
 * Deliberately narrow. The vendor SDK exposes ~90 calls; the service needs a
 * dozen, and every extra one is surface that has to keep working.
 */
interface GlassesTransport {

    val events: Flow<GlassesEvent>

    suspend fun connect(): Boolean
    suspend fun disconnect()

    /** Protocol 0x0007. False means the link is gone even if it looks up. */
    suspend fun heartbeat(): Boolean

    /** Protocol 0x0005 — call before issuing anything else. */
    suspend fun features(): Features

    /** Protocol 0x0101. */
    suspend fun battery(): BatteryStatus

    /** Protocol 0x0903. Every transcript timestamp depends on this. */
    suspend fun syncTime()

    /** Protocol 0x0E04. Records to the glasses' own 4 GB, not over the radio. */
    suspend fun startLocalRecording()
    suspend fun stopLocalRecording()

    /** Protocol 0x0E01. */
    suspend fun listFiles(): List<RemoteFile>

    /** Protocol 0x090B. Bulk sync only — costs the phone its Wi-Fi uplink. */
    suspend fun openAccessPoint(): AccessPoint
    suspend fun closeAccessPoint()

    suspend fun fetchFile(name: String, onProgress: (Long, Long) -> Unit = { _, _ -> }): ByteArray
}

sealed interface GlassesEvent {
    data class Wear(val worn: Boolean) : GlassesEvent
    data class Touch(val action: String) : GlassesEvent
    data class Battery(val percent: Int, val charging: Boolean) : GlassesEvent
    data class RecordingState(val recording: Boolean, val durationSeconds: Int) : GlassesEvent
    /**
     * Text the **app's** recogniser produced, re-emitted here so every screen
     * reads one stream. Not device ASR — the glasses have none. See
     * `SYSTEM.md` §7b: `0x0803`/`0x0805` report that the wearer wants to talk,
     * and every verb in them belongs to the app.
     */
    data class Transcript(val text: String) : GlassesEvent
    data class AudioChunk(val data: ByteArray, val sequence: Int) : GlassesEvent {
        // ByteArray in a data class needs these or equality is identity-based,
        // which silently breaks any dedupe built on it.
        override fun equals(other: Any?): Boolean =
            this === other || (other is AudioChunk && sequence == other.sequence && data.contentEquals(other.data))

        override fun hashCode(): Int = 31 * sequence + data.contentHashCode()
    }
    data object Disconnected : GlassesEvent
}

data class Features(
    val localRecording: Boolean,
    val wifiAp: Boolean,
    val livePreview: Boolean,
    val voiceWakeup: Boolean,
    val wearDetection: Boolean,
)

data class BatteryStatus(val percent: Int, val charging: Boolean)

data class RemoteFile(
    val name: String,
    val sizeBytes: Long,
    val uploaded: Boolean,
    val durationSeconds: Int? = null,
)

data class AccessPoint(val ssid: String, val password: String, val host: String)

/**
 * Chooses the transport for this build.
 *
 * The vendor AAR is device-only and needs real glasses, so debug builds and
 * instrumentation default to the mock. Swapping here rather than at every call
 * site is the entire point of the interface.
 */
object TransportProvider {
    @Volatile
    private var override: GlassesTransport? = null

    /** Inject a fake from tests. */
    fun set(transport: GlassesTransport?) { override = transport }

    fun get(context: Context): GlassesTransport =
        override ?: if (BuildConfig.USE_MOCK_GLASSES) {
            MockGlassesTransport()
        } else {
            vendorTransport(context.applicationContext)
        }

    /**
     * Loaded reflectively, because `VendorGlassesTransport` only exists in builds
     * that had the proprietary AAR on disk (see the module's build.gradle.kts).
     * A direct reference would make `main` fail to compile without it, which
     * would defeat the point of keeping the vendor SDK out of the tree.
     *
     * Falling back to the mock here would be much worse than failing: a release
     * build that silently reports fabricated battery levels and never connects to
     * anything is indistinguishable from working software until a user relies on it.
     */
    private fun vendorTransport(context: Context): GlassesTransport {
        check(BuildConfig.HAS_VENDOR_SDK) {
            "This build was compiled without LIB_GLASSES_SDK, so it cannot talk to " +
                "glasses. Copy the AAR to apps/android/libs/ and rebuild — see " +
                "apps/android/README.md."
        }
        return try {
            Class.forName(VENDOR_TRANSPORT)
                .getConstructor(Context::class.java)
                .newInstance(context) as GlassesTransport
        } catch (e: ReflectiveOperationException) {
            throw IllegalStateException("$VENDOR_TRANSPORT could not be loaded", e)
        }
    }

    private const val VENDOR_TRANSPORT = "glass.relay.bridge.vendor.VendorGlassesTransport"
}

/**
 * Base class handling the event plumbing so implementations only deal with the SDK.
 */
abstract class BaseGlassesTransport : GlassesTransport {
    private val _events = MutableSharedFlow<GlassesEvent>(replay = 0, extraBufferCapacity = 64)
    override val events: Flow<GlassesEvent> = _events.asSharedFlow()

    protected fun emit(event: GlassesEvent) {
        // tryEmit, not emit: dropping one battery sample under backpressure is
        // strictly better than suspending the SDK's callback thread.
        _events.tryEmit(event)
    }
}
