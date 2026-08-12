import Foundation

/// The app's whole capture policy, above the transport and below the UI.
///
/// Everything here runs identically against ``MockTransport`` on a laptop and
/// against the vendor SDK on a phone, which is the point of the seam
/// (`docs/APPS-SCOPE.md` §5). The two capture paths from §3 live here:
///
///   **Path A — live stream.** Opened on a tap or a wake word, closed
///   immediately after. Costs both radios and both batteries, so it is a turn,
///   not a state.
///
///   **Path B — record on the glasses, transfer later.** The all-day path. The
///   glasses write to their own storage, which is why capture survives the phone
///   being out of range, in a pocket, or dead.
///
/// The rule that ties them together: **the app never records without consent,
/// and consent is checked here rather than in a view.** A gate in a view is a
/// gate one new screen can walk around; `docs/ARCHITECTURE.md` §6 makes this a
/// legal question in Québec, not a UX one.

// MARK: - recognition

/// Speech recognition is the *app's* job.
///
/// The glasses are a microphone and a button: `0x0803`/`0x0805` report that
/// something was said, and no command anywhere asks the device to transcribe
/// (`glasses/bridge/src/commands.ts`). On iOS the real implementation is
/// `SFSpeechRecognizer` with an `SFSpeechAudioBufferRecognitionRequest`, which
/// needs `NSSpeechRecognitionUsageDescription` — and which is why this is a
/// protocol: it is one more thing that cannot run in a unit test.
public protocol SpeechRecognizing: AnyObject, Sendable {
    func start() throws
    /// Feed a chunk as it arrives off the BLE link.
    func append(_ chunk: AudioChunk)
    /// Stop and return the final transcript. Empty means nothing was heard,
    /// which is a normal outcome and not an error.
    func finish() async -> String
    func cancel()
}

/// Returns whatever it was told to. Deterministic, no audio stack.
public final class MockRecognizer: SpeechRecognizing, @unchecked Sendable {
    private let lock = NSLock()
    private var scripted: [String]
    private var running = false
    public private(set) var chunksSeen = 0

    public init(transcripts: [String] = []) {
        self.scripted = transcripts
    }

    public var isRunning: Bool {
        lock.lock(); defer { lock.unlock() }
        return running
    }

    public func start() throws {
        lock.lock(); defer { lock.unlock() }
        running = true
    }

    public func append(_ chunk: AudioChunk) {
        lock.lock(); defer { lock.unlock() }
        guard running else { return }
        chunksSeen += 1
    }

    public func finish() async -> String {
        lock.lock(); defer { lock.unlock() }
        running = false
        return scripted.isEmpty ? "" : scripted.removeFirst()
    }

    public func cancel() {
        lock.lock(); defer { lock.unlock() }
        running = false
    }
}

// MARK: - status

public struct RecordingIndicator: Sendable, Equatable {
    /// Drives the amber dot. True whenever audio is being captured *anywhere*,
    /// including on the glasses while the phone cannot see them.
    public let active: Bool
    public let headline: String
    public let detail: String?
}

public struct CaptureStatus: Sendable, Equatable {
    public var consent: ConsentState = .notAsked
    public var captureEnabled = false
    public var connection: ConnectionState = .disconnected
    public var worn = false
    /// What we last knew about Path B. Not "what is true right now" — the
    /// glasses keep recording without us.
    public var recording = false
    public var voiceTurnOpen = false
    public var battery: BatteryStatus?
    public var disk: DiskInfo?
    public var storage: StorageAssessment?
    public var lastError: String?
    public var lastTranscript: String?

    public init() {}

    public var indicator: RecordingIndicator { CaptureStatus.indicator(for: self) }

    /// Pure, so every branch is a test rather than a device you have to walk out
    /// of range of.
    ///
    /// The rule worth stating: **never say "not recording" merely because the
    /// link dropped.** The glasses write to their own storage, so losing the
    /// phone means we stopped knowing, not that recording stopped. Saying
    /// otherwise pushes people to restart a recording that never ended, which
    /// produces two files and one confused user (`docs/ARCHITECTURE.md` §6).
    public static func indicator(for status: CaptureStatus) -> RecordingIndicator {
        guard status.captureEnabled else {
            return RecordingIndicator(active: false, headline: "Capture off", detail: nil)
        }
        if status.voiceTurnOpen {
            return RecordingIndicator(
                active: true,
                headline: "Listening",
                detail: "Your glasses' microphone is open."
            )
        }
        switch status.connection {
        case .connected:
            if status.recording {
                return RecordingIndicator(active: true, headline: "Recording", detail: nil)
            }
            return RecordingIndicator(
                active: false,
                headline: status.worn ? "Connected" : "Connected — put them on to start",
                detail: nil
            )
        case .connecting:
            return RecordingIndicator(active: false, headline: "Connecting…", detail: nil)
        case .reconnecting, .disconnected:
            if status.recording {
                return RecordingIndicator(
                    active: true,
                    headline: "Recording on the glasses",
                    detail: "Out of range — the day is safe on the glasses and will sync later."
                )
            }
            return RecordingIndicator(active: false, headline: "Reconnecting…", detail: nil)
        }
    }
}

public enum CaptureError: Error, Sendable, Equatable {
    /// The gate. Every path that can open a microphone goes through it.
    case consentRequired
    case notConnected
    case alreadyListening
}

/// Why a voice turn opened. Both are user intent; the wake word is just a
/// hands-free tap.
public enum VoiceTrigger: String, Sendable, Equatable {
    case tap
    case wakeWord
    case app
}

// MARK: - the coordinator

public final class CaptureCoordinator: @unchecked Sendable {

    private let glasses: GlassesTransport
    private let audio: AudioSessionControlling
    private let recognizer: SpeechRecognizing
    private let snapshots: SnapshotStore
    private let clock: RelayClock
    private let link: RelaydLink?
    private var statusHandler: (@Sendable (CaptureStatus) -> Void)?

    private let lock = NSLock()
    private var state = CaptureStatus()
    private var deviceId: String?
    private var recordingStartedAtMs: Int?
    private var holdingAccessPoint = false
    private var subscription: Subscription?
    private var interruptionToken: Subscription?

    public init(
        glasses: GlassesTransport,
        audio: AudioSessionControlling = MockAudioSession(),
        recognizer: SpeechRecognizing = MockRecognizer(),
        snapshots: SnapshotStore = MemorySnapshotStore(),
        clock: RelayClock = SystemClock(),
        link: RelaydLink? = nil,
        onStatus: (@Sendable (CaptureStatus) -> Void)? = nil
    ) {
        self.glasses = glasses
        self.audio = audio
        self.recognizer = recognizer
        self.snapshots = snapshots
        self.clock = clock
        self.link = link
        self.statusHandler = onStatus
        subscribe()
    }

    deinit {
        subscription?.unsubscribe()
        interruptionToken?.unsubscribe()
    }

    public var status: CaptureStatus {
        lock.lock(); defer { lock.unlock() }
        return state
    }

    /// The demand ``AudioSessionPolicy`` reasons about. Exposed so the status
    /// screen can show the honest sentence about what is keeping the app alive.
    public func audioDemand(appInForeground: Bool) -> AudioDemand {
        let snapshot = status
        return AudioDemand(
            voiceTurnOpen: snapshot.voiceTurnOpen,
            replyPlaying: false,
            captureEnabled: snapshot.captureEnabled,
            localRecording: snapshot.recording,
            appInForeground: appInForeground
        )
    }

    // MARK: - consent

    public func grantConsent() {
        mutate { $0.consent = .granted(version: currentConsentVersion, atMs: clock.now()) }
        link?.send(.consentDecision, .object([
            "granted": .bool(true),
            "version": .number(Double(currentConsentVersion)),
        ]))
        persist()
    }

    /// Withdrawing stops capture *first* and records it second. The other order
    /// leaves a window in which the app is recording under a consent it has
    /// already been told it does not have.
    public func withdrawConsent() async {
        await stopCapture()
        mutate { $0.consent = .withdrawn(atMs: clock.now()) }
        link?.send(.consentDecision, .object(["granted": .bool(false)]))
        persist()
    }

    // MARK: - Path B, all-day capture

    public func startCapture(deviceId: String? = nil) async throws {
        guard status.consent.allowsCapture else { throw CaptureError.consentRequired }

        mutate {
            $0.captureEnabled = true
            $0.lastError = nil
        }
        if let deviceId { lock.lock(); self.deviceId = deviceId; lock.unlock() }
        persist()

        do {
            try await glasses.connect(options: ConnectOptions(deviceId: deviceId))
            // Before anything is captured: every chunk carries `deviceTimeMs`
            // and the box segments episodes by time.
            try await glasses.setTime(Date())
            _ = await refreshDeviceFacts()
            if status.worn { try await beginLocalRecording() }
        } catch let error as GlassesError {
            mutate { $0.lastError = error.message }
            throw error
        }
        persist()
    }

    public func stopCapture() async {
        mutate { $0.captureEnabled = false }
        await endVoiceTurn()
        try? await glasses.stopLocalRecording()
        mutate { $0.recording = false }
        lock.lock(); recordingStartedAtMs = nil; lock.unlock()
        await glasses.disconnect()
        persist()
    }

    @discardableResult
    public func refreshDeviceFacts() async -> StorageAssessment? {
        let battery = try? await glasses.getBattery()
        let disk = try? await glasses.getDiskInfo()
        let files = (try? await glasses.listFiles()) ?? []
        let assessment = disk.map { StoragePolicy.assess(disk: $0, files: files) }
        mutate {
            if let battery { $0.battery = battery }
            if let disk { $0.disk = disk }
            if let assessment { $0.storage = assessment }
        }
        return assessment
    }

    private func beginLocalRecording() async throws {
        guard status.consent.allowsCapture else { throw CaptureError.consentRequired }
        try await glasses.startLocalRecording()
        let now = clock.now()
        lock.lock(); recordingStartedAtMs = now; lock.unlock()
        mutate { $0.recording = true }
        persist()
    }

    // MARK: - Path A, the interactive turn

    /// Open the live microphone, recognise, and send the utterance up the link.
    ///
    /// The audio session comes up *before* the glasses start streaming. The
    /// other order gives you a few hundred milliseconds of chunks arriving with
    /// nowhere to play a reply, and on a phone that just took a call it gives
    /// you a turn that can never be answered.
    public func beginVoiceTurn(_ trigger: VoiceTrigger = .tap) async throws {
        guard status.consent.allowsCapture else { throw CaptureError.consentRequired }
        guard !status.voiceTurnOpen else { throw CaptureError.alreadyListening }
        guard glasses.state == .connected else { throw CaptureError.notConnected }

        try audio.activate(.voiceTurn)
        do {
            try recognizer.start()
            try await glasses.startVoiceSession()
        } catch {
            // Do not leave a session held for a turn that never opened: iOS
            // keeps ducking everything else until it is released.
            try? audio.deactivate()
            recognizer.cancel()
            throw error
        }

        mutate { $0.voiceTurnOpen = true }
        link?.send(.touch, .object(["action": .string(trigger.rawValue)]))
        persist()
    }

    /// Close the turn and hand the transcript to the box.
    ///
    /// Idempotent: an interruption and a user tap can both land, and closing
    /// twice must not send the utterance twice.
    @discardableResult
    public func endVoiceTurn() async -> String? {
        let wasOpen: Bool = {
            lock.lock(); defer { lock.unlock() }
            let open = state.voiceTurnOpen
            state.voiceTurnOpen = false
            return open
        }()
        guard wasOpen else { return nil }

        try? await glasses.stopVoiceSession()
        let transcript = await recognizer.finish()
        try? audio.deactivate()

        if !transcript.isEmpty {
            mutate { $0.lastTranscript = transcript }
            link?.send(.utterance, .object([
                "text": .string(transcript),
                "atMs": .number(Double(clock.now())),
            ]))
        }
        notify()
        persist()
        return transcript.isEmpty ? nil : transcript
    }

    // MARK: - catalog

    /// Run a catalog action, with the consent gate applied where it belongs.
    ///
    /// The gate is here rather than in the view because ``GlassesCatalog``
    /// generates the view: a new action gets the gate for free, and a new screen
    /// cannot route around it.
    public func run(_ action: GlassesAction, argument: String? = nil) async throws -> String {
        if action.opensMicrophone, !status.consent.allowsCapture {
            throw CaptureError.consentRequired
        }
        do {
            let result = try await action.run(glasses, argument)
            mutate { $0.lastError = nil }
            return result
        } catch let error as GlassesError {
            mutate { $0.lastError = error.message }
            throw error
        }
    }

    // MARK: - restoration

    /// Apply the plan for how this process started.
    ///
    /// The interesting case is ``LaunchKind/bluetoothRestoration``: iOS has
    /// relaunched us into the background because the glasses reconnected, there
    /// is no window, and we have a few seconds. Reconnect, find out whether Path
    /// B is actually running, and get out.
    @discardableResult
    public func applyLaunch(_ launch: LaunchKind) async -> RestorationPlan {
        let snapshot = snapshots.load()
        let plan = Restoration.plan(for: snapshot, launch: launch)

        if let snapshot, !plan.coldStart {
            mutate {
                $0.consent = snapshot.consent
                $0.captureEnabled = snapshot.captureEnabled
                $0.recording = snapshot.localRecordingBelievedRunning
                $0.worn = snapshot.wornAtLastSample
            }
            lock.lock()
            deviceId = snapshot.deviceId
            recordingStartedAtMs = snapshot.recordingStartedAtMs
            lock.unlock()
        } else if let snapshot {
            // Even a cold start keeps the consent record. Forgetting a
            // withdrawal because a schema changed would be the worst possible
            // way to lose it.
            mutate { $0.consent = snapshot.consent }
        }

        if plan.leaveAccessPoint {
            // The last run died holding the glasses' network, so this phone may
            // have no uplink at all until we let go.
            try? await glasses.closeWifiAccessPoint()
            lock.lock(); holdingAccessPoint = false; lock.unlock()
        }

        if let deviceId = plan.reconnectDeviceId {
            try? await glasses.connect(options: ConnectOptions(deviceId: deviceId))
        }

        if plan.confirmRecordingWithDevice {
            // There is no "are you recording?" command — `0x0E05` is a status
            // *report*, not a query — so confirmation is a re-assert.
            // `startLocalRecording` is idempotent in `MockTransport` and is
            // documented as idempotent on the device; **whether the real
            // firmware agrees is one of the first things a hardware session has
            // to check**, because the failure mode is two files where there
            // should be one.
            if plan.resumeLocalRecording, status.captureEnabled, status.consent.allowsCapture {
                try? await beginLocalRecording()
            }
        }

        persist()
        return plan
    }

    // MARK: - events

    private func subscribe() {
        subscription = glasses.on { [weak self] event in
            guard let self else { return }
            self.handle(event)
        }
        interruptionToken = audio.onInterruption { [weak self] interruption in
            guard let self else { return }
            switch interruption {
            case .began, .mediaServicesReset:
                // A call took the session. Close the turn rather than leaving
                // the glasses streaming into a session we no longer hold.
                Task { await self.endVoiceTurn() }
            case .ended:
                // Deliberately do not reopen. The turn is over; the user can
                // tap again. Resuming a microphone by itself after a phone call
                // is exactly the behaviour that makes people distrust this
                // category of product.
                break
            }
        }
    }

    private func handle(_ event: GlassesEvent) {
        switch event {
        case let .connectionChanged(connection):
            mutate { $0.connection = connection }
            persist()

        case let .wear(worn):
            mutate { $0.worn = worn }
            link?.send(.wear, .object(["worn": .bool(worn)]))
            Task { [weak self] in await self?.applyWearGate(worn) }

        case let .touch(action):
            link?.send(.touch, .object(["action": .string(action.rawValue)]))
            // A tap is the hands-on half of "tap or wake word" (§4.4). Wear and
            // remove arrive on the same channel and are handled above, so they
            // are excluded here rather than opening a microphone when someone
            // puts their glasses on.
            if action == .singleTap || action == .doubleTap {
                Task { [weak self] in try? await self?.beginVoiceTurn(.tap) }
            }

        case let .audioChunk(chunk):
            recognizer.append(chunk)

        case let .transcriptText(text):
            mutate { $0.lastTranscript = text }

        case let .battery(battery):
            mutate { $0.battery = battery }

        case let .diskInfo(disk):
            mutate {
                $0.disk = disk
                $0.storage = StoragePolicy.assess(disk: disk)
            }

        case let .recordingState(recording):
            mutate { $0.recording = recording.recording }
            persist()

        case let .voiceSessionChanged(open):
            mutate { $0.voiceTurnOpen = open }

        case let .error(error):
            mutate { $0.lastError = error.message }

        case .photo, .photoProgress, .rtspUrl:
            break
        }
    }

    /// Start on wear, stop on removal — `docs/APPS-SCOPE.md` §4.4.
    ///
    /// Gated on capture being on *and* consent being in force, so putting the
    /// glasses on never starts a recording by itself.
    private func applyWearGate(_ worn: Bool) async {
        guard status.captureEnabled, status.consent.allowsCapture else { return }
        if worn {
            try? await beginLocalRecording()
        } else {
            try? await glasses.stopLocalRecording()
            mutate { $0.recording = false }
            lock.lock(); recordingStartedAtMs = nil; lock.unlock()
            persist()
        }
    }

    // MARK: - internals

    private func mutate(_ body: (inout CaptureStatus) -> Void) {
        lock.lock()
        body(&state)
        lock.unlock()
        notify()
    }

    /// Set after construction: the app layer's closure captures the object that
    /// owns this coordinator, so it cannot be passed to `init`.
    public func setStatusHandler(_ handler: @escaping @Sendable (CaptureStatus) -> Void) {
        lock.lock(); statusHandler = handler; lock.unlock()
        handler(status)
    }

    private func notify() {
        lock.lock()
        let handler = statusHandler
        lock.unlock()
        handler?(status)
    }

    /// Written on every state change, because the process can end at any point
    /// between two of them.
    public func persist() {
        let snapshot: CaptureSnapshot = {
            lock.lock(); defer { lock.unlock() }
            var snapshot = CaptureSnapshot()
            snapshot.setConsent(state.consent)
            snapshot.captureEnabled = state.captureEnabled
            snapshot.localRecordingBelievedRunning = state.recording
            snapshot.recordingStartedAtMs = recordingStartedAtMs
            snapshot.deviceId = deviceId
            snapshot.wornAtLastSample = state.worn
            snapshot.holdingAccessPoint = holdingAccessPoint
            snapshot.savedAtMs = clock.now()
            return snapshot
        }()
        snapshots.save(snapshot)
    }

    /// Told by ``BulkSync`` so a crash mid-sync can be recovered from.
    public func setHoldingAccessPoint(_ value: Bool) {
        lock.lock(); holdingAccessPoint = value; lock.unlock()
        persist()
    }
}
