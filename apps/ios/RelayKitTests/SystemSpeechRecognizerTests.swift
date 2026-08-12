#if canImport(Speech)
import AVFoundation
import XCTest
@testable import RelayKit

/// Covers the parts of the real recogniser that do not need a device.
///
/// `SFSpeechRecognizer` itself needs a phone, an authorisation grant and a
/// locale model, so it is not tested here — that is the honest boundary, and it
/// is why ``SpeechRecognizing`` is a protocol with a mock behind it. What *is*
/// testable is the buffer conversion and the partial-selection policy, and both
/// have a failure mode that produces silence rather than an error, which is the
/// worst kind to leave uncovered.
///
/// The policy assertions deliberately mirror `RecognitionTest.kt` on Android.
/// The two implementations are duplicated rather than shared; these tests are
/// what keep them from drifting.
final class SystemSpeechRecognizerTests: XCTestCase {

    private func chunk(_ format: AudioFormat, bytes: Int = 320) -> AudioChunk {
        AudioChunk(
            data: Data(repeating: 0x01, count: bytes),
            format: format,
            sampleRate: 16_000,
            channels: 1,
            sequence: 1,
            deviceTimeMs: 20
        )
    }

    // MARK: - buffers

    func testPcmBecomesABufferTheRecogniserCanRead() throws {
        let buffer = try XCTUnwrap(SystemSpeechRecognizer.pcmBuffer(from: chunk(.pcm16)))
        // 320 bytes at 16-bit mono is 160 frames — 10 ms at 16 kHz.
        XCTAssertEqual(buffer.frameLength, 160)
        XCTAssertEqual(buffer.format.sampleRate, 16_000)
        XCTAssertEqual(buffer.format.channelCount, 1)
    }

    func testOpusIsRefusedRatherThanGuessedAt() {
        // The whole reason this refuses: iOS has no system Opus decoder, and
        // bytes in the wrong encoding do not error — they transcribe as
        // nothing, which is indistinguishable from a quiet room.
        XCTAssertNil(SystemSpeechRecognizer.pcmBuffer(from: chunk(.opus)))
    }

    func testAnEmptyChunkMakesNoBuffer() {
        XCTAssertNil(SystemSpeechRecognizer.pcmBuffer(from: chunk(.pcm16, bytes: 0)))
        // Less than one whole frame is not a frame.
        XCTAssertNil(SystemSpeechRecognizer.pcmBuffer(from: chunk(.pcm16, bytes: 1)))
    }

    // MARK: - policy, mirrored from RecognitionTest.kt

    func testTheLastNonEmptyPartialWinsIncludingAShorterOne() {
        XCTAssertEqual(
            SystemSpeechRecognizer.best(["set the", "set the timer", "set a timer for ten"]),
            "set a timer for ten"
        )
        XCTAssertEqual(SystemSpeechRecognizer.best(["go now please", "go"]), "go")
    }

    func testAnEmptyRevisionNeverBeatsRealText() {
        XCTAssertEqual(SystemSpeechRecognizer.best(["deploy staging", "", "   "]), "deploy staging")
        XCTAssertEqual(SystemSpeechRecognizer.best([]), "")
    }

    func testPartialsAreNotSentTwiceOrOneCharacterAtATime() {
        XCTAssertTrue(SystemSpeechRecognizer.shouldEmit(previous: "", next: "check staging"))
        XCTAssertFalse(SystemSpeechRecognizer.shouldEmit(previous: "check staging", next: "check staging"))
        XCTAssertFalse(SystemSpeechRecognizer.shouldEmit(previous: "check staging", next: " check staging "))
        XCTAssertFalse(SystemSpeechRecognizer.shouldEmit(previous: "", next: "a"))
        XCTAssertFalse(SystemSpeechRecognizer.shouldEmit(previous: "", next: "   "))
    }

    // MARK: - lifecycle

    func testAppendingBeforeStartingIsSafe() {
        // No turn is open, so there is nowhere for audio to go. It must not
        // crash and must not buffer: audio outside a session the user started
        // is the one thing this product cannot do.
        let r = SystemSpeechRecognizer()
        r.append(chunk(.pcm16))
        r.cancel()
    }

    func testFinishingWithoutStartingIsEmptyRatherThanAnError() async {
        let r = SystemSpeechRecognizer()
        let text = await r.finish()
        XCTAssertEqual(text, "")
    }
}
#endif
