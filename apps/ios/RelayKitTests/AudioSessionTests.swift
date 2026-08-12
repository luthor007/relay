import Foundation
import XCTest
@testable import RelayKit

/// The `audio` background mode is the single largest App Review risk in the
/// project, and the thing that gets apps rejected is declaring it to stay
/// resident. These tests pin the policy that keeps us honest.
final class AudioSessionPolicyTests: XCTestCase {

    func testAVoiceTurnActivatesPlayAndRecord() {
        let decision = AudioSessionPolicy.decide(AudioDemand(voiceTurnOpen: true))
        guard case let .activate(configuration, reason) = decision else {
            return XCTFail("a live microphone must hold a session")
        }
        XCTAssertEqual(configuration.category, .playAndRecord)
        XCTAssertEqual(configuration.mode, .voiceChat)
        XCTAssertEqual(reason, .voiceTurn)
    }

    func testASpokenReplyActivatesPlaybackOnly() {
        let decision = AudioSessionPolicy.decide(AudioDemand(replyPlaying: true))
        guard case let .activate(configuration, reason) = decision else {
            return XCTFail("a reply must hold a session")
        }
        XCTAssertEqual(
            configuration.category, .playback,
            "asking for a microphone we are not using is a permission prompt for nothing"
        )
        XCTAssertEqual(reason, .spokenReply)
    }

    func testCaptureBeingOnIsNotByItselfAReasonToHoldASession() {
        // This is the whole rejection risk, as one assertion. iOS suspends an
        // app that holds an active session and plays nothing, so the pattern
        // does not even work — it just looks like it might.
        let decision = AudioSessionPolicy.decide(
            AudioDemand(captureEnabled: true, localRecording: true, appInForeground: false)
        )
        XCTAssertEqual(decision, .deactivate)
    }

    func testAllDayCaptureIsHeldUpByBluetoothNotByAudio() {
        let demand = AudioDemand(captureEnabled: true, localRecording: true, appInForeground: false)
        XCTAssertEqual(AudioSessionPolicy.runtimeSource(demand), .bluetoothCentral)
        XCTAssertTrue(AudioSessionPolicy.explain(demand).contains("their own storage"))
    }

    func testAVoiceTurnInTheBackgroundIsHeldUpByTheAudioSession() {
        let demand = AudioDemand(voiceTurnOpen: true, captureEnabled: true, appInForeground: false)
        XCTAssertEqual(AudioSessionPolicy.runtimeSource(demand), .audioSession)
    }

    func testWithNothingHappeningNothingKeepsUsAlive() {
        let demand = AudioDemand(appInForeground: false)
        XCTAssertEqual(
            AudioSessionPolicy.runtimeSource(demand), .none,
            "being suspended here is correct, and the UI should not imply otherwise"
        )
    }

    func testTheForegroundCaseIsNamedRatherThanInferred() {
        XCTAssertEqual(
            AudioSessionPolicy.runtimeSource(AudioDemand(appInForeground: true)),
            .foreground
        )
    }
}

final class MockAudioSessionTests: XCTestCase {

    func testActivationIsRecordedWithItsConfiguration() throws {
        let audio = MockAudioSession()
        try audio.activate(.voiceTurn)

        XCTAssertTrue(audio.isActive)
        XCTAssertEqual(audio.activations.count, 1)
        XCTAssertEqual(audio.activations.first?.configuration, .voiceTurn)
    }

    func testActivationCanFailBecauseItDoesOnAPhone() {
        let audio = MockAudioSession()
        audio.failNextActivation()

        XCTAssertThrowsError(try audio.activate(.voiceTurn)) { error in
            XCTAssertEqual(
                error as? AudioSessionError,
                .activationFailed("mock: activation refused")
            )
        }
        XCTAssertFalse(audio.isActive)
    }

    func testAnInterruptionTakesTheSessionAndTellsEveryone() {
        let audio = MockAudioSession()
        let seen = Locked([AudioInterruption]())
        let token = audio.onInterruption { interruption in seen.mutate { $0.append(interruption) } }
        defer { token.unsubscribe() }

        try? audio.activate(.voiceTurn)
        audio.interrupt(.began)

        XCTAssertFalse(audio.isActive, "a call takes the session whether we like it or not")
        XCTAssertEqual(seen.get(), [.began])
    }

    func testUnsubscribingStopsInterruptionDelivery() {
        let audio = MockAudioSession()
        let count = Locked(0)
        let token = audio.onInterruption { _ in count.mutate { $0 += 1 } }

        audio.interrupt(.began)
        XCTAssertEqual(count.get(), 1)

        token.unsubscribe()
        audio.interrupt(.ended(shouldResume: true))
        XCTAssertEqual(count.get(), 1)
    }
}
