package glass.relay.bridge.session

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class SessionSurfaceTest {

    private fun session(
        id: String,
        state: AgentSession.State = AgentSession.State.Running,
        updatedAtMs: Long = 0,
    ) = AgentSession(id, "title $id", "claude-code", state, updatedAtMs)

    // --- the list -----------------------------------------------------------

    @Test
    fun `a blocked session goes to the top, because it is the one costing time`() {
        val registry = SessionRegistry()
        registry.update(
            listOf(
                session("busy", AgentSession.State.Running, updatedAtMs = 100),
                session("waiting", AgentSession.State.Blocked, updatedAtMs = 1),
                session("done", AgentSession.State.Finished, updatedAtMs = 200),
            ),
        )
        assertEquals("waiting", registry.all().first().id)
        assertEquals(listOf("waiting"), registry.blocked().map { it.id })
    }

    @Test
    fun `the rest are ordered by recency`() {
        val registry = SessionRegistry()
        registry.update(
            listOf(
                session("old", updatedAtMs = 1),
                session("new", updatedAtMs = 9),
                session("middle", updatedAtMs = 5),
            ),
        )
        assertEquals(listOf("new", "middle", "old"), registry.all().map { it.id })
    }

    @Test
    fun `attaching returns the previous target so the UI can offer undo`() {
        val registry = SessionRegistry()
        registry.update(listOf(session("a"), session("b")))

        assertNull(registry.attach("a"))
        assertEquals("a", registry.attach("b")?.id)
        assertEquals("b", registry.attached()?.id)
    }

    @Test
    fun `a session that disappears cannot stay attached`() {
        // Otherwise the next utterance is routed into nothing.
        val registry = SessionRegistry()
        registry.update(listOf(session("a")))
        registry.attach("a")

        registry.update(listOf(session("b")))

        assertNull(registry.attached())
    }

    @Test
    fun `attaching to a session that does not exist is a programming error`() {
        val registry = SessionRegistry()
        registry.update(listOf(session("a")))
        val error = runCatching { registry.attach("ghost") }.exceptionOrNull()
        assertTrue(error is IllegalArgumentException)
    }

    // --- approvals ----------------------------------------------------------

    @Test
    fun `an approval can only be answered once`() {
        // A double tap or a replayed envelope must not approve twice.
        val queue = ApprovalQueue()
        queue.offer("deploy", "session-1", "push to production", nowMs = 0)

        assertEquals(ApprovalQueue.Answer.Approved, queue.answer("deploy", approve = true, nowMs = 1)?.answer)
        assertNull("the second answer must do nothing", queue.answer("deploy", approve = true, nowMs = 2))
    }

    @Test
    fun `a request the box repeats is not queued twice`() {
        val queue = ApprovalQueue()
        assertTrue(queue.offer("deploy", "s", "push", nowMs = 0))
        assertFalse(queue.offer("deploy", "s", "push", nowMs = 1))
        assertEquals(1, queue.pending(nowMs = 1).size)
    }

    @Test
    fun `expiry denies, it never grants`() {
        val queue = ApprovalQueue(defaultTtlMs = 1000)
        queue.offer("rm", "s", "delete the repository", nowMs = 0)

        assertTrue(queue.pending(nowMs = 500).isNotEmpty())
        assertTrue(queue.pending(nowMs = 1500).isEmpty())

        val swept = queue.sweep(nowMs = 1500)
        assertEquals(listOf(ApprovalQueue.Answer.Expired), swept.map { it.answer })
        assertEquals(ApprovalQueue.Answer.Expired, queue.answerFor("rm"))
    }

    @Test
    fun `answering after expiry reports expiry rather than approving`() {
        // The box has already timed this out; sending an approval now is how
        // something runs when nobody meant it to.
        val queue = ApprovalQueue(defaultTtlMs = 1000)
        queue.offer("rm", "s", "delete", nowMs = 0)

        val resolution = queue.answer("rm", approve = true, nowMs = 5000)

        assertEquals(ApprovalQueue.Answer.Expired, resolution?.answer)
    }

    @Test
    fun `answering something never offered sends nothing`() {
        assertNull(ApprovalQueue().answer("unknown", approve = false, nowMs = 0))
    }

    @Test
    fun `a denial is recorded as a denial`() {
        val queue = ApprovalQueue()
        queue.offer("wipe", "s", "format the disk", nowMs = 0)
        assertEquals(
            ApprovalQueue.Answer.Denied,
            queue.answer("wipe", approve = false, nowMs = 1)?.answer,
        )
        assertTrue(queue.pending(nowMs = 2).isEmpty())
    }
}
