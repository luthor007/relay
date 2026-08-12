import Foundation

/// Capture state that has to survive the app being killed.
///
/// `docs/APPS-SCOPE.md` §4.1: *extend the existing state-restoration path to
/// restore capture state, not just the BLE connection.* The vendor already does
/// the hard half — `QCCentralManager.m:63` sets
/// `CBCentralManagerOptionRestoreIdentifierKey` and line 376 implements
/// `centralManager:willRestoreState:`, which is what makes iOS relaunch a
/// terminated app into the background when the glasses reconnect.
///
/// What that mechanism restores is a `CBCentralManager` and its peripherals. It
/// knows nothing about consent, about whether the user had capture switched on,
/// or about a recording that was running when we died. Those are ours, and
/// without them a background relaunch produces an app that is connected to the
/// glasses and does nothing — which looks exactly like working.
///
/// ## Why this is a snapshot and not `NSUserActivity`
///
/// UIKit's own state restoration runs for a *user-visible* relaunch. A CoreBluetooth
/// relaunch has no window, no scene worth restoring, and may be over in a few
/// hundred milliseconds. So this is a small file written on every state change and
/// read before anything else happens.

// MARK: - consent

/// Consent is not a boolean, because "never asked" and "said no" have to lead to
/// different screens — and because a withdrawal has to survive a relaunch that
/// happens to be triggered by the glasses reconnecting.
public enum ConsentState: Sendable, Equatable {
    case notAsked
    case granted(version: Int, atMs: Int)
    case withdrawn(atMs: Int)

    public var allowsCapture: Bool {
        if case .granted = self { return true }
        return false
    }
}

/// Bump when the consent copy changes materially. A grant against an older
/// version is not a grant against the new text, and `ConsentState.granted`
/// carries the version so the app can ask again rather than assume.
public let currentConsentVersion = 1

// MARK: - the snapshot

public struct CaptureSnapshot: Codable, Sendable, Equatable {

    /// Bump on any incompatible change. An unreadable snapshot is discarded
    /// rather than migrated: everything in it is re-derivable from the glasses,
    /// and a half-migrated capture state is worse than a cold start.
    public static let currentSchemaVersion = 2

    public var schemaVersion: Int
    public var consentVersion: Int?
    public var consentGrantedAtMs: Int?
    public var consentWithdrawnAtMs: Int?
    /// The user's intent, not the link's state.
    public var captureEnabled: Bool
    /// Path B was running when we were killed. Believing this without asking the
    /// device is the mistake — see ``RestorationPlan``.
    public var localRecordingBelievedRunning: Bool
    public var recordingStartedAtMs: Int?
    /// CoreBluetooth peripheral identifier, so a relaunch reconnects to *these*
    /// glasses rather than scanning.
    public var deviceId: String?
    public var wornAtLastSample: Bool
    /// We were holding the glasses' access point. If this is true at launch the
    /// last run died mid-sync and the phone may still be on a network with no
    /// uplink.
    public var holdingAccessPoint: Bool
    public var pendingQueueRecords: Int
    public var savedAtMs: Int

    public init(
        schemaVersion: Int = CaptureSnapshot.currentSchemaVersion,
        consentVersion: Int? = nil,
        consentGrantedAtMs: Int? = nil,
        consentWithdrawnAtMs: Int? = nil,
        captureEnabled: Bool = false,
        localRecordingBelievedRunning: Bool = false,
        recordingStartedAtMs: Int? = nil,
        deviceId: String? = nil,
        wornAtLastSample: Bool = false,
        holdingAccessPoint: Bool = false,
        pendingQueueRecords: Int = 0,
        savedAtMs: Int = 0
    ) {
        self.schemaVersion = schemaVersion
        self.consentVersion = consentVersion
        self.consentGrantedAtMs = consentGrantedAtMs
        self.consentWithdrawnAtMs = consentWithdrawnAtMs
        self.captureEnabled = captureEnabled
        self.localRecordingBelievedRunning = localRecordingBelievedRunning
        self.recordingStartedAtMs = recordingStartedAtMs
        self.deviceId = deviceId
        self.wornAtLastSample = wornAtLastSample
        self.holdingAccessPoint = holdingAccessPoint
        self.pendingQueueRecords = pendingQueueRecords
        self.savedAtMs = savedAtMs
    }

    public var consent: ConsentState {
        if let withdrawn = consentWithdrawnAtMs {
            return .withdrawn(atMs: withdrawn)
        }
        if let version = consentVersion, let at = consentGrantedAtMs {
            return .granted(version: version, atMs: at)
        }
        return .notAsked
    }

    public mutating func setConsent(_ state: ConsentState) {
        switch state {
        case .notAsked:
            consentVersion = nil
            consentGrantedAtMs = nil
            consentWithdrawnAtMs = nil
        case let .granted(version, at):
            consentVersion = version
            consentGrantedAtMs = at
            consentWithdrawnAtMs = nil
        case let .withdrawn(at):
            consentWithdrawnAtMs = at
            consentGrantedAtMs = nil
        }
    }
}

// MARK: - storage

public protocol SnapshotStore: AnyObject, Sendable {
    func load() -> CaptureSnapshot?
    func save(_ snapshot: CaptureSnapshot)
    func clear()
}

public final class MemorySnapshotStore: SnapshotStore, @unchecked Sendable {
    private let lock = NSLock()
    private var snapshot: CaptureSnapshot?
    public private(set) var writes = 0

    public init(_ snapshot: CaptureSnapshot? = nil) {
        self.snapshot = snapshot
    }

    public func load() -> CaptureSnapshot? {
        lock.lock(); defer { lock.unlock() }
        return snapshot
    }

    public func save(_ snapshot: CaptureSnapshot) {
        lock.lock(); defer { lock.unlock() }
        self.snapshot = snapshot
        writes += 1
    }

    public func clear() {
        lock.lock(); defer { lock.unlock() }
        snapshot = nil
    }
}

/// One small JSON file, written atomically.
///
/// Not `UserDefaults`: a CoreBluetooth relaunch can be over in well under a
/// second, and `UserDefaults` writes are coalesced on a background queue with no
/// flush the caller can wait on. A file write that has returned is a file write
/// that happened.
public final class FileSnapshotStore: SnapshotStore, @unchecked Sendable {
    private let url: URL
    private let lock = NSLock()

    public init(url: URL) {
        self.url = url
    }

    public static func inApplicationSupport(name: String = "capture-state.json") -> FileSnapshotStore {
        let base = (try? FileManager.default.url(
            for: .applicationSupportDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: true
        )) ?? URL(fileURLWithPath: NSTemporaryDirectory())
        return FileSnapshotStore(url: base.appendingPathComponent(name))
    }

    public func load() -> CaptureSnapshot? {
        lock.lock(); defer { lock.unlock() }
        guard
            let data = try? Data(contentsOf: url),
            let decoded = try? JSONDecoder().decode(CaptureSnapshot.self, from: data)
        else { return nil }
        return decoded
    }

    public func save(_ snapshot: CaptureSnapshot) {
        lock.lock(); defer { lock.unlock() }
        guard let data = try? JSONEncoder().encode(snapshot) else { return }
        try? data.write(to: url, options: [.atomic])
    }

    public func clear() {
        lock.lock(); defer { lock.unlock() }
        try? FileManager.default.removeItem(at: url)
    }
}

// MARK: - the plan

/// How the app was started. The three cases behave differently and conflating
/// them is how a background relaunch ends up presenting a consent screen nobody
/// is looking at.
public enum LaunchKind: String, Sendable, Equatable {
    case user
    /// iOS relaunched us because a restored `CBCentralManager` had an event.
    /// `UIApplication.LaunchOptionsKey.bluetoothCentrals` is present in
    /// `didFinishLaunchingWithOptions`.
    case bluetoothRestoration
    /// A `BGProcessingTask` fired — the nightly sync window.
    case backgroundTask
}

/// What restoration should actually do. Each field is a decision, and each has a
/// test.
public struct RestorationPlan: Sendable, Equatable {
    /// Reconnect to this specific peripheral rather than scanning. Scanning on a
    /// background relaunch is both slow and a battery cost for no reason.
    public var reconnectDeviceId: String?
    /// Ask the device whether it is recording (`0x0E05`) before believing the
    /// snapshot. The snapshot says what we *thought* when we were killed.
    public var confirmRecordingWithDevice: Bool
    /// Re-issue `startLocalRecording` if the device says it is not recording and
    /// the user's intent says it should be.
    public var resumeLocalRecording: Bool
    /// Reload the durable queue and try to flush it.
    public var restoreQueue: Bool
    /// Show UI. False for a background relaunch: there is no window.
    public var presentUI: Bool
    /// The last run died holding the glasses' access point, so the phone may
    /// have no uplink. Leave it before anything tries to reach the box.
    public var leaveAccessPoint: Bool
    /// Nothing usable was stored, or consent is not in force. Start cold.
    public var coldStart: Bool
    /// Why, in one line, for the log.
    public var reason: String

    public init(
        reconnectDeviceId: String? = nil,
        confirmRecordingWithDevice: Bool = false,
        resumeLocalRecording: Bool = false,
        restoreQueue: Bool = false,
        presentUI: Bool = false,
        leaveAccessPoint: Bool = false,
        coldStart: Bool = false,
        reason: String = ""
    ) {
        self.reconnectDeviceId = reconnectDeviceId
        self.confirmRecordingWithDevice = confirmRecordingWithDevice
        self.resumeLocalRecording = resumeLocalRecording
        self.restoreQueue = restoreQueue
        self.presentUI = presentUI
        self.leaveAccessPoint = leaveAccessPoint
        self.coldStart = coldStart
        self.reason = reason
    }
}

public enum Restoration {

    /// Pure, so the interesting cases are enumerable in a test rather than
    /// reachable only by killing an app on a physical phone.
    public static func plan(for snapshot: CaptureSnapshot?, launch: LaunchKind) -> RestorationPlan {
        let presentUI = launch == .user

        guard let snapshot else {
            return RestorationPlan(
                presentUI: presentUI,
                coldStart: true,
                reason: "no saved capture state"
            )
        }

        guard snapshot.schemaVersion == CaptureSnapshot.currentSchemaVersion else {
            // Everything in the snapshot is re-derivable from the glasses and
            // the queue on disk. Guessing at a foreign layout is not.
            return RestorationPlan(
                presentUI: presentUI,
                restoreQueue: true,
                coldStart: true,
                reason: "snapshot schema \(snapshot.schemaVersion) is not "
                    + "\(CaptureSnapshot.currentSchemaVersion); discarded"
            )
        }

        // Consent first, and it outranks everything. A relaunch triggered by the
        // glasses reconnecting must not restart capture for someone who turned
        // it off — that is the one failure here with a legal consequence
        // (`docs/ARCHITECTURE.md` §6).
        guard snapshot.consent.allowsCapture else {
            return RestorationPlan(
                restoreQueue: true,
                presentUI: presentUI,
                leaveAccessPoint: snapshot.holdingAccessPoint,
                coldStart: true,
                reason: "consent is not in force; nothing is resumed, but already-captured "
                    + "audio still uploads"
            )
        }

        guard let version = snapshot.consentVersion, version == currentConsentVersion else {
            return RestorationPlan(
                restoreQueue: true,
                presentUI: presentUI,
                leaveAccessPoint: snapshot.holdingAccessPoint,
                coldStart: true,
                reason: "consent was given against an older version of the text; ask again"
            )
        }

        guard snapshot.captureEnabled else {
            return RestorationPlan(
                reconnectDeviceId: snapshot.deviceId,
                restoreQueue: true,
                presentUI: presentUI,
                leaveAccessPoint: snapshot.holdingAccessPoint,
                reason: "capture was off; reconnect only"
            )
        }

        return RestorationPlan(
            reconnectDeviceId: snapshot.deviceId,
            // Always ask. The glasses keep recording without us, so "we thought
            // it was running" and "it is running" are different facts, and both
            // possible answers are normal.
            confirmRecordingWithDevice: true,
            resumeLocalRecording: snapshot.wornAtLastSample,
            restoreQueue: true,
            presentUI: presentUI,
            leaveAccessPoint: snapshot.holdingAccessPoint,
            reason: launch == .bluetoothRestoration
                ? "relaunched by CoreBluetooth with capture on; resume in the background"
                : "capture was on; resume"
        )
    }
}
