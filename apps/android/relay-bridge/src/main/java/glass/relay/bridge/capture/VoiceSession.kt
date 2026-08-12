package glass.relay.bridge.capture

import glass.relay.bridge.commands.CommandCatalog
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock

/**
 * Path A — the interactive voice loop.
 *
 * One rule shapes everything here: **the microphone is open only while someone
 * is speaking to the assistant, and it is closed the instant they stop.** Not
 * when the answer arrives, not when playback ends — the instant the utterance
 * ends. Path A is the heaviest thing either battery does, and a voice loop that
 * leaves the uplink open through a four-second model call has paid for four
 * seconds of full-rate radio on both devices for nothing.
 *
 * Everything else follows from that:
 *
 *  - [offerAudio] **refuses frames when the session is not listening**, and
 *    counts the refusals. That is not defensive coding: it is the mechanical
 *    guarantee that no audio is captured outside a session the user started, in
 *    a product where "was it listening?" is the entire trust surface.
 *  - A hung endpoint detector cannot hold the mic all day. [Config.maxListenMs]
 *    closes it and moves on.
 *  - The mic is closed in a `finally`. A channel that throws mid-turn must not
 *    leave the uplink open; that failure is invisible until the battery is flat.
 *  - Barge-in works: a trigger while the assistant is talking stops it and
 *    listens again, because interrupting is how people actually talk.
 *
 * The recogniser is the **app's**, not the device's. `SYSTEM.md` §7b: there is
 * no command anywhere in the protocol that asks the glasses to transcribe. They
 * are a microphone and a button. So this class never asks for text — it hands
 * frames to a sink and waits for the app to say the turn is over.
 */
class VoiceSession(
    private val channel: VoiceChannel,
    private val scope: CoroutineScope,
    private val sink: AudioSink,
    private val config: Config = Config(),
    /**
     * Consent, checked at the moment of the trigger rather than cached.
     *
     * ARCHITECTURE.md §6 — and a cached "yes" from this morning is not consent
     * for a conversation happening now.
     */
    private val consentGranted: () -> Boolean = { true },
) {

    data class Config(
        /**
         * Hard cap on one utterance. Long enough for a rambling question,
         * short enough that a stuck endpoint costs seconds, not a battery.
         */
        val maxListenMs: Long = 30_000,
        /** How long the assistant may hold the floor before we give it back. */
        val maxSpeakMs: Long = 120_000,
    )

    /** Where frames go while listening. The app's recogniser sits behind this. */
    interface AudioSink {
        fun onFrame(frame: AudioFrame)

        /** The turn ended; nothing more is coming for [turnId]. */
        fun onTurnClosed(turnId: Long, reason: CloseReason)
    }

    enum class State {
        Idle,

        /** Mic open. The only state in which audio is accepted. */
        Listening,

        /** Mic closed, model working. The device shows a thinking state. */
        Thinking,

        /** Reply streaming down `0x0A03`. */
        Speaking,
    }

    enum class Trigger {
        /** A tap on the glasses, bound through `0x0601`/`0x0602`. */
        Touch,

        /** Wake word spotted by the firmware and reported on `0x0805`. */
        WakeWord,

        /** A button in the app. Works in a quiet room and in a loud one. */
        AppButton,
    }

    enum class CloseReason {
        /** The app's endpoint detector said the utterance finished. */
        Endpoint,

        /** [Config.maxListenMs] elapsed. */
        Timeout,

        /** The user or the app cancelled. */
        Cancelled,

        /** A new trigger arrived mid-turn. */
        BargeIn,

        /**
         * The channel threw while closing the uplink.
         *
         * Reported rather than swallowed, because this is the one failure whose
         * consequence is a microphone we believe is shut and is not.
         */
        Failed,
    }

    data class Rejection(val reason: String)

    private val _state = MutableStateFlow(State.Idle)
    val state: StateFlow<State> = _state.asStateFlow()

    private val lock = Mutex()
    private var listenWatchdog: Job? = null
    private var turn = 0L

    /**
     * Frames offered while the session was not listening.
     *
     * Should be zero in a healthy build. Anything else means the device is
     * streaming when we did not ask it to, which is worth surfacing rather than
     * discarding quietly.
     */
    var strayFrames: Int = 0
        private set

    var acceptedFrames: Int = 0
        private set

    val currentTurn: Long get() = turn

    /**
     * Open the mic for one utterance.
     *
     * Returns null on success or a [Rejection] explaining why not. A trigger
     * during `Speaking` is barge-in and succeeds; a trigger during `Listening`
     * is a no-op, because a double tap should not restart the sentence someone
     * is halfway through.
     */
    suspend fun trigger(source: Trigger): Rejection? {
        if (!consentGranted()) {
            return Rejection("capture consent has not been given")
        }

        lock.withLock {
            when (_state.value) {
                State.Listening -> return null
                State.Speaking, State.Thinking -> closeTurnLocked(CloseReason.BargeIn)
                State.Idle -> Unit
            }

            turn += 1
            return try {
                channel.openMicUplink()
                _state.value = State.Listening
                startWatchdog()
                // Acknowledge on the device before the answer exists. SYSTEM.md
                // §7b: most of the perceived latency budget is bought here.
                runCatching { channel.setSpeakMode(CommandCatalog.SpeakMode.THINKING_START) }
                null
            } catch (error: Exception) {
                // Never leave the uplink open on a failed open.
                runCatching { channel.closeMicUplink() }
                _state.value = State.Idle
                Rejection("could not open the microphone: ${error.message}")
            }
        }
    }

    /**
     * Hand a frame to the recogniser, if we asked for it.
     *
     * Returns false when the session is not listening, and counts it. The
     * device streaming outside a session is a bug worth seeing; silently
     * accepting the frames would turn it into a privacy incident.
     */
    fun offerAudio(frame: AudioFrame): Boolean {
        if (_state.value != State.Listening) {
            strayFrames += 1
            return false
        }
        acceptedFrames += 1
        sink.onFrame(frame)
        return true
    }

    /**
     * The utterance finished. **Closes the microphone immediately** — before the
     * model is called, before anything is spoken.
     */
    suspend fun endListening(reason: CloseReason = CloseReason.Endpoint) {
        lock.withLock {
            if (_state.value != State.Listening) return
            closeMicLocked(reason)
            _state.value = State.Thinking
        }
    }

    /** Begin streaming a reply. The mic is already closed by this point. */
    suspend fun beginSpeaking() {
        lock.withLock {
            if (_state.value == State.Listening) closeMicLocked(CloseReason.Endpoint)
            _state.value = State.Speaking
            runCatching { channel.setSpeakMode(CommandCatalog.SpeakMode.START) }
        }
    }

    /**
     * Push one piece of the reply down `0x0A03`.
     *
     * Chunked rather than whole-answer because the downlink is the same ~3 KB/s
     * as the uplink: buffering a whole reply before sending the first byte adds
     * its entire duration to the wait.
     */
    suspend fun speak(opus: ByteArray) {
        if (_state.value != State.Speaking) return
        channel.sendAudio(opus)
        // The device wants a keep-alive while a long reply streams, per
        // QGAISpeakMode. Dropping it mid-reply truncates the answer.
        runCatching { channel.setSpeakMode(CommandCatalog.SpeakMode.HOLD) }
    }

    /** The reply finished normally. */
    suspend fun finish() {
        lock.withLock {
            if (_state.value == State.Idle) return
            closeTurnLocked(CloseReason.Endpoint)
        }
    }

    /** Abandon whatever is happening and close the mic. */
    suspend fun cancel() {
        lock.withLock {
            if (_state.value == State.Idle) return
            closeTurnLocked(CloseReason.Cancelled)
        }
    }

    private fun startWatchdog() {
        listenWatchdog?.cancel()
        listenWatchdog = scope.launch {
            delay(config.maxListenMs)
            // A stuck endpoint detector must not hold the microphone open for
            // the rest of the day. Close it and let the turn proceed.
            endListening(CloseReason.Timeout)
        }
    }

    private suspend fun closeMicLocked(reason: CloseReason) {
        listenWatchdog?.cancel()
        listenWatchdog = null
        try {
            channel.closeMicUplink()
        } catch (error: Exception) {
            sink.onTurnClosed(turn, CloseReason.Failed)
            _state.value = State.Idle
            throw error
        }
        sink.onTurnClosed(turn, reason)
    }

    private suspend fun closeTurnLocked(reason: CloseReason) {
        if (_state.value == State.Listening) {
            runCatching { closeMicLocked(reason) }
        }
        runCatching { channel.setSpeakMode(CommandCatalog.SpeakMode.STOP) }
        _state.value = State.Idle
    }
}
