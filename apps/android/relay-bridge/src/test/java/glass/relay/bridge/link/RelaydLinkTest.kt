package glass.relay.bridge.link

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.Random

/**
 * The link, driven with no network and no real timers.
 *
 * Every case here is one the phone actually hits: the screen goes off, the cell
 * hands over, the app joins the glasses' access point and loses its uplink. The
 * TypeScript's own header calls sleep/wake "the normal path, not the exception",
 * and these are that path.
 */
class RelaydLinkTest {

    private class Recorder : RelaydLink.Listener {
        val states = mutableListOf<RelaydLink.State>()
        val frames = mutableListOf<RelayEnvelope>()
        val unknown = mutableListOf<RelayEnvelope>()
        val errors = mutableListOf<LinkException>()
        val redelivered = mutableListOf<List<String>>()
        val sent = mutableListOf<RelayEnvelope>()

        override fun onState(state: RelaydLink.State) { states += state }
        override fun onFrame(envelope: RelayEnvelope) { frames += envelope }
        override fun onUnknownFrame(envelope: RelayEnvelope) { unknown += envelope }
        override fun onError(error: LinkException) { errors += error }
        override fun onRedelivered(ids: List<String>) { redelivered += ids }
        override fun onSent(envelope: RelayEnvelope) { sent += envelope }
    }

    private class Harness(outboxLimit: Int = 1_000, idleTimeoutMs: Long = 0) {
        val factory = MockRelaySocketFactory()
        val scheduler = FakeLinkScheduler(nowMs = 1_700_000_000_000)
        val recorder = Recorder()
        val link = RelaydLink(
            url = "ws://127.0.0.1:8787/v1/ws",
            auth = LinkAuth(token = "t0ken", deviceId = "phone-1"),
            socketFactory = factory,
            scheduler = scheduler,
            random = Random(1),
            backoff = BackoffOptions(jitter = 0.0),
            outboxLimit = outboxLimit,
            idleTimeoutMs = idleTimeoutMs,
        ).also { it.addListener(recorder) }

        val socket: MockRelaySocket get() = factory.latest!!

        fun connectAndOpen() {
            link.connect()
            socket.acceptOpen()
        }

        fun typesSent(): List<String> = socket.sent.map { parseEnvelope(it).type }

        fun idsSent(): List<String> = socket.sent.map { parseEnvelope(it).id }
    }

    @Test
    fun `an envelope queued before the socket opens goes out on open`() {
        // The ordinary case for a phone: the user says something while the link
        // is still coming back after a screen-off.
        val h = Harness()
        h.link.connect()
        val id = h.link.send(PhoneFrame.UTTERANCE, JSONObject().put("text", "hello"))

        assertNotNull(id)
        assertEquals(0, h.socket.sent.size)

        h.socket.acceptOpen()

        assertEquals(listOf(PhoneFrame.UTTERANCE), h.typesSent())
        assertEquals(listOf(id), h.idsSent())
    }

    @Test
    fun `the envelope on the wire is exactly SYSTEM md section 6 1's shape`() {
        val h = Harness()
        h.connectAndOpen()
        h.link.send(PhoneFrame.WEAR, JSONObject().put("worn", true))

        val json = JSONObject(h.socket.sent.single())

        assertEquals(1, json.getInt("v"))
        assertEquals("wear", json.getString("type"))
        assertEquals(1_700_000_000_000, json.getLong("at"))
        assertTrue(json.getJSONObject("payload").getBoolean("worn"))
        assertTrue(json.getString("id").isNotEmpty())
    }

    @Test
    fun `an abnormal close puts in-flight envelopes back at the head of the queue`() {
        // The single most important behaviour in this file. A send the socket
        // accepted may have died in a buffer, and relayd segments episodes by
        // time — so a redelivered utterance must come back *before* the ones
        // that followed it, not after.
        val h = Harness()
        h.connectAndOpen()
        val first = h.link.send(PhoneFrame.UTTERANCE, JSONObject().put("text", "one"))
        val second = h.link.send(PhoneFrame.UTTERANCE, JSONObject().put("text", "two"))
        assertEquals(2, h.socket.sent.size)

        h.socket.dropAbruptly()
        val third = h.link.send(PhoneFrame.UTTERANCE, JSONObject().put("text", "three"))

        assertEquals(listOf(listOf(first, second)), h.recorder.redelivered)
        assertEquals(listOf(first, second, third), h.link.pendingIds)

        h.scheduler.runPending()
        h.socket.acceptOpen()

        assertEquals(listOf(first, second, third), h.idsSent())
    }

    @Test
    fun `a clean close also replays, because a clean close still loses the buffer`() {
        val h = Harness()
        h.connectAndOpen()
        val id = h.link.send(PhoneFrame.TOUCH, JSONObject().put("gesture", "tap1"))

        h.socket.close(1000, "server restarting")

        assertEquals(listOf(listOf(id)), h.recorder.redelivered)
        assertEquals(1, h.link.pending)
    }

    @Test
    fun `an ack prunes what the link is holding`() {
        val h = Harness()
        h.connectAndOpen()
        val id = h.link.send(PhoneFrame.UTTERANCE, JSONObject().put("text", "one"))
        assertEquals(1, h.link.pending)

        h.socket.deliverAck(id!!)

        assertEquals(0, h.link.pending)

        // And an acked envelope is not replayed when the socket dies.
        h.socket.dropAbruptly()
        assertEquals(emptyList<List<String>>(), h.recorder.redelivered)
    }

    @Test
    fun `an error prunes too, so a refused frame is not retried forever`() {
        // relayd answers audio.chunk with not_implemented, M4 today. A link
        // that treated that as "not delivered" would spend the phone's battery
        // resending it until the milestone ships.
        val h = Harness()
        h.connectAndOpen()
        val id = h.link.send(PhoneFrame.AUDIO_CHUNK, JSONObject().put("seq", 1))

        h.socket.deliverError(
            re = id!!,
            code = LinkErrorCodes.NOT_IMPLEMENTED,
            message = "relayd is not capturing yet — keep it on the device",
            milestone = "M4",
        )

        assertEquals(0, h.link.pending)
        val refusal = h.recorder.errors.single { it.code == LinkException.Code.Refused }
        assertTrue(refusal.message!!.contains("not_implemented"))
        assertTrue("the milestone is the actionable half", refusal.message!!.contains("M4"))
        // The frame is still delivered: a caller told "not stored" has to be
        // able to keep the audio rather than deleting it.
        assertEquals(1, h.recorder.frames.count { it.type == ServerFrame.ERROR })
    }

    @Test
    fun `an unaddressed error is reported and prunes nothing`() {
        val h = Harness()
        h.connectAndOpen()
        h.link.send(PhoneFrame.UTTERANCE, JSONObject().put("text", "one"))

        h.socket.deliverEnvelope(
            ServerFrame.ERROR,
            JSONObject().put("code", LinkErrorCodes.BAD_ENVELOPE).put("message", "frames are JSON text"),
        )

        assertEquals(1, h.link.pending)
        assertTrue(h.recorder.errors.any { it.code == LinkException.Code.Refused })
    }

    @Test
    fun `every server frame relayd implements is routed rather than reported as unknown`() {
        val h = Harness()
        h.connectAndOpen()

        for (type in ServerFrame.ALL) h.socket.deliverEnvelope(type, JSONObject())

        assertEquals(ServerFrame.ALL.size, h.recorder.frames.size)
        assertEquals(emptyList<RelayEnvelope>(), h.recorder.unknown)
    }

    @Test
    fun `a type this build has never heard of is surfaced, never fatal`() {
        // A daemon newer than the app must not force an update to stay usable.
        val h = Harness()
        h.connectAndOpen()

        h.socket.deliverEnvelope("memory.recall", JSONObject().put("q", "what did I say"))

        assertEquals(1, h.recorder.unknown.size)
        assertEquals("memory.recall", h.recorder.unknown.single().type)
        assertEquals(RelaydLink.State.Open, h.link.currentState)
    }

    @Test
    fun `a malformed inbound frame is an error, not a disconnect`() {
        val h = Harness()
        h.connectAndOpen()

        h.socket.deliver("{ this is not json")

        assertEquals(RelaydLink.State.Open, h.link.currentState)
        assertTrue(h.recorder.errors.any { it.code == LinkException.Code.Malformed })
    }

    @Test
    fun `a full outbox refuses the newest and says so`() {
        // Refuse the newest, never evict the oldest — the same rule as
        // StoreAndForwardQueue. Dropping the oldest here discards the utterance
        // the user is waiting on an answer to.
        val h = Harness(outboxLimit = 2)
        h.link.connect()
        val first = h.link.send(PhoneFrame.UTTERANCE, JSONObject().put("text", "one"))
        val second = h.link.send(PhoneFrame.UTTERANCE, JSONObject().put("text", "two"))

        val third = h.link.send(PhoneFrame.UTTERANCE, JSONObject().put("text", "three"))

        assertNull(third)
        assertEquals(listOf(first, second), h.link.pendingIds)
        assertTrue(h.recorder.errors.any { it.code == LinkException.Code.OutboxFull })
    }

    @Test
    fun `backoff grows and wake collapses it`() {
        val h = Harness()
        h.link.connect()

        h.socket.dropAbruptly()
        assertEquals(RelaydLink.State.Reconnecting, h.link.currentState)
        assertEquals(500L, h.scheduler.nextDelayMs())

        h.scheduler.advance(500)
        h.socket.dropAbruptly()
        assertEquals(1_000L, h.scheduler.nextDelayMs())

        h.scheduler.advance(1_000)
        h.socket.dropAbruptly()
        assertEquals(2_000L, h.scheduler.nextDelayMs())

        // The OS says the network is back. Waiting out two seconds of a timer
        // in good WiFi is the sleep/wake case behaving badly.
        h.link.wake()
        assertEquals(RelaydLink.State.Connecting, h.link.currentState)
        h.socket.acceptOpen()
        assertEquals(0, h.link.attempts)
    }

    @Test
    fun `a deliberate close stays closed, and wake does not reopen it`() {
        val h = Harness()
        h.connectAndOpen()

        h.link.close()

        assertEquals(RelaydLink.State.Closed, h.link.currentState)
        h.link.wake()
        assertEquals(RelaydLink.State.Closed, h.link.currentState)
        assertNull("a closed link schedules no retry", h.scheduler.nextDelayMs())
    }

    @Test
    fun `closing keeps queued work, because the app closing is not the user retracting`() {
        val h = Harness()
        h.link.connect()
        val id = h.link.send(PhoneFrame.UTTERANCE, JSONObject().put("text", "remember this"))

        h.link.close()

        assertEquals(listOf(id), h.link.pendingIds)

        h.link.connect()
        h.socket.acceptOpen()
        assertEquals(listOf(id), h.idsSent())
    }

    @Test
    fun `a socket that refuses a send leaves the envelope at the head`() {
        val h = Harness()
        h.connectAndOpen()
        h.socket.sendFails = true

        val id = h.link.send(PhoneFrame.UTTERANCE, JSONObject().put("text", "one"))

        assertEquals(listOf(id), h.link.pendingIds)
        assertTrue(h.recorder.errors.any { it.code == LinkException.Code.SocketFailed })
    }

    @Test
    fun `a factory that cannot open schedules a retry instead of throwing`() {
        val h = Harness()
        h.factory.openFails = true

        h.link.connect()

        assertEquals(RelaydLink.State.Reconnecting, h.link.currentState)
        assertTrue(h.recorder.errors.any { it.code == LinkException.Code.SocketFailed })
        assertEquals(500L, h.scheduler.nextDelayMs())
    }

    @Test
    fun `an idle link closes itself rather than holding a half-open socket`() {
        // A file descriptor and a wake lock held forever is the failure this
        // prevents; the reconnect that follows is the recovery.
        val h = Harness(idleTimeoutMs = 60_000)
        h.connectAndOpen()

        h.scheduler.advance(60_000)

        assertTrue(h.recorder.errors.any { it.message!!.contains("half-open") })
        assertEquals(RelaydLink.State.Reconnecting, h.link.currentState)
    }

    @Test
    fun `inbound traffic re-arms the idle timer`() {
        val h = Harness(idleTimeoutMs = 60_000)
        h.connectAndOpen()

        h.scheduler.advance(50_000)
        h.socket.deliverEnvelope(ServerFrame.SPEAK, JSONObject().put("text", "still here"))
        h.scheduler.advance(50_000)

        assertEquals(RelaydLink.State.Open, h.link.currentState)
    }

    @Test
    fun `the socket is opened with the bearer token relayd checks`() {
        val h = Harness()
        h.link.connect()

        assertEquals("Bearer t0ken", h.socket.auth.authorizationHeader())
        assertEquals("ws://127.0.0.1:8787/v1/ws", h.socket.url)
    }

    @Test
    fun `a dead socket's late close does not disturb its replacement`() {
        val h = Harness()
        h.connectAndOpen()
        val stale = h.socket

        stale.dropAbruptly()
        h.scheduler.runPending()
        val fresh = h.socket
        fresh.acceptOpen()

        // The old socket coughs after the link has moved on. Acting on it would
        // cancel the retry for a connection that no longer exists.
        stale.dropAbruptly()

        assertEquals(RelaydLink.State.Open, h.link.currentState)
    }
}

/**
 * Failing over to the relay, and coming home again.
 *
 * The address list is decided in `BoxAddressTest`. This is the half with the
 * reconnect loop in it: that the link alternates rather than sticking, and that
 * a successful connection puts it back at the front of the list.
 */
class RelaydLinkRouteTest {

    private class Recorder : RelaydLink.Listener {
        val states = mutableListOf<RelaydLink.State>()
        override fun onState(state: RelaydLink.State) { states += state }
    }

    private val addresses = listOf(
        BoxEndpoint("ws://relay.local:8765", BoxRoute.Direct),
        BoxEndpoint("wss://rz.relay.glass/rz/v1/connect/box-abc", BoxRoute.Relay),
    )

    private class Harness(addresses: List<BoxEndpoint>) {
        val factory = MockRelaySocketFactory()
        val scheduler = FakeLinkScheduler(nowMs = 1_700_000_000_000)
        val link = RelaydLink(
            addresses = addresses,
            auth = LinkAuth(token = "t0ken", deviceId = "phone-1"),
            socketFactory = factory,
            scheduler = scheduler,
            random = Random(1),
            backoff = BackoffOptions(jitter = 0.0),
        )

        /** Let the current attempt fail and run the backoff timer. */
        fun failAndRetry() {
            factory.latest!!.dropAbruptly()
            scheduler.runPending()
        }

        fun urls(): List<String> = factory.sockets.map { it.url }
    }

    @Test
    fun `a phone that cannot reach the LAN tries the relay next`() {
        // The defect this exists to prevent: a phone that leaves the house
        // retrying a hostname that does not resolve, forever, while a perfectly
        // good relay sits unused.
        val h = Harness(addresses)
        h.link.connect()
        assertEquals("ws://relay.local:8765", h.urls().last())

        h.failAndRetry()
        assertEquals("wss://rz.relay.glass/rz/v1/connect/box-abc", h.urls().last())
        assertEquals(BoxRoute.Relay, h.link.currentRoute)
    }

    @Test
    fun `it alternates rather than settling on whichever answered`() {
        // Sticking to the relay once it works would leave a household's traffic
        // on our bill until the app was restarted — and SYSTEM.md §7's whole
        // bandwidth argument is that the day's audio never crosses it.
        val h = Harness(addresses)
        h.link.connect()
        repeat(3) { h.failAndRetry() }
        assertEquals(
            listOf(
                "ws://relay.local:8765",
                "wss://rz.relay.glass/rz/v1/connect/box-abc",
                "ws://relay.local:8765",
                "wss://rz.relay.glass/rz/v1/connect/box-abc",
            ),
            h.urls(),
        )
    }

    @Test
    fun `a connection that opens puts the phone back at the front of the list`() {
        // `attempt` resets on open, so the next reconnect starts at the direct
        // address. That is how a phone that failed over at the office returns to
        // the LAN by itself when it gets home, with nothing having to notice.
        val h = Harness(addresses)
        h.link.connect()
        h.failAndRetry()
        h.factory.latest!!.acceptOpen()
        assertEquals(BoxRoute.Relay, h.link.currentRoute)

        h.failAndRetry()
        assertEquals("ws://relay.local:8765", h.urls().last())
        assertEquals(BoxRoute.Direct, h.link.currentRoute)
    }

    @Test
    fun `one address still works, which is a box with no relay`() {
        val h = Harness(listOf(BoxEndpoint("ws://relay.local:8765", BoxRoute.Direct)))
        h.link.connect()
        h.failAndRetry()
        assertEquals(listOf("ws://relay.local:8765", "ws://relay.local:8765"), h.urls())
    }

    @Test
    fun `a link with nowhere to dial is refused at construction`() {
        // It would retry forever against an empty list and divide by zero doing
        // it. The caller's job is to not build one, and this asserts that
        // contract rather than papering over it.
        var refused = false
        try {
            RelaydLink(
                addresses = emptyList(),
                auth = LinkAuth(token = "t"),
                socketFactory = MockRelaySocketFactory(),
                scheduler = FakeLinkScheduler(nowMs = 0),
            )
        } catch (expected: IllegalArgumentException) {
            refused = true
        }
        assertTrue("an empty address list was accepted", refused)
    }
}
