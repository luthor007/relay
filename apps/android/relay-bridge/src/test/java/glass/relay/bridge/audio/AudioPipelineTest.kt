package glass.relay.bridge.audio

import glass.relay.bridge.capture.AudioFrame
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The ring buffer and the upload plan. The rule under test throughout is that
 * nothing is lost quietly.
 */
class AudioPipelineTest {

    private fun frame(sequence: Int, bytes: Int = 60) =
        AudioFrame(ByteArray(bytes), sequence, deviceTimeMs = sequence * 20L)

    // --- ring ---------------------------------------------------------------

    @Test
    fun `a full ring refuses the newest and says so`() {
        val ring = AudioRing(AudioRing.Config(capacityFrames = 4, highWaterFrames = 3, lowWaterFrames = 1))
        repeat(4) { ring.offer(frame(it)) }

        val result = ring.offer(frame(4))

        assertTrue("expected a refusal, got $result", result is AudioRing.Result.Refused)
        assertEquals(1, ring.refusedFrames)
        assertEquals("nothing may be evicted under Refuse", 4, ring.size)
    }

    @Test
    fun `backpressure is raised before anything is lost`() {
        val ring = AudioRing(AudioRing.Config(capacityFrames = 10, highWaterFrames = 6, lowWaterFrames = 3))

        repeat(5) { assertEquals(AudioRing.Result.Accepted, ring.offer(frame(it))) }
        assertFalse(ring.shouldThrottle)

        assertEquals(AudioRing.Result.AcceptedThrottle, ring.offer(frame(5)))
        assertTrue("the producer must be told before the ring fills", ring.shouldThrottle)
    }

    @Test
    fun `throttling clears with hysteresis, not at the first free slot`() {
        val ring = AudioRing(AudioRing.Config(capacityFrames = 10, highWaterFrames = 6, lowWaterFrames = 3))
        repeat(6) { ring.offer(frame(it)) }
        assertTrue(ring.shouldThrottle)

        ring.drain(2)
        assertTrue("four frames left is still above the low water mark", ring.shouldThrottle)

        ring.drain(1)
        assertFalse(ring.shouldThrottle)
    }

    @Test
    fun `dropping the oldest records a gap, so the transcript can show the hole`() {
        // A splice with no marker produces a transcript that reads as continuous
        // and is not, which is worse than an acknowledged gap.
        val ring = AudioRing(
            AudioRing.Config(
                capacityFrames = 4,
                highWaterFrames = 3,
                lowWaterFrames = 1,
                overflow = AudioRing.Overflow.DropOldest,
            ),
        )
        repeat(4) { ring.offer(frame(it)) }

        val result = ring.offer(frame(4))

        assertTrue(result is AudioRing.Result.AcceptedWithGap)
        val gap = (result as AudioRing.Result.AcceptedWithGap).gap
        assertEquals(0, gap.fromSequence)
        assertEquals(2, gap.toSequence)
        assertEquals(3, gap.frames)
        assertEquals(3 * 60, gap.bytes)
        assertEquals(1, ring.gaps.size)
        assertEquals(3, ring.droppedFrames)
    }

    @Test
    fun `drain returns frames oldest first`() {
        val ring = AudioRing()
        repeat(5) { ring.offer(frame(it)) }

        assertEquals(listOf(0, 1, 2), ring.drain(3).map { it.sequence })
        assertEquals(listOf(3, 4), ring.drain().map { it.sequence })
        assertEquals(0, ring.size)
        assertEquals(0L, ring.usedBytes)
    }

    @Test
    fun `used bytes tracks the payload, not the frame count`() {
        val ring = AudioRing()
        ring.offer(frame(0, bytes = 100))
        ring.offer(frame(1, bytes = 250))
        assertEquals(350L, ring.usedBytes)
    }

    @Test
    fun `a healthy run has no gaps at all`() {
        val ring = AudioRing(AudioRing.Config(capacityFrames = 8, highWaterFrames = 6, lowWaterFrames = 2))
        repeat(100) {
            ring.offer(frame(it))
            ring.drain(1)
        }
        assertTrue(ring.gaps.isEmpty())
        assertEquals(0, ring.refusedFrames)
        assertEquals(0, ring.droppedFrames)
    }

    @Test
    fun `a nonsensical configuration is rejected at construction`() {
        val error = runCatching {
            AudioRing(AudioRing.Config(capacityFrames = 4, highWaterFrames = 4, lowWaterFrames = 4))
        }.exceptionOrNull()
        assertTrue(error is IllegalArgumentException)
    }

    // --- upload plan --------------------------------------------------------

    @Test
    fun `chunking covers every byte exactly once, in order`() {
        val plan = UploadPlan.forSession(totalBytes = 10_000, chunkBytes = 4_096)

        assertEquals(3, plan.totalChunks)
        assertEquals(listOf(0, 1, 2), plan.toSend.map { it.index })
        assertEquals(10_000L, plan.toSend.sumOf { it.length.toLong() })
        assertEquals(4_096, plan.toSend[0].length)
        assertEquals(1_808, plan.toSend[2].length)
        assertEquals(8_192, plan.toSend[2].offset)
    }

    @Test
    fun `a resumed upload sends only what the box is missing`() {
        // The phone that uploaded 39 of 40 chunks before the tunnel does not
        // know that it did. The box's answer is the only trustworthy one.
        val plan = UploadPlan.forSession(
            totalBytes = 40 * 4_096L,
            chunkBytes = 4_096,
            received = (0 until 39).toSet(),
        )

        assertEquals(listOf(39), plan.toSend.map { it.index })
        assertEquals(39, plan.skipped.size)
        assertEquals(4_096L, plan.bytesToSend)
    }

    @Test
    fun `bytes the box already holds still count as progress`() {
        // Otherwise a nearly-finished transfer reports 3% and looks broken.
        val plan = UploadPlan.forSession(
            totalBytes = 1000,
            chunkBytes = 100,
            received = (0 until 9).toSet(),
        )
        assertEquals(900L, plan.bytesAlreadyThere)
        assertEquals(100L, plan.bytesToSend)
    }

    @Test
    fun `a session the box already has completely is complete`() {
        val plan = UploadPlan.forSession(1000, 100, received = (0 until 10).toSet())
        assertTrue(plan.complete)
        assertTrue(plan.toSend.isEmpty())
    }

    @Test
    fun `an empty session has no chunks rather than one empty one`() {
        val plan = UploadPlan.forSession(0, 4_096)
        assertEquals(0, plan.totalChunks)
        assertTrue(plan.complete)
    }

    @Test
    fun `opus is passed through rather than re-encoded`() {
        // APPS-SCOPE.md §4.2. Decoding and re-encoding on the phone spends
        // battery to make the audio worse.
        assertFalse(UploadPlan.shouldTranscode("opus", setOf("opus", "pcm16")))
        assertTrue(UploadPlan.shouldTranscode("opus", setOf("wav")))
    }
}
