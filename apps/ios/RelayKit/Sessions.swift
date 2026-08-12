import Foundation

/// Sessions and approvals — the two parts of the product surface that are
/// *inbound* from the box.
///
/// `docs/APPS-SCOPE.md` §4.4: "Session control: list and attach to running agent
/// sessions" and "Approvals: an agent that wants to run something dangerous has
/// to be able to ask". Both ride the link from `docs/SYSTEM.md` §6.1 and neither
/// adds a message type to it — server → phone `session.list` and
/// `confirm.request` come down, phone → server `session.command` goes back up
/// carrying the verb. Inventing `confirm.response` would have been the obvious
/// move and would have put the app out of contract with `relayd`.

// MARK: - sessions

public enum SessionRunState: String, Sendable, Equatable {
    case running
    /// Blocked on a `confirm.request` the user has not answered.
    case waiting
    case idle
    case finished
    case failed
}

public struct AgentSession: Sendable, Equatable, Identifiable {
    public let id: String
    public let title: String
    /// Which CLI is behind it. `docs/SYSTEM.md` §6.2 calls this the weakest seam
    /// in the system, so the app shows which one rather than pretending they are
    /// interchangeable.
    public let runtime: String
    public let state: SessionRunState
    public let startedAtMs: Int
    /// Last line of output, for the list row. Nil when there is none yet.
    public let lastLine: String?

    public init(
        id: String,
        title: String,
        runtime: String,
        state: SessionRunState,
        startedAtMs: Int,
        lastLine: String? = nil
    ) {
        self.id = id
        self.title = title
        self.runtime = runtime
        self.state = state
        self.startedAtMs = startedAtMs
        self.lastLine = lastLine
    }

    /// Decode leniently: an unknown `state` becomes `.idle` rather than dropping
    /// the session. A session the app cannot render is still a session the user
    /// may need to stop.
    public static func decode(_ value: JSONValue) -> AgentSession? {
        guard let id = value["id"]?.stringValue, !id.isEmpty else { return nil }
        return AgentSession(
            id: id,
            title: value["title"]?.stringValue ?? id,
            runtime: value["runtime"]?.stringValue ?? "unknown",
            state: SessionRunState(rawValue: value["state"]?.stringValue ?? "") ?? .idle,
            startedAtMs: value["startedAtMs"]?.intValue ?? 0,
            lastLine: value["lastLine"]?.stringValue
        )
    }

    public static func decodeList(_ envelope: RelaydEnvelope) -> [AgentSession] {
        guard let items = envelope.payload["sessions"]?.arrayValue else { return [] }
        return items.compactMap(AgentSession.decode)
    }
}

/// What the phone can ask a session to do. The string is the `verb` field inside
/// a `session.command` payload.
public enum SessionVerb: String, Sendable, Equatable, CaseIterable {
    /// Make this the session that voice input and spoken replies belong to.
    case attach
    case detach
    /// Send a line of text — the quiet-room path from `docs/ORCHESTRATOR.md` §5,
    /// where speech is not an option.
    case input
    case stop
    /// Answer a `confirm.request`.
    case confirm
}

/// Holds the session list and which one is attached.
///
/// Attachment is *local intent*: the phone says which session its voice belongs
/// to and the box agrees or corrects on the next `session.list`. Waiting for a
/// round trip before showing the change makes the tap feel broken on cellular.
public final class SessionDirectory: @unchecked Sendable {

    private let lock = NSLock()
    private var sessions: [AgentSession] = []
    private var attached: String?
    private var changeHandler: (@Sendable ([AgentSession], String?) -> Void)?

    public init(onChange: (@Sendable ([AgentSession], String?) -> Void)? = nil) {
        self.changeHandler = onChange
    }

    /// Set after construction, because the app layer's closure captures the
    /// object that owns this directory.
    public func setChangeHandler(_ handler: @escaping @Sendable ([AgentSession], String?) -> Void) {
        lock.lock(); changeHandler = handler; lock.unlock()
    }

    private var onChange: (@Sendable ([AgentSession], String?) -> Void)? {
        lock.lock(); defer { lock.unlock() }
        return changeHandler
    }

    public var all: [AgentSession] {
        lock.lock(); defer { lock.unlock() }
        return sessions
    }

    public var attachedId: String? {
        lock.lock(); defer { lock.unlock() }
        return attached
    }

    public var attachedSession: AgentSession? {
        lock.lock(); defer { lock.unlock() }
        guard let attached else { return nil }
        return sessions.first { $0.id == attached }
    }

    /// Apply a `session.list` envelope.
    public func apply(_ envelope: RelaydEnvelope) {
        let incoming = AgentSession.decodeList(envelope)
        let snapshot: ([AgentSession], String?) = {
            lock.lock(); defer { lock.unlock() }
            sessions = incoming
            // The box is authoritative about what exists. A session that has
            // gone away cannot stay attached, or the next utterance is spoken
            // into nothing.
            if let current = attached, !incoming.contains(where: { $0.id == current }) {
                attached = nil
            }
            return (sessions, attached)
        }()
        onChange?(snapshot.0, snapshot.1)
    }

    /// Attach locally and return the envelope payload to send.
    public func attach(_ id: String) -> JSONValue {
        let snapshot: ([AgentSession], String?) = {
            lock.lock(); defer { lock.unlock() }
            attached = id
            return (sessions, attached)
        }()
        onChange?(snapshot.0, snapshot.1)
        return Self.command(.attach, sessionId: id)
    }

    public func detach() -> JSONValue? {
        let previous: String? = {
            lock.lock(); defer { lock.unlock() }
            let previous = attached
            attached = nil
            return previous
        }()
        guard let previous else { return nil }
        onChange?(all, nil)
        return Self.command(.detach, sessionId: previous)
    }

    /// The `session.command` payload shape. One place, so the three verbs that
    /// carry extra fields cannot disagree about the field names.
    public static func command(
        _ verb: SessionVerb,
        sessionId: String,
        text: String? = nil,
        requestId: String? = nil,
        decision: String? = nil
    ) -> JSONValue {
        var fields: [String: JSONValue] = [
            "verb": .string(verb.rawValue),
            "sessionId": .string(sessionId),
        ]
        if let text { fields["text"] = .string(text) }
        if let requestId { fields["requestId"] = .string(requestId) }
        if let decision { fields["decision"] = .string(decision) }
        return .object(fields)
    }
}

// MARK: - approvals

public enum ApprovalRisk: String, Sendable, Equatable {
    case low
    case medium
    case high
}

public struct ApprovalRequest: Sendable, Equatable, Identifiable {
    public let id: String
    public let sessionId: String
    /// One line. This is what the user actually reads.
    public let summary: String
    /// The exact thing that will run, verbatim. Never paraphrased: an approval
    /// screen that summarises a shell command is an approval for something else.
    public let detail: String?
    public let risk: ApprovalRisk
    /// Unanswered after this, the agent gives up. Nil means it waits.
    public let expiresAtMs: Int?

    public init(
        id: String,
        sessionId: String,
        summary: String,
        detail: String? = nil,
        risk: ApprovalRisk = .medium,
        expiresAtMs: Int? = nil
    ) {
        self.id = id
        self.sessionId = sessionId
        self.summary = summary
        self.detail = detail
        self.risk = risk
        self.expiresAtMs = expiresAtMs
    }

    public static func decode(_ envelope: RelaydEnvelope) -> ApprovalRequest? {
        let payload = envelope.payload
        guard
            let id = payload["requestId"]?.stringValue ?? payload["id"]?.stringValue,
            let sessionId = payload["sessionId"]?.stringValue,
            let summary = payload["summary"]?.stringValue
        else { return nil }
        return ApprovalRequest(
            id: id,
            sessionId: sessionId,
            summary: summary,
            detail: payload["detail"]?.stringValue,
            // An unrecognised risk is treated as `high`, not as the default.
            // Guessing low on something we do not understand is the wrong way
            // round.
            risk: ApprovalRisk(rawValue: payload["risk"]?.stringValue ?? "") ?? .high,
            expiresAtMs: payload["expiresAtMs"]?.intValue
        )
    }
}

public enum ApprovalDecision: String, Sendable, Equatable {
    case approve
    case deny
}

/// Pending approvals, oldest first.
///
/// Deliberately not auto-expiring on a timer: an approval that vanishes while
/// the user is reading it teaches them to tap fast, which is the opposite of
/// what an approval is for. ``expired(now:)`` reports them and the UI shows them
/// as lapsed.
public final class ApprovalInbox: @unchecked Sendable {

    private let lock = NSLock()
    private var requests: [ApprovalRequest] = []
    private var changeHandler: (@Sendable ([ApprovalRequest]) -> Void)?

    public init(onChange: (@Sendable ([ApprovalRequest]) -> Void)? = nil) {
        self.changeHandler = onChange
    }

    public func setChangeHandler(_ handler: @escaping @Sendable ([ApprovalRequest]) -> Void) {
        lock.lock(); changeHandler = handler; lock.unlock()
    }

    private var onChange: (@Sendable ([ApprovalRequest]) -> Void)? {
        lock.lock(); defer { lock.unlock() }
        return changeHandler
    }

    public var pending: [ApprovalRequest] {
        lock.lock(); defer { lock.unlock() }
        return requests
    }

    @discardableResult
    public func apply(_ envelope: RelaydEnvelope) -> ApprovalRequest? {
        guard let request = ApprovalRequest.decode(envelope) else { return nil }
        let snapshot: [ApprovalRequest] = {
            lock.lock(); defer { lock.unlock() }
            // Idempotent: relayd redelivers on reconnect, and the same request
            // arriving twice must not become two rows the user has to answer.
            if !requests.contains(where: { $0.id == request.id }) {
                requests.append(request)
            }
            return requests
        }()
        onChange?(snapshot)
        return request
    }

    public func expired(now: Int) -> [ApprovalRequest] {
        pending.filter { request in
            guard let expiry = request.expiresAtMs else { return false }
            return expiry <= now
        }
    }

    /// Take a question down because it is no longer true — `confirm.resolved`.
    ///
    /// The approval was answered in a terminal, or the turn was cancelled.
    /// Without this the ping outlives its question and wakes someone to approve
    /// what is already approved. Nil when the id is unknown, which is a
    /// resolution outrunning its request across a reconnect rather than an
    /// error, and is silent for the same reason ``answer(_:_:)`` is.
    @discardableResult
    public func retract(_ id: String) -> ApprovalRequest? {
        let found: ApprovalRequest? = {
            lock.lock(); defer { lock.unlock() }
            guard let index = requests.firstIndex(where: { $0.id == id }) else { return nil }
            return requests.remove(at: index)
        }()
        guard found != nil else { return nil }
        onChange?(pending)
        return found
    }

    /// Answer, and return the payload to send. Nil when the id is unknown, which
    /// is a double-tap or a stale screen rather than an error.
    public func answer(_ id: String, _ decision: ApprovalDecision) -> JSONValue? {
        let found: ApprovalRequest? = {
            lock.lock(); defer { lock.unlock() }
            guard let index = requests.firstIndex(where: { $0.id == id }) else { return nil }
            return requests.remove(at: index)
        }()
        guard let request = found else { return nil }
        onChange?(pending)
        return SessionDirectory.command(
            .confirm,
            sessionId: request.sessionId,
            requestId: request.id,
            decision: decision.rawValue
        )
    }
}
