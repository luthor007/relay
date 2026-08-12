package glass.relay.bridge

import glass.relay.bridge.protocol.FrameDecode
import glass.relay.bridge.protocol.Packet
import glass.relay.bridge.protocol.PacketType
import glass.relay.bridge.protocol.decodeFrame
import glass.relay.bridge.storage.StoragePolicy
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
) : BaseGlassesTransport(), GlassesVoice, DiskProbe, GlassesRawCommands {

    private var connected = false
    private var apOpen = false
    private var recordingSince: Long? = null
    private val files = mutableListOf<RemoteFile>()
    private var counter = 0

    /**
     * Every command that reached the "device", in order.
     *
     * Tests assert that the app reached for `0x0A02` rather than that a promise
     * resolved, which is the same discipline `glasses/bridge/src/mock.ts` keeps.
     */
    val commandLog = mutableListOf<String>()

    var micUplinkOpen: Boolean = false
        private set

    override suspend fun connect(): Boolean {
        delay(connectDelayMs)
        connected = true
        emit(GlassesEvent.Battery(88, charging = true))
        return true
    }

    override suspend fun disconnect() {
        connected = false
        apOpen = false
        micUplinkOpen = false
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
        commandLog += "0x0E04:start"
        recordingSince = System.currentTimeMillis()
        emit(GlassesEvent.RecordingState(recording = true, durationSeconds = 0))
    }

    override suspend fun stopLocalRecording() {
        commandLog += "0x0E04:stop"
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
        commandLog += "0x090B:open"
        apOpen = true
        return AccessPoint("Relay-MOCK", "12345678", "192.168.31.1")
    }

    override suspend fun closeAccessPoint() {
        commandLog += "0x090B:close"
        apOpen = false
    }

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

    // --- raw command channel ---------------------------------------------------

    /**
     * Decode what the console built, log it, and answer.
     *
     * The reply is an **echo with the response type set** and no invented
     * payload. A mock that answered `GET_BATTERY` with a plausible-looking
     * `[0x5A, 0x01]` would be teaching the UI a byte layout the spec does not
     * attest (`APPS-SCOPE.md` §5.1), and the first real device would disagree.
     */
    override suspend fun sendFrame(frame: ByteArray): ByteArray? {
        check(connected) { "not connected" }
        val decoded = decodeFrame(frame)
        check(decoded is FrameDecode.Ok) { "the app built a frame that does not decode: $decoded" }

        val packet = Packet.decode(decoded.data)
        commandLog += "0x%04X:%s".format(packet.commandId, packet.payload.joinToString("") { "%02x".format(it) })
        delay(frame.size * 1000L / bleBytesPerSecond)

        return Packet(
            commandId = packet.commandId,
            type = PacketType.RESPONSE,
            sequence = packet.sequence,
        ).toFrame()
    }

    // --- storage --------------------------------------------------------------

    /**
     * The device's 4 GB, minus whatever the mock has "recorded".
     *
     * Settable so a test or a demo can put the app into the states that matter —
     * a device with twenty minutes of video on it, or one that is nearly full —
     * without filling a real 4 GB first.
     */
    var simulatedVideoBytes: Long = 0

    override suspend fun diskInfo(): StoragePolicy.DiskSnapshot {
        val used = files.sumOf { it.sizeBytes } + simulatedVideoBytes
        return StoragePolicy.DiskSnapshot(
            totalBytes = StoragePolicy.DEVICE_TOTAL_BYTES,
            freeBytes = maxOf(0, StoragePolicy.DEVICE_TOTAL_BYTES - used),
        )
    }

    override suspend fun inventory(): StoragePolicy.Inventory =
        inventoryOf(files).copy(videoBytes = simulatedVideoBytes)

    // --- Path A: the live voice loop -----------------------------------------
    //
    // Implemented here so the interactive loop can be developed with no glasses
    // on the desk. The rates are the documented ones (SYSTEM.md §3.1): the mic
    // and the downlink share ~3 KB/s, which is why sendAudio takes about as long
    // as the reply lasts rather than resolving instantly. A mock that returns
    // immediately teaches the UI that speech is free.

    override suspend fun openMicUplink() {
        check(connected) { "not connected" }
        check(!micUplinkOpen) { "the mic uplink is already open" }
        micUplinkOpen = true
        commandLog += "0x0A02:start"
    }

    override suspend fun closeMicUplink() {
        micUplinkOpen = false
        commandLog += "0x0A02:stop"
    }

    override suspend fun sendAudio(opus: ByteArray) {
        check(connected) { "not connected" }
        commandLog += "0x0A03:${opus.size}"
        delay(opus.size * 1000L / bleBytesPerSecond)
    }

    override suspend fun setSpeakMode(mode: Int) {
        commandLog += "0x0D01:mode=0x%02X".format(mode)
    }

    // --- test controls -------------------------------------------------------

    /** One 200 ms chunk of Opus off the microphone, at the documented rate. */
    fun simulateMicChunk(sequence: Int): ByteArray? {
        if (!micUplinkOpen) return null
        return ByteArray((bleBytesPerSecond / 5).toInt()).also {
            emit(GlassesEvent.AudioChunk(it, sequence))
        }
    }

    fun simulateWear(worn: Boolean) = emit(GlassesEvent.Wear(worn))
    fun simulateTouch(action: String) = emit(GlassesEvent.Touch(action))
    fun simulateDisconnect() {
        connected = false
        emit(GlassesEvent.Disconnected)
    }
}
