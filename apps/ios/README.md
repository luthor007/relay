# iOS — `Relay`

```
xcodegen generate
xcodebuild test  -scheme RelayKit -destination 'platform=iOS Simulator,name=iPhone 17' \
                 -derivedDataPath .build
xcodebuild build -scheme Relay    -destination 'platform=iOS Simulator,name=iPhone 17' \
                 -derivedDataPath .build
```

`-derivedDataPath` is not optional here. This machine's Xcode preference points
at a DerivedData directory that does not exist, and `xcodebuild` fails with a
path error that reads like a project problem. Pass it explicitly, and `.build/`
is already gitignored.

## Status — none of this has been compiled

Read this part first, because it is the honest bit.

**No Swift in this repository has ever been through a Swift compiler.** The
build needs macOS and Xcode; the machine that wrote it is a Linux container.
That is a different and weaker claim than the Android module can make, which at
least compiles. Everything below describes what the code is *meant* to do, and
the first hour on a Mac should be budgeted for the ordinary consequences of
that: a wrong overload, an actor-isolation complaint, a `Sendable` warning
promoted to an error.

What that first hour is *not* likely to be spent on is design, because the
design is in `RelayKit` and `RelayKit` is where every rule has a test.

| Layer | Verifiable on a Mac with no hardware | Needs an iPhone | Needs the glasses |
|---|---|---|---|
| `RelayKit/` + `RelayKitTests/` | **all of it** | — | — |
| `Relay/` SwiftUI screens | builds, runs in the Simulator | — | — |
| `Relay/HotspotSyncNetwork.swift` | builds only | joining a hotspot | the AP itself |
| `RelayKit/SystemAudioSession.swift` | builds only | interruptions, routes | — |
| `VendorTransport` | **does not exist yet** | yes | yes |

## The seam, and why it is not optional here

The five vendor frameworks are **arm64 device-only** (`lipo -info` on all of
them). Linking them removes the Simulator as a target entirely — no unit tests
on a laptop, no CI without a device farm. `RelayKit` exists so the whole product
surface can be built and tested without them, which is the argument
`docs/APPS-SCOPE.md` §5 makes.

The discipline that follows: **anything that decides something lives in
`RelayKit`.** `Relay/` is SwiftUI and OS glue. If a rule is in `Relay/`, it has
escaped the only suite that runs without a phone.

```
RelayKit/
  Transport.swift          the protocol — 21 methods, mirrors glasses/bridge
  MockTransport.swift      the glasses, without the glasses
  Clock.swift              deterministic time
  Types.swift              domain types, ported from types.ts
  CommandCatalog.swift     every glasses command, as data the UI is generated from
  CaptureCoordinator.swift consent gate, wear gating, Path A and Path B
  AudioSession.swift       when to hold an AVAudioSession, and why
  SystemAudioSession.swift the AVAudioSession itself — translation only
  Restoration.swift        what a background relaunch should do
  StoragePolicy.swift      the 4 GB, and the audio reserve video may not take
  Queue.swift              store-and-forward, durable
  BulkSync.swift           the two-phase WiFi ritual
  RelaydLink.swift         SYSTEM.md §6.1 — the authenticated WebSocket
  Sessions.swift           session list, attach, approvals
  Connector.swift          resumable bulk upload to the box
Relay/
  RelayApp.swift           composition root, AppDelegate, tabs
  CaptureModel.swift       @MainActor republishing and nothing else
  Views.swift / Screens.swift
  PermissionsModel.swift
  HotspotSyncNetwork.swift NEHotspotConfiguration — the untestable edge
```

## The two capture paths

Same fork as Android (`docs/APPS-SCOPE.md` §3), and both live in
`CaptureCoordinator`.

**Path A — live stream.** Opened on a tap or a wake word, closed immediately
after. It is a *turn*, not a state: it costs both radios and both batteries, and
`beginVoiceTurn` brings the audio session up before the glasses start streaming
so a reply always has somewhere to go. A phone call closes the turn rather than
leaving the glasses streaming into a session we no longer hold.

Recognition happens on the phone. The glasses are a microphone and a button —
`0x0803`/`0x0805` report that something was said and no command anywhere asks
the device to transcribe — so `SpeechRecognizing` is a protocol with
`SFSpeechRecognizer` behind it on device and `MockRecognizer` in tests.

**Path B — record on the glasses, transfer later.** The all-day path, gated on
wear and on consent. This is what survives the phone being out of range, in a
pocket, or dead, and it is why the recording indicator never says "not
recording" merely because the link dropped.

## Background execution, honestly

This is the part most designs get wrong, so it is written down in
`AudioSession.swift` and asserted in `AudioSessionPolicyTests`.

**An active audio session does not keep an iOS app running.** iOS grants
background execution under the `audio` mode while the app is actually producing
or consuming audio. A session that is merely active and silent gets the app
suspended — and an app that declares `audio` and never makes any is exactly the
rejection the review guidelines describe.

So the two modes buy different things:

| Background mode | What it buys | What it does not |
|---|---|---|
| `audio` | the interactive loop: a voice turn and a spoken reply, in the background, uninterrupted | all-day residency |
| `bluetooth-central` + state restoration | relaunch when the glasses reconnect, with limited runtime per event | continuous execution |
| `processing` (`BGProcessingTask`) | the nightly upload while charging | the WiFi-AP phase (see below) |

`AudioSessionPolicy.runtimeSource(_:)` names which one is holding the app up at
any moment, and `explain(_:)` turns it into the sentence the status screen
shows. The case that matters — capture on, nothing audible, app backgrounded —
returns `.bluetoothCentral`, not `.audioSession`, and there is a test for it.

## State restoration

The vendor already does the hard half: `QCCentralManager.m:63` sets
`CBCentralManagerOptionRestoreIdentifierKey` and line 376 implements
`centralManager:willRestoreState:`. That is what makes iOS relaunch a terminated
app into the background when the glasses reconnect.

What it restores is a `CBCentralManager` and its peripherals. It knows nothing
about consent, about whether capture was on, or about a recording that was
running when we died — so a background relaunch produces an app that is
connected to the glasses and does nothing, which looks exactly like working.

`Restoration.swift` closes that gap:

- `CaptureSnapshot` is written on **every** state change, to a small JSON file
  rather than `UserDefaults` (whose writes are coalesced with no flush a caller
  can wait on, and a CoreBluetooth relaunch can be over in under a second).
- `Restoration.plan(for:launch:)` is pure, so the branches are tests rather than
  something you reproduce by getting an app killed on a physical phone.
- **Consent outranks everything.** A relaunch triggered by the glasses
  reconnecting does not restart capture for someone who turned it off. That is
  the one failure here with a legal consequence.
- An unreadable snapshot is discarded, not migrated — but the consent record
  survives even then, because forgetting a withdrawal is the worst possible
  thing to lose to a schema change.
- `LaunchKind.bluetoothRestoration` means `presentUI: false`. There is no
  window.

`AppDelegate` exists solely to see
`UIApplication.LaunchOptionsKey.bluetoothCentrals`, which SwiftUI's scene phases
never surface.

## The nightly sync, and why it is two phases

A day of Opus is ~173 MB. Over BLE at ~3 KB/s that is ~16 hours — longer than
the day took to record. So it goes over the glasses' own access point, and the
phone cannot hold that and its own uplink at once (`docs/ARCHITECTURE.md` §2.1).
`BulkSync` models it as a state machine because the interesting states are the
ones where nothing is transferring.

iOS adds one deferral the other platforms do not have.
`NEHotspotConfigurationManager.apply` puts up a system dialog — "Relay wants to
join QCGlasses-XXXX" — that the user has to accept. There is no dialog at 3am,
so **the AP phase cannot run from a `BGProcessingTask`.** `BulkSync` reports
that as `SyncDeferral.needsForeground` rather than hanging until iOS kills the
task; the background window runs the upload half, which needs no dialog.

Entitlement required: `com.apple.developer.networking.HotspotConfiguration`, in
`Relay/Relay.entitlements`. It is not granted by a personal team — it has to be
enabled on the App ID, and a stale provisioning profile fails to install with a
message that does not mention hotspots.

## Storage policy

`StoragePolicy` implements `docs/APPS-SCOPE.md` §3.2 against the number that
drives it: 1080p video is ~4.5 GB/h and Opus is ~10.8 MB/h, so **video is about
four hundred times more expensive per second**. Forty minutes of video is a
whole day of conversation.

- A day of audio is reserved and video is refused below it — *before* the
  shutter, via `videoWouldBreachReserve(seconds:disk:)`, not after.
- `safeToDelete` returns only files the **device** says are uploaded. The
  firmware tracks it (`0x0911` is "clear un-uploaded files"), and our own
  bookkeeping is exactly what a crash loses.
- `StorageMonitor` polls, because `0x0909`/`0x091C` are commands and not
  notifications, and flags only *changes* of level — a warning repeated every
  five minutes is a warning nobody reads.

## Every glasses command, by hand

`docs/ORCHESTRATOR.md` §5, job two: *"Voice is the point, but a product where
the only input is speech fails in a quiet room, a loud room, and on a bad day."*

`GlassesCatalog` is that list as data — 20 actions, each naming the transport
method it calls, the protocol ids it produces, whether it is destructive, and
whether it opens a microphone. The Commands screen is generated from it, and the
consent gate is applied from the `opensMicrophone` flag in `CaptureCoordinator`,
so a new action gets the gate for free and a new screen cannot route around it.

`CommandCatalogTests` re-parses `Transport.swift` — carried into the test bundle
as a resource by `project.yml` — and fails if the protocol has grown a method
with no way to reach it. Same trick `glasses/bridge` plays against
`glasses/protocol/commands.py`, and for the same reason: this claim rots the
moment it is only prose.

## Cross-platform parity, pinned

Two vectors, both generated by the TypeScript, both failing loudly on drift:

- `ConnectorSigningTests` — the connector's `HMAC-SHA256` body signature.
- `LinkAuthTests` — the link's `relay.auth.…` subprotocol header. Note that the
  connector signs with the signing key's *characters* and the link signs with
  its *bytes*; getting that backwards produces a header that looks entirely
  plausible and never verifies.

`QueueTests` ports the four shared queue semantics — idempotent enqueue,
refuse-newest, FIFO, stop-at-first-failure — under the same names the TypeScript
and Kotlin suites use.

And the link's **vocabulary** is pinned from the other side: no machine in this
repo can compile Swift, so `glasses/bridge/test/relayd.test.ts` re-parses
`RelaydLink.swift`'s `PhoneMessage` and `ServerMessage` enums and asserts them
against `docs/SYSTEM.md` §6.1 and `relayd/internal/api/wire.go`. That is the only
check these two enums get, and it is worth having: four of §6.1's ten
server→phone frames were missing here, so `ack`, `error`, `notify` and
`confirm.resolved` all arrived and were discarded as unknown types. All ten are
handled now — `ack` prunes the outbox on `{ re, ok }` (not the invented batched
`link.ack`), `error` retires the frame it names instead of redelivering it
forever, `notify` reaches the phone without speech, and `confirm.resolved`
retracts a specific outstanding question.

---

# App Review narrative — always-on capture

`docs/APPS-SCOPE.md` §7 calls this the longest pole. Mentra ships an approved
client in this category (`AGENT-BRIEF` §8-B), so the argument has been won
before; this is what we will claim and what it rests on. **Draft — to be
reviewed against Mentra's actual submission before we file.**

## What the app is

Relay is a companion app for a pair of camera-and-microphone glasses. The
glasses have no internet path of their own: no WiFi station mode, no cellular.
Every byte they capture reaches the user's own machine through this app. That is
structural, and it is the first thing the review notes should say, because it
explains why the background modes are not conveniences.

## The three entitlements, and what each is for

**`UIBackgroundModes: bluetooth-central`.** The app maintains a link to a
Bluetooth accessory the user owns and wears. `CBCentralManager` state
restoration relaunches the app when that accessory reconnects. Nothing here is
unusual; this is the ordinary accessory-companion pattern.

**`UIBackgroundModes: audio`.** The user speaks to the glasses and the app
speaks back — an assistant reply, played to the user, that they asked for by
tapping the frame or saying a wake word. The audio session is `.playAndRecord`
with mode `.voiceChat` while a turn is open, and `.playback` with
`.spokenAudio` while a reply plays.

The claim we make, precisely: **the session is held only while there is audible
work in progress.** `AudioSessionPolicy.decide(_:)` is a pure function with two
activating branches, both of which correspond to something the user can hear,
and a test named `testCaptureBeingOnIsNotByItselfAReasonToHoldASession` asserts
that all-day capture does *not* hold one. If a reviewer asks whether the mode is
being used to stay resident, that function is the answer.

**`UIBackgroundModes: processing`.** One `BGProcessingTaskRequest`, with
`requiresExternalPower = true`, that uploads the day's recordings to the user's
own machine while the glasses are charging. It transfers nothing to us.

## What is recorded, and what the user sees

- Capture is **off** until the user turns it on, behind an explanation screen
  that precedes any system permission dialog. The system dialog has no room for
  the part that matters.
- Capture is gated on **wear**: it starts when the glasses go on and stops when
  they come off.
- A recording indicator is visible in the app at all times, and it never claims
  recording has stopped merely because the phone lost the link — because the
  glasses keep recording to their own storage.
- The glasses have their own hardware indication. **Open question**: whether the
  M01 Pro has a capture LED and whether it can be driven from the protocol
  (`docs/ARCHITECTURE.md` §7). This must be answered before submission, because
  bystander-visible indication is the strongest single fact in this narrative and
  Meta ships a hardware LED on Ray-Bans for exactly this reason.
- Consent can be withdrawn from the home screen. Withdrawal stops capture
  *first* and records the decision second, and survives a background relaunch.

## Where the data goes

To the user's own machine — a Mac mini or equivalent in their home — over an
end-to-end encrypted link. When both are on the same network the bytes never
leave it. When they are not, a rendezvous relay we operate pipes encrypted bytes
it cannot read, and **the day's audio never crosses it**: bulk sync waits for
the LAN rather than spending the user's cellular plan (`docs/SYSTEM.md` §7).

There is no Relay account holding transcripts. This is worth stating plainly:
the reviewer's instinct about an always-on recorder is that a company somewhere
ends up with the recordings, and here nobody does.

## Two-party consent

The user's home market is Québec, which is a two-party-consent jurisdiction, as
are California, Illinois, Washington, Pennsylvania, Massachusetts and Florida.
The app's consent screen says so in as many words. This is not a legal opinion
offered to Apple; it is evidence that the design took the question seriously.

## Likely objections, and the answer to each

| Objection | Answer |
|---|---|
| "`audio` is being used to stay alive." | It is held only for audible work; the policy is one pure function and one test. All-day capture uses `bluetooth-central`, which is what it is for. |
| "The app records people without their knowledge." | Wear-gated, indicator in-app, hardware indication on the device, consent screen before any permission prompt, withdrawal in one tap. |
| "Where do the recordings go?" | To hardware the user owns. We hold nothing and cannot read the relay traffic. |
| "Why does it need Local Network access?" | To reach the user's own machine, and to reach the glasses' access point at 192.168.31.1 during a sync. |
| "Why hotspot configuration?" | A day of audio is 173 MB; over Bluetooth that is sixteen hours. The transfer uses the glasses' own WiFi, and the user confirms the join. |

## Before we file

1. Read Mentra's approved client and match the justification rather than
   inventing one.
2. Settle the hardware recording indicator (`docs/ARCHITECTURE.md` §7).
3. Record a demo video of the whole loop, including withdrawal. Always-on
   capture is reviewed by a person, and the consent flow is the thing to show.
4. Have the `HotspotConfiguration` entitlement enabled on the App ID before the
   first TestFlight build, not during it.

---

## `TestClock`, and how it differs from the TypeScript one

`glasses/bridge`'s `FakeClock` drains the microtask queue between timers, so a
promise chain settles fully before the next timer fires. Swift has no equivalent
queue: cooperative tasks resume when the executor gets to them, and
`Task.yield()` makes no promise that a *specific* other task has run.

So `TestClock` yields **and** sleeps briefly for real between timers, and
`advance` will not commit the end of its window until it has confirmed nothing
new became due. That second part matters more than it looks: committing early
makes `clock.now()`, read inside a continuation chained off the final timer,
report the end of the advance rather than the moment the work finished —
silently turning "this took 2 s" into "this took as long as I advanced". There
is a test for exactly that.

Tests that spawn work in a `Task` must call `clock.waitForSleepers(n)` before
advancing. Spawning does not run a task, so advancing straight away moves time
past a `sleep` that was never registered.

There are two doubles for the transport, on purpose. `MockTransport` models
rates — a photo takes as long over BLE as a photo takes over BLE — which is
right when the subject is the UI's relationship with latency. `StubGlasses` (in
the test target) answers instantly, which is right when the subject is a state
machine like `BulkSync`, where every `await` would otherwise need a matching
`clock.advance` and the test would end up being about the clock.

## Permissions

Bluetooth is the awkward one: iOS has no check-without-asking API, and the
prompt fires the moment a `CBCentralManager` is constructed. `PermissionsModel`
therefore only constructs one from `requestBluetooth()`, after the explanation
screen — reading the status eagerly at launch would trigger the dialog before the
user has been told what is being recorded.

## Known gaps

- **`VendorTransport` does not exist.** A device build gets `MockTransport` and
  says so. This is the largest single piece of unwritten code in the app, and it
  is deliberately last: nothing else needs hardware.
- **Sealed relay mode is not implemented.** `RelaydLink` takes an
  `EnvelopeSealer` and there is no implementation, because the keys come out of
  a PAKE this platform does not have yet (`docs/APPS-SCOPE.md` §4.3 flags it as
  a build blocker). The link **fails closed**: asked to run sealed with no
  sealer, it refuses to connect rather than send an utterance in the clear
  through a server we operate.
- **No pairing flow.** `ConnectorClient.pair` exists; the nine-character-code UI
  and the PAKE behind it do not.
- **`SFSpeechRecognizer` is not wired.** `SpeechRecognizing` is the seam and
  `MockRecognizer` is what the composition root injects today, so a voice turn
  currently produces an empty transcript and sends nothing. The protocol, the
  turn lifecycle, the interruption handling and the `utterance` envelope are all
  built and tested around it; what is missing is thirty lines of
  `SFSpeechAudioBufferRecognitionRequest` and the authorisation request that goes
  with `NSSpeechRecognitionUsageDescription`.
- **`0x0E05` is a report, not a query.** There is no "are you recording?"
  command, so restoration re-asserts `startLocalRecording` and relies on it being
  idempotent. `MockTransport` is; whether the firmware is has to be checked in
  the first hardware session, because the failure mode is two files where there
  should be one.
- **Response payload layouts are unattested** (`docs/APPS-SCOPE.md` §5.1). Disk
  info and file lists are typed objects here; the native adapter owns decoding
  them, and nothing in this app presents a guessed layout as a fact.
