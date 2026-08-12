# Relay

**Glasses that run your coding agents, remember your day, and answer when you ask.**

Relay One is smart glasses for people who already live inside AI agents. Your
Claude Code session keeps going after you stand up. Your day gets recorded,
transcribed and searchable. Your agent can finally answer *"what did I decide
about this yesterday?"*

This repository is the whole stack: the BLE protocol, the SDK, the host apps, and
everything you need to build on it or run it yourself.

**[relay-vert-nu.vercel.app](https://relay-vert-nu.vercel.app) · [Buy — $249](https://relay-vert-nu.vercel.app#buy)**

The software is free and open source. Run it on any machine that stays on and you
pay nothing; if you have no machine to spare, Relay Cloud runs one for $39/mo or
$429/yr. There is no tier in between.

---

## Why this exists

Raw egocentric video is already a commodity — Build AI gave away a million hours
of it. Meta Ray-Bans can see and hear. Neither can touch your repo, run your
tests, or remember what you promised someone in a hallway.

The gap is not perception. It's **continuity**: an agent that was present for your
day and can act on it.

## What's here

```
relayd/               The orchestrator — runs your coding agents, Go
glasses/protocol/     BLE protocol codec — all 92 commands, 92 tests, Python
glasses/bridge/       TypeScript transport interface + hardware-free mock
apps/sdk/             @relay/sdk — build apps for Relay One
apps/android/         Always-on capture service (Kotlin)
tools/                Device probe and session capture instruments
docs/                 Architecture, app platform, protocol notes
```

`relayd` is the part that does the work. It speaks to Claude Code, Codex and
any ACP agent through one interface, keeps their credentials in your OS
keychain, and never sends a credential to a model — there is a detector in
front of every path that reaches one. Each adapter has a recorded session in
`docs/fixtures/adapters/` that its tests replay message by message, so you can
see exactly what the wire looks like without owning the hardware.

Two things it will not do quietly: it refuses to start an app that can read
your life and reach the network without a boundary between them, and it
refuses to expose itself to the network without an explicit flag.

## Start without hardware

The mock is protocol-accurate and encodes real device timing, so you can build
and test a full app before your glasses arrive.

```bash
git clone https://github.com/luthor007/relay
cd relay/glasses/bridge
npm test          # 66 tests, no hardware required
```

```ts
import { MockTransport, FakeClock } from "@relay/glasses-bridge";

const clock = new FakeClock();
const glasses = new MockTransport({ clock });

const connecting = glasses.connect();
await clock.advance(800);
await connecting;

glasses.on("wear", (worn) => console.log(worn ? "capturing" : "stopped"));
```

The mock is deliberately inconvenient. A full-size photo really does take ~84
seconds over BLE; fetching an hour of audio really does take an hour unless you
open the Wi-Fi access point. A mock that resolves instantly produces a UI that
lies about latency and has no error states.

## Write an app

Apps run on **your own box**, not on the developer's server. The author never
receives your transcript and never pays to host anything.

```ts
import { defineApp } from "@relay/sdk";

export default defineApp({
  async onTrigger(ctx) {
    const meeting = await ctx.memory.recentEpisode({ kind: "meeting" });
    if (!meeting) return ctx.say("No meeting found in the last hour.");

    const summary = await ctx.agent.ask(`Summarise:\n\n${meeting.transcript}`);
    await ctx.memory.write({ kind: "note", title: "Standup", body: summary });
    await ctx.say("Saved.");
  },
});
```

Declare what it needs in `relay.json` and it becomes a tool your agent can call:

```json
{
  "id": "dev.you.standup-notes",
  "permissions": [
    { "scope": "memory.read", "reason": "To read the meeting you just left." }
  ],
  "triggers": [{ "type": "phrase", "match": "wrap up the standup" }]
}
```

Full guide: **[docs/APP-PLATFORM.md](docs/APP-PLATFORM.md)** · Working example:
[`apps/sdk/examples/standup-notes`](apps/sdk/examples/standup-notes)

## Run it yourself

You never have to pay us. Everything needed to run the agent runtime, the memory
pipeline and the app host on your own machine is here and MIT licensed. The
hosted tier rents you a box that is already set up and awake when you are not at
your desk — that's convenience, not a gate.

## Notes from reverse-engineering the hardware

Some of this was expensive to learn and is written down so nobody has to learn it
twice:

- **The CRC-16 variant is not the one the vendor spec implies.** The spec's own
  appendix reproduces Linux `lib/crc16.c`, whose caller always passes init `0` —
  that would be CRC-16/ARC. Disassembling the shipping framework shows
  `mov w8, #0xffff`. It's **CRC-16/MODBUS**. Init 0 makes the glasses reject
  every frame with no useful error. → [`glasses/protocol/crc.py`](glasses/protocol/crc.py)

- **There is no Wi-Fi station mode.** Across all 109 pages of the protocol, the
  only Wi-Fi commands configure the device's *own* hotspot. The glasses can never
  reach the internet on their own, which makes the phone bridge structural rather
  than optional. → [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) §2

- **Storage isn't the constraint; transfer is.** 4 GB holds 2–23 days of audio,
  but moving one day over BLE takes 16 hours to a week. Sync has to ride Wi-Fi,
  which is why it's a nightly ritual and not a background trickle.
  → [`docs/APPS-SCOPE.md`](docs/APPS-SCOPE.md) §3

## Privacy, plainly

- The indicator LEDs are lit while recording, and there is **no API to disable
  them**. There never will be.
- Transcripts live on your box, encrypted at rest, exportable and deletable.
- Audio is discarded after transcription by default. Keeping a recording of
  someone's entire life is a liability with no matching benefit.
- Recording law varies. Quebec, California, Illinois and others require every
  party to consent. Complying where you live is on you.

## Status

Pre-launch. Founders Edition units ship in 6–8 weeks.

| | |
|---|---|
| Protocol codec | working, 92 tests |
| Transport + mock | working, 264 tests |
| Pairing, box link, store-and-forward | working, in the transport package |
| App SDK | types and manifest validation, 13 tests |
| Android capture service | compiles; 327 unit tests pass; release APK and Play bundle build **signed**. Not yet uploaded |
| iOS host app | compiles; 192 tests pass on the simulator. **Never run on a device**, and not submitted |
| Orchestrator (`relayd`) | working, 45 packages and 1,611 tests |
| Agent adapters | Claude Code, Codex, ACP — each replays a recorded session |
| App runtime | working on Linux; two packages fail on macOS, deliberately |

Nothing above is dressed up as further along than it is. See
[`docs/`](docs/) for what is verified against hardware and what is still an
estimate.

If you run `go test ./...` in `relayd/` on a Mac, `internal/apps` and
`internal/appruntime` fail. That is the sandbox doing its job rather than a
broken build: an app that holds `memory.read` is refused unless the network can
be isolated from it, and there is no network namespace on macOS reachable
without cgo. Linux is the supported host. A handful of tests also skip there,
because the secret-detection corpus is credential-*shaped* by design and stays
in the private repo — the detector itself ships here and is compiled in.

## Licence

MIT. Build a competitor with it if you like.

The vendor SDK for the underlying M01 Pro hardware is proprietary to Shenzhen
QC.wireless and is **not** included here. Everything in this repository is our own
implementation, written from the published protocol documentation.

---

Built by [UU Lab](https://relay-vert-nu.vercel.app), a trade name of Jappuie Inc., Québec.
