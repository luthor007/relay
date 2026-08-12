package glass.relay.bridge

import glass.relay.bridge.session.AgentSession
import glass.relay.bridge.session.ApprovalQueue
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * How the UI sees what the capture service is doing.
 *
 * [RelayCaptureService] returns null from `onBind` on purpose — capture must not
 * depend on anything staying bound to it, because the whole point of the service
 * is to outlive every Activity. But the UI still needs to render real state, and
 * polling a service you cannot bind to is not a thing.
 *
 * So the service publishes here and the UI collects. Both live in the same
 * process, so this is a plain in-memory hand-off, not IPC.
 *
 * The honest caveat: if the process is killed and restarted by the system, this
 * resets to [CaptureState.Idle] before the service republishes. The UI therefore
 * treats `Idle` as "not known yet", never as "definitely not recording" — see
 * [isCapturing].
 */
object CaptureBus {

    private val _state = MutableStateFlow(CaptureState.Idle)
    val state: StateFlow<CaptureState> = _state.asStateFlow()

    /** Whether the service has published anything since this process started. */
    private val _live = MutableStateFlow(false)
    val live: StateFlow<Boolean> = _live.asStateFlow()

    /**
     * What the box says is running. `session.list`, via `RelaydRouter`.
     *
     * Here rather than on the service for the same reason as everything else on
     * this object: `onBind` returns null, so a screen cannot hold a reference to
     * the thing that owns the link.
     */
    private val _sessions = MutableStateFlow<List<AgentSession>>(emptyList())
    val sessions: StateFlow<List<AgentSession>> = _sessions.asStateFlow()

    /**
     * Confirmations waiting on a person. `confirm.request`, via `RelaydRouter`.
     *
     * `ORCHESTRATOR.md` §5 job 3 — an agent that wants to run something
     * dangerous has to be able to ask, and a request that reaches the phone and
     * is never rendered is the same as one that never arrived.
     */
    private val _approvals = MutableStateFlow<List<ApprovalQueue.Request>>(emptyList())
    val approvals: StateFlow<List<ApprovalQueue.Request>> = _approvals.asStateFlow()

    internal fun publish(state: CaptureState) {
        _state.value = state
        _live.value = true
    }

    internal fun publishSessions(sessions: List<AgentSession>) {
        _sessions.value = sessions
    }

    internal fun publishApprovals(requests: List<ApprovalQueue.Request>) {
        _approvals.value = requests
    }

    /** Called when capture stops for good, so the UI stops claiming a live session. */
    internal fun clear() {
        _state.value = CaptureState.Idle
        _live.value = false
        _sessions.value = emptyList()
        _approvals.value = emptyList()
    }

    /**
     * True only when the service is actually reporting a recording session. A
     * fresh process reports false, which is correct: we do not know yet, and
     * claiming "recording" without evidence is the one error this surface must
     * never make.
     */
    val isCapturing: Boolean
        get() = _live.value && _state.value.recording
}
