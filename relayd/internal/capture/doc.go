// Package capture is the front door for audio: SYSTEM.md §10 step 5, the
// "capture" half of capture → transcript → episodes.
//
// Audio arrives from the phone over SYSTEM.md §6.1's envelope. Two paths reach
// this package, and APPS-SCOPE.md §3 is explicit that they are different
// products rather than two sizes of the same one:
//
//   - **The live stream** ([Ingester.OpenLive]), opened on a tap or a wake word
//     and closed immediately after. Chunked, ordered, deduped on the envelope
//     `id`, and small — a turn, not a state.
//   - **The nightly bulk sync** ([Bulk]), 170 MB–1.8 GB of the day arriving over
//     the LAN after the phone pulled it off the glasses' access point. Sized for
//     a *file*: a manifest declared up front, 256 KB chunks addressed by index,
//     a received-set that survives a restart, and a content hash per chunk.
//
// The bulk wire contract is not invented here. `connector/src/protocol.ts` is
// the one written-down copy — three implementations have to agree on it, this
// one, the Kotlin client and the Swift client — so [Manifest], [Status],
// [ChunkBytes] and [ErrorCode] mirror it field for field. Where this package
// differs it is in *where the bytes go*: the TypeScript reference holds chunks
// in a map, which is fine for a test double and wrong for 1.8 GB, so here they
// are written at their offset into one file and the received-set is a bitmap.
//
// # Three rules that are enforced rather than documented
//
// **Consent gates capture, not the other way round.** ARCHITECTURE.md §6 is a
// legal requirement in Quebec, not a nicety. [Gate] mirrors the Android
// `ConsentPolicy`/`ConsentGate` that already ships, and [Ingester] refuses a
// stream or a bulk session that consent did not cover — with the reason in
// words — rather than accepting the bytes and filtering later. Filtering later
// means the recording already happened.
//
// **No raw audio after transcription.** SYSTEM.md §5: audio is kept only long
// enough to re-transcribe, then discarded. That is a [Spool] with a retention
// window and a [Spool.Sweep] that deletes the file, not a comment asking
// somebody to. TestSweepActuallyDeletesTheAudio is what holds it.
//
// **Never delete un-transcribed audio.** APPS-SCOPE.md §3.2 states the rule
// from the device end — `0x0911` 清除未上传文件 exists because the firmware tracks
// uploaded from un-uploaded — and this is the same rule from ours. Once the box
// acknowledges a chunk the phone is entitled to free it, so the box is holding
// the only copy until a transcript exists. The sweeper therefore never touches
// a segment that has not been transcribed; a segment that stays untranscribed
// too long is reported as [SweepResult.Stuck] rather than quietly deleted or
// quietly kept forever.
//
// # Degrading visibly
//
// A chunk that never arrives is a hole in someone's day. Splicing the remaining
// frames together produces a transcript that reads as continuous and is not,
// which is worse than an acknowledged hole — `APPS-SCOPE.md` §4.2 settled this
// on the phone side and the same answer applies here. So a missing sequence
// becomes a [Gap] carrying its range and its byte estimate, the gap travels
// with the segment into the transcript, and nothing in this package ever
// reports audio it did not receive.
package capture
