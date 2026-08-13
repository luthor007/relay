import RelayKit
import SwiftUI

// The Commands screen was here: a generated list of all 20 GlassesCatalog
// actions, one row each, as a tab of its own. Removed — it was an exhaustive
// control panel in a product whose first screen should say what to do.
//
// GlassesCatalog and CommandCatalogTests stay in RelayKit, untouched. The
// catalog is the load-bearing half: it is data, it names the transport method
// and protocol ids behind each action, and the test still fails the build if a
// transport method ever lacks one. Regenerating a screen from it is an
// afternoon; re-deriving the catalog would not be.
//
// ORCHESTRATOR.md 5 job two — "everything the voice loop can do should be
// tappable" — is now only partly met: start and stop capture, and the voice
// turn, are on the main screen. The other seventeen actions are not reachable
// by hand. That gap is recorded there rather than papered over here.
// MARK: - sessions and approvals

struct SessionsView: View {
    @EnvironmentObject private var model: CaptureModel

    var body: some View {
        Screen {
            Text("Sessions")
                .font(.system(size: 32, weight: .medium))
                .foregroundStyle(Palette.ink)

            if !model.approvals.isEmpty {
                Text("WAITING ON YOU")
                    .font(.system(size: 11, weight: .medium))
                    .foregroundStyle(Palette.live)
                ForEach(model.approvals) { request in
                    ApprovalCard(request: request) { decision in
                        model.answer(request, decision)
                    }
                }
            }

            if model.sessions.isEmpty {
                Card {
                    Text("Nothing running. Sessions appear here when an agent starts one.")
                        .font(.system(size: 14))
                        .foregroundStyle(Palette.inkMid)
                }
            }

            ForEach(model.sessions) { session in
                Card {
                    HStack(alignment: .top) {
                        VStack(alignment: .leading, spacing: 4) {
                            Text(session.title)
                                .font(.system(size: 15, weight: .medium))
                                .foregroundStyle(Palette.ink)
                            Text("\(session.runtime) · \(session.state.rawValue)")
                                .font(.system(size: 12))
                                .foregroundStyle(Palette.inkDim)
                            if let line = session.lastLine {
                                Text(line)
                                    .font(.system(size: 12, design: .monospaced))
                                    .foregroundStyle(Palette.inkMid)
                                    .lineLimit(2)
                            }
                        }
                        Spacer()
                        if model.attachedSessionId == session.id {
                            Button("Detach") { model.detach() }
                                .font(.system(size: 13))
                                .foregroundStyle(Palette.live)
                        } else {
                            Button("Attach") { model.attach(session) }
                                .font(.system(size: 13))
                                .foregroundStyle(Palette.ink)
                        }
                    }
                }
            }
        }
    }
}

private struct ApprovalCard: View {
    let request: ApprovalRequest
    let onDecision: (ApprovalDecision) -> Void

    var body: some View {
        Card {
            Text(request.summary)
                .font(.system(size: 15, weight: .medium))
                .foregroundStyle(Palette.ink)
            if let detail = request.detail {
                // Verbatim, never summarised: an approval screen that
                // paraphrases a command is an approval for something else.
                Text(detail)
                    .font(.system(size: 12, design: .monospaced))
                    .foregroundStyle(request.risk == .high ? Palette.live : Palette.inkMid)
                    .textSelection(.enabled)
            }
            HStack(spacing: 12) {
                Button("Deny") { onDecision(.deny) }
                    .font(.system(size: 14, weight: .medium))
                    .foregroundStyle(Palette.ink)
                Button("Allow") { onDecision(.approve) }
                    .font(.system(size: 14, weight: .medium))
                    .foregroundStyle(Palette.live)
            }
            .padding(.top, 4)
        }
    }
}

// MARK: - the nightly ritual

struct SyncView: View {
    @EnvironmentObject private var model: CaptureModel

    var body: some View {
        Screen {
            Text("Sync")
                .font(.system(size: 32, weight: .medium))
                .foregroundStyle(Palette.ink)

            Text("A day of audio is about 173 MB. Over Bluetooth that is sixteen hours — "
                + "longer than the day took to record. So sync joins your glasses' own "
                + "WiFi, and your phone has no internet while it does.")
                .font(.system(size: 14))
                .foregroundStyle(Palette.inkMid)

            Card {
                HStack {
                    Text("Phase")
                        .font(.system(size: 14))
                        .foregroundStyle(Palette.inkDim)
                    Spacer()
                    Text(Self.phaseLabel(model.syncPhase))
                        .font(.system(size: 14))
                        .foregroundStyle(Palette.ink)
                }
                if let message = model.syncMessage {
                    Text(message)
                        .font(.system(size: 13))
                        .foregroundStyle(Palette.inkMid)
                }
            }

            PrimaryButton("Sync now") { Task { await model.runSync() } }

            if !model.files.isEmpty {
                Text("ON THE GLASSES")
                    .font(.system(size: 11, weight: .medium))
                    .foregroundStyle(Palette.inkDim)
                Card {
                    ForEach(model.files, id: \.name) { file in
                        HStack {
                            Text(file.name)
                                .font(.system(size: 13, design: .monospaced))
                                .foregroundStyle(Palette.ink)
                            Spacer()
                            Text(file.uploaded ? "synced" : "only here")
                                .font(.system(size: 12))
                                .foregroundStyle(file.uploaded ? Palette.inkDim : Palette.live)
                        }
                    }
                }
            }
        }
        .task { await model.refreshFiles() }
    }

    /// Named for what is happening to the user, not for the state machine. The
    /// states where nothing transfers are the ones that need a sentence.
    private static func phaseLabel(_ phase: SyncPhase) -> String {
        switch phase {
        case .idle: return "Idle"
        case .waiting: return "Waiting"
        case .openingAccessPoint: return "Opening the glasses' WiFi"
        case .joiningAccessPoint: return "Joining — your phone is offline"
        case .pullingFiles: return "Pulling the day"
        case .leavingAccessPoint: return "Leaving the glasses' WiFi"
        case .rejoiningUplink: return "Back on your network"
        case .uploading: return "Uploading to your box"
        case .done: return "Done"
        case .failed: return "Failed"
        }
    }
}
