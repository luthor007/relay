# Android — `relay-bridge`

The always-on half of the Android host app. This is the piece the vendor SDK
does not provide at all: their sample is Activity-only, with **zero `<service>`
declarations**, so capture stops the moment the user switches apps.

```
relay-bridge/
  AndroidManifest.xml             typed FGS + the permissions API 34 requires
  RelayCaptureService.kt          the foreground service
  ConnectionSupervisor.kt         reconnect, heartbeat, wear reporting
  CaptureNotifications.kt         the standing recording indicator
  CaptureDiagnostics.kt           heartbeat trail + "did this phone kill us?"
  BootReceiver.kt                 restart after reboot, if the user had it on
  BatteryOptimisation.kt          exemption flow + per-OEM escape hatches
  SyncNetwork.kt                  joining the glasses' AP, and giving it back
  GlassesTransport.kt             the seam — mirrors glasses/bridge/src/transport.ts
  TransportAdapters.kt            narrow views: voice, recording, disk, raw commands
  MockGlassesTransport.kt         glasses-free transport for debug builds
  CaptureAudioSink.kt             live frames into the ring buffer

  protocol/    CRC-16/MODBUS, frame + packet codec, all 92 command ids
  commands/    the catalog, the console, the runner — every command, by hand
  capture/     the fork: VoiceSession (Path A), LocalRecordingController (Path B)
  audio/       ring buffer with backpressure, chunked resumable upload plan
  storage/     the 4 GB policy — reserve, warn, sync, never delete un-uploaded
  consent/     ARCHITECTURE.md §6 as a decision function — and the gate that reads it
  link/        SYSTEM.md §6.1: the phone ↔ relayd WebSocket, envelope to socket
  oem/         who kills background services, and how to tell it happened
  session/     session list/attach and the approval queue
  connector/   bulk upload to the box: signing, resumable upload, durable queue
```

---

## Status — **nothing here has been compiled by the Android toolchain**

Say that plainly before anything else. The machine this was written on has no
Android SDK, and `dl.google.com` — where both the Android Gradle Plugin and
every SDK package live — returns a 403 policy denial. That is an organisation
egress policy, not a misconfiguration. So:

| | State |
|---|---|
| `./gradlew :app:assembleDebug` | **not run** — needs AGP from `dl.google.com` |
| `./gradlew :relay-bridge:testDebugUnitTest` | **not run** — same |
| `./tools/verify-jvm-logic.sh` | **green: 262 tests** |

The last line is the floor, not a substitute. What it proves is real, and what
it cannot see is listed below.

### What was actually verified

```
./tools/verify-jvm-logic.sh          # 262 tests, all green
```

That script compiles every Kotlin file in a *sub*package of
`glass.relay.bridge` that names no platform symbol — the protocol codec, the
command catalog and console, both capture paths, the audio ring, the upload
plan, the storage policy, the consent rules **and the gate that applies them**,
**the whole relayd link including its RFC 6455 codec and a loopback socket**,
the OEM watchdog, the session and approval surface, and the store-and-forward
queue — together with their JUnit tests, using `kotlinc` fetched from Maven
Central, and runs them on a plain JVM.

That is 262 of the module's 268 assertions. The six it skips are
`ConnectionSupervisorTest`, which uses `android.util.Log`; those were run
separately against hand-written platform stubs during development and pass,
which is weaker evidence and is why they are counted separately here.

Two of those 262 are worth naming because they are not pure logic:
`JvmWebSocketTest` binds a `ServerSocket` on `127.0.0.1` and drives the real
socket through a real handshake, a real masked frame, a ping/pong and an abrupt
hang-up; and `ConsentWiringTest` re-reads `RelayCaptureService.kt` from disk to
assert that the consent policy still has a caller. Both exist because the
alternative — "that half cannot be checked here" — is how untested code ships.

### What it cannot see, and therefore what is still unproven

- **Anything in the root package** (`RelayCaptureService`, `BootReceiver`,
  `CaptureNotifications`, `CaptureDiagnostics`, `AndroidSyncNetwork`,
  `BatteryOptimisation`, `MockGlassesTransport`) — these were type-checked
  against hand-written Android stubs during development, which catches typos
  and internal inconsistency but proves nothing about the real SDK signatures.
- **The whole `:app` module.** Every Compose file is unbuilt.
- **The manifest.** It is not merged, so a wrong attribute is not caught.
- **Resources.** A missing `R.string` is a compile error nobody has hit yet.
- **`src/vendor/`**, which needs the proprietary AAR.
- **Everything that needs hardware**, which is everything in `src/vendor/`.

**The exact command that would settle it**, on a machine with an Android SDK:

```
cd apps/android
./gradlew :relay-bridge:testDebugUnitTest   # unit tests, incl. the six the JVM harness skips
./gradlew :app:assembleDebug                # manifest merge, resources, Compose
```

---

## The capture fork, both paths

`APPS-SCOPE.md` §3 says the SDK offers two ways to get audio and that they imply
different products. Both are built, in `capture/`, because they do different
jobs.

**Path A — `VoiceSession`.** The interactive loop. One rule shapes it: the
microphone is open only while someone is speaking to the assistant, and it
closes the instant they stop — not when the answer arrives, not when playback
ends. A loop that holds the uplink through a four-second model call has paid for
four seconds of full-rate radio on both devices for nothing. `offerAudio`
*refuses* frames outside a session and counts them, which is the mechanical
guarantee that nothing is captured outside a session the user started. A hung
endpoint detector cannot hold the mic open: `maxListenMs` closes it. Barge-in
works, because interrupting is how people talk.

**Path B — `LocalRecordingController`.** The all-day pipeline. The glasses
record to their own 4 GB and the phone pulls files later, so a phone that is out
of range all day loses nothing. Consent and wear gate it; the connection does
not, because the glasses keep writing whether or not we are listening. The day
is cut into fifteen-minute segments so the box can transcribe as it goes, a
failed transfer resumes at a boundary, and a segment can be deleted once it is
safely uploaded — none of which a single sixteen-hour file allows.

---

## Storage is a requirement, not a nicety

`storage/StoragePolicy.kt`, from `APPS-SCOPE.md` §3.2. The arithmetic is the
argument:

| | Rate | 4 GB holds |
|---|---|---|
| Opus ~24 kbps | ~10.8 MB/h | ~370 h |
| PCM 16 kHz/16-bit | ~115 MB/h | ~35 h |
| 1080p video | **~4.5 GB/h** | ~53 min |

So **one minute of video costs about seven hours of Opus**, and a few minutes of
it can evict a day. The policy reserves headroom for audio, states free space in
hours rather than percent, asks for a sync before the device is full rather than
after, blocks video before it eats the reserve — and **never proposes deleting
un-uploaded audio at any level of pressure**, which is pinned by a test that
sweeps every level. `0x0911` 清除未上传文件 exists because the firmware tracks
that distinction; its existence is not permission to use it.

---

## Every glasses command, by hand

`ORCHESTRATOR.md` §5, job 2. `commands/CommandCatalog.kt` carries all 92 command
ids with role, category, the spec's own term, a destructive flag and an argument
spec, and the command screen is **generated from it**. There is no per-command
UI code, so a command cannot be added to the protocol and quietly fail to appear.

Three tests keep it honest, and all three run in the JVM harness:

- the catalog covers exactly the ids in `glasses/protocol/commands.py`, once
  each — that file is re-parsed on every run
- roles and destructive flags match `COMMAND_CATALOG` in
  `glasses/bridge/src/commands.ts`, also re-parsed
- every command the catalog calls sendable can actually be built into a frame

The frame is built here, not by the vendor SDK: `protocol/` is a Kotlin port of
`glasses/protocol/{crc,frame,commands}.py`, pinned against vectors generated by
running that Python. That is what reduces the vendor surface this needs from
sixty typed SDK methods to one `sendFrame` call — and sixty untestable methods
is exactly what a machine with no SDK cannot afford.

Commands the app must not send are shown and refused, not hidden: `0x0801`-`0x0804`
are 未使用, `0x0901`/`0x0902` are 已弃用 *and* set the glasses' own hotspot
rather than joining a network (there is no station mode at all), device reports
have nothing to send, and anything whose request layout is not attested is
refused with a pointer to `APPS-SCOPE.md` §5.1 rather than guessed.

---

## The things that are easy to get wrong

**Foreground service types are not a formality.** From API 34 a service must
declare its type *and* hold the matching permission. The service declares
`connectedDevice` only, and adds `microphone` through `useMicrophone()` at the
moment a phone-side recorder opens — for two reasons. All-day capture reads
audio off the glasses over BLE and never opens `AudioRecord`, so a permanent
microphone chip in the status bar would be claiming something untrue. And
Android restricts which foreground service types may be started from
`BOOT_COMPLETED`; `microphone` is on that list for recent target levels, so a
boot start that declared it would throw rather than degrade. **The exact
restricted-type list per API level is unverified here** — `connectedDevice`
alone is conservative under every version of it.

**`FOREGROUND_SERVICE_DATA_SYNC` is deliberately absent.** The nightly WiFi sync
runs inside the existing `connectedDevice` service. `dataSync` carries a per-day
runtime cap on recent releases, and a capture service that stops after N hours
is the exact failure this module exists to prevent.

**`START_STICKY` is not enough.** Several OEM skins kill background work
regardless of foreground-service status. `oem/OemPolicy.kt` knows eleven
manufacturers, which of them need an *autostart* grant on top of the battery
exemption (without it the boot receiver is a decoration), and which settings
screen to open. `oem/CaptureWatchdog.kt` closes the loop: the service leaves a
heartbeat trail, and the watchdog turns a silent kill into a sentence — "your
phone has stopped Relay 3 times, the longest gap was 94 minutes" — plus the
specific fix. Advice nobody believes is worth less than evidence.

**The boot receiver could never have fired.** `CapturePreferences.captureEnabled`
was read by `BootReceiver` and written by nothing, so capture never restarted
after a reboot. It is now written where capture actually starts and stops.

**Consent had two homes, and then it had no callers.** It first lived in the app
module's `RelayPrefs`, which the service could not read — so the component that
actually records could not see a revocation. Moving it to `CapturePreferences`
fixed that and left a worse problem: `consent/ConsentPolicy.kt` was a
well-tested state machine that **no production code read**, while a plain
boolean did the gating. See the next section.

**`RECORD_AUDIO` must not block capture.** `blockingPermissions` and
`missingPermissions` are two different lists. The app asks for the microphone —
the phone-side recogniser needs it — but capture starts without it, because
all-day capture reads audio off the glasses over BLE. Gating on one list would
leave a user who declined the microphone permanently parked on a screen asking
for it, unable to record anything at all.

**`BLUETOOTH_SCAN` needs `neverForLocation`.** Without that flag, Android 12+
forces the app to hold location permission just to find the glasses — a prompt
with no honest justification.

**Recording survives disconnection.** The glasses write to their own 4 GB, so
losing the phone link does not stop the recording. The notification says so
explicitly, and a test pins it. Showing "not recording" there would push people
to restart a recording that never stopped.

---

## Consent is a gate, not a stored yes

`ARCHITECTURE.md` §6 is not advisory — Quebec, the user's home market, is a
two-party consent jurisdiction — and it states two requirements:

- capture **defaults to off in a new location or with new voices present, until
  confirmed**
- bystander-visible recording indication, ideally in hardware

`consent/ConsentPolicy.kt` turned the first into a decision function and was
tested. Nothing called it. Capture was gated on
`CapturePreferences.consentGranted`, a boolean set once at onboarding — which
cannot say *where* or *with whom* consent was given, so the rule was enforced
nowhere while the tree read as compliant.

`consent/ConsentGate.kt` is the caller. It holds the scope, the confirmed
places and whatever the box has said about the room, runs `ConsentPolicy` over
them, and produces the one boolean the capture path may read.
`RelayCaptureService` gates `LocalRecordingController` **and** `VoiceSession` on
`consent.verdict.value.capture` and on nothing else.

Three consequences that are behaviour changes, not refactors:

- **A new place stops a running recording and asks.** Same stored consent, same
  everything; the box says the place changed and capture goes off until
  somebody answers. Pinned by `ConsentGateTest`.
- **No signal is treated as a new place.** The box is the only thing that knows
  whether a place is familiar and it may be unreachable, so unknown asks.
  Defaulting it to allow would make the whole rule decorative. In practice a
  tap on "Start capture" is the wearer starting a conversation and clears it; a
  reboot is not, so under `Scope.FamiliarPlaces` a restarted service waits for
  one tap. Under `Scope.Always` it resumes on its own.
- **No indicator, no recording.** `indicatorRequired()` has no off switch, so
  the service tells the gate whether the ongoing notification can actually be
  posted, and a phone with notifications blocked does not record.

The question is answerable from the notification shade as well as the app,
because the wearer is wearing glasses and holding nothing.

---

## The link to the box

`link/`, built to `SYSTEM.md` §6.1 and ported from
`glasses/bridge/src/relayd.ts`. Before it, **Android had no channel to the box
at all** — the only network code was bulk HTTP upload — so `speak`, `ui.render`,
`session.list` and `confirm.request` were unimplemented on this platform, which
is most of `ORCHESTRATOR.md` §5.

The frame set is the one `relayd/internal/api/wire.go` actually implements: the
six product frames plus `ack`, `error`, `notify` and `confirm.resolved`. The
acknowledgement is **`ack`** with a payload of `{re, ok}` — not the `link.ack`
with `{ids: [...]}` the TypeScript invented before the daemon existed. A phone
built against the invented one never prunes its outbox.

- `Envelope.kt` — the wire format, parsed strictly. A half-parsed envelope
  produces a UI acting on a field it invented.
- `Backoff.kt` — exponential with jitter that **subtracts**, so the ceiling is a
  ceiling. Every phone in a building loses WiFi at the same moment when an
  access point reboots.
- `Outbox.kt` — refuse-newest at the limit, and unacknowledged envelopes replay
  to the **head** of the queue on any close, clean or not. `relayd` segments
  episodes by time, so a redelivered utterance that lands after the three that
  followed it joins the wrong conversation. Duplicates are the price; the
  envelope `id` is what pays it.
- `RelaydLink.kt` — the state machine. `error` prunes as surely as `ack` does:
  the daemon received the frame and decided, and retrying `audio.chunk` against
  today's `not_implemented, M4` forever is a battery complaint with no upside.
- `RelaydRouter.kt` — the frames' effects. `session.list` into `SessionRegistry`,
  `confirm.request` into `ApprovalQueue`, answers back out as
  `consent.decision`. An answer for something the box has already timed out
  sends nothing, because that is how an action runs twice.
- `WebSocketProtocol.kt` / `JvmWebSocket.kt` — RFC 6455 by hand, because this
  module carries no third-party dependencies and the platform has no WebSocket
  client. The upside is that the handshake and framing are arithmetic over
  bytes, checked against the RFC's own §1.3 and §5.7 vectors, and the socket is
  driven against a loopback server.

**Sealing is not ported.** `relayd.ts` seals envelopes when the route is the
rendezvous relay (`SYSTEM.md` §7); there is no Kotlin port of `pairing.ts`, and
hand-rolling the primitive is the one thing `APPS-SCOPE.md` §4.3 says not to do.
The seam is the socket factory, so sealing wraps the socket rather than the
state machine.

**Auth is the bearer token `relayd` checks**, in an `Authorization` header, never
in a query string. `relayd.ts`'s HMAC subprotocol is deliberately not ported: a
credential the server ignores reads as security to whoever inspects the tree,
which is the same failure as an unwired consent policy.

---

## The queue

`connector/` is the phone's bulk-upload half of the channel to the box. It is
complementary to `link/`, not a rival: control traffic is small and
bidirectional, uploads are resumable bulk transfer (`APPS-SCOPE.md` §4.3). The wire format
lives in `connector/src/protocol.ts`, and `ConnectorSigningTest` pins this
implementation's HMAC against a vector produced by that reference.

`StoreAndForwardQueue` keeps the four semantics all three implementations share
— idempotent enqueue, refuse-newest, FIFO, stop-at-first-failure — including
the `ItemLimit` refusal the other two had and this one could not express. That
one is small and the defaults made it harmless, but `queue.ts` asserts the three
are identical, and a parity note is only worth anything if someone can trust it.
The precedence is pinned too: delivered → duplicate → tooLarge → full →
itemLimit → storeFailed, so two platforms cannot name the same refusal
differently.

It also pays the two debts `APPS-SCOPE.md` §4.2 recorded:

- **Durability.** `enqueue` does not return until the bytes are on disk, so a
  caller may delete its source the moment it does. `FileQueueStore` writes to a
  temporary file, `fsync`s, then renames; a half-written record is skipped on
  load rather than decoded into a truncated day. The remaining honest gap: the
  directory entry is not fsynced, because Java has no portable way to do it.
- **Delivered-id memory.** A replayed nightly sync does not re-upload the day,
  and it survives a restart. A record that is both pending and delivered — the
  crash window inside `flush` — resolves in favour of delivered.

---

## Building against real hardware

Debug builds link the mock (`USE_MOCK_GLASSES = true`), so the whole product
surface can be developed and instrumented with no glasses present. The mock now
also implements the live-voice channel, the disk probe and the raw command
channel, so Path A, the storage screen and the command console are all driveable
without hardware.

Release builds need the vendor AAR at:

    apps/android/libs/LIB_GLASSES_SDK-release-20260709_8.aar

It is in this repo at `glasses/sdk/android/`, and `apps/android/libs/` is
gitignored — it is proprietary Shenzhen QC.wireless material and must not reach a
public artifact. Without it the build still succeeds and `src/vendor/` is simply
excluded; a *release* build in that state fails loudly at runtime rather than
falling back to the mock, because a build that reports fabricated battery levels
and never connects to anything is indistinguishable from working software until
someone relies on it.

### What the vendor adapter still owes

`VendorGlassesTransport` implements the always-on transport and nothing else. It
does **not** implement `GlassesVoice` (Path A), `DiskProbe`, or
`GlassesRawCommands`, so on a vendor build the talk button, the storage panel
and the command console are inert rather than wrong. Each is small and each
needs a first session with hardware:

| Interface | What it needs | SDK surface |
|---|---|---|
| `GlassesVoice` | live mic up, audio down, speak mode | `voiceFromGlasses` / `voiceFromGlassesStatus`, `AudioRawDataResponse` |
| `DiskProbe` | `0x0909` / `0x091C` and the file listing | `LargeDataHandler`, `RecordHandle` |
| `GlassesRawCommands` | write a framed packet, read the reply | `LargeDataHandler.glassesControl` |

The third one settles `VendorGlassesTransport.CONTROL_PAYLOAD_IS_FRAMED` — the
open question of whether the SDK wants a bare `[command_id][args]` payload or a
fully framed packet. One capture answers it, and the codec in `protocol/` is
needed either way because the reply has to be decoded.

---

## Not built here

- **Pairing.** `glasses/bridge/src/pairing.ts` needs a real PAKE per platform
  (`APPS-SCOPE.md` §4.3 calls this a build blocker). Nothing here hand-rolls one.
  The practical consequence is that `CapturePreferences.boxUrl` / `boxToken` are
  empty on every install, so `RelayCaptureService` builds no link — deliberately,
  because `RelaydLink` retries forever and an unconfigured phone must not spend
  its battery on a URL that does not exist. **There is no screen that fills them
  in**, and adding a text box for a bearer token would be the thing that ships
  instead of pairing.
- **Speech.** `speak` arrives and is surfaced as the last line from the box;
  there is no phone-side TTS and `GlassesTransport` carries no playback call.
  Visibly partial on purpose — a silently dropped `speak` makes the box look
  broken from the phone.
- **Sealing the link** for the rendezvous-relay route (`SYSTEM.md` §7), which
  waits on the same PAKE.
- **Memory, mini-app rendering, connector management** — `ORCHESTRATOR.md` §5
  job 3.

---

## Running the tests without the Android SDK

```bash
cd apps/android && ./tools/verify-jvm-logic.sh
```

**313 tests, 27 classes.** Configuring this module needs the Android Gradle
Plugin, which is published only on `dl.google.com`, so on a machine without an
Android SDK nothing here can be compiled at all and every line of Kotlin is
unverified prose. The script compiles the Android-free half with `kotlinc` from
Maven Central and runs its JUnit tests.

It is a floor, not a substitute: it cannot see a manifest error, a Compose
mistake, a missing resource, or anything in `src/vendor/`. The real command is
`./gradlew :relay-bridge:testDebugUnitTest`, on a machine that has the SDK.

**There is one such harness and there must stay one.** A second — a Gradle
build over the same sources — was written and removed in the same afternoon:
two harnesses that can disagree about which files are covered are worse than
either, and this one already derives its file set by a rule (a subpackage of
`glass.relay.bridge` that names no `android.*` symbol) rather than a list
somebody has to remember to update.

## Reaching the box from outside the house

`SYSTEM.md` §7's relay was built on the box (`relayd/internal/relaylink`) and on
the console (`console/src/cloud`) and, for a while, on neither phone — so a phone
that left the house stopped reaching its box, forever, retrying a LAN hostname
that does not resolve.

`link/BoxAddress.kt` is the decision and `RelaydLink` is the socket, split so the
first can be tested exhaustively:

```
endpoints(BoxAddress(direct, relayUrl, boxId))
  → [ direct, wss://relay/rz/v1/connect/<boxId> ]
```

**Direct first, always, and the two alternate.** §7's argument that the relay is
affordable rests on the day's audio never crossing it, which is only true if the
phone prefers the LAN whenever it has one. `RelaydLink` resets to the front of
the list on every successful open, so a phone that failed over at the office
comes home to the LAN by itself.

The relayed URL carries **no protocol label** — the console appends
`?p=console.v1` and the phone's protocol is the unlabelled default, which is what
lets a box built before labels existed still answer a phone.

**iOS does not have this yet**, and the omission is deliberate rather than
forgotten: `RelaydLink.swift` takes a single `URL`, and no Swift toolchain is
reachable from the environment this was written in. Porting the algorithm blind
into a 1,300-line file would produce exactly the shape of defect this repository
keeps finding — code that looks done and has never been compiled. The Kotlin is
worth having because it is verified; the Swift would not be.
