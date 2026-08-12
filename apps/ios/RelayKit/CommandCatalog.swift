import Foundation

/// Every glasses command, by hand.
///
/// `docs/ORCHESTRATOR.md` §5 lists this as the mobile app's second job, and the
/// reasoning is worth keeping in front of us: *"Voice is the point, but a
/// product where the only input is speech fails in a quiet room, a loud room,
/// and on a bad day."*
///
/// So this is a catalog rather than a screen full of buttons. The UI is
/// generated from it, and `CommandCatalogTests` re-parses `Transport.swift` on
/// every run and fails if the protocol grows a method with no way to reach it —
/// the same trick `glasses/bridge/test/commands.test.ts` plays against
/// `glasses/protocol/commands.py`, and for the same reason: "every command is
/// tappable" is a claim that rots the moment it is only prose.

public enum CommandGroup: String, Sendable, Equatable, CaseIterable {
    case link
    case device
    case voice
    case camera
    case recording
    case transfer
    case video
}

/// What the action needs from the user before it can run.
public enum CommandArgument: String, Sendable, Equatable {
    case none
    /// A file from `listFiles`.
    case fileName
}

public struct GlassesAction: Sendable, Identifiable {
    /// Stable key. Tests and the UI both index on it, so renaming the title is
    /// free and renaming this is not.
    public let id: String
    public let title: String
    public let group: CommandGroup
    /// The method on ``GlassesTransport`` this reaches. The coverage test reads
    /// these, so it is load-bearing rather than documentation.
    public let transportMethod: String
    /// Protocol command ids from `glasses/protocol/commands.py`, for the log and
    /// for whoever is holding a packet capture.
    public let protocolIds: [String]
    /// Shown behind a confirmation. `deleteFile` is the obvious one; opening the
    /// access point is here too, because it costs the phone its uplink.
    public let destructive: Bool
    /// Opens a microphone. Gated on consent by ``CaptureCoordinator``, and the
    /// gate is structural — an action with this flag cannot be run without it.
    public let opensMicrophone: Bool
    public let argument: CommandArgument
    /// One line for the console, so a tap produces evidence rather than a
    /// spinner that stops.
    public let run: @Sendable (GlassesTransport, String?) async throws -> String

    public init(
        id: String,
        title: String,
        group: CommandGroup,
        transportMethod: String,
        protocolIds: [String],
        destructive: Bool = false,
        opensMicrophone: Bool = false,
        argument: CommandArgument = .none,
        run: @escaping @Sendable (GlassesTransport, String?) async throws -> String
    ) {
        self.id = id
        self.title = title
        self.group = group
        self.transportMethod = transportMethod
        self.protocolIds = protocolIds
        self.destructive = destructive
        self.opensMicrophone = opensMicrophone
        self.argument = argument
        self.run = run
    }
}

public enum GlassesCatalog {

    /// Methods on ``GlassesTransport`` that are deliberately not actions.
    ///
    /// One entry. `on` is a subscription, not a command: it takes a closure, has
    /// no user-visible effect, and every screen already uses it.
    public static let nonActionMethods: Set<String> = ["on"]

    public static let actions: [GlassesAction] = [

        GlassesAction(
            id: "connect",
            title: "Connect",
            group: .link,
            transportMethod: "connect",
            protocolIds: []
        ) { glasses, _ in
            try await glasses.connect()
            return "link: \(glasses.state.rawValue)"
        },

        GlassesAction(
            id: "disconnect",
            title: "Disconnect",
            group: .link,
            transportMethod: "disconnect",
            protocolIds: [],
            destructive: true
        ) { glasses, _ in
            await glasses.disconnect()
            return "link: \(glasses.state.rawValue)"
        },

        GlassesAction(
            id: "features",
            title: "Read capabilities",
            group: .device,
            transportMethod: "getFeatures",
            protocolIds: ["0x0005"]
        ) { glasses, _ in
            let features = try await glasses.getFeatures()
            var present: [String] = []
            if features.localRecording { present.append("recording") }
            if features.wifiAp { present.append("wifi-ap") }
            if features.wifiP2p { present.append("wifi-p2p") }
            if features.livePreview { present.append("preview") }
            if features.voiceWakeup { present.append("wake-word") }
            if features.wearDetection { present.append("wear") }
            if features.stabilization { present.append("stabiliser") }
            let unknown = features.unknownBits.isEmpty
                ? ""
                : " · unknown bits \(features.unknownBits.map(String.init).joined(separator: ","))"
            return present.joined(separator: " · ") + unknown
        },

        GlassesAction(
            id: "battery",
            title: "Battery",
            group: .device,
            transportMethod: "getBattery",
            protocolIds: ["0x0101"]
        ) { glasses, _ in
            let battery = try await glasses.getBattery()
            return "\(battery.percent)%\(battery.charging ? " · charging" : "")"
        },

        GlassesAction(
            id: "disk",
            title: "Storage",
            group: .device,
            transportMethod: "getDiskInfo",
            protocolIds: ["0x0909", "0x091C"]
        ) { glasses, _ in
            let disk = try await glasses.getDiskInfo()
            let assessment = StoragePolicy.assess(disk: disk)
            return assessment.message
        },

        GlassesAction(
            id: "setTime",
            title: "Sync the clock",
            group: .device,
            transportMethod: "setTime",
            protocolIds: ["0x0903"]
        ) { glasses, _ in
            try await glasses.setTime(Date())
            // Not cosmetic: every chunk carries `deviceTimeMs`, and the box
            // segments episodes by time. A device clock hours out of step files
            // this afternoon under yesterday.
            return "device clock aligned"
        },

        GlassesAction(
            id: "startVoice",
            title: "Start listening",
            group: .voice,
            transportMethod: "startVoiceSession",
            protocolIds: ["0x0805", "0x0A03"],
            opensMicrophone: true
        ) { glasses, _ in
            try await glasses.startVoiceSession()
            return "microphone open (Path A)"
        },

        GlassesAction(
            id: "stopVoice",
            title: "Stop listening",
            group: .voice,
            transportMethod: "stopVoiceSession",
            protocolIds: ["0x0805"]
        ) { glasses, _ in
            try await glasses.stopVoiceSession()
            return "microphone closed"
        },

        GlassesAction(
            id: "capturePhoto",
            title: "Photo to the glasses",
            group: .camera,
            transportMethod: "capturePhoto",
            protocolIds: ["0x0906"]
        ) { glasses, _ in
            let file = try await glasses.capturePhoto()
            return "\(file.name) · \(file.sizeBytes / 1024) KB, stays on the glasses"
        },

        GlassesAction(
            id: "thumbnail",
            title: "Preview a photo",
            group: .camera,
            transportMethod: "fetchThumbnail",
            protocolIds: ["0x0C01", "0x0C02", "0x0C03"],
            argument: .fileName
        ) { glasses, name in
            guard let name else { throw GlassesError(.unsupported, "pick a file first") }
            let photo = try await glasses.fetchThumbnail(name: name)
            return "\(photo.data.count / 1024) KB thumbnail"
        },

        GlassesAction(
            id: "takePhoto",
            title: "Photo to the phone",
            group: .camera,
            transportMethod: "takePhoto",
            protocolIds: ["0x0906", "0x0907"]
        ) { glasses, _ in
            // Tens of seconds over BLE. The catalog row says so, and the mock
            // takes that long, so nobody discovers it on hardware.
            let photo = try await glasses.takePhoto()
            return "\(photo.data.count / 1024) KB over BLE"
        },

        GlassesAction(
            id: "startRecording",
            title: "Start recording",
            group: .recording,
            transportMethod: "startLocalRecording",
            protocolIds: ["0x0E04"],
            opensMicrophone: true
        ) { glasses, _ in
            try await glasses.startLocalRecording()
            return "recording to the glasses (Path B)"
        },

        GlassesAction(
            id: "stopRecording",
            title: "Stop recording",
            group: .recording,
            transportMethod: "stopLocalRecording",
            protocolIds: ["0x0E04", "0x0E05"]
        ) { glasses, _ in
            try await glasses.stopLocalRecording()
            return "recording stopped"
        },

        GlassesAction(
            id: "listFiles",
            title: "What is on the glasses",
            group: .recording,
            transportMethod: "listFiles",
            protocolIds: ["0x0E01"]
        ) { glasses, _ in
            let files = try await glasses.listFiles()
            let unsynced = files.filter { !$0.uploaded }.count
            return "\(files.count) files, \(unsynced) not yet synced"
        },

        GlassesAction(
            id: "deleteFile",
            title: "Delete a file",
            group: .recording,
            transportMethod: "deleteFile",
            protocolIds: ["0x0E02", "0x0911"],
            destructive: true,
            argument: .fileName
        ) { glasses, name in
            guard let name else { throw GlassesError(.unsupported, "pick a file first") }
            // The device's own `uploaded` flag is the gate, not ours. See
            // `StoragePolicy.safeToDelete`.
            let files = try await glasses.listFiles()
            guard let file = files.first(where: { $0.name == name }) else {
                throw GlassesError(.transferFailed, "no such file")
            }
            guard file.uploaded else {
                throw GlassesError(
                    .unsupported,
                    "\(name) has not been synced yet — this is the only copy"
                )
            }
            try await glasses.deleteFile(name: name)
            return "deleted \(name)"
        },

        GlassesAction(
            id: "openAp",
            title: "Open the glasses' WiFi",
            group: .transfer,
            transportMethod: "openWifiAccessPoint",
            protocolIds: ["0x090B"],
            destructive: true
        ) { glasses, _ in
            let accessPoint = try await glasses.openWifiAccessPoint()
            return "\(accessPoint.ssid) at \(accessPoint.host) — your phone loses its uplink "
                + "while it is joined"
        },

        GlassesAction(
            id: "closeAp",
            title: "Close the glasses' WiFi",
            group: .transfer,
            transportMethod: "closeWifiAccessPoint",
            protocolIds: ["0x090B"]
        ) { glasses, _ in
            try await glasses.closeWifiAccessPoint()
            return "access point closed"
        },

        GlassesAction(
            id: "fetchFile",
            title: "Pull a recording",
            group: .transfer,
            transportMethod: "fetchFile",
            protocolIds: ["0x0C01", "0x0C05"],
            argument: .fileName
        ) { glasses, name in
            guard let name else { throw GlassesError(.unsupported, "pick a file first") }
            let body = try await glasses.fetchFile(name: name)
            return "pulled \(body.count / 1024) KB"
        },

        GlassesAction(
            id: "startPreview",
            title: "Live video",
            group: .video,
            transportMethod: "startPreview",
            protocolIds: ["0x090A", "0x0908"],
            destructive: true
        ) { glasses, _ in
            let url = try await glasses.startPreview()
            return url
        },

        GlassesAction(
            id: "stopPreview",
            title: "Stop live video",
            group: .video,
            transportMethod: "stopPreview",
            protocolIds: ["0x090A"]
        ) { glasses, _ in
            try await glasses.stopPreview()
            return "preview stopped"
        },
    ]

    public static func action(id: String) -> GlassesAction? {
        actions.first { $0.id == id }
    }

    public static func actions(in group: CommandGroup) -> [GlassesAction] {
        actions.filter { $0.group == group }
    }

    /// Every transport method the catalog can reach.
    public static var coveredMethods: Set<String> {
        Set(actions.map(\.transportMethod))
    }
}
