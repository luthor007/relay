package glass.relay.bridge

import kotlinx.coroutines.delay
import kotlin.math.min

/**
 * Glasses-free transport for debug builds and instrumentation.
 *
 * Same intent as `MockTransport` in glasses/bridge: encode the real constraints
 * rather than resolving instantly, so the UI is built against honest latency.
 * A fetch over BLE really is slower than the recording took; a fetch over the
 * access point really is seconds.
 */
class MockGlassesTransport(
    private val connectDelayMs: Long = 800,
    private val bleBytesPerSecond: Long = 3_000,
    private val wifiBytesPerSecond: Long = 2_000_000,
) : BaseGlassesTransport() {

    private var connected = false
    private var apOpen = false
    private var recordingSince: Long? = null
    private val files = mutableListOf<RemoteFile>()
    private var counter = 0

    override suspend fun connect(): Boolean {
        delay(connectDelayMs)
        connected = true
        emit(GlassesEvent.Battery(88, charging = true))
        return true
    }

    override suspend fun disconnect() {
        connected = false
        apOpen = false
        emit(GlassesEvent.Disconnected)
    }

    override suspend fun heartbeat(): Boolean = connected

    override suspend fun features() = Features(
        localRecording = true,
        wifiAp = true,
        livePreview = true,
        voiceWakeup = true,
        wearDetection = true,
    )

    override suspend fun battery() = BatteryStatus(88, charging = true)

    override suspend fun syncTime() = Unit

    override suspend fun startLocalRecording() {
        check(connected) { "not connected" }
        recordingSince = System.currentTimeMillis()
        emit(GlassesEvent.RecordingState(recording = true, durationSeconds = 0))
    }

    override suspend fun stopLocalRecording() {
        val since = recordingSince ?: return
        val seconds = ((System.currentTimeMillis() - since) / 1000).toInt()
        recordingSince = null
        files += RemoteFile(
            name = "REC_${(++counter).toString().padStart(4, '0')}.opus",
            sizeBytes = seconds * 3_000L,   // Opus ~24 kbps
            uploaded = false,
            durationSeconds = seconds,
        )
        emit(GlassesEvent.RecordingState(recording = false, durationSeconds = 0))
    }

    override suspend fun listFiles(): List<RemoteFile> = files.toList()

    override suspend fun openAccessPoint(): AccessPoint {
        apOpen = true
        return AccessPoint("Relay-MOCK", "12345678", "192.168.31.1")
    }

    override suspend fun closeAccessPoint() { apOpen = false }

    override suspend fun fetchFile(name: String, onProgress: (Long, Long) -> Unit): ByteArray {
        val file = files.firstOrNull { it.name == name } ?: error("no such file: $name")
        val rate = if (apOpen) wifiBytesPerSecond else bleBytesPerSecond
        val totalMs = file.sizeBytes * 1000 / rate
        val steps = min(40L, maxOf(1L, totalMs / 200))

        for (step in 1..steps) {
            delay(totalMs / steps)
            onProgress(file.sizeBytes * step / steps, file.sizeBytes)
        }
        files[files.indexOf(file)] = file.copy(uploaded = true)
        return ByteArray(file.sizeBytes.toInt())
    }

    // --- test controls -------------------------------------------------------

    fun simulateWear(worn: Boolean) = emit(GlassesEvent.Wear(worn))
    fun simulateTouch(action: String) = emit(GlassesEvent.Touch(action))
    fun simulateDisconnect() {
        connected = false
        emit(GlassesEvent.Disconnected)
    }
}
