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

**Android**
- Foreground service hosting the SDK, types `connectedDevice` + `microphone`
- `FOREGROUND_SERVICE`, `FOREGROUND_SERVICE_CONNECTED_DEVICE`,
  `FOREGROUND_SERVICE_MICROPHONE`, `POST_NOTIFICATIONS`
- Boot-completed receiver to restart after reboot
- Battery-optimization exemption flow, plus OEM-specific handling — Xiaomi,
  Huawei, Samsung and OnePlus all kill background services in ways stock Android
  does not, and this is a well-known source of "it stopped recording" bugs
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

### 4.3 Transport and backend
- Authenticated streaming channel to the user's box (WebSocket or gRPC)
- Device identity and pairing: glasses ↔ phone ↔ account ↔ box
- Token/key handling for BYO-API-key and BYO-subscription modes
- Push notifications — the agent needs to reach the user, not only be polled

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

### 4.5 Reliability
- Offline behaviour end to end
- Battery telemetry from glasses (`0x0101`) surfaced before the user is stranded
- Crash/restart recovery that does not lose the day's capture

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
