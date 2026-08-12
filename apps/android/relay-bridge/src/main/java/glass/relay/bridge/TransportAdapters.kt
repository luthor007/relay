package glass.relay.bridge

import glass.relay.bridge.capture.RecordingDevice
import glass.relay.bridge.capture.VoiceChannel
import glass.relay.bridge.storage.StoragePolicy

/**
 * Narrow views onto a [GlassesTransport].
 *
 * The capture logic in `glass.relay.bridge.capture` deliberately does not depend
 * on the transport: [glass.relay.bridge.capture.VoiceSession] cannot start a
 * recording and [glass.relay.bridge.capture.LocalRecordingController] cannot
 * open the microphone, which is worth having in a product whose whole trust
 * surface is "was it listening?".
 *
 * Adapters rather than a wider interface, so that none of that logic drags in
 * `android.*` and all of it stays testable on a plain JVM — see
 * `apps/android/tools/verify-jvm-logic.sh`.
 */

/** Path B, from whatever transport this build is using. */
fun GlassesTransport.asRecordingDevice(): RecordingDevice = object : RecordingDevice {
    override suspend fun startLocalRecording() = this@asRecordingDevice.startLocalRecording()
    override suspend fun stopLocalRecording() = this@asRecordingDevice.stopLocalRecording()
}

/**
 * Path A, for transports that have it.
 *
 * `GlassesTransport` deliberately does not carry the live-audio calls: the
 * always-on service does not need them, and every method on that interface is
 * surface the vendor adapter has to keep working. A transport that supports the
 * interactive loop implements [GlassesVoice] as well; one that does not returns
 * null here, and the UI disables the talk button rather than failing at the
 * moment someone taps it.
 */
fun GlassesTransport.asVoiceChannel(): VoiceChannel? = this as? GlassesVoice

/**
 * The live-audio half of the device.
 *
 * `MockGlassesTransport` implements this, so the whole voice loop is driveable
 * with no glasses on the desk. `VendorGlassesTransport` does **not** yet: the
 * vendor path needs `voiceFromGlasses` / `voiceFromGlassesStatus` wired to the
 * SDK's `AudioRawDataResponse`, and no line of that has ever run against
 * hardware. Claiming it works would be worse than saying it is missing.
 */
interface GlassesVoice : VoiceChannel

/**
 * One framed packet in, one out.
 *
 * This is the whole vendor surface the command screen needs. `CommandConsole`
 * builds the frame — id, type, sequence, payload, CRC-16/MODBUS — so the
 * adapter's job is to write bytes and hand back whatever came home. Sixty typed
 * SDK methods would be sixty things nobody can test until hardware arrives;
 * this is one, and the codec behind it has vectors.
 *
 * The vendor SDK's `LargeDataHandler.glassesControl(payload, callback)` is the
 * intended landing point, with the open question already named in
 * `VendorGlassesTransport.CONTROL_PAYLOAD_IS_FRAMED`: whether the SDK wants a
 * bare `[command_id][args]` payload or a fully framed packet. If it wants the
 * bare form, this adapter strips the frame the console built; the codec is
 * needed either way, because the reply has to be decoded.
 */
interface GlassesRawCommands {
    suspend fun sendFrame(frame: ByteArray): ByteArray?
}

/** What `0x0909` / `0x091C` answers, in the shape the storage policy wants. */
interface DiskProbe {
    suspend fun diskInfo(): StoragePolicy.DiskSnapshot
    suspend fun inventory(): StoragePolicy.Inventory
}

/**
 * Build an inventory from a file listing.
 *
 * `uploaded` is the firmware's own answer (`0x0E01`), not ours. Classifying by
 * extension is crude but it is what the listing gives us, and getting it wrong
 * only mis-labels a warning — it never authorises a deletion, because nothing
 * here deletes anything.
 */
fun inventoryOf(files: List<RemoteFile>): StoragePolicy.Inventory {
    var unuploadedAudio = 0L
    var uploadedAudio = 0L
    var video = 0L
    var photo = 0L

    for (file in files) {
        val name = file.name.lowercase()
        when {
            name.endsWith(".mp4") || name.endsWith(".mov") -> video += file.sizeBytes
            name.endsWith(".jpg") || name.endsWith(".jpeg") || name.endsWith(".png") ->
                photo += file.sizeBytes
            file.uploaded -> uploadedAudio += file.sizeBytes
            else -> unuploadedAudio += file.sizeBytes
        }
    }

    return StoragePolicy.Inventory(
        unuploadedAudioBytes = unuploadedAudio,
        uploadedAudioBytes = uploadedAudio,
        videoBytes = video,
        photoBytes = photo,
    )
}
