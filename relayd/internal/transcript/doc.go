// Package transcript turns spooled audio into text, and is the second stage of
// SYSTEM.md §10 step 5.
//
// # Speech recognition is ours
//
// SYSTEM.md §7b corrects an earlier claim in this repo and is emphatic about it:
// **the glasses have no recogniser.** `0x0803`/`0x0805` describe the device
// reporting an *event* and the app acting on it — "非 AI 界面：直接进入 AI 对话界面，
// 开始语言识别" — and every verb in those lines belongs to the app.
// `didReceiveAIChatTextMessage` is the vendor's own cloud assistant returning
// text through their app, not a recogniser we inherit. Nothing in the 109 pages
// offers device-side ASR.
//
// So ASR is ours to run and ours to pay for, and §8's stack table says how:
// "**streaming, ours** — phone-native first because it is free and offline;
// cloud STT when a noisy room beats it. Streaming either way, so the prompt is
// ready the moment they stop talking."
//
// Both halves of that are structural here rather than aspirational:
//
//   - [Recognizer] is a **streaming** interface. [Stream.Write] takes audio as
//     it arrives and [Stream.Results] yields partials before the audio ends. A
//     batch recogniser can implement it (and [Batch] adapts one), but it has to
//     admit that it is one — [Capabilities.Streaming] is a field [Batch] forces
//     to false, and [Router] records the cost in its reasons rather than hiding
//     it. SYSTEM.md §7b's
//     third perceived-latency fix is "recognise while they speak so the prompt
//     is ready the instant they stop, rather than starting a 400 ms job at that
//     point".
//   - [SourcePhone] is a first-class source, not an afterthought. The iOS
//     client already recognises on the handset and sends `utterance` text
//     (`apps/ios/RelayKit/CaptureCoordinator.swift`), which is free, offline and
//     private. The box's job there is to *accept* that result, not to re-run
//     recognition it was not asked to pay for.
//
// # The deterministic fake
//
// [Fake] is the local recogniser tests use, and its trick is one sentence:
// **the audio is the text.** Frames carry UTF-8, so a fixture can be written as
// prose, the results are exactly reproducible, and no audio stack is involved.
// It is the same shape as `MockRecognizer` in RelayKit, for the same reason
// that file gives: recognition "is one more thing that cannot run in a unit
// test", so the seam is where the test lives.
//
// # Secrets before anything is written
//
// [Builder] cannot be constructed without a redactor, and the redactor is
// `internal/index`'s measured detector rather than a second ruleset. Someone
// reading an API key aloud is not a hypothetical — MEMORY.md §12.2 measured the
// detector against a corpus for exactly this — and a key that reaches an
// embedding cannot be unembedded. The ordering is enforced by the constructor,
// not by a comment.
//
// # And the audio is destroyed on the way out
//
// [Pipeline] marks a segment transcribed only after a transcript actually
// exists, which is what starts `internal/capture`'s retention clock. A failed
// recognition leaves the audio alone: it is the only copy, and deleting it
// because a model timed out would turn a retryable error into a lost hour of
// somebody's life.
package transcript
