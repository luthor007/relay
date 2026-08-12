package glass.relay.bridge.capture

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock

/**
 * Path B — the all-day pipeline.
 *
 * The glasses record to their own 4 GB and the phone pulls the files later.
 * This controller decides *when* they record, and cuts the day into segments
 * the box can transcribe as they arrive.
 *
 * ## Why segments
 *
 * `APPS-SCOPE.md` §4.2 asks for segment boundaries "so the box can transcribe
 * incrementally". A single sixteen-hour file cannot be transcribed until the
 * day is over, cannot be resumed if a transfer fails halfway, and cannot be
 * partially deleted. Rolling the recording every [Config.segmentSeconds]
 * costs one stop/start pair per segment and buys all three.
 *
 * ## What does *not* stop a recording
 *
 *  - **Losing the phone link.** The glasses are writing to their own storage;
 *    our visibility ended, the recording did not. This controller never reacts
 *    to connection state, and `ConnectionSupervisor` deliberately leaves the
 *    `recording` flag set on disconnect for the same reason.
 *  - **Low storage.** Audio at ~10.8 MB/h against 4 GB is not the constraint
 *    (`APPS-SCOPE.md` §3.1); video is. Stopping the day's capture to protect
 *    disk space would trade the thing the product is for the thing it needs.
 *    Storage pressure raises a warning and asks for a sync — see
 *    `glass.relay.bridge.storage.StoragePolicy`.
 *
 * ## What does
 *
 * Consent, wear, and the user pausing. In that order of authority: consent is
 * absolute, wear is the physical signal, pause is deliberate.
 */
class LocalRecordingController(
    private val device: RecordingDevice,
    private val scope: CoroutineScope,
    private val config: Config = Config(),
    private val clock: () -> Long = System::currentTimeMillis,
) {

    data class Config(
        /**
         * Segment length. Fifteen minutes is a compromise: short enough that
         * the box starts transcribing while the conversation is still happening,
         * long enough that a day is ~64 files rather than thousands.
         */
        val segmentSeconds: Int = 15 * 60,

        /**
         * Whether wear detection gates capture.
         *
         * Off for firmware that does not report wear (`0x0301` says so) — with
         * no wear signal, gating on it means never recording at all.
         */
        val gateOnWear: Boolean = true,
    )

    /** One recorded stretch. Names come from the device's own file listing. */
    data class Segment(
        val index: Int,
        val startedAtMs: Long,
        val endedAtMs: Long?,
        val stopReason: StopReason?,
    ) {
        val durationSeconds: Int?
            get() = endedAtMs?.let { ((it - startedAtMs) / 1000).toInt() }
    }

    enum class StopReason { Removed, ConsentWithdrawn, Paused, SegmentRolled, Stopped, Failed }

    data class Snapshot(
        val recording: Boolean = false,
        val worn: Boolean = false,
        val consent: Boolean = false,
        val paused: Boolean = false,
        val segments: List<Segment> = emptyList(),
        /** Why capture is not running, in words the UI can show verbatim. */
        val blockedBy: String? = null,
    )

    private val _state = MutableStateFlow(Snapshot())
    val state: StateFlow<Snapshot> = _state.asStateFlow()

    private val lock = Mutex()
    private var roller: Job? = null
    private var segmentIndex = 0

    val segments: List<Segment> get() = _state.value.segments

    // --- inputs --------------------------------------------------------------

    suspend fun setConsent(granted: Boolean) = update { it.copy(consent = granted) }

    suspend fun setWorn(worn: Boolean) = update { it.copy(worn = worn) }

    /** A deliberate pause. Survives reconnects until [resume]. */
    suspend fun pause() = update { it.copy(paused = true) }

    suspend fun resume() = update { it.copy(paused = false) }

    /** Stop for good — the user turned capture off. */
    suspend fun stop() {
        lock.withLock {
            if (_state.value.recording) stopRecordingLocked(StopReason.Stopped)
            roller?.cancel()
            roller = null
            _state.value = _state.value.copy(paused = true, blockedBy = "capture is off")
        }
    }

    // --- the decision --------------------------------------------------------

    /**
     * Why capture is not running, or null when it should be.
     *
     * Split out because the notification and the home screen both need the
     * *reason*, and "not recording" with no explanation is the state that makes
     * people restart a recording that never stopped.
     */
    private fun blockedReason(snapshot: Snapshot): String? = when {
        !snapshot.consent -> "waiting for consent"
        snapshot.paused -> "paused"
        config.gateOnWear && !snapshot.worn -> "glasses are not being worn"
        else -> null
    }

    private suspend fun update(mutate: (Snapshot) -> Snapshot) {
        lock.withLock {
            val next = mutate(_state.value)
            val blocked = blockedReason(next)
            _state.value = next.copy(blockedBy = blocked)

            if (blocked == null && !next.recording) {
                startRecordingLocked()
            } else if (blocked != null && next.recording) {
                stopRecordingLocked(
                    when {
                        !next.consent -> StopReason.ConsentWithdrawn
                        next.paused -> StopReason.Paused
                        else -> StopReason.Removed
                    },
                )
            }
        }
    }

    private suspend fun startRecordingLocked() {
        try {
            device.startLocalRecording()
        } catch (error: Exception) {
            _state.value = _state.value.copy(
                recording = false,
                blockedBy = "the glasses refused to start recording: ${error.message}",
            )
            return
        }
        val segment = Segment(
            index = segmentIndex++,
            startedAtMs = clock(),
            endedAtMs = null,
            stopReason = null,
        )
        _state.value = _state.value.copy(
            recording = true,
            blockedBy = null,
            segments = _state.value.segments + segment,
        )
        startRoller()
    }

    private suspend fun stopRecordingLocked(reason: StopReason) {
        roller?.cancel()
        roller = null
        val failed = runCatching { device.stopLocalRecording() }.isFailure
        _state.value = _state.value.copy(
            recording = false,
            segments = closeLastSegment(if (failed) StopReason.Failed else reason),
        )
    }

    private fun closeLastSegment(reason: StopReason): List<Segment> {
        val all = _state.value.segments
        val last = all.lastOrNull() ?: return all
        if (last.endedAtMs != null) return all
        return all.dropLast(1) + last.copy(endedAtMs = clock(), stopReason = reason)
    }

    /**
     * Rolls the recording on a timer.
     *
     * Deliberately a stop/start pair rather than a "split" command: the protocol
     * has no split, and `0x0E04` is the only recording control there is. The
     * gap between them is milliseconds of BLE round trip, which is a real hole
     * in the day — small, honest, and recorded in the segment list rather than
     * papered over.
     */
    private fun startRoller() {
        roller?.cancel()
        roller = scope.launch {
            while (isActive) {
                delay(config.segmentSeconds * 1000L)
                lock.withLock {
                    if (!_state.value.recording) return@withLock
                    runCatching { device.stopLocalRecording() }
                    _state.value = _state.value.copy(segments = closeLastSegment(StopReason.SegmentRolled))
                    runCatching { device.startLocalRecording() }
                        .onSuccess {
                            _state.value = _state.value.copy(
                                segments = _state.value.segments + Segment(
                                    index = segmentIndex++,
                                    startedAtMs = clock(),
                                    endedAtMs = null,
                                    stopReason = null,
                                ),
                            )
                        }
                        .onFailure {
                            _state.value = _state.value.copy(
                                recording = false,
                                blockedBy = "the glasses refused to resume after a segment roll",
                            )
                        }
                }
            }
        }
    }
}
