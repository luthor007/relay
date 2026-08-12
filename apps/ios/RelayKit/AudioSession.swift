import Foundation

/// The audio session, as a decision instead of a side effect.
///
/// `docs/APPS-SCOPE.md` §4.1 lists "add the `audio` background mode and an
/// `AVAudioSession` that justifies it" as iOS's highest-risk work. The risk is
/// not the API — it is three lines — but the *policy*, and policy is the part
/// that can be tested on a laptop. So this file holds:
///
///   - `AudioSessionConfiguration`, the category/mode/options as plain values
///   - `AudioSessionPolicy`, which decides when a session should be active
///   - `AudioSessionControlling`, the seam
///   - `MockAudioSession`, which records everything and can fail or interrupt
///
/// `SystemAudioSession` (the `AVAudioSession` half) is thirty lines in its own
/// file and contains no decisions at all.
///
/// ## The thing most designs get wrong
///
/// **An active audio session does not keep an iOS app running.** iOS grants
/// background execution to an app with the `audio` background mode while it is
/// actually *producing or consuming* audio; a session that is merely active and
/// silent gets the app suspended, and — worse — an app that declares `audio` and
/// never makes any is exactly the rejection Apple's review guidelines describe.
///
/// So the `audio` mode here buys the **interactive loop**: the voice turn and the
/// spoken reply, which are audible and are the app's purpose. All-day capture
/// does not depend on it and must not be built as if it did — that is Path B,
/// where the glasses record to their own storage and BLE state restoration
/// (`bluetooth-central` + `CBCentralManagerOptionRestoreIdentifierKey`) relaunches
/// us when something happens. ``BackgroundRuntimeSource`` names which of the two
/// is holding the app up at any moment, and there is a test for the case that
/// matters: capture on, nothing audible, app backgrounded.

// MARK: - configuration

/// `AVAudioSession.Category`, as a value this framework can reason about without
/// importing AVFoundation. Translated in `SystemAudioSession`.
public enum AudioCategory: String, Sendable, Equatable {
    /// Output only. The spoken reply when no microphone is open.
    case playback
    /// Both directions. Required while a voice turn is open.
    case playAndRecord
}

/// `AVAudioSession.Mode`.
public enum AudioMode: String, Sendable, Equatable {
    /// Long-form speech: iOS routes it like a podcast, and the system respects
    /// the user's speech-rate and route preferences.
    case spokenAudio
    /// Short conversational turns; enables the voice-processing signal chain.
    case voiceChat
    case `default`
}

public struct AudioSessionConfiguration: Sendable, Equatable {
    public var category: AudioCategory
    public var mode: AudioMode
    /// `AVAudioSession.CategoryOptions.allowBluetooth` — hands-free profile.
    /// Named for what it does rather than for the constant, because Apple
    /// renamed the constant to `.allowBluetoothHFP` after our deployment target.
    public var allowsBluetoothHandsFree: Bool
    /// `.allowBluetoothA2DP`. The reply should be able to come out of the user's
    /// own headphones, which is a stereo route.
    public var allowsBluetoothA2DP: Bool
    /// `.duckOthers`. A four-word reply should quiet the podcast, not stop it.
    public var duckOthers: Bool

    public init(
        category: AudioCategory,
        mode: AudioMode,
        allowsBluetoothHandsFree: Bool = true,
        allowsBluetoothA2DP: Bool = true,
        duckOthers: Bool = true
    ) {
        self.category = category
        self.mode = mode
        self.allowsBluetoothHandsFree = allowsBluetoothHandsFree
        self.allowsBluetoothA2DP = allowsBluetoothA2DP
        self.duckOthers = duckOthers
    }

    /// A voice turn: the glasses' microphone is open and a reply is expected on
    /// the same channel. `0x0A03` is bidirectional, so this is genuinely
    /// full-duplex from the product's point of view even though the bytes ride
    /// BLE rather than an audio route.
    public static let voiceTurn = AudioSessionConfiguration(
        category: .playAndRecord,
        mode: .voiceChat
    )

    /// Speaking an answer with no microphone open — a digest, or a reply to
    /// something typed.
    public static let spokenReply = AudioSessionConfiguration(
        category: .playback,
        mode: .spokenAudio
    )
}

// MARK: - the seam

public enum AudioSessionError: Error, Sendable, Equatable {
    case activationFailed(String)
    case deactivationFailed(String)
}

/// Interruptions are not an edge case on a phone: a call, an alarm and Siri all
/// take the session away mid-sentence.
public enum AudioInterruption: Sendable, Equatable {
    case began
    /// `shouldResume` is Apple's `.shouldResume` option — honour it rather than
    /// always resuming, or the app talks over the call the user just took.
    case ended(shouldResume: Bool)
    /// `AVAudioSession.mediaServicesWereResetNotification`. Everything must be
    /// rebuilt from scratch; nothing that was configured before survives.
    case mediaServicesReset
}

public protocol AudioSessionControlling: AnyObject, Sendable {
    var isActive: Bool { get }
    var activeConfiguration: AudioSessionConfiguration? { get }
    func activate(_ configuration: AudioSessionConfiguration) throws
    func deactivate() throws
    /// Register for interruptions. Releasing the token unregisters.
    @discardableResult
    func onInterruption(_ handler: @escaping @Sendable (AudioInterruption) -> Void) -> Subscription
}

// MARK: - policy

/// What the app is being asked to do right now.
public struct AudioDemand: Sendable, Equatable {
    /// Path A is open: the glasses' microphone is streaming.
    public var voiceTurnOpen: Bool
    /// We are playing, or about to play, a reply.
    public var replyPlaying: Bool
    /// The user has capture switched on. On its own this is *not* a reason to
    /// hold an audio session — see the note at the top of this file.
    public var captureEnabled: Bool
    /// Path B: the glasses are recording to their own storage.
    public var localRecording: Bool
    public var appInForeground: Bool

    public init(
        voiceTurnOpen: Bool = false,
        replyPlaying: Bool = false,
        captureEnabled: Bool = false,
        localRecording: Bool = false,
        appInForeground: Bool = true
    ) {
        self.voiceTurnOpen = voiceTurnOpen
        self.replyPlaying = replyPlaying
        self.captureEnabled = captureEnabled
        self.localRecording = localRecording
        self.appInForeground = appInForeground
    }
}

/// Why a session is being held. Every activation has to have one of these, and
/// each is something the user can hear — which is the whole App Review argument
/// for the `audio` background mode, expressed as code rather than as prose.
public enum AudioSessionReason: String, Sendable, Equatable {
    case voiceTurn
    case spokenReply
}

/// What is keeping the process alive.
public enum BackgroundRuntimeSource: String, Sendable, Equatable {
    /// The `audio` background mode, earned by audible work in progress.
    case audioSession
    /// `bluetooth-central` plus state restoration. Limited runtime per event,
    /// but the app is relaunched when the glasses have something to say.
    case bluetoothCentral
    /// The app is in the foreground and none of this applies.
    case foreground
    /// Nothing. iOS will suspend us, and that is *correct*: the glasses are
    /// recording to their own storage and nothing is being lost.
    case none
}

public enum AudioSessionDecision: Sendable, Equatable {
    case activate(AudioSessionConfiguration, AudioSessionReason)
    case deactivate
}

public enum AudioSessionPolicy {

    /// Pure. The only input is the demand, so a test can enumerate the whole
    /// space in a dozen lines.
    public static func decide(_ demand: AudioDemand) -> AudioSessionDecision {
        if demand.voiceTurnOpen {
            return .activate(.voiceTurn, .voiceTurn)
        }
        if demand.replyPlaying {
            return .activate(.spokenReply, .spokenReply)
        }
        // Deliberately no `captureEnabled` branch. Holding a silent session to
        // stay resident is the pattern Apple rejects, and it does not even work:
        // iOS suspends an app that holds an active session and plays nothing.
        return .deactivate
    }

    /// What will keep the app running, given the same demand.
    public static func runtimeSource(_ demand: AudioDemand) -> BackgroundRuntimeSource {
        if demand.appInForeground { return .foreground }
        if case .activate = decide(demand) { return .audioSession }
        if demand.captureEnabled || demand.localRecording { return .bluetoothCentral }
        return .none
    }

    /// The sentence the status screen shows, and the one that goes in the App
    /// Review notes. Written to be true in the state it describes.
    public static func explain(_ demand: AudioDemand) -> String {
        switch runtimeSource(demand) {
        case .foreground:
            return "Relay is open."
        case .audioSession:
            return "Relay is holding an audio session because it is listening or speaking."
        case .bluetoothCentral:
            return "Your glasses are recording to their own storage. "
                + "Relay wakes up when they reconnect."
        case .none:
            return "Nothing is being captured."
        }
    }
}

// MARK: - mock

/// Records every activation, and can refuse or interrupt on demand.
///
/// A mock that always succeeds produces a coordinator with no failure path, and
/// audio activation genuinely fails on a phone — a call in progress is enough.
public final class MockAudioSession: AudioSessionControlling, @unchecked Sendable {

    public struct Activation: Sendable, Equatable {
        public let configuration: AudioSessionConfiguration
        public let atMs: Int
    }

    private let lock = NSLock()
    private var handlers: [Int: @Sendable (AudioInterruption) -> Void] = [:]
    private var nextHandlerID = 0
    private var active = false
    private var current: AudioSessionConfiguration?
    private var shouldFailActivation = false
    private let clock: RelayClock

    /// Every activation in order, including repeats. Assert on this rather than
    /// on the final state: reconfiguring the session twice per turn is a real
    /// bug that a final-state assertion cannot see.
    public private(set) var activations: [Activation] = []
    public private(set) var deactivations = 0

    public init(clock: RelayClock = SystemClock()) {
        self.clock = clock
    }

    public var isActive: Bool {
        lock.lock(); defer { lock.unlock() }
        return active
    }

    public var activeConfiguration: AudioSessionConfiguration? {
        lock.lock(); defer { lock.unlock() }
        return current
    }

    public func failNextActivation(_ value: Bool = true) {
        lock.lock(); defer { lock.unlock() }
        shouldFailActivation = value
    }

    public func activate(_ configuration: AudioSessionConfiguration) throws {
        lock.lock()
        if shouldFailActivation {
            shouldFailActivation = false
            lock.unlock()
            throw AudioSessionError.activationFailed("mock: activation refused")
        }
        active = true
        current = configuration
        activations.append(Activation(configuration: configuration, atMs: clock.now()))
        lock.unlock()
    }

    public func deactivate() throws {
        lock.lock()
        active = false
        current = nil
        deactivations += 1
        lock.unlock()
    }

    @discardableResult
    public func onInterruption(
        _ handler: @escaping @Sendable (AudioInterruption) -> Void
    ) -> Subscription {
        lock.lock()
        nextHandlerID += 1
        let id = nextHandlerID
        handlers[id] = handler
        lock.unlock()
        return Subscription { [weak self] in
            guard let self else { return }
            self.lock.lock()
            self.handlers.removeValue(forKey: id)
            self.lock.unlock()
        }
    }

    /// Test control: a call arrives, Siri fires, the alarm goes off.
    public func interrupt(_ interruption: AudioInterruption) {
        lock.lock()
        let snapshot = Array(handlers.values)
        if case .began = interruption {
            active = false
            current = nil
        }
        lock.unlock()
        for handler in snapshot { handler(interruption) }
    }
}
