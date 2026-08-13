import Foundation
import RelayKit
import SwiftUI

/// SwiftUI's view of ``CaptureCoordinator``, and nothing more.
///
/// Everything that decides anything moved into `RelayKit` — the consent gate,
/// wear gating, the two capture paths, the indicator wording, restoration. What
/// is left here is `@MainActor` republishing, which is the part that genuinely
/// belongs to the UI layer and the part that cannot be unit-tested without a
/// host application anyway.
///
/// If a rule ends up in this file, it is in the wrong file: it will have escaped
/// `RelayKitTests`, which is the only suite that runs without a phone.
@MainActor
final class CaptureModel: ObservableObject {

    @Published private(set) var status = CaptureStatus()
    @Published private(set) var sessions: [AgentSession] = []
    @Published private(set) var attachedSessionId: String?
    @Published private(set) var approvals: [ApprovalRequest] = []
    @Published private(set) var syncPhase: SyncPhase = .idle
    @Published private(set) var syncMessage: String?
    @Published private(set) var files: [RemoteFile] = []
    @Published private(set) var consoleLines: [String] = []

    let coordinator: CaptureCoordinator
    private let glasses: GlassesTransport
    private let link: RelaydLink?
    private let directory: SessionDirectory
    private let inbox: ApprovalInbox
    private let queue: StoreAndForwardQueue
    private let network: SyncNetwork
    private let notifier = NotificationPresenter()

    init(
        glasses: GlassesTransport,
        audio: AudioSessionControlling,
        snapshots: SnapshotStore,
        queue: StoreAndForwardQueue,
        network: SyncNetwork,
        link: RelaydLink? = nil
    ) {
        self.glasses = glasses
        self.link = link
        self.queue = queue
        self.network = network

        let directory = SessionDirectory()
        let inbox = ApprovalInbox()
        self.directory = directory
        self.inbox = inbox

        self.coordinator = CaptureCoordinator(
            glasses: glasses,
            audio: audio,
            snapshots: snapshots,
            link: link
        )

        // The coordinator hands out status off the transport's thread; hopping
        // to the main actor is this layer's whole job.
        self.coordinator.setStatusHandler { [weak self] status in
            Task { @MainActor [weak self] in self?.status = status }
        }
        directory.setChangeHandler { [weak self] sessions, attached in
            Task { @MainActor [weak self] in
                self?.sessions = sessions
                self?.attachedSessionId = attached
            }
        }
        inbox.setChangeHandler { [weak self] pending in
            Task { @MainActor [weak self] in self?.approvals = pending }
        }

        // Route inbound server messages to whichever holder understands them.
        // None of these captures `self`, so there is no cycle through the link.
        let notifier = self.notifier
        link?.setObserver(LinkObserver(
            onMessage: { envelope in
                switch ServerMessage(rawValue: envelope.type) {
                case .sessionList: directory.apply(envelope)
                case .confirmRequest: inbox.apply(envelope)
                default: break
                }
            },
            // A notification that arrives without speech — `ADAPTERS.md` §7.
            // Under quiet hours the box holds the speaking and still sends this.
            onNotify: { frame in notifier.present(frame) },
            // The question is gone: answered in a terminal, or the turn was
            // cancelled. Take the row down rather than leaving a ping that
            // wakes someone to approve what is already approved.
            onConfirmResolved: { frame in inbox.retract(frame.actionId) }
        ))

        status = coordinator.status
    }

    // MARK: - consent

    var consentGiven: Bool { status.consent.allowsCapture }

    func grantConsent() { coordinator.grantConsent() }

    func withdrawConsent() async { await coordinator.withdrawConsent() }

    // MARK: - capture

    /// Starting capture is the consent.
    ///
    /// The gate is still the coordinator's — it refuses to record without a
    /// granted consent, and that refusal is the guarantee. What this does is
    /// answer it at the moment the person acts, rather than on a wall shown
    /// before the app. Pressing a button that says it starts recording, next to
    /// a line saying what recording does, is a clearer act of consent than
    /// agreeing to three paragraphs to get past a screen.
    ///
    /// Withdrawing is unchanged and still one tap, at the bottom of this screen.
    func startCapture() async {
        if !consentGiven { coordinator.grantConsent() }
        do { try await coordinator.startCapture() } catch { log(error) }
        await refreshFiles()
    }

    func stopCapture() async { await coordinator.stopCapture() }

    func beginVoiceTurn() async {
        do { try await coordinator.beginVoiceTurn(.tap) } catch { log(error) }
    }

    func endVoiceTurn() async { _ = await coordinator.endVoiceTurn() }

    // MARK: - the command console

    func run(_ action: GlassesAction, argument: String? = nil) async {
        do {
            let summary = try await coordinator.run(action, argument: argument)
            append("\(action.title): \(summary)")
        } catch {
            append("\(action.title): \(readable(error))")
        }
        if action.transportMethod.contains("File") || action.transportMethod.contains("Recording") {
            await refreshFiles()
        }
    }

    func refreshFiles() async {
        files = (try? await glasses.listFiles()) ?? []
    }

    // MARK: - sessions and approvals

    func attach(_ session: AgentSession) {
        link?.send(.sessionCommand, directory.attach(session.id))
    }

    func detach() {
        guard let payload = directory.detach() else { return }
        link?.send(.sessionCommand, payload)
    }

    func answer(_ request: ApprovalRequest, _ decision: ApprovalDecision) {
        guard let payload = inbox.answer(request.id, decision) else { return }
        link?.send(.sessionCommand, payload)
    }

    // MARK: - sync

    /// The AP phase needs a system dialog, so it is only ever offered from the
    /// foreground. `BackgroundSync` runs the upload half from a
    /// `BGProcessingTask`; see `AppDelegate`.
    func runSync() async {
        let sync = BulkSync(
            glasses: glasses,
            queue: queue,
            network: network,
            upload: { _ in
                // The uploader is wired once the box is paired; until then a
                // sync run pulls the day onto the phone and holds it, which is
                // the correct half-built behaviour.
                throw GlassesError(.unsupported, "not paired with a box yet")
            },
            canPresentSystemDialogs: { true },
            observer: SyncObserver(
                onPhaseChanged: { [weak self] phase in
                    Task { @MainActor [weak self] in self?.syncPhase = phase }
                },
                onDeferred: { [weak self] _, message in
                    Task { @MainActor [weak self] in self?.syncMessage = message }
                }
            )
        )
        coordinator.setHoldingAccessPoint(false)
        let result = await sync.run()
        syncMessage = result.errorDescription ?? "Pulled \(result.filesPulled) recordings."
        await refreshFiles()
    }

    // MARK: - internals

    private func log(_ error: Error) {
        append(readable(error))
    }

    private func append(_ line: String) {
        consoleLines.append(line)
        if consoleLines.count > 200 { consoleLines.removeFirst(consoleLines.count - 200) }
    }

    private func readable(_ error: Error) -> String {
        if let error = error as? GlassesError { return error.message }
        if let error = error as? CaptureError {
            switch error {
            case .consentRequired: return "Turn capture on first — this needs your consent."
            case .notConnected: return "Not connected to your glasses."
            case .alreadyListening: return "Already listening."
            }
        }
        return String(describing: error)
    }
}
