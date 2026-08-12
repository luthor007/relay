import RelayKit
import SwiftUI

// MARK: - every glasses command, by hand

/// `docs/ORCHESTRATOR.md` §5, job two. The screen is *generated* from
/// ``GlassesCatalog`` rather than hand-written, so a command added to the
/// transport shows up here without anyone remembering to add a button — and
/// `CommandCatalogTests` fails the build if one is ever missing.
struct CommandsView: View {
    @EnvironmentObject private var model: CaptureModel
    @State private var selectedFile: String?
    @State private var confirming: GlassesAction?

    var body: some View {
        Screen {
            Text("Commands")
                .font(.system(size: 32, weight: .medium))
                .foregroundStyle(Palette.ink)

            Text("Everything the voice loop can do, by hand. A product whose only "
                + "input is speech fails in a quiet room.")
                .font(.system(size: 14))
                .foregroundStyle(Palette.inkMid)

            if !model.files.isEmpty {
                Card {
                    Text("File for the commands that need one")
                        .font(.system(size: 13))
                        .foregroundStyle(Palette.inkDim)
                    Picker("File", selection: $selectedFile) {
                        Text("—").tag(String?.none)
                        ForEach(model.files, id: \.name) { file in
                            Text(file.uploaded ? file.name : "\(file.name) · not synced")
                                .tag(String?.some(file.name))
                        }
                    }
                    .pickerStyle(.menu)
                    .tint(Palette.ink)
                }
            }

            ForEach(CommandGroup.allCases, id: \.rawValue) { group in
                let actions = GlassesCatalog.actions(in: group)
                if !actions.isEmpty {
                    Text(group.rawValue.uppercased())
                        .font(.system(size: 11, weight: .medium))
                        .foregroundStyle(Palette.inkDim)
                    Card {
                        ForEach(actions) { action in
                            CommandRow(action: action) {
                                if action.destructive {
                                    confirming = action
                                } else {
                                    Task { await model.run(action, argument: selectedFile) }
                                }
                            }
                        }
                    }
                }
            }

            if !model.consoleLines.isEmpty {
                Text("CONSOLE")
                    .font(.system(size: 11, weight: .medium))
                    .foregroundStyle(Palette.inkDim)
                Card {
                    // Newest first: the answer to "what did that do" is always
                    // the last line, and scrolling to find it is a tax.
                    ForEach(Array(model.consoleLines.reversed().enumerated()), id: \.offset) { entry in
                        Text(entry.element)
                            .font(.system(size: 12, design: .monospaced))
                            .foregroundStyle(Palette.inkMid)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                }
            }
        }
        .task { await model.refreshFiles() }
        .confirmationDialog(
            confirming?.title ?? "",
            isPresented: Binding(
                get: { confirming != nil },
                set: { if !$0 { confirming = nil } }
            ),
            titleVisibility: .visible
        ) {
            if let action = confirming {
                Button(action.title, role: .destructive) {
                    let argument = selectedFile
                    confirming = nil
                    Task { await model.run(action, argument: argument) }
                }
            }
            Button("Cancel", role: .cancel) { confirming = nil }
        }
    }
}

private struct CommandRow: View {
    let action: GlassesAction
    let onTap: () -> Void

    var body: some View {
        Button(action: onTap) {
            HStack(alignment: .firstTextBaseline) {
                VStack(alignment: .leading, spacing: 2) {
                    Text(action.title)
                        .font(.system(size: 15))
                        .foregroundStyle(action.destructive ? Palette.live : Palette.ink)
                    if !action.protocolIds.isEmpty {
                        // The command id, visible. Whoever is holding a packet
                        // capture should not have to guess which row produced
                        // which frame.
                        Text(action.protocolIds.joined(separator: " · "))
                            .font(.system(size: 11, design: .monospaced))
                            .foregroundStyle(Palette.inkDim)
                    }
                }
                Spacer()
                if action.opensMicrophone {
                    Image(systemName: "mic.fill")
                        .font(.system(size: 11))
                        .foregroundStyle(Palette.live)
                }
            }
            .padding(.vertical, 6)
        }
    }
}

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
