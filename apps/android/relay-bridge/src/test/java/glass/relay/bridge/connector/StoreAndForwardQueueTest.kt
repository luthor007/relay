package glass.relay.bridge.connector

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class StoreAndForwardQueueTest {

    private fun manifest(id: String, bytes: Long) = ConnectorClient.SessionManifest(
        sessionId = id,
        kind = "audio",
        startedAtMs = 1_700_000_000_000,
        durationS = 60,
        totalBytes = bytes,
        chunkBytes = 4096,
        encoding = "opus",
        sourceName = "REC_0001.opus",
    )

    @Test
    fun `queueing the same session twice does not duplicate it`() {
        val queue = StoreAndForwardQueue()
        val data = ByteArray(128)

        assertTrue(queue.enqueue(StoreAndForwardQueue.Queued(manifest("a", 128), data)))
        assertTrue(queue.enqueue(StoreAndForwardQueue.Queued(manifest("a", 128), data)))

        assertEquals(1, queue.size)
    }

    @Test
    fun `a full queue refuses new sessions rather than evicting old ones`() {
        // Dropping the oldest would silently lose last Tuesday, which for a
        // memory product is the worst available failure. Refusing is visible.
        val queue = StoreAndForwardQueue(capacityBytes = 1000)

        assertTrue(queue.enqueue(StoreAndForwardQueue.Queued(manifest("keep", 800), ByteArray(800))))
        assertFalse(queue.enqueue(StoreAndForwardQueue.Queued(manifest("drop", 800), ByteArray(800))))

        assertEquals(listOf("keep"), queue.sessionIds)
    }

    @Test
    fun `a record larger than the whole queue is refused as tooLarge, not full`() {
        // It can never fit, so retrying it forever would block everything
        // behind it. Ported from queue.test.ts and QueueTests.swift.
        val queue = StoreAndForwardQueue(capacityBytes = 1000)
        val result = queue.offer(StoreAndForwardQueue.Queued(manifest("huge", 5000), ByteArray(5000)))

        assertEquals(StoreAndForwardQueue.Refusal.TooLarge, result.reason)
    }

    @Test
    fun `an item limit is enforced as well as a byte limit`() {
        // Ported verbatim from `queue.test.ts` and `QueueTests.swift`. The
        // Kotlin queue could not express this refusal at all, while the header
        // of every one of the three files claimed the implementations agree.
        val queue = StoreAndForwardQueue(capacityBytes = 1_000_000, capacityItems = 2)
        queue.enqueue(StoreAndForwardQueue.Queued(manifest("a", 1), ByteArray(1)))
        queue.enqueue(StoreAndForwardQueue.Queued(manifest("b", 1), ByteArray(1)))

        val third = queue.offer(StoreAndForwardQueue.Queued(manifest("c", 1), ByteArray(1)))

        assertFalse(third.accepted)
        assertEquals(StoreAndForwardQueue.Refusal.ItemLimit, third.reason)
        assertEquals(listOf("a", "b"), queue.sessionIds)
    }

    @Test
    fun `the byte cap is reported before the item cap when both are exceeded`() {
        // Precedence, not preference: delivered → duplicate → tooLarge → full →
        // itemLimit → storeFailed, the same order in all three implementations.
        // Two platforms naming the same refusal differently is a support ticket
        // nobody can reproduce.
        val queue = StoreAndForwardQueue(capacityBytes = 100, capacityItems = 1)
        queue.enqueue(StoreAndForwardQueue.Queued(manifest("a", 80), ByteArray(80)))

        val second = queue.offer(StoreAndForwardQueue.Queued(manifest("b", 80), ByteArray(80)))

        assertEquals(StoreAndForwardQueue.Refusal.Full, second.reason)
    }

    @Test
    fun `an already delivered id is accepted as a no-op even at the item limit`() {
        // Delivered comes first in the precedence, so a replayed sync is a
        // no-op rather than an itemLimit refusal the UI would show as an error.
        val queue = StoreAndForwardQueue(capacityItems = 1)
        queue.enqueue(StoreAndForwardQueue.Queued(manifest("monday", 10), ByteArray(10)))
        kotlinx.coroutines.runBlocking { queue.flushWith { } }
        queue.enqueue(StoreAndForwardQueue.Queued(manifest("tuesday", 10), ByteArray(10)))

        val replay = queue.offer(StoreAndForwardQueue.Queued(manifest("monday", 10), ByteArray(10)))

        assertTrue(replay.accepted)
        assertTrue(replay.alreadyDelivered)
        assertEquals(null, replay.reason)
    }

    @Test
    fun `used bytes tracks what is actually held`() {
        val queue = StoreAndForwardQueue()
        queue.enqueue(StoreAndForwardQueue.Queued(manifest("a", 100), ByteArray(100)))
        queue.enqueue(StoreAndForwardQueue.Queued(manifest("b", 250), ByteArray(250)))

        assertEquals(350L, queue.usedBytes)
    }

    @Test
    fun `order is preserved, because the box segments episodes by time`() {
        val queue = StoreAndForwardQueue()
        queue.enqueue(StoreAndForwardQueue.Queued(manifest("first", 10), ByteArray(10)))
        queue.enqueue(StoreAndForwardQueue.Queued(manifest("second", 10), ByteArray(10)))
        queue.enqueue(StoreAndForwardQueue.Queued(manifest("third", 10), ByteArray(10)))

        assertEquals(listOf("first", "second", "third"), queue.sessionIds)
    }
}
