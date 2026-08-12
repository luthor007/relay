package glass.relay.bridge.capture

/**
 * The capture fork, as two narrow device interfaces.
 *
 * `APPS-SCOPE.md` §3 says the SDK gives two different ways to get audio and
 * that they imply different products. This package builds **both**, because
 * they do different jobs:
 *
 * | | Path A — live stream | Path B — record then transfer |
 * |---|---|---|
 * | Commands | `0x0A02` mic uplink, `0x0A03` audio both ways | `0x0E04` / `0x0E05`, files over `0x0C01`-`0x0C05` or the AP |
 * | SDK | `voiceFromGlasses` / `voiceFromGlassesStatus` | `recordingToPcm`, `RecordHandle` / `IRecordCallback` |
 * | Latency | immediate | delayed until sync |
 * | Radios | both busy continuously | idle between syncs |
 * | Out of range | **the audio is lost** | the glasses still have it |
 * | Used for | the interactive voice loop, opened on intent and closed straight after | the all-day pipeline |
 *
 * "Records your whole day" is a battery problem before it is anything else, and
 * streaming sixteen hours over BLE is the expensive way to solve it — ~173 MB of
 * Opus at ~3 KB/s is about sixteen hours of transfer, longer than the day took
 * to record. So Path B carries the day and Path A carries the conversation.
 *
 * The interfaces are deliberately separate and narrow rather than one big
 * transport. `VoiceSession` cannot start a recording and
 * `LocalRecordingController` cannot open the microphone, which is a property
 * worth having in a product whose whole risk surface is "was it listening?".
 * `glass.relay.bridge.TransportAdapters` maps a `GlassesTransport` onto them.
 */

/** Path A. Protocol `0x0A02` (uplink) and `0x0A03` (both directions). */
interface VoiceChannel {
    /**
     * Open the live microphone. `0x0A02` with `0x01`.
     *
     * Roughly 3 KB/s of Opus arrives until it is closed, and both radios stay
     * busy the whole time. Open on intent; close the moment the utterance ends.
     */
    suspend fun openMicUplink()

    /** `0x0A02` with `0x00`. Must be safe to call when already closed. */
    suspend fun closeMicUplink()

    /**
     * Push the assistant's reply down `0x0A03`.
     *
     * The same ~3 KB/s applies downward, so a ten-second reply is ~30 KB and
     * takes about ten seconds to move. Callers stream it in pieces rather than
     * buffering the whole answer, or the first word arrives late.
     */
    suspend fun sendAudio(opus: ByteArray)

    /**
     * `QGAISpeakMode` via `0x0D01` — what the glasses show while we work.
     *
     * This is the cheapest latency win available: `SYSTEM.md` §7b puts the real
     * budget in perceived latency, and the device acknowledging the trigger
     * before the answer exists is most of it.
     */
    suspend fun setSpeakMode(mode: Int)
}

/** Path B. Protocol `0x0E04` control, `0x0E05` status. */
interface RecordingDevice {
    /** `0x0E04` with `0x01`. Writes to the glasses' own 4 GB, not over the radio. */
    suspend fun startLocalRecording()

    /** `0x0E04` with `0x00`. Must be safe to call when not recording. */
    suspend fun stopLocalRecording()
}

/** One frame off the live microphone. Opus unless the firmware says otherwise. */
data class AudioFrame(
    val data: ByteArray,
    /** Monotonic within a voice session. A gap means frames were lost. */
    val sequence: Int,
    /** Device clock, milliseconds — aligned by `0x0903`. */
    val deviceTimeMs: Long,
    val format: String = OPUS,
) {
    override fun equals(other: Any?): Boolean =
        this === other || (
            other is AudioFrame &&
                sequence == other.sequence &&
                deviceTimeMs == other.deviceTimeMs &&
                format == other.format &&
                data.contentEquals(other.data)
            )

    override fun hashCode(): Int {
        var result = sequence
        result = 31 * result + deviceTimeMs.hashCode()
        result = 31 * result + format.hashCode()
        result = 31 * result + data.contentHashCode()
        return result
    }

    companion object {
        /** What the device streams natively. Do not transcode without a reason. */
        const val OPUS = "opus"
        const val PCM16 = "pcm16"
    }
}
