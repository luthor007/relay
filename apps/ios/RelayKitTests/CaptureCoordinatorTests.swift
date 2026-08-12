import Foundation
import XCTest
@testable import RelayKit

/// The consent gate, the two capture paths and the recording indicator.
///
/// All of it runs with no glasses, no SDK and no phone — which is the argument
/// for keeping it in `RelayKit` rather than in a view model.
final class CaptureCoordinatorTests: XCTestCase {

    /// Let spawned work run. Several paths here hand off to a `Task` on purpose
    /// (a wear event must not block the transport's callback thread), and
    /// `Task.yield()` alone does not promise that a *specific* other task ran.
    private func settle() async {
        for _ in 0 ..< 12 {
            await Task.yield()
            try? await Task.sleep(nanoseconds: 1_000_000)
        }
    }

    private func makeCoordinator(
        glasses: StubGlasses = StubGlasses(),
        audio: MockAudioSession = MockAudioSession(),
        recognizer: MockRecognizer = MockRecognizer(),
        snapshots: MemorySnapshotStore = MemorySnapshotStore(),
        link: RelaydLink? = nil
    ) -> CaptureCoordinator {
        CaptureCoordinator(
            glasses: glasses,
            audio: audio,
            recognizer: recognizer,
            snapshots: snapshots,
            clock: TestClock(startMs: 1_700_000_000_000),
            link: link
        )
    }

    private func openLink() -> (RelaydLink, MockSocketFactory) {
        let factory = MockSocketFactory()
        let link = RelaydLink(
            url: URL(string: "wss://relay.example/link")!,
            credential: DeviceCredential(
                deviceId: "phone-a",
                boxId: "box-1",
                deviceToken: "1122334455667788",
                signingKey: "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
            ),
            socketFactory: factory.make(),
            clock: TestClock(startMs: 1_700_000_000_000),
            random: countingRandom
        )
        link.connect()
        factory.latest?.simulateOpen()
        return (link, factory)
    }

    // MARK: the gate

    func testCaptureCannotStartWithoutConsent() async {
        let glasses = StubGlasses()
        let coordinator = makeCoordinator(glasses: glasses)

        do {
            try await coordinator.startCapture()
            XCTFail("capture started without consent")
        } catch {
            XCTAssertEqual(error as? CaptureError, .consentRequired)
        }
        XCTAssertFalse(glasses.callLog.contains("startLocalRecording"))
    }

    func testAVoiceTurnCannotOpenWithoutConsent() async {
        let glasses = StubGlasses()
        let audio = MockAudioSession()
        let coordinator = makeCoordinator(glasses: glasses, audio: audio)

        do {
            try await coordinator.beginVoiceTurn()
            XCTFail("a microphone opened without consent")
        } catch {
            XCTAssertEqual(error as? CaptureError, .consentRequired)
        }
        XCTAssertTrue(audio.activations.isEmpty)
        XCTAssertFalse(glasses.callLog.contains("startVoiceSession"))
    }

    func testACatalogActionThatOpensAMicrophoneIsGatedToo() async throws {
        let coordinator = makeCoordinator()
        let action = try XCTUnwrap(GlassesCatalog.action(id: "startRecording"))

        do {
            _ = try await coordinator.run(action)
            XCTFail("the catalog walked around the consent gate")
        } catch {
            XCTAssertEqual(error as? CaptureError, .consentRequired)
        }
    }

    func testAHarmlessCatalogActionIsNotGated() async throws {
        let coordinator = makeCoordinator()
        let action = try XCTUnwrap(GlassesCatalog.action(id: "battery"))
        let summary = try await coordinator.run(action)
        XCTAssertTrue(summary.contains("%"))
    }

    func testWithdrawingConsentStopsCaptureBeforeRecordingTheWithdrawal() async throws {
        let glasses = StubGlasses()
        let coordinator = makeCoordinator(glasses: glasses)
        coordinator.grantConsent()
        try await coordinator.startCapture()

        await coordinator.withdrawConsent()

        XCTAssertEqual(coordinator.status.consent, .withdrawn(atMs: 1_700_000_000_000))
        XCTAssertFalse(coordinator.status.captureEnabled)
        XCTAssertTrue(glasses.callLog.contains("stopLocalRecording"))
        XCTAssertTrue(glasses.callLog.contains("disconnect"))
    }

    // MARK: Path B and wear gating

    func testCaptureStartsTheClockAlignmentBeforeAnythingIsRecorded() async throws {
        let glasses = StubGlasses()
        let coordinator = makeCoordinator(glasses: glasses)
        coordinator.grantConsent()

        try await coordinator.startCapture()

        let log = glasses.callLog
        XCTAssertTrue(log.contains("setTime"))
        let setTimeIndex = log.firstIndex(of: "setTime")
        XCTAssertNotNil(setTimeIndex)
        XCTAssertLessThan(
            setTimeIndex!, log.firstIndex(of: "startLocalRecording") ?? Int.max,
            "every chunk carries deviceTimeMs and the box segments by time"
        )
    }

    func testPuttingTheGlassesOnStartsRecordingAndTakingThemOffStopsIt() async throws {
        let glasses = StubGlasses()
        let coordinator = makeCoordinator(glasses: glasses)
        coordinator.grantConsent()
        try await coordinator.startCapture()

        glasses.emit(.wear(true))
        await settle()
        XCTAssertTrue(coordinator.status.worn)
        XCTAssertTrue(glasses.callLog.contains("startLocalRecording"))

        glasses.emit(.wear(false))
        await settle()
        XCTAssertFalse(coordinator.status.worn)
        XCTAssertFalse(coordinator.status.recording)
    }

    func testWearWithCaptureOffDoesNotStartRecording() async {
        let glasses = StubGlasses()
        let coordinator = makeCoordinator(glasses: glasses)
        coordinator.grantConsent()

        glasses.emit(.wear(true))
        await settle()

        XCTAssertFalse(
            glasses.callLog.contains("startLocalRecording"),
            "putting glasses on is not by itself an instruction to record"
        )
    }

    // MARK: Path A

    func testAVoiceTurnOpensTheAudioSessionBeforeTheMicrophone() async throws {
        let glasses = StubGlasses()
        let audio = MockAudioSession()
        let coordinator = makeCoordinator(glasses: glasses, audio: audio)
        coordinator.grantConsent()

        try await coordinator.beginVoiceTurn()

        XCTAssertEqual(audio.activations.first?.configuration, .voiceTurn)
        XCTAssertTrue(coordinator.status.voiceTurnOpen)
        XCTAssertTrue(glasses.callLog.contains("startVoiceSession"))
    }

    func testAFailedAudioActivationLeavesNothingHeldOpen() async {
        let glasses = StubGlasses()
        let audio = MockAudioSession()
        audio.failNextActivation()
        let coordinator = makeCoordinator(glasses: glasses, audio: audio)
        coordinator.grantConsent()

        do {
            try await coordinator.beginVoiceTurn()
            XCTFail("a turn opened without a session")
        } catch {
            XCTAssertTrue(error is AudioSessionError)
        }
        XCTAssertFalse(coordinator.status.voiceTurnOpen)
        XCTAssertFalse(glasses.callLog.contains("startVoiceSession"))
    }

    func testTheTranscriptGoesUpAsAnUtteranceAndTheSessionIsReleased() async throws {
        let glasses = StubGlasses()
        let audio = MockAudioSession()
        let (link, factory) = openLink()
        let coordinator = makeCoordinator(
            glasses: glasses,
            audio: audio,
            recognizer: MockRecognizer(transcripts: ["what did I say about the fixture?"]),
            link: link
        )
        coordinator.grantConsent()
        try await coordinator.beginVoiceTurn()

        let transcript = await coordinator.endVoiceTurn()

        XCTAssertEqual(transcript, "what did I say about the fixture?")
        XCTAssertFalse(audio.isActive, "holding the session after the turn keeps everything ducked")
        let types = factory.latest?.sentEnvelopes.map(\.type) ?? []
        XCTAssertTrue(types.contains("utterance"))
        XCTAssertTrue(types.contains("consent.decision"))
    }

    func testAnEmptyTranscriptSendsNothing() async throws {
        let (link, factory) = openLink()
        let coordinator = makeCoordinator(recognizer: MockRecognizer(transcripts: [""]), link: link)
        coordinator.grantConsent()
        try await coordinator.beginVoiceTurn()

        let transcript = await coordinator.endVoiceTurn()

        XCTAssertNil(transcript)
        let utterances = (factory.latest?.sentEnvelopes ?? []).filter { $0.type == "utterance" }
        XCTAssertTrue(utterances.isEmpty, "silence is a normal outcome, not an utterance")
    }

    func testClosingATurnTwiceSendsOneUtterance() async throws {
        let (link, factory) = openLink()
        let coordinator = makeCoordinator(
            recognizer: MockRecognizer(transcripts: ["once"]),
            link: link
        )
        coordinator.grantConsent()
        try await coordinator.beginVoiceTurn()

        _ = await coordinator.endVoiceTurn()
        _ = await coordinator.endVoiceTurn()

        let utterances = (factory.latest?.sentEnvelopes ?? []).filter { $0.type == "utterance" }
        XCTAssertEqual(utterances.count, 1)
    }

    func testAPhoneCallClosesTheTurnRatherThanLeavingTheGlassesStreaming() async throws {
        let glasses = StubGlasses()
        let audio = MockAudioSession()
        let coordinator = makeCoordinator(glasses: glasses, audio: audio)
        coordinator.grantConsent()
        try await coordinator.beginVoiceTurn()

        audio.interrupt(.began)
        await settle()

        XCTAssertFalse(coordinator.status.voiceTurnOpen)
        XCTAssertTrue(glasses.callLog.contains("stopVoiceSession"))
    }

    func testAnInterruptionEndingDoesNotReopenTheMicrophone() async throws {
        let glasses = StubGlasses()
        let audio = MockAudioSession()
        let coordinator = makeCoordinator(glasses: glasses, audio: audio)
        coordinator.grantConsent()
        try await coordinator.beginVoiceTurn()

        audio.interrupt(.began)
        await settle()
        audio.interrupt(.ended(shouldResume: true))
        await settle()

        XCTAssertFalse(
            coordinator.status.voiceTurnOpen,
            "a microphone that reopens itself after a phone call is why people distrust this "
                + "category of product"
        )
    }

    func testATapOpensATurn() async throws {
        let glasses = StubGlasses()
        let coordinator = makeCoordinator(glasses: glasses)
        coordinator.grantConsent()

        glasses.emit(.touch(.singleTap))
        await settle()

        XCTAssertTrue(coordinator.status.voiceTurnOpen)
    }

    func testChunksReachTheRecogniserRatherThanTheDevice() async throws {
        let glasses = StubGlasses()
        let recognizer = MockRecognizer(transcripts: ["heard it"])
        let coordinator = makeCoordinator(glasses: glasses, recognizer: recognizer)
        coordinator.grantConsent()
        try await coordinator.beginVoiceTurn()

        glasses.emit(.audioChunk(AudioChunk(data: Data(count: 320), sequence: 0, deviceTimeMs: 0)))
        glasses.emit(.audioChunk(AudioChunk(data: Data(count: 320), sequence: 1, deviceTimeMs: 200)))

        XCTAssertEqual(
            recognizer.chunksSeen, 2,
            "the glasses have no ASR — every transcript in this app is ours"
        )
    }

    // MARK: the indicator

    func testTheIndicatorNeverClaimsRecordingStoppedJustBecauseTheLinkDropped() {
        var status = CaptureStatus()
        status.captureEnabled = true
        status.recording = true
        status.connection = .disconnected

        let indicator = CaptureStatus.indicator(for: status)

        XCTAssertTrue(indicator.active)
        XCTAssertEqual(indicator.headline, "Recording on the glasses")
        XCTAssertNotNil(indicator.detail)
    }

    func testTheIndicatorIsOffWhenCaptureIsOff() {
        var status = CaptureStatus()
        status.captureEnabled = false
        status.recording = true
        XCTAssertFalse(CaptureStatus.indicator(for: status).active)
    }

    func testConnectedButNotWornExplainsWhatToDo() {
        var status = CaptureStatus()
        status.captureEnabled = true
        status.connection = .connected
        status.worn = false
        XCTAssertEqual(
            CaptureStatus.indicator(for: status).headline,
            "Connected — put them on to start"
        )
    }

    func testAnOpenVoiceTurnOutranksEverythingElseInTheIndicator() {
        var status = CaptureStatus()
        status.captureEnabled = true
        status.connection = .connected
        status.voiceTurnOpen = true
        let indicator = CaptureStatus.indicator(for: status)
        XCTAssertTrue(indicator.active)
        XCTAssertEqual(indicator.headline, "Listening")
    }

    // MARK: persistence and restoration

    func testConsentAndCaptureStateArePersistedOnEveryChange() async throws {
        let snapshots = MemorySnapshotStore()
        let coordinator = makeCoordinator(snapshots: snapshots)

        coordinator.grantConsent()
        XCTAssertEqual(snapshots.load()?.consent, .granted(version: 1, atMs: 1_700_000_000_000))

        try await coordinator.startCapture(deviceId: "device-1")
        XCTAssertEqual(snapshots.load()?.captureEnabled, true)
        XCTAssertEqual(snapshots.load()?.deviceId, "device-1")
    }

    func testABackgroundRelaunchResumesWithoutPresentingUI() async throws {
        var snapshot = CaptureSnapshot()
        snapshot.setConsent(.granted(version: currentConsentVersion, atMs: 1))
        snapshot.captureEnabled = true
        snapshot.wornAtLastSample = true
        snapshot.deviceId = "device-1"
        let glasses = StubGlasses()
        let coordinator = makeCoordinator(
            glasses: glasses,
            snapshots: MemorySnapshotStore(snapshot)
        )

        let plan = await coordinator.applyLaunch(.bluetoothRestoration)

        XCTAssertFalse(plan.presentUI)
        XCTAssertTrue(coordinator.status.captureEnabled)
        XCTAssertTrue(glasses.callLog.contains("connect"))
        XCTAssertTrue(glasses.callLog.contains("startLocalRecording"))
    }

    func testARelaunchAfterWithdrawnConsentResumesNothing() async {
        var snapshot = CaptureSnapshot()
        snapshot.setConsent(.withdrawn(atMs: 5))
        snapshot.captureEnabled = true
        snapshot.wornAtLastSample = true
        snapshot.deviceId = "device-1"
        let glasses = StubGlasses()
        let coordinator = makeCoordinator(
            glasses: glasses,
            snapshots: MemorySnapshotStore(snapshot)
        )

        _ = await coordinator.applyLaunch(.bluetoothRestoration)

        XCTAssertFalse(glasses.callLog.contains("startLocalRecording"))
        XCTAssertFalse(coordinator.status.captureEnabled)
        XCTAssertEqual(
            coordinator.status.consent, .withdrawn(atMs: 5),
            "a withdrawal must survive the schema, the relaunch and everything else"
        )
    }

    func testARelaunchHoldingTheAccessPointLetsGoOfItFirst() async {
        var snapshot = CaptureSnapshot()
        snapshot.setConsent(.granted(version: currentConsentVersion, atMs: 1))
        snapshot.holdingAccessPoint = true
        let glasses = StubGlasses()
        let coordinator = makeCoordinator(glasses: glasses, snapshots: MemorySnapshotStore(snapshot))

        _ = await coordinator.applyLaunch(.user)

        XCTAssertTrue(glasses.callLog.contains("closeWifiAccessPoint"))
    }

    func testTheAudioDemandReportsWhatIsHoldingTheAppUp() async throws {
        let coordinator = makeCoordinator()
        coordinator.grantConsent()
        try await coordinator.startCapture()

        let demand = coordinator.audioDemand(appInForeground: false)
        XCTAssertEqual(
            AudioSessionPolicy.runtimeSource(demand), .bluetoothCentral,
            "all-day capture is held up by BLE restoration, not by an audio session"
        )
    }
}
