package glass.relay.bridge.link

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The ordering rules on their own, with no socket and no clock.
 *
 * `RelaydLinkTest` covers these through the link; this file covers them where
 * they are decided, because the head-of-queue replay is the property the whole
 * at-least-once story rests on and it should fail here first.
 */
class OutboxTest {

    private fun entry(id: String): Outbox.Entry {
        val envelope = RelayEnvelope(id = id, type = PhoneFrame.UTTERANCE, atMs = 0)
        return Outbox.Entry(envelope, envelope.serialise())
    }

    @Test
    fun `sent envelopes leave the pending queue but stay counted`() {
        // In-flight is not delivered. A send the socket accepted may still have
        // died in a kernel buffer, and nothing below us will say so.
        val outbox = Outbox()
        outbox.offer(entry("a"))

        outbox.markSent()

        assertEquals(0, outbox.pendingCount)
        assertEquals(1, outbox.inFlightCount)
        assertEquals(1, outbox.size)
    }

    @Test
    fun `requeue puts unacknowledged work back at the head, oldest first`() {
        // relayd segments episodes by time, so a redelivered utterance that
        // lands after the three that followed it joins the wrong conversation.
        val outbox = Outbox()
        outbox.offer(entry("a"))
        outbox.offer(entry("b"))
        outbox.markSent()
        outbox.markSent()
        outbox.offer(entry("c"))

        val returned = outbox.requeueInFlight()

        assertEquals(listOf("a", "b"), returned.map { it.envelope.id })
        assertEquals(listOf("a", "b", "c"), outbox.ids)
    }

    @Test
    fun `an acknowledged envelope is not replayed`() {
        val outbox = Outbox()
        outbox.offer(entry("a"))
        outbox.offer(entry("b"))
        outbox.markSent()
        outbox.markSent()

        assertTrue(outbox.acknowledge("a"))
        val returned = outbox.requeueInFlight()

        assertEquals(listOf("b"), returned.map { it.envelope.id })
    }

    @Test
    fun `acknowledging something that was never sent is a no-op, not an error`() {
        val outbox = Outbox()
        assertFalse(outbox.acknowledge("never-heard-of-it"))
    }

    @Test
    fun `the limit counts in-flight work too`() {
        // A phone whose socket keeps accepting frames that are never
        // acknowledged is exactly as full as one whose socket accepts nothing.
        val outbox = Outbox(limit = 2)
        outbox.offer(entry("a"))
        outbox.markSent()
        outbox.offer(entry("b"))

        assertTrue(outbox.isFull)
        assertFalse(outbox.offer(entry("c")))
        assertEquals(listOf("a", "b"), outbox.ids)
    }

    @Test
    fun `requeue with nothing in flight changes nothing`() {
        val outbox = Outbox()
        outbox.offer(entry("a"))

        assertEquals(emptyList<Outbox.Entry>(), outbox.requeueInFlight())
        assertEquals(listOf("a"), outbox.ids)
    }

    @Test
    fun `an empty outbox has no head`() {
        assertNull(Outbox().head())
        assertNull(Outbox().markSent())
    }
}
