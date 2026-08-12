package glass.relay.bridge.link

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.Random

/**
 * `SYSTEM.md` §6.1's envelope, and the frame set `relayd` actually implements.
 *
 * The vocabulary assertions are the point of this file. Three implementations
 * have to agree on the spelling of every type name — a phone that listens for
 * `link.ack` while the daemon sends `ack` looks like a working link right up
 * until the outbox fills.
 */
class EnvelopeTest {

    @Test
    fun `an envelope round-trips through the wire format`() {
        val envelope = RelayEnvelope(
            id = "6f1a-…",
            type = PhoneFrame.UTTERANCE,
            atMs = 1_700_000_000_000,
            payload = JSONObject().put("text", "where was I").put("final", true),
        )

        val parsed = parseEnvelope(envelope.serialise())

        assertEquals(envelope.id, parsed.id)
        assertEquals(envelope.type, parsed.type)
        assertEquals(envelope.atMs, parsed.atMs)
        assertEquals("where was I", parsed.payloadObject()?.optString("text"))
        assertEquals(LINK_VERSION, parsed.v)
    }

    @Test
    fun `a frame with no payload keeps the key absent, matching the Go omitempty`() {
        val text = RelayEnvelope(id = "a", type = PhoneFrame.WEAR, atMs = 1).serialise()

        assertTrue("payload must not appear at all", !text.contains("payload"))
        assertNull(parseEnvelope(text).payload)
    }

    @Test
    fun `a different envelope version is refused rather than best-guessed`() {
        val error = runCatching {
            parseEnvelope("""{"v":2,"id":"a","type":"speak","at":1,"payload":{}}""")
        }.exceptionOrNull() as LinkException

        assertEquals(LinkException.Code.VersionMismatch, error.code)
    }

    @Test
    fun `a half-parsing envelope is malformed, not accepted`() {
        // A UI that acts on a field it invented is worse than one that reports a
        // bad frame, and §6.1 is a contract three languages implement.
        val bad = listOf(
            "not json at all",
            """{"v":1,"type":"speak","at":1}""",
            """{"v":1,"id":"","type":"speak","at":1}""",
            """{"v":1,"id":"a","at":1}""",
            """{"v":1,"id":"a","type":"speak"}""",
            """{"v":1,"id":"a","type":"speak","at":"soon"}""",
            """{"id":"a","type":"speak","at":1}""",
        )

        for (text in bad) {
            val error = runCatching { parseEnvelope(text) }.exceptionOrNull()
            assertTrue("expected a LinkException for: $text", error is LinkException)
        }
    }

    @Test
    fun `the acknowledgement frame is ack, not link ack`() {
        // relayd/internal/api/wire.go names it `ack` with a payload of
        // {re, ok}. The TypeScript invented `link.ack` with {ids: [...]}
        // before the daemon existed. A phone built against the invented one
        // never prunes its outbox.
        assertEquals("ack", ServerFrame.ACK)
        assertTrue(ServerFrame.ACK in ServerFrame.ALL)
        assertTrue("link.ack" !in ServerFrame.ALL)
    }

    @Test
    fun `the server frame set is the six product frames plus the four transport ones`() {
        assertEquals(
            setOf(
                "speak", "ui.render", "session.list", "confirm.request",
                "connector.proposal", "digest",
                "ack", "error", "notify", "confirm.resolved",
            ),
            ServerFrame.ALL,
        )
    }

    @Test
    fun `the phone frame set is exactly what SYSTEM md section 6 1 lists`() {
        assertEquals(
            setOf(
                "utterance", "touch", "wear", "audio.chunk", "photo",
                "session.command", "consent.decision", "sync.offer",
            ),
            PhoneFrame.ALL,
        )
    }

    @Test
    fun `ids are RFC 4122 version 4 and come from the injected randomness`() {
        val first = newEnvelopeId(Random(7))
        val second = newEnvelopeId(Random(7))

        assertEquals("a seeded source must produce a stable id, or replay tests cannot exist", first, second)
        assertTrue(Regex("^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$").matches(first))
        assertTrue(first != newEnvelopeId(Random(8)))
    }
}
