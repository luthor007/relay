package glass.relay.bridge.link

import glass.relay.bridge.session.ApprovalQueue
import glass.relay.bridge.session.SessionRegistry
import glass.relay.bridge.ui.AppView
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.Random

/**
 * `ORCHESTRATOR.md` §5 jobs 1 and 3, end to end over the wire format.
 *
 * `SessionRegistry` and `ApprovalQueue` were already tested as state machines,
 * but nothing fed them: the phone had no link. These are the assertions that a
 * frame `relayd` actually sends turns into the state a screen renders, and that
 * the answer goes back in a frame `relayd` actually parses.
 */
class RelaydRouterTest {

    private class Sink : RelaydRouter.Sink {
        val spoken = mutableListOf<Triple<String, String?, Boolean>>()
        val notified = mutableListOf<Triple<String, String, Boolean>>()
        val rendered = mutableListOf<AppView.Rendered>()
        val renderRefusals = mutableListOf<String>()
        var approvalChanges = 0
        var sessionChanges = 0
        val states = mutableListOf<RelaydLink.State>()

        override fun speak(text: String, sessionId: String?, interrupt: Boolean) {
            spoken += Triple(text, sessionId, interrupt)
        }

        override fun notify(title: String, body: String, silent: Boolean) {
            notified += Triple(title, body, silent)
        }

        override fun render(view: AppView.Rendered) { rendered += view }
        override fun renderRefused(reason: String) { renderRefusals += reason }
        override fun approvalsChanged() { approvalChanges += 1 }
        override fun sessionsChanged() { sessionChanges += 1 }
        override fun linkState(state: RelaydLink.State) { states += state }
    }

    private class Harness(nowMs: Long = 1_700_000_000_000) {
        val factory = MockRelaySocketFactory()
        val scheduler = FakeLinkScheduler(nowMs)
        val sessions = SessionRegistry()
        val approvals = ApprovalQueue()
        val sink = Sink()
        val link = RelaydLink(
            url = "ws://127.0.0.1:8787/v1/ws",
            auth = LinkAuth("t0ken"),
            socketFactory = factory,
            scheduler = scheduler,
            random = Random(3),
        )
        val router = RelaydRouter(link, sessions, approvals, scheduler::now, sink).also { it.attach() }
        val socket: MockRelaySocket get() = factory.latest!!

        init {
            link.connect()
            socket.acceptOpen()
        }

        fun lastSent(): JSONObject = JSONObject(socket.sent.last())
    }

    private fun sessionRow(
        id: String,
        subject: String,
        state: String,
        blocked: Boolean = false,
        lastActive: Long = 0,
    ) = JSONObject()
        .put("id", id)
        .put("runtime", "claude")
        .put("subject", subject)
        .put("workspace", "/w")
        .put("state", state)
        .put("last_active", lastActive)
        .put("blocked", blocked)
        .put("live", true)

    @Test
    fun `a session list frame becomes the list a screen renders`() {
        val h = Harness()

        h.socket.deliverEnvelope(
            ServerFrame.SESSION_LIST,
            JSONObject().put(
                "sessions",
                JSONArray()
                    .put(sessionRow("a", "payments refactor", "running", lastActive = 10))
                    .put(sessionRow("b", "flaky test", "idle", blocked = true, lastActive = 5)),
            ),
        )

        val all = h.sessions.all()
        assertEquals(2, all.size)
        // Blocked first: DASHBOARD.md §3.1 hoists the one failure that silently
        // stops all work, even though it is the older row.
        assertEquals("b", all.first().id)
        assertEquals("flaky test", all.first().title)
        assertTrue(all.first().needsAttention)
        assertEquals(1, h.sink.sessionChanges)
    }

    @Test
    fun `the row's field is subject, which is what wire go calls it`() {
        // A phone reading `title` off a frame that carries `subject` shows a
        // list of blank rows and looks like an empty box.
        val h = Harness()

        h.socket.deliverEnvelope(
            ServerFrame.SESSION_LIST,
            JSONObject().put("sessions", JSONArray().put(sessionRow("a", "the subject", "idle"))),
        )

        assertEquals("the subject", h.sessions.all().single().title)
    }

    @Test
    fun `a confirm request reaches the approval queue and the answer goes back once`() {
        val h = Harness()

        h.socket.deliverEnvelope(
            ServerFrame.CONFIRM_REQUEST,
            JSONObject()
                .put("action_id", "act-1")
                .put("session", "a")
                .put("runtime", "claude")
                .put("ask", "permission")
                .put("prompt", "Delete the staging database?")
                .put("consequential", true),
        )

        assertEquals(1, h.approvals.pending(h.scheduler.now()).size)

        val id = h.router.answer("act-1", approve = true)

        assertTrue(id != null)
        val frame = h.lastSent()
        assertEquals(PhoneFrame.CONSENT_DECISION, frame.getString("type"))
        assertEquals("act-1", frame.getJSONObject("payload").getString("action_id"))
        assertTrue(frame.getJSONObject("payload").getBoolean("approved"))

        // A double tap, or a replayed envelope, must not approve twice.
        val second = h.router.answer("act-1", approve = true)
        assertNull(second)
        assertEquals(1, h.socket.sent.count { JSONObject(it).getString("type") == PhoneFrame.CONSENT_DECISION })
    }

    @Test
    fun `answering after the box has given up sends nothing`() {
        // Sending an approval for an action relayd has already timed out is how
        // something runs twice. Expiry denies; it never grants.
        val h = Harness()
        h.socket.deliverEnvelope(
            ServerFrame.CONFIRM_REQUEST,
            JSONObject()
                .put("action_id", "act-1")
                .put("session", "a")
                .put("prompt", "Push to main?")
                .put("deadline", h.scheduler.now() + 30_000),
        )

        h.scheduler.advance(31_000)
        val id = h.router.answer("act-1", approve = true)

        assertNull(id)
        assertEquals(0, h.socket.sent.count { JSONObject(it).getString("type") == PhoneFrame.CONSENT_DECISION })
    }

    @Test
    fun `the daemon's own deadline is honoured rather than the phone's default`() {
        // A phone that expires later than the box does is how an answer arrives
        // after the box has already taken another route.
        val h = Harness()
        h.socket.deliverEnvelope(
            ServerFrame.CONFIRM_REQUEST,
            JSONObject()
                .put("action_id", "act-1")
                .put("session", "a")
                .put("prompt", "rm -rf?")
                .put("deadline", h.scheduler.now() + 10_000),
        )

        h.scheduler.advance(11_000)

        assertEquals(0, h.approvals.pending(h.scheduler.now()).size)
    }

    @Test
    fun `confirm resolved retracts a question that has already been answered elsewhere`() {
        // Codex's serverRequest/resolved: it was approved in a terminal. Without
        // the retraction a ping outlives its question and wakes someone to
        // approve what is already approved.
        val h = Harness()
        h.socket.deliverEnvelope(
            ServerFrame.CONFIRM_REQUEST,
            JSONObject().put("action_id", "act-1").put("session", "a").put("prompt", "Merge?"),
        )

        h.socket.deliverEnvelope(
            ServerFrame.CONFIRM_RESOLVED,
            JSONObject().put("action_id", "act-1").put("reason", "answered in a terminal"),
        )

        assertEquals(0, h.approvals.pending(h.scheduler.now()).size)
        assertNull("a queued tap must not approve it now", h.router.answer("act-1", approve = true))
    }

    @Test
    fun `speak and notify reach the platform sink, silent flag intact`() {
        val h = Harness()

        h.socket.deliverEnvelope(
            ServerFrame.SPEAK,
            JSONObject().put("text", "adding that to the payments refactor")
                .put("session", "a").put("interrupt", false),
        )
        h.socket.deliverEnvelope(
            ServerFrame.NOTIFY,
            JSONObject().put("title", "Build finished").put("body", "3 tests failed").put("silent", true),
        )

        assertEquals(
            Triple("adding that to the payments refactor", "a", false),
            h.sink.spoken.single(),
        )
        // Quiet hours hold the speech and keep the notification. ADAPTERS.md §7.
        assertEquals(Triple("Build finished", "3 tests failed", true), h.sink.notified.single())
    }

    @Test
    fun `an utterance is routed to the attached session`() {
        val h = Harness()
        h.socket.deliverEnvelope(
            ServerFrame.SESSION_LIST,
            JSONObject().put("sessions", JSONArray().put(sessionRow("a", "payments", "idle"))),
        )
        h.sessions.attach("a")

        h.router.sendUtterance("where was I")

        val payload = h.lastSent().getJSONObject("payload")
        assertEquals(PhoneFrame.UTTERANCE, h.lastSent().getString("type"))
        assertEquals("a", payload.getString("session"))
        assertTrue(payload.getBoolean("final"))
    }

    @Test
    fun `a sync offer carries whether the phone is on the LAN`() {
        // SYSTEM.md §7: if the box is only reachable through the relay, bulk
        // sync defers rather than spending a gigabyte of someone's data plan.
        val h = Harness()

        h.router.offerSync(files = 64, bytes = 173_000_000, onLan = false)

        val payload = h.lastSent().getJSONObject("payload")
        assertEquals(64, payload.getInt("files"))
        assertEquals(false, payload.getBoolean("on_lan"))
    }

    @Test
    fun `an unknown frame does not disturb the router`() {
        val h = Harness()

        h.socket.deliverEnvelope("memory.recall", JSONObject().put("q", "?"))

        assertEquals(0, h.sink.approvalChanges)
        assertEquals(RelaydLink.State.Open, h.link.currentState)
    }

    @Test
    fun `a session list with no rows clears the attachment rather than routing into nothing`() {
        val h = Harness()
        h.socket.deliverEnvelope(
            ServerFrame.SESSION_LIST,
            JSONObject().put("sessions", JSONArray().put(sessionRow("a", "payments", "idle"))),
        )
        h.sessions.attach("a")

        h.socket.deliverEnvelope(ServerFrame.SESSION_LIST, JSONObject().put("sessions", JSONArray()))

        assertNull(h.sessions.attachedId)
    }

    // --- mini-app views -------------------------------------------------------

    private fun renderFrame(view: String, extra: JSONObject.() -> Unit = {}): JSONObject =
        JSONObject()
            .put("app", "dev.test.standup")
            .put("appName", "Standup")
            .put("view", JSONObject(view))
            .apply(extra)

    @Test
    fun `a mini-app view reaches the sink typed`() {
        val h = Harness()
        h.socket.deliverEnvelope(
            ServerFrame.UI_RENDER,
            renderFrame("""{"vocabulary":1,"blocks":[{"kind":"card","title":"Standup","body":"Two things."}]}"""),
        )

        assertEquals(1, h.sink.rendered.size)
        val view = h.sink.rendered.single()
        assertEquals("dev.test.standup", view.app)
        assertEquals("Standup", view.appName)
        assertNull(view.actionId)
        assertTrue(view.blocks.single() is AppView.Block.Card)
    }

    @Test
    fun `a mini-app question goes through the same approval queue as a runtime's`() {
        // The point of this test. If an app's question had its own queue, the
        // once-only answer guarantee would be written twice and the second copy
        // would be the one that is wrong.
        val h = Harness()
        h.socket.deliverEnvelope(
            ServerFrame.UI_RENDER,
            renderFrame(
                """{"vocabulary":1,"blocks":[
                    {"kind":"card","title":"About to send"},
                    {"kind":"confirm","question":"Send it?"}]}""",
            ) { put("action_id", "act-app-1") },
        )

        assertEquals(1, h.sink.rendered.size)
        assertEquals("act-app-1", h.sink.rendered.single().actionId)
        assertEquals(1, h.approvals.pending(h.scheduler.now()).size)

        // Answered through the ordinary path, with no branch for mini-apps.
        assertTrue(h.router.answer("act-app-1", approve = true) != null)
        val sent = h.lastSent()
        assertEquals(PhoneFrame.CONSENT_DECISION, sent.getString("type"))
        assertEquals("act-app-1", sent.getJSONObject("payload").getString("action_id"))
        assertTrue(sent.getJSONObject("payload").getBoolean("approved"))

        // And the queue's second property, inherited rather than restated: a
        // second tap sends nothing.
        assertNull(h.router.answer("act-app-1", approve = true))
    }

    @Test
    fun `a view this build cannot draw is reported rather than dropped`() {
        val h = Harness()
        h.socket.deliverEnvelope(
            ServerFrame.UI_RENDER,
            renderFrame("""{"vocabulary":9,"blocks":[{"kind":"card","title":"Standup"}]}"""),
        )
        // An app that appears to do nothing is a support question whose answer
        // ("your daemon is newer than your phone") nobody can guess.
        assertEquals(0, h.sink.rendered.size)
        assertEquals(1, h.sink.renderRefusals.size)
        assertTrue(h.sink.renderRefusals.single().contains("vocabulary 9"))
        assertEquals(0, h.approvals.pending(h.scheduler.now()).size)
    }

}
