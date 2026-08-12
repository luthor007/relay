# Phone apps — what the SDKs give us and what we build

*Scoping pass, 2026-08-08. Read `ARCHITECTURE.md` first — the phone bridge is
the spine of the product, not an accessory, because the glasses have no internet
path of their own.*

---

## 1. What ships in the vendor SDKs

Both platforms come with a working demo app, not just a library. This is a much
better starting position than the brief assumed.

### Android — `glasses/sdk/android/`

`LIB_GLASSES_SDK-release-20260709_8.aar` plus `stabilizer-release.aar`, and a
complete Kotlin sample (`GlassesSDKSample`) with real screens:

| Sample screen | Covers |
|---|---|
| `DeviceBindActivity` | scan, pair, bind |
| `DeviceActivity` | battery, settings, device control commands |
| `MediaActivity` | file list, thumbnails, download, audio callbacks |
| `RealTimePreviewActivity` | RTSP preview via `libvlc-all:3.7.0` |
| `OTAActivity` | firmware update |
| `OpticalStabDemo` | gyro stabilization pipeline |

The SDK internals we care about (`com.glasses.*`): `GlassesControl`,
`WifiP2pManagerSingleton`, `LargeDataHandler`, `AiChatResponse`,
`AudioRawDataResponse`, `GlassesAiVoiceRsp`, `ILargeDataImageResponse`,
`RecordHandle` / `IRecordCallback` / `RecordEntity`.

Deps it drags in: EventBus 3.2, OkHttp 4.9.3, Gson, VLC, XXPermissions.

### iOS — `glasses/sdk/ios/QCSDKDemo/`

Xcode project with five frameworks (`QCSDK`, `QZSDK`, `AWEISGYROSDK`,
`JLAudioUnitKit`, `JLLogHelper`) plus VLCKit, and demo controllers for scanning,
device control, live preview and Opus decoding.

**The hard part of iOS BLE is already done.** `QCCentralManager.m:63` sets
`CBCentralManagerOptionRestoreIdentifierKey` and implements
`centralManager:willRestoreState:` at line 376. That is the mechanism that lets
iOS relaunch a killed app into the background when the glasses reconnect, and it
is the thing most teams get wrong.

`Info.plist` declares `UIBackgroundModes = [bluetooth-central, fetch]`, and the
project sets usage descriptions for Bluetooth and Local Network.

---

## 2. Verified gaps

These are the things that are demonstrably absent, checked rather than assumed.

| Gap | Evidence | Consequence |
|---|---|---|
| **No Android background service** | zero `<service>` in `AndroidManifest.xml`; the sample is Activity-only | capture dies when the app leaves the foreground — i.e. the product does not work |
| **No Android 14 foreground-service permissions** | manifest has `WAKE_LOCK` and `REQUEST_IGNORE_BATTERY_OPTIMIZATIONS` but no `FOREGROUND_SERVICE*` | required for `connectedDevice` + `microphone` service types on API 34+ |
| **No iOS `audio` background mode** | `UIBackgroundModes` is `bluetooth-central` + `fetch` only | `bluetooth-central` keeps BLE alive but gives limited runtime; sustained audio handling in background needs the `audio` mode and an active `AVAudioSession` |
| **No `NSMicrophoneUsageDescription`** | absent from `project.pbxproj` `INFOPLIST_KEY_*` | needed the moment we touch `AVAudioSession` for recording |
| **iOS frameworks are device-only** | `lipo -info` reports bare `arm64` for all five — no simulator slices | **the app cannot run in the iOS Simulator once the SDK is linked** (see §5) |
| No account, backend, or pairing-to-a-box | — | all of it is ours |
| No memory, transcription, or agent integration | — | all of it is ours |

Everything above the line is plumbing the vendor had no reason to build. None of
it is a surprise; it is just unbuilt.

---

## 3. The capture fork — decide this first

The SDK exposes **two different ways to get audio**, and they imply different
products. This is the most consequential decision in the app.

### Path A — live stream

`voiceFromGlasses(pcmData: ByteArray)` with `voiceFromGlassesStatus(status)`.
Audio arrives as it happens.

- Low latency, needed for the interactive voice loop
- Phone and glasses radios are busy continuously
- Heaviest on both batteries

### Path B — on-device record, then transfer

`recordingToPcm(fileName, filePath, duration)`, backed by `RecordHandle` /
`IRecordCallback`, with protocol commands `0x0E04` 本地录音控制 and `0x0E05`
status, and file transfer over `0x0C01`–`0x0C05` or the WiFi AP.

- The glasses record to their own storage; the phone pulls files later
- Far better battery on both ends — no sustained radio
- Transcription is delayed, not live

**Recommendation: both, for different jobs.** Path B is the all-day capture
pipeline — "records your whole day" is a battery problem before it is anything
else, and streaming 16 hours of audio over BLE is the expensive way to solve it.
Path A is the interactive loop, opened only when the user taps or says the wake
word, and closed immediately after.

This also degrades well: if the phone is out of range all day, Path B still has
the day's audio on the glasses to sync later. Path A would simply have lost it.

### 3.1 Storage is not the constraint — transfer is

The glasses have **4 GB**. The protocol does not state the on-device recording
format, so bound it both ways. The SDK ships Opus encoders (`libjl_opus.so`,
`libst_opus.so`, `QCAIChatOpusDecoder`) and the callback is named
`recordingToPcm`, which suggests Opus on the device decoded on arrival — but the
conclusion is the same either way:

| On-device format | Rate | 4 GB holds | One 16 h day |
|---|---|---|---|
| Opus ~24 kbps mono | ~10.8 MB/h | ~370 h (≈23 days) | ~173 MB |
| PCM 16 kHz/16-bit mono | ~115 MB/h | ~35 h (≈2 days) | ~1.84 GB |

Even the pessimistic case holds more than two full days of capture. **Storage
does not gate all-day recording.**

What does gate it is getting the audio *off*. A day's recording over BLE at a
practical ~3 KB/s:

- Opus: ~173 MB → **~16 hours**, longer than the day took to record
- PCM: ~1.84 GB → about a week

**So the daily sync cannot ride BLE.** It has to use the WiFi AP path — `0x090B`
opens the access point, files come over it. At even 2 MB/s that is ~90 seconds
for Opus or ~15 minutes for PCM, both fine unattended.

That makes the sync **a designed ritual, not a background trickle**: while
charging, the phone joins the glasses' AP, pulls the day, rejoins normal WiFi,
then uploads to the box. Two phases, because per `ARCHITECTURE.md` §2.1 the phone
cannot hold the glasses' AP and its own uplink at the same time.

### 3.2 Storage policy is a real requirement

4 GB is shared with photos and video. 1080p video runs roughly 4.5 GB/h, so a
few minutes of video can evict a day of audio. The app therefore needs to:

- Poll disk state (`0x0909` / `0x091C` 获取磁盘容量信息)
- Reserve headroom for audio and warn before video eats it
- Sync-and-free proactively rather than waiting to hit full
- Never delete un-uploaded audio — `0x0911` 清除未上传文件 exists precisely because
  the firmware tracks that distinction

### 3.3 Charging changes the battery story

The glasses **can be used while plugged in**. That matters more than it sounds:
the primary user is a developer at a desk, and desk hours are most of the
capture-worthy day. Plugged means unlimited capture for that window, and battery
only binds while mobile.

Worth verifying rather than assuming, though: the sibling product class the brief
rejected (W610/W630/MT5 Ultra) was documented as "not recommended to record while
charging" for thermal reasons, and HY-16 was a candidate specifically because it
*could*. Confirm that **recording** while charging is supported, and check how
warm it gets over several hours — this sits against the wearer's temple.

---

## 4. Build list

Grouped by layer. Nothing here is provided by the vendor.

### 4.1 Background runtime — the highest-risk work

**Android** — built in `apps/android/relay-bridge/`, **unbuilt by the Android
toolchain**: there is no SDK on the machine and `dl.google.com` is an egress
policy denial, so none of it has been compiled. See that module's README for the
exact commands that would settle it.

- Foreground service hosting the SDK, permissions `FOREGROUND_SERVICE`,
  `FOREGROUND_SERVICE_CONNECTED_DEVICE`, `FOREGROUND_SERVICE_MICROPHONE`,
  `POST_NOTIFICATIONS`
- **Service types, decided.** It runs as `connectedDevice` alone and adds
  `microphone` only while a *phone-side* recorder is open. All-day capture reads
  audio off the glasses over BLE and never opens `AudioRecord`, so a permanent
  microphone chip in the status bar would claim something untrue; and Android
  restricts which foreground service types may be started from `BOOT_COMPLETED`,
  with `microphone` on that list for recent target levels. `dataSync` is
  deliberately not used: it carries a per-day runtime cap, and a capture service
  that stops after N hours is the failure this whole section exists to prevent.
- Boot-completed receiver to restart after reboot. It could never fire before:
  the flag it reads was written by nothing, so capture never came back after a
  reboot. Now written where capture actually starts and stops.
- Battery-optimization exemption flow, plus OEM-specific handling — Xiaomi,
  Huawei, Samsung and OnePlus all kill background services in ways stock Android
  does not, and this is a well-known source of "it stopped recording" bugs.
  `oem/OemPolicy.kt` covers eleven manufacturers and records which of them need
  an *autostart* grant on top of the exemption — where that is true, a boot
  receiver without it is a decoration.
- **Detection, not just advice.** Advice about battery managers is ignored until
  it has already cost someone a day, and the failure is silent. The service
  leaves a heartbeat trail and `oem/CaptureWatchdog.kt` turns a kill into a
  sentence with the specific fix attached. Both are plain Kotlin and tested.
- Reconnect/backoff state machine

**iOS**
- Add the `audio` background mode and an `AVAudioSession` that justifies it
- Extend the existing state-restoration path to restore *capture* state, not just
  the BLE connection
- `NSMicrophoneUsageDescription`
- **App Review narrative** for always-on capture — Mentra ships an approved app
  in this category (AGENT-BRIEF §8-B), so read their client and match the
  justification

### 4.2 Audio pipeline
- Ring buffer with backpressure; never drop silently
- Pass Opus through un-transcoded where possible — re-encoding costs battery and quality
- Chunked, resumable upload with ordering and dedupe
- Store-and-forward for subway/plane; bounded on-disk queue with an eviction policy
- Segment boundaries so the box can transcribe incrementally

**The eviction policy, decided.** Built in `glasses/bridge/src/queue.ts`, and it is
*refuse the newest*, never evict the oldest: for a memory product the queue holds
the only copy of something that already happened, so dropping the oldest silently
loses last Tuesday while a refusal is a state the UI can honestly report. A record
larger than the whole capacity is refused as `tooLarge` rather than `full`,
because it can never fit and retrying it forever blocks everything behind it. A
flush stops at the first failure instead of skipping past it, because the box
segments episodes by time.

Three implementations now exist — `glasses/bridge/src/queue.ts`,
`connector/src/client.ts`, and `apps/android/.../connector/StoreAndForwardQueue.kt`
— and the Kotlin tests are ported verbatim into the TypeScript suite so a change
to one fails the other. Two things the TypeScript one had that the other two
owed — durability across a crash (§4.5), and delivered-id memory so a replayed
sync does not re-upload the day — **are now paid on Android**: `QueueStore` with
a `FileQueueStore` that writes to a temporary file, `fsync`s and renames, and a
delivered-id ring that survives a restart. `connector/src/client.ts` still owes
both.

**Audio segmentation, decided.** `LocalRecordingController` rolls `0x0E04` every
fifteen minutes rather than recording one file per wear session. A sixteen-hour
file cannot be transcribed until the day is over, cannot resume a failed
transfer, and cannot be partially deleted once uploaded. The gap between stop
and start is a real hole of a few hundred milliseconds; it is recorded in the
segment list rather than papered over, because the protocol has no split command
and inventing a seamless one would be a lie the transcript inherits.

**Backpressure, decided.** The ring buffer takes an explicit overflow policy.
Stored capture refuses the newest, matching the queue. The live voice path may
drop the oldest — the useful audio is the sentence being spoken — but every drop
records a **gap** carrying its sequence range and byte count, so the box can mark
the hole. Splicing the remaining frames together silently produces a transcript
that reads as continuous and is not, which is worse than an acknowledged gap.

### 4.3 Transport and backend
- Authenticated streaming channel to the user's box — **WebSocket**, per
  `SYSTEM.md` §6.1, built in `glasses/bridge/src/relayd.ts`. Sleep/wake is the
  normal path: reconnect with jittered backoff, put in-flight envelopes back at
  the *head* of the outbox on an abnormal close, and let the server dedupe on the
  envelope `id`. Bulk upload keeps its own resumable HTTP path
  (`connector/src/protocol.ts`) — the two are complementary, not rivals: control
  traffic is small and bidirectional, uploads are resumable bulk transfer.
- Device identity and pairing: glasses ↔ phone ↔ account ↔ box
- Token/key handling for BYO-API-key and BYO-subscription modes
- Push notifications — the agent needs to reach the user, not only be polled

**The server→phone vocabulary is ten frames, not six.** `SYSTEM.md` §6.1 grew
`ack`, `error`, `notify` and `confirm.resolved` while `relayd/internal/api` was
written, and both clients implement all ten: silently dropping a `notify` is the
quiet-hours behaviour of `ADAPTERS.md` §7 failing with nothing in the log, and a
phone that never sees `error` cannot tell "not stored, M4" from "stored", so it
deletes audio the box never got. Two rules came out of building it:

- **The daemon and the doc own the wire.** The bridge had invented a batched
  `link.ack` (`{ ids: [...] }`) that no daemon sends; `wire.go`'s `{ re, ok }`
  wins. A client that unilaterally speaks a different acknowledgement is a client
  whose queue never drains.
- **An `error` retires the frame it names**, on both clients. A refusal is an
  answer; holding the envelope would redeliver it on every reconnect for the life
  of the queue, and `not_implemented` does not become implemented because a phone
  asked again.

`glasses/bridge/test/relayd.test.ts` re-parses §6.1, `RelaydLink.swift` and
`wire.go` on every run, so doc, iOS and daemon cannot drift apart quietly — and
it is the only check the Swift gets, since no machine here can compile it.

**Pairing needs a PAKE, and that is a per-platform dependency.** `SYSTEM.md` §7
puts our rendezvous relay between the phone and the box, so "the relay cannot read
it" has to be true against *us*. A short code that authenticates a key exchange is
safe; a short code that travels over one is not — a 30-bit secret recovered offline
from a captured transcript falls in seconds. `glasses/bridge/src/pairing.ts`
therefore implements everything around the primitive (nine-character Crockford
base32 code, single-use, ten-minute expiry, three counted attempts, transcript
binding, key confirmation, and a credential *derived* rather than transmitted) and
leaves the group operations behind a `PakeEngine` interface, because `src/` carries
no dependencies and no Node built-ins.

**This is a build blocker for pairing on device and should not be discovered
during integration.** The rest of this section is what each platform binds, so
that it is not discovered then.

#### The primitive

**CPace** (CFRG `draft-irtf-cfrg-cpace`, ristretto255 suite) or **SPAKE2**
(RFC 9382), from a reviewed implementation, pinned by version. Both are balanced
PAKEs, which is what this protocol is: the box printed the code and the phone
typed it, so the two sides hold the same secret. Not hand-rolled, on any of the
three sides — a PAKE built out of a group library is a PAKE nobody reviewed.

| Side | Binds | Note |
|---|---|---|
| iOS | a C CPace/SPAKE2 over ristretto255 — jedisct1's `libcpace` (on libsodium), or BoringSSL's `SPAKE2_CTX` — as a SwiftPM system-library target, wrapped in a type conforming to `PakeEngine` | **CryptoKit cannot do this.** It exposes no ristretto255 group and no hash-to-group, so there is nothing to build one out of |
| Android | the same C library through JNI, or the RustCrypto `spake2` crate (magic-wormhole's) through UniFFI | neither the JCA nor Tink ships a PAKE |
| box (`relayd`) | its own binding, of the **same** primitive and suite | mismatched suites derive different keys, and the failure surfaces as "confirmation failed" with no cause |

Candidates, not decisions: confirm each against the library's current API and
record the choice here once made.

#### What the interface demands of it

Stated in full at `PakeEngine` in `pairing.ts`; the load-bearing ones:

- The secret is the six-character code as UTF-8 — **low entropy by design**, fed
  as the password input, never as a key.
- `associatedData` must be bound into the run (CPace's `AD`; SPAKE2's identities
  plus context). A library with no such parameter must have it hashed into the
  password input, and the binding site must say so — unbound, the transcript
  binding stops binding and a relay can splice two runs together.
- `finish` rejects malformed, identity and low-order peer elements, and returns
  the 32-byte ISK rather than a raw group element.
- One session is **one online guess**. 30 bits is safe only because
  `PairingHost` counts wrong confirmations and locks at three; an engine that
  lets a caller test several secrets against one transcript deletes the counting.
- No fallback. An engine that degrades when a library is missing is the bug this
  seam exists to prevent.

#### Until then

`unsafeTestOnlyPake` exercises the state machine and is **not cryptography** — an
observer holding its transcript recovers the code offline, which is precisely the
property a real PAKE provides, and `test/pairing.test.ts` demonstrates the
recovery rather than asserting it away. Four controls keep it out of a build: the
name; a required `{ iUnderstandThisIsNotCryptography: true }` argument; a runtime
throw when the environment says production (`NODE_ENV`, `RELAY_ENV`, React
Native's `__DEV__ === false`); and no default engine on `PairingHost` or
`PairingClient`, so nothing falls back to it by omission.

**One more thing to know about that file:** it hand-writes SHA-256, HMAC-SHA-256
and HKDF, because `src/` may not import `node:crypto` (§5.1) and React Native has
no WebCrypto. They are pinned against the published vectors *and* differentially
against `node:crypto` — the test has no such import restriction — and the file
says so at the top. They are the executable reference the two platforms agree
with, not the shipping cipher path: `apps/ios/RelayKit` uses CryptoKit and
`apps/android/…/ConnectorClient.kt` uses `MessageDigest` / `javax.crypto.Mac`.

### 4.4 Product surface
- Onboarding: BLE pair, account, box provisioning, model-access choice
- **Consent and recording state machine** with a visible indicator — see
  `ARCHITECTURE.md` §6; this is a legal requirement in Quebec, not a nicety
- Wear-detect gating: start on wear, stop on removal
- Voice loop: tap or wake word → utterance → agent → streamed response to the speaker
- Session control: list and attach to running agent sessions
- Approvals: an agent that wants to run something dangerous has to be able to ask
- Memory UI: ask questions, browse notes, review extracted commitments
- Connector management, including the suggestion prompts

**Consent as a decision function, not a dialog.** `consent/ConsentPolicy.kt` on
Android turns §6 into something testable: session scope covers one conversation,
a new or *unknown* place asks before recording, and "always on" still asks when
an unfamiliar voice appears — because always-on is the wearer's consent and
cannot be the other person's. Unknown defaults to asking; defaulting it to allow
would make the rule decorative, and "new voices" is a signal the box provides
rather than something the phone can determine.

**The approval queue has three properties the dialog cannot have**, so they live
in `session/`: an action can be answered once (a replayed envelope or a double
tap must not approve twice), a request expires rather than lingering, and expiry
*denies* — answering after the box has timed out is how something runs twice.

**Every glasses command by hand is generated, not written.** The Android command
screen is built from a catalog of all 92 ids; there is no per-command UI code to
forget. Commands the spec retired or marked unused are shown and refused with
the reason rather than hidden, because a missing row reads as a bug in the app
while "已弃用" is the truth about the device.

### 4.5 Reliability
- Offline behaviour end to end
- Battery telemetry from glasses (`0x0101`) surfaced before the user is stranded
- Crash/restart recovery that does not lose the day's capture

Crash recovery is the queue's problem and is solved there: `QueueStore.append`
must not resolve until the bytes are durable, so a caller may delete its source
the moment it does, and a restart reloads pending records in their original order.
The platform stores are the unbuilt half — files under `NSFileManager` and
`Context.filesDir`, one per record plus an index. `MemoryQueueStore` is the test
double and loses everything, which is the point.

**The sync ritual is a state machine, not an `await`.** `BulkSync` in the same
file models §3.1's two phases explicitly, because the interesting states are the
ones where nothing is transferring: waiting for the LAN, holding the glasses' AP
with no uplink, and rejoining. If the box is only reachable through the relay,
bulk sync *defers and says why* rather than spending a gigabyte of someone's data
plan — `SYSTEM.md` §7. It never deletes anything from the glasses.

---

## 5. The simulator problem

All five iOS frameworks are `arm64` device-only. Once linked, **the app will not
build or run in the iOS Simulator.** Two consequences:

1. Every iteration needs a physical iPhone *and* the glasses. The glasses are in
   Quebec, so this blocks most iOS work right now.
2. It breaks the Maestro-on-simulator pipeline used for the Jappuie apps.

**Fix: a transport abstraction with a mock behind it.** Define the bridge's
interface (connect, subscribe, send command, receive audio, receive photo), put
the vendor SDK behind it on device, and a mock behind it in simulator that
replays canned frames. Then:

- The whole product surface — onboarding, memory, sessions, connectors — is
  developed and UI-tested in the simulator with no hardware at all
- The Maestro pipeline keeps working
- `glasses/protocol/` (already built and tested) is the reference for what the
  mock should emit, so the fake frames are protocol-accurate rather than invented

This is the single highest-leverage thing to build first: it unblocks everything
that isn't literally BLE, while the hardware is 4,000 km away.

### 5.1 What the abstraction covers today

`glasses/bridge` is that seam, in TypeScript, with no dependencies and no React
Native imports, so it runs in the app, in a browser and under `node --test`
unchanged.

- **`src/commands.ts`** — all 92 command IDs, generated from
  `glasses/protocol/commands.py` rather than transcribed, plus the payload
  vocabularies the shipping SDK headers attest (`QCOperatorDeviceMode`,
  `QGAISpeakMode`, `QGSpeakerPlaybackStatus`, `QGAIImageSharpnessLevel`), a
  CRC-16/MODBUS frame codec, and `COMMAND_CATALOG` mapping every command to the
  method or event that reaches it. A test re-parses `commands.py` on every run,
  so the two cannot drift silently.
- **`src/mock.ts`** — the whole surface, driveable with no hardware, on a fake
  clock. It honours two rules that a convenient mock quietly breaks: rates are
  the ones in `SYSTEM.md` §3.1 (mic ~3 KB/s each way, battery ~1/min, video
  ~4.5 GB/h), and **absent capabilities fail** — no station mode, no device ASR,
  no wake phrase outside the firmware list.
- **`fixtures/*.trace.json`** — replayable sessions in the `tools/capture_trace.py`
  schema, carrying real encoded wire frames alongside the decoded events, so a
  fixture that drifts out of protocol fails a test instead of teaching the app
  something false.

**What is still unattested** and must be checked against a capture: the byte
layout of most response payloads. The command IDs, the framing, the CRC and the
enumerated control bytes are attested; the *shape* of a disk-info or file-list
reply is not, so those methods take typed objects and the native adapter owns the
decoding. Nothing in `commands.ts` presents a guessed layout as a fact.

**Both phone apps drive this and only this.** That is what makes the command
surface testable today on a Linux box with no Android SDK and no Xcode.

### 5.2 The same trick, on Android

The Simulator problem has an Android twin, and it is worse in one way: the iOS
frameworks at least *exist* on the machine, whereas the Android Gradle Plugin
lives on `dl.google.com` and is an egress-policy denial here. Nothing in
`apps/android/` can be compiled at all.

The answer is the same shape. Everything that does not need an Android API lives
in a subpackage of `glass.relay.bridge` with JUnit tests, and
`apps/android/tools/verify-jvm-logic.sh` compiles exactly that set with `kotlinc`
from Maven Central and runs it on a plain JVM — 156 tests today. That covers the
protocol codec, the command catalog and console, both capture paths, the audio
ring, the upload plan, the storage policy, the consent rules, the OEM watchdog,
the session and approval surface and the durable queue.

Two consequences worth stating, because they changed decisions rather than just
file layout:

- **The command surface is a codec, not sixty SDK calls.** `protocol/` is a
  Kotlin port of `glasses/protocol/{crc,frame,commands}.py`, pinned against
  vectors generated from that Python, so the app builds its own frames and the
  vendor adapter needs one `sendFrame` method. Sixty typed SDK methods would be
  sixty things unverifiable until hardware arrives; one call plus a codec with
  test vectors is not.
- **The drift guards are cross-language.** The Android catalog's tests re-parse
  `glasses/protocol/commands.py` for ids and
  `glasses/bridge/src/commands.ts` for roles and destructive flags. Two apps
  disagreeing about whether `0x0911` destroys data is not a difference of
  opinion.

What the harness cannot see is the whole point of naming it: the manifest,
resources, Compose, and every file that imports `android.*`. Those remain
unverified until a machine has an SDK.

---

## 6. Framework decision

The critical paths — background BLE, foreground services, audio — are native on
both platforms regardless of what we choose. The product surface is ordinary app
UI. That argues for:

**React Native shell + thin native modules per platform.**

- You already ship React Native (the Jappuie apps), so the shell is familiar ground
- Native code is confined to where it is unavoidable: the SDK wrapper, the
  Android foreground service, the iOS background session
- One implementation of onboarding, memory, sessions, connectors

The honest counter-argument: if the app is *mostly* background audio plumbing,
the RN layer earns less. But the roadmap in §4.4 is a real app's worth of screens,
so it earns its place.

Alternative worth naming: Kotlin Multiplatform shares business logic with native
UI on both sides. Better ceiling, more setup, and less aligned with the tooling
you already run.

---

## 7. Sequence

1. **Transport interface + mock** (§5) — unblocks everything else, needs no hardware
2. **Android foreground service** — Android is the faster platform to get an
   always-on capture loop proven on, with no App Review in the way
3. **Capture pipeline** against the mock, then against real glasses
4. **Backend channel + box pairing** — parallel with 2–3, different skill set
5. **Product surface** in RN against the mock
6. **iOS background audio** — hardest review risk; start the App Review narrative
   early, ship it after Android proves the loop
7. **Measure on hardware** the moment the glasses are reachable: battery under
   Path A vs Path B, local storage capacity, real BLE throughput

Steps 1, 4 and 5 need no glasses. Steps 2, 3, 6 and 7 do.

---

## 8. Open questions

| Question | Blocks |
|---|---|
| ~~Local storage capacity~~ | **answered: 4 GB — not the constraint (§3.1)** |
| ~~Can it record while charging?~~ | **answered: yes — verify thermals over hours (§3.3)** |
| On-device recording format — Opus or PCM? | sync duration and storage headroom; changes §3.1 by ~10× |
| Real WiFi-AP transfer throughput | how long the nightly sync takes |
| Battery life while recording **unplugged** | mobile capture window |
| Is `voiceFromGlasses` continuous, or gated to an AI-chat session? | whether Path A can do passive capture at all |
| Hardware recording indicator, and can it be driven? | consent design, legal viability |
| iOS App Review appetite for always-on capture | ship date for iOS |
| FCC ID from the supplier | US resale |

Everything above the last two needs a unit in hand. The recording format is the
cheapest to answer and has the largest effect — one capture, then look at the
file size and header.
