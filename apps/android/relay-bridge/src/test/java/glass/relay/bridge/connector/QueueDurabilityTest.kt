package glass.relay.bridge.connector

import kotlinx.coroutines.test.runTest
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.io.File

/**
 * The two things `APPS-SCOPE.md` §4.2 says the Kotlin queue owed the TypeScript
 * one: durability across a crash, and delivered-id memory so a replayed sync
 * does not re-upload the day.
 *
 * `StoreAndForwardQueueTest` still holds the four semantics all three
 * implementations share, unchanged. This file only covers what was added.
 */
class QueueDurabilityTest {

    private lateinit var directory: File

    @Before
    fun setUp() {
        directory = File.createTempFile("relay-queue", "").let { file ->
            file.delete()
            file.mkdirs()
            file
        }
    }

    @After
    fun tearDown() {
        directory.deleteRecursively()
    }

    private fun manifest(id: String, bytes: Long) = ConnectorClient.SessionManifest(
        sessionId = id,
        kind = "audio",
        startedAtMs = 1_700_000_000_000,
        durationS = 60,
        totalBytes = bytes,
        chunkBytes = 4096,
        encoding = "opus",
        sourceName = "$id.opus",
    )

    private fun queued(id: String, bytes: Int) =
        StoreAndForwardQueue.Queued(manifest(id, bytes.toLong()), ByteArray(bytes) { (it % 251).toByte() })

    // --- durability ---------------------------------------------------------

    @Test
    fun `a restart reloads the day in its original order`() {
        val first = StoreAndForwardQueue(store = FileQueueStore(directory))
        first.enqueue(queued("morning", 1_000))
        first.enqueue(queued("lunch", 2_000))
        first.enqueue(queued("afternoon", 3_000))

        // The process dies here. Nothing is closed, nothing is flushed.
        val second = StoreAndForwardQueue(store = FileQueueStore(directory)).also { it.restore() }

        assertEquals(listOf("morning", "lunch", "afternoon"), second.sessionIds)
        assertEquals(6_000L, second.usedBytes)
    }

    @Test
    fun `the bytes are durable before enqueue returns, so a caller may delete its source`() {
        val store = FileQueueStore(directory)
        val queue = StoreAndForwardQueue(store = store)

        assertTrue(queue.enqueue(queued("session", 4_096)))

        // A different store instance sees it without the first one being closed.
        val restored = FileQueueStore(directory).load()
        assertEquals(1, restored.pending.size)
        assertEquals(4_096, restored.pending.single().body.size)
        assertEquals("session.opus", restored.pending.single().manifest.sourceName)
    }

    @Test
    fun `a half-written record is skipped rather than decoded into a truncated day`() {
        val queue = StoreAndForwardQueue(store = FileQueueStore(directory))
        queue.enqueue(queued("good", 500))

        // Simulate the crash window: a record file that was never committed.
        File(directory, "deadbeef.rec").writeBytes(byteArrayOf(0x52, 0x4C, 0x51, 0x31, 0x00))

        val restored = StoreAndForwardQueue(store = FileQueueStore(directory)).also { it.restore() }
        assertEquals(listOf("good"), restored.sessionIds)
    }

    @Test
    fun `session ids that look like paths cannot escape the directory`() {
        val queue = StoreAndForwardQueue(store = FileQueueStore(directory))
        assertTrue(queue.enqueue(queued("../../etc/passwd", 10)))

        val restored = StoreAndForwardQueue(store = FileQueueStore(directory)).also { it.restore() }
        assertEquals(listOf("../../etc/passwd"), restored.sessionIds)
        assertEquals(1, directory.listFiles { f -> f.name.endsWith(".rec") }!!.size)
    }

    // --- delivered memory ---------------------------------------------------

    @Test
    fun `a replayed sync does not re-upload the day`() = runTest {
        val store = FileQueueStore(directory)
        val queue = StoreAndForwardQueue(store = store)
        queue.enqueue(queued("REC_0001.opus", 1_000))

        val sent = mutableListOf<String>()
        val result = queue.flushWith { sent += it.id }
        assertEquals(1, result.sent)

        // The nightly sync runs again and offers the same file.
        val again = queue.offer(queued("REC_0001.opus", 1_000))
        assertTrue("must be accepted as a no-op", again.accepted)
        assertTrue(again.alreadyDelivered)
        assertEquals(0, queue.size)

        // And survives a restart.
        val afterRestart = StoreAndForwardQueue(store = FileQueueStore(directory)).also { it.restore() }
        assertTrue(afterRestart.offer(queued("REC_0001.opus", 1_000)).alreadyDelivered)
        assertEquals(0, afterRestart.size)
    }

    @Test
    fun `a record both pending and delivered is resolved in favour of delivered`() {
        // The crash window inside flush: markDelivered has run, remove has not.
        // The other resolution re-uploads a day on every restart.
        val store = MemoryQueueStore()
        val record = StoredRecord(manifest("ghost", 10), ByteArray(10), 1, 0)
        store.append(record)
        store.markDelivered("ghost")

        val queue = StoreAndForwardQueue(store = store).also { it.restore() }

        assertEquals(0, queue.size)
        assertTrue(queue.deliveredIds.contains("ghost"))
    }

    @Test
    fun `delivered memory is bounded`() = runTest {
        val queue = StoreAndForwardQueue(store = MemoryQueueStore(), deliveredMemory = 3)
        for (index in 0 until 6) {
            queue.enqueue(queued("s$index", 10))
            queue.flushWith { }
        }
        assertEquals(listOf("s3", "s4", "s5"), queue.deliveredIds)
        // The oldest has aged out, so it would be re-uploaded — which is the
        // correct trade at the boundary: a duplicate costs bandwidth, a
        // permanently growing set costs the phone.
        assertFalse(queue.offer(queued("s0", 10)).alreadyDelivered)
    }

    // --- refusals -----------------------------------------------------------

    @Test
    fun `a record larger than the whole queue is refused as tooLarge, not full`() {
        // Refusing it as "full" invites a retry loop that blocks everything
        // behind it forever.
        val queue = StoreAndForwardQueue(capacityBytes = 1_000, store = MemoryQueueStore())
        val result = queue.offer(queued("enormous", 2_000))

        assertFalse(result.accepted)
        assertEquals(StoreAndForwardQueue.Refusal.TooLarge, result.reason)
    }

    @Test
    fun `a full queue refuses the newest with a reason the UI can show`() {
        val queue = StoreAndForwardQueue(capacityBytes = 1_000, store = MemoryQueueStore())
        queue.enqueue(queued("keep", 800))

        val result = queue.offer(queued("newest", 800))

        assertFalse(result.accepted)
        assertEquals(StoreAndForwardQueue.Refusal.Full, result.reason)
        assertNotNull(result.message)
        assertEquals("the oldest must survive", listOf("keep"), queue.sessionIds)
    }

    @Test
    fun `a failing disk refuses rather than pretending, because the caller still owns the bytes`() {
        // Saying "accepted" here is how a day of audio is deleted off the
        // glasses and never arrives anywhere.
        val store = MemoryQueueStore(appendFails = true)
        val queue = StoreAndForwardQueue(store = store)

        val result = queue.offer(queued("session", 100))

        assertFalse(result.accepted)
        assertEquals(StoreAndForwardQueue.Refusal.StoreFailed, result.reason)
        assertEquals(0, queue.size)
    }

    @Test
    fun `a flush that fails halfway leaves the rest queued in order`() = runTest {
        val queue = StoreAndForwardQueue(store = FileQueueStore(directory))
        queue.enqueue(queued("a", 10))
        queue.enqueue(queued("b", 10))
        queue.enqueue(queued("c", 10))

        val result = queue.flushWith { record ->
            if (record.id == "b") throw IllegalStateException("no signal")
        }

        assertEquals(1, result.sent)
        assertEquals(2, result.remaining)
        assertNotNull(result.error)
        assertEquals(listOf("b", "c"), queue.sessionIds)

        // And the disk agrees, so a restart resumes from the same place.
        val restored = StoreAndForwardQueue(store = FileQueueStore(directory)).also { it.restore() }
        assertEquals(listOf("b", "c"), restored.sessionIds)
    }

    @Test
    fun `has and freeBytes report what the sync ritual needs to decide`() {
        val queue = StoreAndForwardQueue(capacityBytes = 1_000, store = MemoryQueueStore())
        queue.enqueue(queued("held", 400))

        assertTrue(queue.has("held"))
        assertFalse(queue.has("never seen"))
        assertEquals(600L, queue.freeBytes)
    }
}
