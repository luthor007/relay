package glass.relay.bridge.capture

import glass.relay.bridge.capture.DayPipeline.Recording
import glass.relay.bridge.capture.DayPipeline.Stage
import glass.relay.bridge.capture.DayPipeline.Step
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The phone is the last copy of somebody's day
 *
 * Between the glasses rotating their 4 GB and the box discarding audio after
 * transcription, this handset is the only place a day exists. These tests are
 * about the two ways that goes wrong — deleting too early, and giving up by
 * deleting — and about the ordering that decides which day is safe first.
 */
class DayPipelineTest {

    private val day = 86_400_000L

    private fun rec(
        id: String,
        capturedAtMs: Long,
        stage: Stage = Stage.Pending,
        attempts: Int = 0,
        nextAttemptAtMs: Long = 0,
        bytes: Long = 500L * 1024 * 1024,
    ) = Recording(id, capturedAtMs, bytes, stage, attempts, nextAttemptAtMs)

    @Test
    fun `audio is only freed after the box acknowledges the transcript`() {
        val now = 10 * day
        // Transcribed is not enough. An upload that reached a socket and no
        // further, followed by a delete, is a day that never existed.
        val transcribed = rec("a", day, stage = Stage.Transcribed)
        assertEquals(Step.Deliver("a"), DayPipeline.next(now, listOf(transcribed)))

        val acked = DayPipeline.acknowledged(transcribed)
        assertEquals(Stage.Delivered, acked.stage)
        assertEquals(Step.Free("a"), DayPipeline.next(now, listOf(acked)))
    }

    @Test
    fun `a recording that cannot be transcribed is kept, not deleted`() {
        var r = rec("bad", day)
        var now = 10 * day
        repeat(DayPipeline.MAX_ATTEMPTS) {
            r = DayPipeline.failed(r, now, "decoder said no")
            now += DayPipeline.backoffMs(r.attempts) + 1
        }
        assertEquals(Stage.Quarantined, r.stage)

        // Quarantine must never produce a Free. Deleting a file we failed to
        // read loses the day permanently; keeping it costs disk, which
        // StoragePolicy already knows how to talk about.
        val step = DayPipeline.next(now, listOf(r))
        assertNotEquals(Step.Free("bad"), step)
        assertEquals(Step.Idle, step)

        // And it still counts as a day the box does not have.
        assertEquals(1, DayPipeline.unsyncedDays(listOf(r)))
        assertEquals(0L, DayPipeline.freeableBytes(listOf(r)))
    }

    @Test
    fun `finishing a transcribed day beats starting a new one`() {
        val now = 10 * day
        val olderPending = rec("older", 1 * day, stage = Stage.Pending)
        val newerTranscribed = rec("newer", 5 * day, stage = Stage.Transcribed)

        // The transcript is the valuable artefact and the small one. Getting it
        // to the box comes before decoding another gigabyte, even though the
        // pending recording is older.
        assertEquals(
            Step.Deliver("newer"),
            DayPipeline.next(now, listOf(olderPending, newerTranscribed)),
        )
    }

    @Test
    fun `within a stage the oldest day goes first`() {
        val now = 10 * day
        val recordings = listOf(rec("b", 3 * day), rec("a", 1 * day), rec("c", 2 * day))
        assertEquals(Step.Transcribe("a"), DayPipeline.next(now, recordings))
    }

    @Test
    fun `one failing recording does not block the month behind it`() {
        val now = 10 * day
        // The oldest is in backoff after a failure; the pipeline moves on rather
        // than waiting, or a single corrupt file stops everything newer.
        val stuck = rec("stuck", 1 * day, attempts = 2, nextAttemptAtMs = now + 60_000)
        val fine = rec("fine", 2 * day)
        assertEquals(Step.Transcribe("fine"), DayPipeline.next(now, listOf(stuck, fine)))
    }

    @Test
    fun `when everything is in backoff it says when rather than spinning`() {
        val now = 10 * day
        val soon = rec("soon", 1 * day, attempts = 1, nextAttemptAtMs = now + 30_000)
        val later = rec("later", 2 * day, attempts = 3, nextAttemptAtMs = now + 900_000)

        // A caller that polls blindly wakes a sleeping phone for nothing, and
        // this runs overnight on a battery.
        assertEquals(Step.Wait(now + 30_000), DayPipeline.next(now, listOf(soon, later)))
    }

    @Test
    fun `backoff grows and is capped`() {
        assertEquals(0L, DayPipeline.backoffMs(0))
        assertEquals(60_000L, DayPipeline.backoffMs(1))
        assertEquals(120_000L, DayPipeline.backoffMs(2))
        assertTrue(DayPipeline.backoffMs(20) <= 3_600_000L)
    }

    @Test
    fun `being killed does not spend an attempt`() {
        // Getting stopped mid-transcription is normal on Android — OemPolicy
        // exists because of it. A phone killed five times must not quarantine a
        // perfectly good file.
        val interrupted = rec("a", day, attempts = 4, nextAttemptAtMs = 99 * day)
        val resumed = DayPipeline.resumed(listOf(interrupted)).single()
        assertEquals(0, resumed.attempts)
        assertEquals(0L, resumed.nextAttemptAtMs)
        assertEquals(Stage.Pending, resumed.stage)

        // Quarantine is not undone by a restart: it is a state a human resolves.
        val quarantined = rec("bad", day, stage = Stage.Quarantined, attempts = 5)
        assertEquals(Stage.Quarantined, DayPipeline.resumed(listOf(quarantined)).single().stage)
    }

    @Test
    fun `a delivered recording is still delivered after a restart`() {
        // The one stage a restart must not reset: resetting it to Pending would
        // re-transcribe and re-send a day the box already has.
        val delivered = rec("a", day, stage = Stage.Delivered)
        assertEquals(Stage.Delivered, DayPipeline.resumed(listOf(delivered)).single().stage)
    }

    @Test
    fun `freeing removes the record so the day is not fetched again`() {
        val delivered = rec("a", day, stage = Stage.Delivered)
        val after = DayPipeline.freed(listOf(delivered, rec("b", 2 * day)), "a")
        assertEquals(listOf("b"), after.map { it.id })
    }

    @Test
    fun `unsynced days is what a user needs before losing the phone`() {
        val recordings = listOf(
            rec("a", 1 * day, stage = Stage.Delivered),
            rec("b", 2 * day, stage = Stage.Transcribed),
            rec("c", 3 * day, stage = Stage.Pending),
            rec("d", 4 * day, stage = Stage.Quarantined),
        )
        // Three days exist only here. Delivered is the only stage the box has.
        assertEquals(3, DayPipeline.unsyncedDays(recordings))
    }

    @Test
    fun `freeable bytes is what StoragePolicy may count on`() {
        val recordings = listOf(
            rec("a", 1 * day, stage = Stage.Delivered, bytes = 100),
            rec("b", 2 * day, stage = Stage.Transcribed, bytes = 900),
        )
        // Only the acknowledged one. StoragePolicy's FreeUploadedAudio action
        // must never be sized from audio the box has not confirmed.
        assertEquals(100L, DayPipeline.freeableBytes(recordings))
    }

    @Test
    fun `a full night drains in order and ends idle`() {
        var recordings = listOf(rec("mon", 1 * day), rec("tue", 2 * day))
        var now = 10 * day
        val done = mutableListOf<String>()

        repeat(20) {
            when (val step = DayPipeline.next(now, recordings)) {
                is Step.Transcribe -> recordings = recordings.map {
                    if (it.id == step.id) DayPipeline.transcribed(it) else it
                }
                is Step.Deliver -> recordings = recordings.map {
                    if (it.id == step.id) DayPipeline.acknowledged(it) else it
                }
                is Step.Free -> {
                    done += step.id
                    recordings = DayPipeline.freed(recordings, step.id)
                }
                is Step.Wait -> now = step.untilMs
                Step.Idle -> return@repeat
            }
        }

        assertEquals(listOf("mon", "tue"), done)
        assertEquals(Step.Idle, DayPipeline.next(now, recordings))
        assertEquals(0, DayPipeline.unsyncedDays(recordings))
    }
}
