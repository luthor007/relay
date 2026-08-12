package glass.relay.bridge.link

import glass.relay.bridge.session.AgentSession
import glass.relay.bridge.session.ApprovalQueue
import glass.relay.bridge.session.SessionRegistry
import glass.relay.bridge.ui.AppView
import org.json.JSONObject

/**
 * What the link's frames actually *do*.
 *
 * `ORCHESTRATOR.md` §5 gives the phone three jobs; two of them are this file.
 * Job 1 is the bridge — `session.list` has to land somewhere a screen can read,
 * and it lands in [SessionRegistry]. Job 3 is approvals — `confirm.request` has
 * to reach a person and the answer has to go back exactly once, which is
 * [ApprovalQueue]'s three properties and why they are not in a dialog.
 *
 * Both of those state machines already existed and were tested; what did not
 * exist was anything feeding them, because there was no link. This is that.
 *
 * ## Speech and notifications are handed out, not handled here
 *
 * `speak`, `notify`, `ui.render`, `digest` and `connector.proposal` need a
 * speaker, a notification manager and a Compose tree, none of which belong in
 * Android-free code. They go to [Sink], which the service implements.
 *
 * ## Unknown frames are surfaced, never fatal
 *
 * A daemon newer than this build will send types this file has never heard of.
 * Refusing them would make every `relayd` release a forced app update.
 */
class RelaydRouter(
    private val link: RelaydLink,
    private val sessions: SessionRegistry,
    private val approvals: ApprovalQueue,
    private val clock: () -> Long,
    private val sink: Sink = object : Sink {},
) : RelaydLink.Listener {

    /** The platform half: speaking, notifying, drawing. */
    interface Sink {
        /** Say it out loud. `interrupt` may talk over narration in progress. */
        fun speak(text: String, sessionId: String?, interrupt: Boolean) {}

        /** Silent-but-present when `silent` — quiet hours. `ADAPTERS.md` §7. */
        fun notify(title: String, body: String, silent: Boolean) {}

        /**
         * A mini-app put something on the screen. `APP-PLATFORM.md` §7.
         *
         * Typed rather than a raw `JSONObject`, because the whole claim of the
         * platform is that the phone draws from a vocabulary it knows: a sink
         * handed loose JSON would have to interpret it, and interpreting is one
         * short step from executing.
         *
         * [AppView.Rendered.actionId] non-null means the view asks something.
         * The request is already in the [ApprovalQueue] by the time this is
         * called, so the sink answers it with the same [answer] every other
         * confirmation uses.
         */
        fun render(view: AppView.Rendered) {}

        /**
         * A view arrived that this build will not draw.
         *
         * Surfaced rather than swallowed: the likeliest cause is a daemon newer
         * than the app, and the user's experience is an app that appears to do
         * nothing. Something has to be able to say why.
         */
        fun renderRefused(reason: String) {}

        fun digest(payload: JSONObject?) {}

        fun connectorProposal(payload: JSONObject?) {}

        /** A confirmation appeared or was retracted; the UI should re-read. */
        fun approvalsChanged() {}

        fun sessionsChanged() {}

        fun linkState(state: RelaydLink.State) {}

        fun linkError(error: LinkException) {}
    }

    fun attach() = link.addListener(this)

    fun detach() = link.removeListener(this)

    // --- inbound --------------------------------------------------------------

    override fun onState(state: RelaydLink.State) = sink.linkState(state)

    override fun onError(error: LinkException) = sink.linkError(error)

    override fun onFrame(envelope: RelayEnvelope) {
        val payload = envelope.payloadObject()
        when (envelope.type) {
            ServerFrame.SESSION_LIST -> onSessionList(payload)
            ServerFrame.CONFIRM_REQUEST -> onConfirmRequest(payload)
            ServerFrame.CONFIRM_RESOLVED -> onConfirmResolved(payload)
            ServerFrame.SPEAK -> sink.speak(
                text = payload?.optString("text").orEmpty(),
                sessionId = payload?.optString("session")?.takeIf { it.isNotEmpty() },
                interrupt = payload?.optBoolean("interrupt", false) ?: false,
            )
            ServerFrame.NOTIFY -> sink.notify(
                title = payload?.optString("title").orEmpty(),
                body = payload?.optString("body").orEmpty(),
                silent = payload?.optBoolean("silent", false) ?: false,
            )
            ServerFrame.UI_RENDER -> onRender(payload)
            ServerFrame.DIGEST -> sink.digest(payload)
            ServerFrame.CONNECTOR_PROPOSAL -> sink.connectorProposal(payload)
            // ack and error are the link's own bookkeeping; RelaydLink has
            // already pruned the outbox by the time this runs.
            ServerFrame.ACK, ServerFrame.ERROR -> Unit
        }
    }

    private fun onSessionList(payload: JSONObject?) {
        val rows = payload?.optJSONArray("sessions") ?: return
        val list = ArrayList<AgentSession>(rows.length())
        for (index in 0 until rows.length()) {
            val row = rows.optJSONObject(index) ?: continue
            val id = row.optString("id")
            if (id.isEmpty()) continue
            list += AgentSession(
                id = id,
                // `wire.go`'s SessionSummary calls it `subject`, not `title`.
                title = row.optString("subject"),
                runtime = row.optString("runtime"),
                state = stateOf(row),
                updatedAtMs = row.optLong("last_active", 0),
                blockedOn = row.optString("state").takeIf { row.optBoolean("blocked", false) },
            )
        }
        sessions.update(list)
        sink.sessionsChanged()
    }

    /**
     * `blocked` outranks the runtime's own state word.
     *
     * `DASHBOARD.md` §3.1 hoists a blocked session because it is the one failure
     * that silently stops all work, and a runtime that reports itself "running"
     * while it waits on a human would sink to the bottom of the list.
     */
    private fun stateOf(row: JSONObject): AgentSession.State {
        if (row.optBoolean("blocked", false)) return AgentSession.State.Blocked
        return when (row.optString("state")) {
            "running", "busy" -> AgentSession.State.Running
            "idle", "ready" -> AgentSession.State.Idle
            "finished", "closed", "exited" -> AgentSession.State.Finished
            "failed", "error" -> AgentSession.State.Failed
            else -> AgentSession.State.Idle
        }
    }

    private fun onConfirmRequest(payload: JSONObject?) {
        if (payload == null) return
        val actionId = payload.optString("action_id")
        if (actionId.isEmpty()) return
        val deadline = payload.optLong("deadline", 0)
        val now = clock()
        val request = ApprovalQueue.Request(
            actionId = actionId,
            sessionId = payload.optString("session"),
            summary = payload.optString("prompt"),
            receivedAtMs = now,
            // The daemon's own deadline where it gave one. A phone that expires
            // a request *later* than the box does is how something gets
            // approved after the box has already given up and run it another way.
            ttlMs = if (deadline > now) deadline - now else DEFAULT_TTL_MS,
            consequential = payload.optBoolean("consequential", true),
        )
        if (approvals.offer(request)) sink.approvalsChanged()
    }

    /**
     * A mini-app drew something.
     *
     * The part worth reading twice is what happens to a view that asks a
     * question: it is offered to the **same** [ApprovalQueue] a runtime's
     * `confirm.request` goes to, so [answer] needs no branch for mini-apps and
     * the queue's three properties — answered once, expiry denies, an unknown id
     * sends nothing — cover an app's question without being restated. An app
     * that could get a second "yes" out of one tap would be a worse hole than a
     * runtime that could, because there are more of them.
     *
     * A view this build cannot draw is refused whole and reported. It is not
     * dropped: an app that appears to do nothing is a support question, and the
     * answer ("your daemon is newer than your phone") is one nobody can guess.
     */
    private fun onRender(payload: JSONObject?) {
        val view = try {
            AppView.parse(payload)
        } catch (err: AppView.Malformed) {
            sink.renderRefused(err.message.orEmpty())
            return
        }

        val question = view.question
        if (question != null && view.actionId != null) {
            val now = clock()
            val deadline = view.deadlineMs
            val offered = approvals.offer(
                ApprovalQueue.Request(
                    actionId = view.actionId,
                    sessionId = "",
                    summary = question.question,
                    receivedAtMs = now,
                    ttlMs = if (deadline > now) deadline - now else DEFAULT_TTL_MS,
                    // An app's question is not automatically consequential — it
                    // reaches the app that asked and nothing outside the box.
                    // The runtime approvals that ARE consequential say so on the
                    // wire; inventing it here would make every card an alarm.
                    consequential = false,
                ),
            )
            if (offered) sink.approvalsChanged()
        }
        sink.render(view)
    }

    /**
     * The question is gone — answered in a terminal, or the turn was cancelled.
     *
     * Resolved as a *denial* rather than dropped: [ApprovalQueue] refuses a
     * second answer to an id it has seen, so recording it here is what stops a
     * queued tap from approving something that is no longer being asked.
     */
    private fun onConfirmResolved(payload: JSONObject?) {
        val actionId = payload?.optString("action_id").orEmpty()
        if (actionId.isEmpty()) return
        approvals.answer(actionId, approve = false, nowMs = clock())
        sink.approvalsChanged()
    }

    // --- outbound -------------------------------------------------------------

    /**
     * Answer a confirmation.
     *
     * Returns null and sends **nothing** when the queue says there is nothing to
     * answer — already answered, expired, or never offered. Sending an approval
     * for an action the box has already timed out is how something runs twice.
     */
    fun answer(actionId: String, approve: Boolean): String? {
        val resolution = approvals.answer(actionId, approve, clock()) ?: return null
        if (resolution.answer == ApprovalQueue.Answer.Expired) {
            sink.approvalsChanged()
            return null
        }
        sink.approvalsChanged()
        return link.send(
            PhoneFrame.CONSENT_DECISION,
            JSONObject()
                .put("action_id", actionId)
                .put("approved", resolution.answer == ApprovalQueue.Answer.Approved),
        )
    }

    /** Sweep expired confirmations. Expiry denies; it never grants. */
    fun sweepExpired(): List<String> {
        val expired = approvals.sweep(clock())
        if (expired.isNotEmpty()) sink.approvalsChanged()
        return expired.map { it.actionId }
    }

    fun requestSessionList(): String? =
        link.send(PhoneFrame.SESSION_COMMAND, JSONObject().put("command", "list"))

    fun sendUtterance(text: String, final: Boolean = true, sessionId: String? = null): String? {
        val payload = JSONObject().put("text", text).put("final", final).put("source", "glasses")
        val target = sessionId ?: sessions.attachedId
        if (target != null) payload.put("session", target)
        return link.send(PhoneFrame.UTTERANCE, payload)
    }

    fun sendWear(worn: Boolean): String? =
        link.send(PhoneFrame.WEAR, JSONObject().put("worn", worn))

    fun sendTouch(gesture: String): String? =
        link.send(PhoneFrame.TOUCH, JSONObject().put("gesture", gesture))

    /**
     * Offer a night's audio. `on_lan` is load-bearing, not informational.
     *
     * `SYSTEM.md` §7: if the box is only reachable through the rendezvous relay,
     * bulk sync defers and says why rather than spending a gigabyte of someone's
     * data plan.
     */
    fun offerSync(files: Int, bytes: Long, onLan: Boolean): String? =
        link.send(
            PhoneFrame.SYNC_OFFER,
            JSONObject().put("files", files).put("bytes", bytes).put("on_lan", onLan),
        )

    private companion object {
        /** `ApprovalQueue`'s own default, restated where the frame is built. */
        const val DEFAULT_TTL_MS = 5 * 60_000L
    }
}
