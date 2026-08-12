import RelayKit
import SwiftUI

// Lifted from site/index.html so the app and the landing page are one product.
enum Palette {
    static let ground = Color(red: 0x0A / 255, green: 0x0B / 255, blue: 0x0D / 255)
    static let surface = Color(red: 0x13 / 255, green: 0x15 / 255, blue: 0x19 / 255)
    static let line = Color(red: 0x23 / 255, green: 0x26 / 255, blue: 0x2C / 255)
    static let ink = Color(red: 0xEF / 255, green: 0xED / 255, blue: 0xE8 / 255)
    static let inkMid = Color(red: 0xA9 / 255, green: 0xAD / 255, blue: 0xB4 / 255)
    static let inkDim = Color(red: 0x79 / 255, green: 0x7E / 255, blue: 0x86 / 255)
    /// Recording amber. Used for exactly one thing — indicating a live mic.
    static let live = Color(red: 0xE9 / 255, green: 0xA2 / 255, blue: 0x3B / 255)
}

// MARK: - consent

struct ConsentView: View {
    let onAccept: () -> Void

    var body: some View {
        Screen {
            Text("Before you start")
                .font(.system(size: 32, weight: .medium))
                .foregroundStyle(Palette.ink)

            Text("""
            Relay records audio from your glasses while you wear them. The glasses show a light \
            when they are recording, and you cannot turn that light off from this app.

            Recording keeps running on the glasses even when your phone is out of range — the day \
            is stored on the glasses and syncs later.

            In Québec and many other places, recording a conversation you are not part of is \
            illegal. Recording one you are part of is not. That distinction is yours to keep.
            """)
            .font(.system(size: 16))
            .foregroundStyle(Palette.inkMid)

            PrimaryButton("I understand — enable capture", action: onAccept)
        }
    }
}

// MARK: - permissions

struct PermissionsView: View {
    @ObservedObject var model: PermissionsModel

    var body: some View {
        Screen {
            Text("Permissions Relay needs")
                .font(.system(size: 32, weight: .medium))
                .foregroundStyle(Palette.ink)

            Text("Each one maps to something you can see in the app.")
                .font(.system(size: 16))
                .foregroundStyle(Palette.inkMid)

            VStack(spacing: 12) {
                PermissionRow(
                    title: "Microphone",
                    detail: "Recording audio from the glasses",
                    granted: model.microphone
                )
                PermissionRow(
                    title: "Bluetooth",
                    detail: "Finding and holding the link to the glasses",
                    granted: model.bluetooth
                )
                PermissionRow(
                    title: "Notifications",
                    detail: "The always-visible recording indicator",
                    granted: model.notifications
                )
            }

            PrimaryButton("Grant") {
                Task { await model.requestAll() }
            }
        }
    }
}

private struct PermissionRow: View {
    let title: String
    let detail: String
    let granted: Bool

    var body: some View {
        Card {
            HStack(alignment: .top) {
                VStack(alignment: .leading, spacing: 4) {
                    Text(title)
                        .font(.system(size: 15, weight: .medium))
                        .foregroundStyle(Palette.ink)
                    Text(detail)
                        .font(.system(size: 13))
                        .foregroundStyle(Palette.inkDim)
                }
                Spacer()
                Image(systemName: granted ? "checkmark.circle.fill" : "circle")
                    .foregroundStyle(granted ? Palette.live : Palette.line)
            }
        }
    }
}

// MARK: - home

struct HomeView: View {
    @EnvironmentObject private var model: CaptureModel
    let onWithdrawConsent: () async -> Void

    var body: some View {
        Screen {
            HStack(spacing: 12) {
                LiveDot(active: model.status.indicator.active)
                VStack(alignment: .leading, spacing: 2) {
                    Text(model.status.indicator.headline)
                        .font(.system(size: 22, weight: .medium))
                        .foregroundStyle(Palette.ink)
                    if let detail = model.status.indicator.detail {
                        Text(detail)
                            .font(.system(size: 13))
                            .foregroundStyle(Palette.inkDim)
                    }
                }
                Spacer()
            }

            if model.status.captureEnabled {
                SecondaryButton("Stop capture") { await model.stopCapture() }
            } else {
                PrimaryButton("Start capture") { Task { await model.startCapture() } }
            }

            if model.status.captureEnabled {
                SecondaryButton(model.status.voiceTurnOpen ? "Stop listening" : "Ask something") {
                    if model.status.voiceTurnOpen {
                        await model.endVoiceTurn()
                    } else {
                        await model.beginVoiceTurn()
                    }
                }
            }

            if let transcript = model.status.lastTranscript {
                Card {
                    Text("“\(transcript)”")
                        .font(.system(size: 15))
                        .foregroundStyle(Palette.ink)
                }
            }

            Card {
                Fact("Worn", model.status.worn ? "Yes" : "No")
                Fact("Battery", model.status.battery.map { "\($0.percent)%\($0.charging ? " · charging" : "")" } ?? "—")
                Fact("Storage free", model.status.disk.map(Self.gigabytes) ?? "—")
                Fact("Link", model.status.connection.rawValue.capitalized)
            }

            if let storage = model.status.storage, storage.level != .ok {
                Card {
                    Text(storage.message)
                        .font(.system(size: 13))
                        .foregroundStyle(Palette.live)
                }
            }

            if let error = model.status.lastError {
                Card {
                    Text(error)
                        .font(.system(size: 13))
                        .foregroundStyle(Palette.live)
                }
            }

            Button {
                Task { await onWithdrawConsent() }
            } label: {
                Text("Turn off capture and withdraw consent")
                    .font(.system(size: 14))
                    .foregroundStyle(Palette.inkDim)
                    .frame(maxWidth: .infinity)
            }
        }
    }

    private static func gigabytes(_ info: DiskInfo) -> String {
        String(format: "%.1f GB", Double(info.freeBytes) / 1_073_741_824)
    }
}

/// A slow pulse, not a blink. Blinking reads as an error state; this has to read
/// as "alive".
private struct LiveDot: View {
    let active: Bool
    @State private var dim = false

    var body: some View {
        Circle()
            .fill(active ? Palette.live : Palette.line)
            .frame(width: 14, height: 14)
            .opacity(active && dim ? 0.35 : 1)
            .animation(
                active
                    ? .easeInOut(duration: 1.4).repeatForever(autoreverses: true)
                    : .default,
                value: dim
            )
            .onAppear { dim = true }
    }
}

// MARK: - components

private struct Fact: View {
    let label: String
    let value: String

    init(_ label: String, _ value: String) {
        self.label = label
        self.value = value
    }

    var body: some View {
        HStack {
            Text(label)
                .font(.system(size: 14))
                .foregroundStyle(Palette.inkDim)
            Spacer()
            Text(value)
                .font(.system(size: 14))
                .foregroundStyle(Palette.ink)
        }
    }
}

struct Screen<Content: View>: View {
    @ViewBuilder let content: Content

    var body: some View {
        ZStack {
            Palette.ground.ignoresSafeArea()
            ScrollView {
                VStack(alignment: .leading, spacing: 20) {
                    content
                }
                .padding(.horizontal, 24)
                .padding(.vertical, 48)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }
}

struct Card<Content: View>: View {
    @ViewBuilder let content: Content

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            content
        }
        .padding(16)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Palette.surface)
        .clipShape(RoundedRectangle(cornerRadius: 12))
        .overlay(
            RoundedRectangle(cornerRadius: 12).stroke(Palette.line, lineWidth: 1)
        )
    }
}

struct PrimaryButton: View {
    let title: String
    let action: () -> Void

    init(_ title: String, action: @escaping () -> Void) {
        self.title = title
        self.action = action
    }

    var body: some View {
        Button(action: action) {
            Text(title)
                .font(.system(size: 15, weight: .medium))
                .foregroundStyle(Palette.ground)
                .frame(maxWidth: .infinity)
                .padding(.vertical, 14)
                .background(Palette.ink)
                .clipShape(RoundedRectangle(cornerRadius: 10))
        }
    }
}

struct SecondaryButton: View {
    let title: String
    let action: () async -> Void

    init(_ title: String, action: @escaping () async -> Void) {
        self.title = title
        self.action = action
    }

    var body: some View {
        Button {
            Task { await action() }
        } label: {
            Text(title)
                .font(.system(size: 15, weight: .medium))
                .foregroundStyle(Palette.ink)
                .frame(maxWidth: .infinity)
                .padding(.vertical, 14)
                .overlay(
                    RoundedRectangle(cornerRadius: 10).stroke(Palette.line, lineWidth: 1)
                )
        }
    }
}
