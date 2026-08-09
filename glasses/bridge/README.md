# @uulab/glasses-bridge

The seam between the app and the glasses. **Zero dependencies** — no React
Native imports, no Node built-ins in `src/` — so it runs unchanged in RN, in a
browser, and under `node --test`.

```
npm test          # 60 tests, no hardware
npm run typecheck # strict, erasableSyntaxOnly
```

## Why this exists

The iOS vendor frameworks are `arm64` **device-only** (`lipo -info` on all five).
Linking them removes the Simulator as an option, which also breaks a
Maestro-on-simulator pipeline. So everything above this interface — onboarding,
memory, sessions, connectors — is written against `GlassesTransport` and nothing
else, and developed with no glasses in the room.

```
                    ┌─ MockTransport      (here — no hardware)
GlassesTransport ───┼─ AndroidTransport   (native module → LIB_GLASSES_SDK)
                    └─ IosTransport       (native module → QCSDK.framework)
```

## Use

```ts
import { MockTransport, FakeClock } from "@uulab/glasses-bridge";

const clock = new FakeClock();
const glasses = new MockTransport({ clock });

const connecting = glasses.connect();
await clock.advance(800);          // nothing happens until time moves
await connecting;

glasses.on("wear", (worn) => (worn ? startCapture() : stopCapture()));

const photo = await glasses.takePhoto({ maxWidth: 320, maxHeight: 240 });
```

## The mock is deliberately inconvenient

It is not there to make calls resolve. It is there to make them resolve *the way
the hardware does*, so the UI is built against real constraints:

| Behaviour | Why |
|---|---|
| `takePhoto` takes seconds and reports progress | BLE moves a few KB/s — a full-size JPEG is ~84 s, a 320×240 is ~2 s. Resolution is a latency dial. |
| `fetchFile` is glacial over BLE, fast over the AP | An hour of audio takes an hour over BLE. This is why sync is a nightly WiFi ritual. |
| Recording consumes the 4 GB budget | Storage can fill; video at ~4.5 GB/h evicts audio. |
| Battery drains, and charges when plugged | The desk case is the common case. |
| Links drop; in-flight commands fail | A mock that never fails produces a UI with no error states. |

Fault injection covers connect failure, mid-transfer failure, and spontaneous
disconnects — see `MockOptions.faults`.

Every default in `MOCK_DEFAULTS` is an **estimate** until the hardware is
measured. Replace them with real figures from `tools/capture_trace.py measure`.

## Traces

A trace is a recorded session, replayable forever:

```ts
const glasses = new MockTransport({ clock, trace });  // replays with original timing
```

`fixtures/` holds synthetic sessions (`desk-session`, `flaky-link`) built through
`TraceBuilder` so they cannot drift out of schema. Regenerate with
`node fixtures/build-fixtures.ts`.

Real captures come from the Python side:

```
python tools/capture_trace.py scan
python tools/capture_trace.py record <addr> -o glasses/bridge/fixtures/real-desk.trace.json
```

Traces carry two parallel views: `frames` (raw wire bytes — ground truth,
decodable by `glasses/protocol`) and `events` (the product-level view the mock
replays). The Python→TypeScript round trip is exercised end to end.

## Testing notes

`FakeClock` makes everything deterministic — no test sleeps on wall-clock time.
Two patterns matter:

```ts
// Measure when work finished, not when the advance ended
let finishedAt = -1;
const pending = glasses.takePhoto().then((p) => { finishedAt = clock.now(); return p; });
await clock.advance(90_000);

// Attach rejection handlers BEFORE advancing, or Node reports them unhandled
const assertion = assert.rejects(glasses.connect(), ...);
await clock.advance(800);
await assertion;
```

## Implementing a real transport

Implement `GlassesTransport` in a native module and pass it wherever
`MockTransport` goes. Each method's doc comment names the protocol command it
maps to (`glasses/protocol` has all 92, and `glasses/NOTES.md` records which
constants are verified against the shipping SDK).

Two things the vendor SDKs do **not** give you, both of which live below this
interface: an Android foreground service, and iOS `audio` background mode. See
`docs/APPS-SCOPE.md` §4.1.
