#if canImport(Speech)
import AVFoundation
import Foundation
import Speech

/// The real recogniser, on the phone's own speech stack.
///
/// The mirror of Android's `AndroidSpeechRecognizer`, and the two share a seam
/// on purpose (`glass.relay.bridge.capture.SpeechRecognizing`): one set of
/// decisions about what a turn is, with the platform differences kept inside
/// the implementations. The differences are real and both are worth knowing.
///
/// ### iOS has the easier half of this
///
/// `SFSpeechAudioBufferRecognitionRequest` exists precisely to be fed audio the
/// app already has, so there is no equivalent of Android's API-33 pipe
/// requirement and no version floor beyond `SFSpeechRecognizer` itself. Feeding
/// it is `append(_:)` with an `AVAudioPCMBuffer`.
///
/// ### iOS has the harder half
///
/// **There is no system Opus decoder.** `AVAudioConverter` handles the codecs
/// in Core Audio, and Opus is not one of them — on Android the platform
/// `MediaCodec` decodes it and there is nothing here that does. The glasses
/// stream Opus. So one of three things has to be true before this works on
/// hardware, and picking between them is a decision for whoever has the device:
///
///   1. the firmware is asked for `.pcm16` on the live path (`Path A` is a
///      turn, not a state, so the bandwidth cost is bounded — this is the
///      cheapest answer if the firmware allows it);
///   2. libopus is vendored into the app;
///   3. the audio is decoded on the box rather than the phone, which moves the
///      cost to the machine with mains power and breaks the rule that
///      recognition is the phone's job.
///
/// Until then ``append(_:)`` refuses Opus and says so once per turn, rather
/// than handing the recogniser bytes it cannot read — which does not error, it
/// returns an empty transcript, and that is indistinguishable from a quiet
/// room.
///
/// ### On-device where possible
///
/// `requiresOnDeviceRecognition` is set when the locale supports it.
/// `PRODUCT.md`'s promise is that the audio is the user's; shipping every
/// utterance to a server by default is a different product. Where no on-device
/// model exists the recogniser falls back, and the caller is told through
/// ``isOnDevice``.
///
/// Untested against a device: there is no Xcode in the environment this was
/// written in.
public final class SystemSpeechRecognizer: SpeechRecognizing, @unchecked Sendable {

    private let lock = NSLock()
    private let recognizer: SFSpeechRecognizer?
    private let onPartial: @Sendable (String) -> Void

    private var request: SFSpeechAudioBufferRecognitionRequest?
    private var task: SFSpeechRecognitionTask?
    private var partials: [String] = []
    private var lastEmitted = ""
    private var finished: CheckedContinuation<String, Never>?
    private var refusedChunks = 0

    /// True when this turn will not leave the device.
    public private(set) var isOnDevice = false

    public init(
        locale: Locale = .current,
        onPartial: @escaping @Sendable (String) -> Void = { _ in }
    ) {
        self.recognizer = SFSpeechRecognizer(locale: locale)
        self.onPartial = onPartial
    }

    /// Whether recognition can run at all right now.
    ///
    /// Authorisation is deliberately not requested here. Asking for a
    /// permission at the moment somebody taps the glasses puts a modal between
    /// them and the thing they asked for; `PermissionsModel` asks up front, and
    /// this reports.
    public var isAvailable: Bool {
        guard let recognizer else { return false }
        return recognizer.isAvailable &&
            SFSpeechRecognizer.authorizationStatus() == .authorized
    }

    public func start() throws {
        guard let recognizer, isAvailable else {
            throw RecognizerError.unavailable
        }
        cancel()

        let request = SFSpeechAudioBufferRecognitionRequest()
        request.shouldReportPartialResults = true
        // The audio is the user's. See the type comment.
        if recognizer.supportsOnDeviceRecognition {
            request.requiresOnDeviceRecognition = true
        }

        lock.lock()
        self.request = request
        self.isOnDevice = recognizer.supportsOnDeviceRecognition
        partials = []
        lastEmitted = ""
        refusedChunks = 0
        lock.unlock()

        task = recognizer.recognitionTask(with: request) { [weak self] result, error in
            guard let self else { return }
            if let result {
                self.handle(result)
            }
            if error != nil || result?.isFinal == true {
                self.complete()
            }
        }
    }

    private func handle(_ result: SFSpeechRecognitionResult) {
        let text = result.bestTranscription.formattedString
        lock.lock()
        partials.append(text)
        let best = Self.best(partials)
        let emit = Self.shouldEmit(previous: lastEmitted, next: best)
        if emit { lastEmitted = best }
        lock.unlock()

        if emit { onPartial(best) }
    }

    public func append(_ chunk: AudioChunk) {
        lock.lock()
        let request = self.request
        lock.unlock()
        guard let request else { return }

        guard let buffer = Self.pcmBuffer(from: chunk) else {
            lock.lock()
            refusedChunks += 1
            let first = refusedChunks == 1
            lock.unlock()
            if first {
                // Once per turn, not once per chunk: at 20 ms frames a refusal
                // per chunk is fifty log lines a second. Once is enough to find
                // it, and the symptom — an empty transcript — is what sends
                // anyone looking.
                NSLog("RelayASR: refusing %@ audio — no decoder on iOS for it; see SystemSpeechRecognizer",
                      String(describing: chunk.format))
            }
            return
        }
        request.append(buffer)
    }

    public func finish() async -> String {
        lock.lock()
        let request = self.request
        lock.unlock()
        guard request != nil else { return "" }

        // Ending the audio is how the task learns the utterance is over.
        request?.endAudio()

        let text = await withCheckedContinuation { (c: CheckedContinuation<String, Never>) in
            lock.lock()
            if self.request == nil {
                // Already completed between endAudio and here.
                let done = Self.best(partials)
                lock.unlock()
                c.resume(returning: done)
                return
            }
            finished = c
            lock.unlock()
        }
        release()
        return text.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    public func cancel() {
        lock.lock()
        let c = finished
        finished = nil
        lock.unlock()

        task?.cancel()
        c?.resume(returning: "")
        release()
    }

    private func complete() {
        lock.lock()
        let c = finished
        finished = nil
        let text = Self.best(partials)
        lock.unlock()
        c?.resume(returning: text)
    }

    private func release() {
        lock.lock()
        request = nil
        lock.unlock()
        task = nil
    }

    // MARK: - shared policy
    //
    // These two mirror `RecognitionPolicy` on Android, and the reasoning is
    // there in full. Duplicated rather than abstracted because the alternative
    // is a shared module for eleven lines of logic, and because the tests on
    // both sides are what keep them honest.

    /// The last non-empty partial wins, including a shorter one: a recogniser
    /// that shortens has usually dropped a misheard word. An empty revision
    /// never beats real text — that is a reset, not a correction.
    static func best(_ partials: [String]) -> String {
        var kept = ""
        for p in partials {
            let t = p.trimmingCharacters(in: .whitespacesAndNewlines)
            if !t.isEmpty { kept = t }
        }
        return kept
    }

    /// Not the same text twice, and not a single character.
    static func shouldEmit(previous: String, next: String) -> Bool {
        let n = next.trimmingCharacters(in: .whitespacesAndNewlines)
        return !n.isEmpty &&
            n != previous.trimmingCharacters(in: .whitespacesAndNewlines) &&
            n.count > 1
    }

    /// Wraps a PCM chunk for the recogniser, or nil when it is not PCM.
    ///
    /// Refusing rather than guessing, for the reason in the type comment: bytes
    /// in the wrong encoding do not produce an error, they produce silence.
    static func pcmBuffer(from chunk: AudioChunk) -> AVAudioPCMBuffer? {
        guard chunk.format == .pcm16 else { return nil }
        guard let format = AVAudioFormat(
            commonFormat: .pcmFormatInt16,
            sampleRate: Double(chunk.sampleRate),
            channels: AVAudioChannelCount(chunk.channels),
            interleaved: true
        ) else { return nil }

        let bytesPerFrame = 2 * chunk.channels
        let frames = chunk.data.count / bytesPerFrame
        guard frames > 0,
              let buffer = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: AVAudioFrameCount(frames))
        else { return nil }

        buffer.frameLength = AVAudioFrameCount(frames)
        guard let dest = buffer.int16ChannelData else { return nil }
        chunk.data.withUnsafeBytes { raw in
            guard let base = raw.baseAddress else { return }
            memcpy(dest[0], base, frames * bytesPerFrame)
        }
        return buffer
    }

    public enum RecognizerError: Error {
        /// No recogniser for the locale, or the user has not authorised one.
        case unavailable
    }
}
#endif
