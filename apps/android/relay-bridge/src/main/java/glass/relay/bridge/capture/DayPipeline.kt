package glass.relay.bridge.capture

/**
 * The overnight journey of a day's audio: transcribe here, send text.
 *
 * `CONTROL-PLANE.md` §1b is the reason this exists and is worth restating,
 * because it inverts what the phone was originally going to do. A day of audio
 * is **170 MB–1.8 GB** (`SYSTEM.md` §3.1) and the box **throws it away after
 * transcription** (`SYSTEM.md` §5). Uploading it means paying to move a gigabyte
 * across somebody's data plan to a machine that deletes it — and it cannot be
 * made reliable on cellular at all, which is the requirement: a customer on
 * holiday, box at home, still gets their day processed.
 *
 * So the phone transcribes and uploads text. A day of speech is tens of
 * kilobytes; it arrives tonight, over any network.
 *
 * ## The invariant everything here exists to protect
 *
 * **The phone is the last copy.** The glasses' 4 GB fills and rotates, and the
 * box discards audio by design, so between "recorded" and "the transcript
 * reached the box" this handset holds the only copy of somebody's day. Every
 * rule below follows from that:
 *
 *  - Audio is freed **only** after the box has acknowledged the transcript.
 *    Not after transcription, not after the upload call returned — after an
 *    acknowledgement, because an upload that failed silently and a delete that
 *    succeeded is a day that never existed.
 *  - A recording that cannot be transcribed is **quarantined, never freed**.
 *    Deleting a file we failed to read loses the day permanently; keeping it
 *    costs disk, which `StoragePolicy` already knows how to talk about.
 *  - Nothing is head-of-line blocked. One corrupt file must not stop the month
 *    behind it, so a failing item goes into backoff and the planner moves on.
 *
 * ## Why this is a planner and not a worker
 *
 * The same shape as [glass.relay.bridge.storage.StoragePolicy]: it takes the
 * world and returns the next thing to do. The work itself — decoding Opus,
 * driving the recogniser, sending a frame — is platform code that needs Android,
 * and none of it belongs in a file that has to be provable. What can go wrong
 * here is ordering, retrying, and deleting too early, and all three are decided
 * in this file and tested without a device.
 *
 * Being a planner is also what makes it survive being killed. An OEM that stops
 * the service mid-transcription (see `OemPolicy`) loses no decision, because
 * there was no decision in flight — the next call to [next] reads the same
 * durable records and picks up where it left off.
 */
object DayPipeline {

    /** How far a recording has got. */
    enum class Stage {
        /** On disk, not yet transcribed. */
        Pending,

        /** Transcribed; the text has not been acknowledged by the box. */
        Transcribed,

        /** The box has the transcript. The audio may now be freed. */
        Delivered,

        /**
         * Repeatedly unreadable. Kept, never freed, and surfaced — see the class
         * comment. This is a state a human has to resolve, not one the pipeline
         * retries out of.
         */
        Quarantined,
    }

    /** One recording, as the phone's durable record holds it. */
    data class Recording(
        /**
         * Stable across restarts and across re-sends. It is the glasses' own
         * filename, which is also what makes delivery idempotent: a box that
         * already stored this id ignores a duplicate, so an acknowledgement lost
         * on the way back costs one wasted send rather than a doubled day.
         */
        val id: String,
        val capturedAtMs: Long,
        val bytes: Long,
        val stage: Stage = Stage.Pending,
        /** Consecutive failures at the current stage. */
        val attempts: Int = 0,
        /** Earliest time to try again. Zero means now. */
        val nextAttemptAtMs: Long = 0,
        /** The last failure, for the UI to show rather than a spinner. */
        val failure: String? = null,
    )

    /** What to do next. */
    sealed interface Step {
        /** Decode and transcribe this recording. */
        data class Transcribe(val id: String) : Step

        /** Send this recording's transcript to the box. */
        data class Deliver(val id: String) : Step

        /** The box has it; delete the audio. */
        data class Free(val id: String) : Step

        /**
         * Everything ready is in backoff. [untilMs] is when the earliest becomes
         * due, so a caller can sleep exactly that long instead of polling.
         */
        data class Wait(val untilMs: Long) : Step

        /** Nothing to do. */
        object Idle : Step
    }

    /**
     * How many times a stage is retried before the recording is quarantined.
     *
     * Five, and the number matters less than what happens at the end of it:
     * quarantine keeps the file. A pipeline that gave up by deleting would turn
     * a transient decoder bug into a permanently lost week.
     */
    const val MAX_ATTEMPTS = 5

    /** Backoff, doubling from a minute and capped at an hour. */
    fun backoffMs(attempts: Int): Long {
        if (attempts <= 0) return 0
        val base = 60_000L
        var delay = base
        repeat(minOf(attempts, 6) - 1) { delay *= 2 }
        return minOf(delay, 3_600_000L)
    }

    /**
     * Decide the next action.
     *
     * Ordered by stage rather than by age, and that is deliberate: **finishing a
     * day already transcribed beats starting a new one.** The transcript is the
     * valuable artefact and it is the small one, so getting it to the box — and
     * therefore off the critical path of a phone that might be wiped, lost or
     * out of space — comes before decoding another gigabyte. Within a stage,
     * oldest first, so a backlog drains in the order the days happened.
     */
    fun next(nowMs: Long, recordings: List<Recording>): Step {
        // Freeing first: it costs nothing, it is the only step that recovers
        // disk, and StoragePolicy may be waiting on exactly this.
        oldestReady(recordings, Stage.Delivered, nowMs)?.let { return Step.Free(it.id) }
        oldestReady(recordings, Stage.Transcribed, nowMs)?.let { return Step.Deliver(it.id) }
        oldestReady(recordings, Stage.Pending, nowMs)?.let { return Step.Transcribe(it.id) }

        // Nothing is due. If anything is merely waiting, say when — a caller
        // that polls blindly wakes a sleeping phone for no reason, and this is
        // running overnight on a battery.
        val soonest = recordings
            .filter { it.stage != Stage.Quarantined && it.nextAttemptAtMs > nowMs }
            .minByOrNull { it.nextAttemptAtMs }
        return if (soonest != null) Step.Wait(soonest.nextAttemptAtMs) else Step.Idle
    }

    private fun oldestReady(recordings: List<Recording>, stage: Stage, nowMs: Long): Recording? =
        recordings
            .filter { it.stage == stage && it.nextAttemptAtMs <= nowMs }
            .minByOrNull { it.capturedAtMs }

    // --- transitions ---------------------------------------------------------

    /** Transcription succeeded. */
    fun transcribed(r: Recording): Recording =
        r.copy(stage = Stage.Transcribed, attempts = 0, nextAttemptAtMs = 0, failure = null)

    /**
     * The box acknowledged the transcript.
     *
     * This is the only path to [Stage.Delivered], and therefore the only path to
     * the audio being freed. "The upload call returned" is not this: a request
     * that reached a socket and no further, followed by a delete, is a day that
     * never existed.
     */
    fun acknowledged(r: Recording): Recording =
        r.copy(stage = Stage.Delivered, attempts = 0, nextAttemptAtMs = 0, failure = null)

    /** The audio is gone. The record stays, so the day is not re-fetched. */
    fun freed(recordings: List<Recording>, id: String): List<Recording> =
        recordings.filterNot { it.id == id }

    /**
     * A step failed.
     *
     * Backs off, and quarantines at the cap — which keeps the file. A recording
     * that has failed five times is a bug report, not garbage.
     */
    fun failed(r: Recording, nowMs: Long, why: String): Recording {
        val attempts = r.attempts + 1
        if (attempts >= MAX_ATTEMPTS && r.stage == Stage.Pending) {
            return r.copy(stage = Stage.Quarantined, attempts = attempts, failure = why)
        }
        return r.copy(
            attempts = attempts,
            nextAttemptAtMs = nowMs + backoffMs(attempts),
            failure = why,
        )
    }

    /**
     * What the phone is holding that the box does not have.
     *
     * The number a UI shows and the number that decides whether it is safe to
     * lose this handset. Quarantined recordings count: they are days nobody has.
     */
    fun unsyncedDays(recordings: List<Recording>): Int =
        recordings.count { it.stage != Stage.Delivered }

    /** Bytes that could be recovered right now, for [StoragePolicy]'s benefit. */
    fun freeableBytes(recordings: List<Recording>): Long =
        recordings.filter { it.stage == Stage.Delivered }.sumOf { it.bytes }

    /**
     * Recover after the process was killed.
     *
     * An OEM stop mid-transcription leaves a record that says Pending, which is
     * already correct — this exists for the *other* half: attempts counted
     * against a run that never finished. Without it a phone that is killed five
     * times quarantines a perfectly good file, and the OEM watchdog exists
     * precisely because being killed is normal here.
     */
    fun resumed(recordings: List<Recording>): List<Recording> =
        recordings.map {
            if (it.stage == Stage.Pending || it.stage == Stage.Transcribed) {
                it.copy(attempts = 0, nextAttemptAtMs = 0)
            } else {
                it
            }
        }
}
