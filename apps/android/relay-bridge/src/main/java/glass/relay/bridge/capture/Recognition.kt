package glass.relay.bridge.capture

/**
 * Speech recognition is the **app's** job, and this is the seam it plugs into.
 *
 * `SYSTEM.md` §7b puts recognition on the phone rather than the glasses, and
 * the protocol agrees: `0x0803`/`0x0805` report that something was said, and no
 * command anywhere asks the device to transcribe. The glasses are a microphone
 * and a button.
 *
 * The shape deliberately mirrors iOS's `SpeechRecognizing` protocol
 * (`apps/ios/RelayKit/CaptureCoordinator.swift`). Two platforms with the same
 * seam means one set of decisions about what a turn is, and the differences —
 * which are real and are documented on the implementations — stay inside the
 * implementations.
 *
 * Everything in this file is Android-free on purpose, so it compiles and runs
 * under `tools/verify-jvm-logic.sh`. The real binding is in
 * `AndroidSpeechRecognizer.kt`, which names platform speech symbols and is
 * therefore excluded from that harness — the same convention the vendor BLE
 * transport follows. The harness excludes on the literal token, so this file
 * does not spell the platform package even in prose.
 */
interface SpeechRecognizing {

    /** Begin a turn. Throws if the recogniser is unavailable. */
    fun start()

    /**
     * Feed a chunk as it arrives off the BLE link.
     *
     * Frames arrive [AudioFrame.OPUS]-encoded unless the firmware says
     * otherwise, and neither platform's recogniser accepts Opus. See
     * [AudioDecoding] — the conversion is a named seam rather than something an
     * implementation does quietly, because a recogniser silently fed the wrong
     * encoding returns an empty transcript, which is indistinguishable from
     * "nobody said anything".
     */
    fun append(frame: AudioFrame)

    /**
     * Stop and return the final transcript.
     *
     * An empty string means nothing was heard. That is a normal outcome and not
     * an error: a tap in a quiet room is a person changing their mind.
     */
    suspend fun finish(): String

    /** Abandon the turn. Must be safe to call when not started. */
    fun cancel()
}

/**
 * Opus → PCM, as a seam.
 *
 * Both recognisers want linear PCM: Android's `SpeechRecognizer` audio-source
 * pipe is described by an `AudioFormat.ENCODING_*` constant, and iOS's
 * `SFSpeechAudioBufferRecognitionRequest` takes `AVAudioPCMBuffer`. The glasses
 * stream Opus. So something has to decode, and naming it here means the cost
 * and the failure mode are visible rather than buried in a recogniser.
 *
 * On Android the implementation is `MediaCodec` with the platform Opus decoder.
 * On iOS there is no system Opus decoder, which is a real finding and not an
 * oversight — see `SystemSpeechRecognizer.swift`.
 */
interface AudioDecoding {
    /** Returns 16-bit little-endian PCM at [sampleRateHz], or null if it could not. */
    fun toPcm16(frame: AudioFrame): ByteArray?

    val sampleRateHz: Int
    val channelCount: Int
}

/**
 * Passes PCM straight through and refuses anything else.
 *
 * Refusing rather than guessing: a decoder that returned the Opus bytes
 * unchanged would produce a recogniser that hears static and reports silence,
 * and that failure looks exactly like a working microphone in a quiet room.
 */
class PassthroughPcm(
    override val sampleRateHz: Int = 16_000,
    override val channelCount: Int = 1,
) : AudioDecoding {
    override fun toPcm16(frame: AudioFrame): ByteArray? =
        if (frame.format == AudioFrame.PCM16) frame.data else null
}

/**
 * What a recogniser did with a turn, in terms the caller can act on.
 *
 * The distinction that matters is [Outcome.Unavailable] against
 * [Outcome.Heard] with an empty transcript. The first is a broken phone and
 * the user has to be told; the second is silence and they must not be.
 */
sealed interface Outcome {
    /** Words, possibly none. */
    data class Heard(val text: String) : Outcome

    /** The recogniser could not run at all. [reason] is for a human. */
    data class Unavailable(val reason: String) : Outcome

    /** The turn was abandoned before it finished. */
    data object Cancelled : Outcome
}

/**
 * The decisions that are the same on both platforms, kept out of both.
 *
 * Android reports failure as an integer error code and iOS as an `NSError`, and
 * neither is a decision. This turns either into the one question the app
 * actually has: does the user need to hear about this?
 */
object RecognitionPolicy {

    /**
     * The platform `SpeechRecognizer.ERROR_*` codes, by number rather than by
     * symbol so this file stays Android-free and testable. Stable since API 8.
     */
    const val ERROR_NETWORK_TIMEOUT = 1
    const val ERROR_NETWORK = 2
    const val ERROR_AUDIO = 3
    const val ERROR_SERVER = 4
    const val ERROR_CLIENT = 5
    const val ERROR_SPEECH_TIMEOUT = 6
    const val ERROR_NO_MATCH = 7
    const val ERROR_RECOGNIZER_BUSY = 8
    const val ERROR_INSUFFICIENT_PERMISSIONS = 9

    /**
     * Turns a platform error code into an outcome.
     *
     * The two that are *not* failures are the point of the function.
     * `ERROR_NO_MATCH` and `ERROR_SPEECH_TIMEOUT` both mean "the microphone
     * worked and nobody said anything a recogniser could use", which is a
     * normal end to a turn. Reporting them as errors would make the assistant
     * apologise every time somebody taps the glasses and thinks better of it.
     */
    fun fromAndroidError(code: Int): Outcome = when (code) {
        ERROR_NO_MATCH, ERROR_SPEECH_TIMEOUT -> Outcome.Heard("")
        ERROR_INSUFFICIENT_PERMISSIONS ->
            Outcome.Unavailable("Relay does not have permission to use speech recognition")
        ERROR_NETWORK, ERROR_NETWORK_TIMEOUT ->
            Outcome.Unavailable("speech recognition needs a network connection right now")
        ERROR_RECOGNIZER_BUSY ->
            Outcome.Unavailable("something else on the phone is using speech recognition")
        ERROR_AUDIO -> Outcome.Unavailable("the phone could not read the audio")
        ERROR_SERVER -> Outcome.Unavailable("the speech recognition service failed")
        ERROR_CLIENT -> Outcome.Unavailable("speech recognition rejected the request")
        else -> Outcome.Unavailable("speech recognition failed (code $code)")
    }

    /**
     * Picks the transcript to keep from a partial stream.
     *
     * Recognisers revise: "set the" becomes "set the timer" becomes "set a
     * timer for ten". The last one wins, and a later *shorter* one still wins,
     * because a recogniser that shortens has usually dropped a misheard word
     * rather than lost information. What must never win is an empty revision
     * over real text — that is the recogniser resetting, not correcting.
     */
    fun best(partials: List<String>): String {
        var kept = ""
        for (p in partials) {
            val t = p.trim()
            if (t.isNotEmpty()) kept = t
        }
        return kept
    }

    /**
     * Whether a partial is worth sending upstream yet.
     *
     * `SYSTEM.md` §7b wants partials so the prompt is ready the moment someone
     * stops talking, and `relayd`'s router drops any utterance whose `Final` is
     * false — so a partial costs a websocket frame and buys latency. It is
     * still not worth sending the same text twice, or a single character.
     */
    fun shouldEmit(previous: String, next: String): Boolean {
        val n = next.trim()
        return n.isNotEmpty() && n != previous.trim() && n.length > 1
    }
}

/**
 * Returns whatever it was told to. Deterministic, no audio stack.
 *
 * The Android twin of iOS's `MockRecognizer`, so `VoiceSession` can be driven
 * end to end under the JVM harness.
 */
class MockRecognizer(
    private val transcripts: List<String> = emptyList(),
) : SpeechRecognizing {

    private var running = false
    private var turn = 0
    var framesSeen: Int = 0
        private set

    val isRunning: Boolean get() = running

    override fun start() {
        running = true
    }

    override fun append(frame: AudioFrame) {
        if (!running) return
        framesSeen++
    }

    override suspend fun finish(): String {
        if (!running) return ""
        running = false
        val text = transcripts.getOrElse(turn) { "" }
        turn++
        return text
    }

    override fun cancel() {
        running = false
    }
}
