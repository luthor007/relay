package glass.relay.bridge.vendor

import android.app.Application
import android.content.Context
import android.util.Log
import com.glasses.ble.base.bluetooth.BleOperateManager
import com.glasses.ble.base.bluetooth.DeviceManager
import com.glasses.ble.base.communication.ILargeDataResponse
import com.glasses.ble.base.communication.LargeDataHandler
import com.glasses.ble.base.communication.bigData.resp.BatteryResponse
import com.glasses.ble.base.communication.bigData.resp.DeviceInfoResponse
import com.glasses.ble.base.communication.bigData.resp.GlassModelControlResponse
import com.glasses.ble.base.communication.bigData.resp.GlassesTouchSupportRsp
import com.glasses.ble.base.communication.bigData.resp.GlassesWearRsp
import com.glasses.ble.base.communication.bigData.resp.SyncTimeResponse
import com.glasses.ble.base.communication.entity.RecordEntity
import com.glasses.ble.base.communication.file.IRecordCallback
import com.glasses.ble.base.communication.file.RecordHandle
import glass.relay.bridge.AccessPoint
import glass.relay.bridge.BaseGlassesTransport
import glass.relay.bridge.BatteryStatus
import glass.relay.bridge.Features
import glass.relay.bridge.GlassesEvent
import glass.relay.bridge.RemoteFile
import kotlinx.coroutines.CancellableContinuation
import kotlinx.coroutines.TimeoutCancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withTimeout
import kotlinx.coroutines.withTimeoutOrNull
import java.io.ByteArrayOutputStream
import java.nio.ByteBuffer
import java.nio.ByteOrder
import java.util.concurrent.atomic.AtomicBoolean
import kotlin.coroutines.resume

/**
 * [glass.relay.bridge.GlassesTransport] over the Shenzhen QC.wireless SDK.
 *
 * ## What is verified and what is not
 *
 * Every symbol used here was read off the shipping AAR with `javap`, so the code
 * compiles against the real API rather than against the documentation, which is
 * wrong in places. What is **not** verified is behaviour: no call in this file
 * has been run against glasses, because the units are in Quebec. Specifically:
 *
 *  - [CONTROL_PAYLOAD_IS_FRAMED] — whether `glassesControl` wants a bare
 *    `[command_id][args]` payload or a full framed packet with the `0xA5` prefix
 *    and CRC. The SDK's own framing lives below this call, which is why the bare
 *    payload is the default, but the spec contains no worked example
 *    (`glasses/NOTES.md`) and this is a one-line flip if it is wrong.
 *  - Wear polarity — [GlassesWearRsp.isOpen] is documented as neither "detection
 *    is enabled" nor "currently worn". It is read as *worn* here because that is
 *    the only reading that makes `ACTION_DEVICE_WEAR` a useful notification.
 *  - The connect handshake. The SDK exposes no registrable connection callback,
 *    only `isConnected`/`isReady`, so [connect] polls. Poll intervals are a
 *    guess tuned to feel instant without spinning.
 *
 * Each of those is a single named constant or a single function, deliberately,
 * so the first session with real hardware is an afternoon of flipping constants
 * rather than a rewrite.
 */
class VendorGlassesTransport(context: Context) : BaseGlassesTransport() {

    private val app = context.applicationContext as Application
    private val ble: BleOperateManager by lazy { BleOperateManager.getInstance(app) }
    private val large: LargeDataHandler by lazy { LargeDataHandler.getInstance() }
    private val records: RecordHandle by lazy { RecordHandle.getInstance() }

    private val prefs = context.applicationContext
        .getSharedPreferences("relay.vendor", Context.MODE_PRIVATE)

    private val listenersRegistered = AtomicBoolean(false)

    /**
     * Serialises every [RecordHandle] operation.
     *
     * Not defensive coding — a hard requirement of the SDK's shape. Callbacks are
     * added with `registerCallback(IRecordCallback)` but `removeCallback` takes
     * an unrelated `ICallback` interface, so the *only* way to deregister is
     * `clearCallback()`, which clears everyone's. Two overlapping transfers would
     * silently steal each other's callbacks and hang.
     */
    private val recordLock = Mutex()

    /**
     * Written by the pairing flow. Without it there is nothing to connect to —
     * the SDK scans for a specific MAC rather than presenting a picker.
     */
    var pairedMac: String?
        get() = prefs.getString(KEY_MAC, null)
        set(value) = prefs.edit().putString(KEY_MAC, value).apply()

    // ------------------------------------------------------------- connection

    override suspend fun connect(): Boolean {
        val mac = pairedMac ?: run {
            Log.w(TAG, "no paired MAC; run pairing first")
            return false
        }

        DeviceManager.getInstance().deviceAddress = mac
        ble.setApplication(app)
        ble.reConnectMac = mac
        ble.setNeedConnect(true)

        // connectWithScan rather than connectDirectly: a direct connect to a MAC
        // the stack has not seen since boot fails on several OEM BLE stacks,
        // and the scan costs about a second.
        ble.connectWithScan(mac)

        val ready = withTimeoutOrNull(CONNECT_TIMEOUT_MS) {
            while (!ble.isReady) delay(CONNECT_POLL_MS)
            true
        } ?: false

        if (!ready) {
            Log.w(TAG, "connect timed out after ${CONNECT_TIMEOUT_MS}ms")
            return false
        }

        large.initEnable()
        registerListeners()
        return true
    }

    override suspend fun disconnect() {
        ble.setNeedConnect(false)
        ble.disconnect()
        large.removeBatteryCallBack(CALLBACK_TAG)
        large.removeOutDeviceListener(LargeDataHandler.ACTION_DEVICE_WEAR.toInt())
        large.disEnable()
        listenersRegistered.set(false)
        emit(GlassesEvent.Disconnected)
    }

    override suspend fun heartbeat(): Boolean = ble.isConnected

    /**
     * Registered once per connection. The SDK keeps these in a map keyed by
     * action byte and silently replaces duplicates, so re-registering on every
     * reconnect would be harmless but would also hide a leak — the flag makes
     * double registration visible instead.
     */
    private fun registerListeners() {
        if (!listenersRegistered.compareAndSet(false, true)) return

        large.addOutDeviceListener(
            LargeDataHandler.ACTION_DEVICE_WEAR.toInt(),
            ILargeDataResponse<GlassesWearRsp> { _, rsp ->
                emit(GlassesEvent.Wear(rsp.isOpen))
            },
        )

        large.addBatteryCallBack(CALLBACK_TAG) { _, rsp: BatteryResponse ->
            emit(GlassesEvent.Battery(rsp.battery, rsp.isCharging))
        }
    }

    // ------------------------------------------------------------ capabilities

    override suspend fun features(): Features {
        val info = await<DeviceInfoResponse> { large.syncDeviceInfo(it) }
        val wear = awaitOrNull<GlassesTouchSupportRsp> { large.wearFunctionSupport(it) }

        // Firmware without a WiFi stack reports an empty WiFi version, and that
        // is the only honest signal available for the AP capability — the
        // capability bitmap at 0x0005 is not exposed through this SDK.
        val hasWifi = !info.wifiFirmwareVersion.isNullOrBlank()

        return Features(
            localRecording = true,
            wifiAp = hasWifi,
            livePreview = hasWifi,
            voiceWakeup = true,
            wearDetection = wear != null,
        )
    }

    override suspend fun battery(): BatteryStatus {
        val rsp = await<BatteryResponse> { callback ->
            large.addBatteryCallBack(ONESHOT_TAG, callback)
            large.syncBattery()
        }
        large.removeBatteryCallBack(ONESHOT_TAG)
        return BatteryStatus(rsp.battery, rsp.isCharging)
    }

    override suspend fun syncTime() {
        await<SyncTimeResponse> { large.syncTime(it) }
    }

    // --------------------------------------------------------------- recording

    override suspend fun startLocalRecording() {
        control(LOCAL_RECORDING_CONTROL, RECORDING_START)
    }

    override suspend fun stopLocalRecording() {
        control(LOCAL_RECORDING_CONTROL, RECORDING_STOP)
    }

    /** Fires a control command and waits for the device to acknowledge it. */
    private suspend fun control(command: Int, vararg args: Byte): GlassModelControlResponse =
        await { callback -> large.glassesControl(payload(command, *args), callback) }

    /**
     * `[command_id_le][args]`.
     *
     * Little-endian per the blanket byte-order rule in §一 of the protocol spec.
     * See [CONTROL_PAYLOAD_IS_FRAMED] for the part that is genuinely uncertain.
     */
    private fun payload(command: Int, vararg args: Byte): ByteArray {
        require(!CONTROL_PAYLOAD_IS_FRAMED) {
            "framed control payloads are not implemented; use glasses/protocol/frame.py " +
                "to generate them and port encode_frame here"
        }
        return ByteBuffer.allocate(2 + args.size)
            .order(ByteOrder.LITTLE_ENDIAN)
            .putShort(command.toShort())
            .put(args)
            .array()
    }

    // --------------------------------------------------------------- transfers

    override suspend fun listFiles(): List<RemoteFile> = recordLock.withLock {
        suspendCancellableCoroutine { cont ->
            val callback = object : RecordCallbackAdapter() {
                override fun onFileNames(names: ArrayList<RecordEntity>?) {
                    records.clearCallback()
                    cont.resume(names.orEmpty().map { it.toRemoteFile() })
                }

                override fun onActionResult(code: Int) {
                    if (code == RESULT_OK) return
                    records.clearCallback()
                    cont.resumeWith(Result.failure(IllegalStateException("listFiles failed: $code")))
                }
            }
            records.registerCallback(callback)
            records.initRegister()
            records.setCurrFileType(FILE_TYPE_RECORDING)
            records.start(FILE_TYPE_RECORDING)

            cont.invokeOnCancellation { records.clearCallback() }
        }
    }

    /**
     * Pulls one file off the glasses.
     *
     * Deliberately has no timeout. Over BLE this genuinely takes about as long as
     * the recording did — an hour of audio is an hour of transfer at ~3 KB/s,
     * which is why bulk sync rides the access point instead (ARCHITECTURE.md
     * §5.3). A timeout here would abort correct, slow transfers.
     */
    override suspend fun fetchFile(
        name: String,
        onProgress: (Long, Long) -> Unit,
    ): ByteArray {
        // The size has to be looked up before the transfer, because the SDK
        // reports progress as a fraction and callers want bytes.
        val total = listFiles().firstOrNull { it.name == name }?.sizeBytes ?: 0L

        return recordLock.withLock {
            suspendCancellableCoroutine { cont ->
                val buffer = ByteArrayOutputStream()

                val callback = object : RecordCallbackAdapter() {
                    override fun onReceiver(chunk: ByteArray?) {
                        chunk ?: return
                        buffer.write(chunk)
                    }

                    override fun onProgress(fraction: Float) {
                        if (total > 0) onProgress((fraction * total).toLong(), total)
                    }

                    override fun onComplete() {
                        records.clearCallback()
                        cont.resume(buffer.toByteArray())
                    }

                    override fun onActionResult(code: Int) {
                        if (code == RESULT_OK) return
                        records.clearCallback()
                        cont.resumeWith(
                            Result.failure(IllegalStateException("fetchFile($name) failed: $code")),
                        )
                    }
                }

                records.registerCallback(callback)
                records.readRecordFile(FILE_TYPE_RECORDING, name)

                cont.invokeOnCancellation {
                    records.clearCallback()
                    records.endAndRelease()
                }
            }
        }
    }

    // -------------------------------------------------------------- access point

    override suspend fun openAccessPoint(): AccessPoint {
        control(WIFI_AP_CONTROL, AP_OPEN)

        // The SSID and password are set by the firmware and read back from the
        // SDK's device model rather than chosen by us.
        val manager = DeviceManager.getInstance()
        return AccessPoint(
            ssid = manager.wifiName ?: error("device reported no AP SSID"),
            password = manager.wifiPassword ?: error("device reported no AP password"),
            host = AP_HOST,
        )
    }

    override suspend fun closeAccessPoint() {
        control(WIFI_AP_CONTROL, AP_CLOSE)
    }

    // --------------------------------------------------------------- callbacks

    /** Bridges the SDK's one-shot callbacks into `suspend`, with a timeout. */
    private suspend inline fun <reified T : com.glasses.ble.base.communication.bigData.resp.BaseResponse>
        await(crossinline block: (ILargeDataResponse<T>) -> Unit): T =
        withTimeout(COMMAND_TIMEOUT_MS) {
            suspendCancellableCoroutine { cont: CancellableContinuation<T> ->
                block(ILargeDataResponse { _, rsp -> if (cont.isActive) cont.resume(rsp) })
            }
        }

    private suspend inline fun <reified T : com.glasses.ble.base.communication.bigData.resp.BaseResponse>
        awaitOrNull(crossinline block: (ILargeDataResponse<T>) -> Unit): T? =
        try {
            await(block)
        } catch (e: TimeoutCancellationException) {
            null
        }

    /** `IRecordCallback` has five methods and each use needs two. */
    private abstract class RecordCallbackAdapter : IRecordCallback {
        override fun onFileNames(names: ArrayList<RecordEntity>?) = Unit
        override fun onProgress(fraction: Float) = Unit
        override fun onComplete() = Unit
        override fun onReceiver(chunk: ByteArray?) = Unit
        override fun onActionResult(code: Int) = Unit
    }

    private companion object {
        const val TAG = "RelayVendor"
        const val KEY_MAC = "paired_mac"
        const val CALLBACK_TAG = "relay"
        const val ONESHOT_TAG = "relay-oneshot"

        const val CONNECT_TIMEOUT_MS = 20_000L
        const val CONNECT_POLL_MS = 120L
        const val COMMAND_TIMEOUT_MS = 8_000L

        /** See the class docs. Flip only with a capture to prove it. */
        const val CONTROL_PAYLOAD_IS_FRAMED = false

        // From glasses/protocol/commands.py, which transcribes all 92 from the spec.
        const val LOCAL_RECORDING_CONTROL = 0x0E04
        const val WIFI_AP_CONTROL = 0x090B

        const val RECORDING_START: Byte = 0x01
        const val RECORDING_STOP: Byte = 0x00
        const val AP_OPEN: Byte = 0x01
        const val AP_CLOSE: Byte = 0x00

        const val FILE_TYPE_RECORDING = 0
        const val RESULT_OK = 0

        /** The glasses' own AP always hands out this gateway. */
        const val AP_HOST = "192.168.31.1"
    }
}

private fun RecordEntity.toRemoteFile() = RemoteFile(
    name = fileName.orEmpty(),
    sizeBytes = length.toLong(),
    uploaded = false,
    // The listing carries a byte count and nothing else. Duration is derivable
    // from it (Opus at ~24 kbps) but a derived figure shown as a fact is how you
    // end up with a UI that confidently reports the wrong length.
    durationSeconds = null,
)
