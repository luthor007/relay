package glass.relay.bridge

import android.util.Log
import glass.relay.bridge.audio.AudioRing
import glass.relay.bridge.capture.AudioFrame
import glass.relay.bridge.capture.VoiceSession

/**
 * Where live microphone frames go.
 *
 * Path A only: this is fed by [VoiceSession] and receives nothing outside a
 * session the user started. It holds the [AudioRing] between the BLE callback
 * thread and the recogniser, and it exists as a named class rather than a
 * lambda so that the two things that must never happen quietly are visible:
 *
 *  - a refused frame is logged and counted, never dropped in silence
 *  - a gap is recorded with its sequence range, so the transcript can mark the
 *    hole instead of splicing across it
 *
 * The recogniser itself is not here. `SYSTEM.md` §7b puts speech recognition on
 * the phone rather than the glasses, and which recogniser is a separate
 * decision; this hands over frames and a turn boundary, which is all any of
 * them need.
 */
class CaptureAudioSink(
    private val ring: AudioRing = AudioRing(
        AudioRing.Config(
            // The live path may drop the oldest: the useful audio is the words
            // being said right now. Stored capture must never use this policy.
            overflow = AudioRing.Overflow.DropOldest,
        ),
    ),
    private val onTurn: (turnId: Long, frames: List<AudioFrame>, gaps: List<AudioRing.Gap>) -> Unit = { _, _, _ -> },
) : VoiceSession.AudioSink {

    val buffer: AudioRing get() = ring

    /** True while the consumer is behind and the device should be asked to slow. */
    val shouldThrottle: Boolean get() = ring.shouldThrottle

    override fun onFrame(frame: AudioFrame) {
        when (val result = ring.offer(frame)) {
            is AudioRing.Result.Refused ->
                Log.w(TAG, "dropped frame ${result.sequence}: recogniser is not keeping up")
            is AudioRing.Result.AcceptedWithGap ->
                Log.w(
                    TAG,
                    "audio gap: sequences ${result.gap.fromSequence}-${result.gap.toSequence}, " +
                        "${result.gap.bytes} bytes",
                )
            else -> Unit
        }
    }

    override fun onTurnClosed(turnId: Long, reason: VoiceSession.CloseReason) {
        val frames = ring.drain()
        val gaps = ring.gaps.toList()
        ring.gaps.clear()
        if (reason == VoiceSession.CloseReason.Failed) {
            // The uplink may still be open. Worth a log line of its own: it is
            // the only failure whose consequence is a live microphone.
            Log.e(TAG, "turn $turnId ended with a failed close — the mic may still be open")
        }
        onTurn(turnId, frames, gaps)
    }

    private companion object {
        const val TAG = "RelayAudioSink"
    }
}
