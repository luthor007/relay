package glass.relay.bridge.session

/**
 * The phone's view of what the box is running, and of what it is being asked.
 *
 * `ORCHESTRATOR.md` §5 job 2 wants session list and attach on the phone, and
 * §4b wants consequential actions confirmed every time. `SYSTEM.md` §6.1 fixes
 * the vocabulary: the server sends `session.list` and `confirm.request`, the
 * phone sends `session.command`. Nothing here invents a message type.
 *
 * Both classes are plain state machines over those messages, with no transport
 * and no Android. The relayd client for this platform does not exist yet — the
 * contract lives in `glasses/bridge/src/relayd.ts` — so this is deliberately the
 * half that can be written and tested before it does.
 */

/** One agent session on the box, as `session.list` describes it. */
data class AgentSession(
    val id: String,
    val title: String,
    val runtime: String,
    val state: State,
    val updatedAtMs: Long,
    /** Set when the session is waiting on a person rather than working. */
    val blockedOn: String? = null,
) {
    enum class State { Idle, Running, Blocked, Finished, Failed }

    /**
     * Whether this one should jump the queue.
     *
     * A blocked session is not making progress and is answerable in one
     * sentence, so it goes to the top. `ADAPTERS.md` §7's reasoning, applied to
     * a list instead of a notification.
     */
    val needsAttention: Boolean get() = state == State.Blocked
}

/**
 * The session list, plus which one the user is talking to.
 *
 * "Attach" is a phone-side concept: it decides where an utterance is routed
 * when the user does not say. Routing announces its choice and is undoable
 * (`ORCHESTRATOR.md` §4), which is why [attach] returns the previous target
 * rather than swallowing it.
 */
class SessionRegistry {

    private var sessions: List<AgentSession> = emptyList()

    var attachedId: String? = null
        private set

    /**
     * Replace the list from a `session.list` envelope.
     *
     * Ordered by attention first, then by recency: a blocked session is the one
     * thing on this screen that is costing the user time.
     */
    fun update(list: List<AgentSession>) {
        sessions = list.sortedWith(
            compareByDescending<AgentSession> { it.needsAttention }
                .thenByDescending { it.updatedAtMs },
        )
        // A session that no longer exists cannot stay attached, or the next
        // utterance is routed into nothing.
        if (attachedId != null && sessions.none { it.id == attachedId }) attachedId = null
    }

    fun all(): List<AgentSession> = sessions

    fun attached(): AgentSession? = sessions.firstOrNull { it.id == attachedId }

    fun blocked(): List<AgentSession> = sessions.filter { it.needsAttention }

    /** Returns the session that was attached before, so the UI can offer undo. */
    fun attach(id: String): AgentSession? {
        require(sessions.any { it.id == id }) { "no such session: $id" }
        val previous = attached()
        attachedId = id
        return previous
    }

    fun detach(): AgentSession? = attached().also { attachedId = null }
}

/**
 * Confirmations the agent is waiting on.
 *
 * `ORCHESTRATOR.md` §4b: consequential actions are confirmed every time and the
 * confirmation is not suppressible. Three properties follow, and all three are
 * here rather than in a dialog:
 *
 *  - **Answer once.** A second answer to the same request is refused, so a
 *    double tap or a replayed envelope cannot approve something twice.
 *  - **Expire rather than linger.** A confirmation the user never saw must not
 *    sit there until it is answered by accident an hour later.
 *  - **Expiry is a denial, not a grant.** Timing out approves nothing.
 */
class ApprovalQueue(private val defaultTtlMs: Long = 5 * 60_000) {

    /** A `confirm.request` envelope, as far as the phone cares about it. */
    data class Request(
        val actionId: String,
        val sessionId: String,
        val summary: String,
        val receivedAtMs: Long,
        val ttlMs: Long,
        /** True where the agent flagged it as destructive or irreversible. */
        val consequential: Boolean = true,
    ) {
        fun expiredAt(nowMs: Long): Boolean = nowMs >= receivedAtMs + ttlMs
    }

    enum class Answer { Approved, Denied, Expired }

    data class Resolution(val actionId: String, val answer: Answer)

    private val open = LinkedHashMap<String, Request>()
    private val answered = LinkedHashMap<String, Answer>()

    fun offer(request: Request): Boolean {
        if (answered.containsKey(request.actionId)) return false
        if (open.containsKey(request.actionId)) return false
        open[request.actionId] = request
        return true
    }

    fun offer(actionId: String, sessionId: String, summary: String, nowMs: Long): Boolean =
        offer(Request(actionId, sessionId, summary, nowMs, defaultTtlMs))

    /** Everything still awaiting an answer, oldest first. */
    fun pending(nowMs: Long): List<Request> =
        open.values.filterNot { it.expiredAt(nowMs) }

    /**
     * Answer one request.
     *
     * Returns null when there is nothing to answer — already answered, expired,
     * or never offered. The caller must treat null as "do not send anything",
     * because sending an approval for an action the box has already timed out is
     * how something runs twice.
     */
    fun answer(actionId: String, approve: Boolean, nowMs: Long): Resolution? {
        val request = open[actionId] ?: return null
        if (request.expiredAt(nowMs)) {
            open.remove(actionId)
            answered[actionId] = Answer.Expired
            return Resolution(actionId, Answer.Expired)
        }
        open.remove(actionId)
        val answer = if (approve) Answer.Approved else Answer.Denied
        answered[actionId] = answer
        return Resolution(actionId, answer)
    }

    /** Sweep expired requests. Expiry denies; it never grants. */
    fun sweep(nowMs: Long): List<Resolution> {
        val expired = open.values.filter { it.expiredAt(nowMs) }
        for (request in expired) {
            open.remove(request.actionId)
            answered[request.actionId] = Answer.Expired
        }
        return expired.map { Resolution(it.actionId, Answer.Expired) }
    }

    fun answerFor(actionId: String): Answer? = answered[actionId]
}
