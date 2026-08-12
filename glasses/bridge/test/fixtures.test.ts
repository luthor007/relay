/**
 * The recorded-session fixtures, checked as artefacts rather than assumed good.
 *
 * `fixtures/*.trace.json` are what the app is developed against while the
 * glasses are 4,000 km away, so a fixture that has quietly drifted out of the
 * protocol — or out of the documented rates — is worse than no fixture at all.
 * Every frame here is decoded with the real codec, and every timing is measured
 * by replaying the trace through the mock on a fake clock.
 */

import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, test } from "node:test";

import { FakeClock } from "../src/clock.ts";
import { MOCK_DEFAULTS, MockTransport } from "../src/mock.ts";
import { parseTrace, traceDurationMs } from "../src/trace.ts";
import type { Trace } from "../src/trace.ts";
import {
  Command,
  commandName,
  decodeFrame,
  decodePacket,
  decodeWakeWordList,
  isKnownCommand,
} from "../src/commands.ts";
import type { AudioChunk } from "../src/types.ts";

const here = dirname(fileURLToPath(import.meta.url));
const FIXTURES = join(here, "..", "fixtures");

function load(name: string): Trace {
  return parseTrace(JSON.parse(readFileSync(join(FIXTURES, `${name}.trace.json`), "utf8")));
}

function bytes(hex: string): Uint8Array {
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = Number.parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  return out;
}

/** Replay a trace and collect (time, event) pairs. */
async function replay(trace: Trace, events: readonly string[]) {
  const clock = new FakeClock();
  const glasses = new MockTransport({ clock, trace, batteryReportIntervalMs: 0 });
  const seen: Array<{ at: number; event: string; payload: unknown }> = [];

  for (const event of events) {
    glasses.on(event as "battery", (payload) =>
      seen.push({ at: clock.now(), event, payload }),
    );
  }

  const pending = glasses.connect();
  await clock.advance(MOCK_DEFAULTS.connectDelayMs);
  await pending;

  const base = clock.now();
  await clock.advance(traceDurationMs(trace) + 1_000);
  return { seen: seen.map((s) => ({ ...s, at: s.at - base })), glasses, clock };
}

const ALL_FIXTURES = readdirSync(FIXTURES)
  .filter((name) => name.endsWith(".trace.json"))
  .map((name) => name.replace(".trace.json", ""))
  .sort();

describe("every fixture is a valid, protocol-accurate artefact", () => {
  test("the expected set is present", () => {
    assert.deepEqual(ALL_FIXTURES, [
      "command-surface",
      "desk-session",
      "flaky-link",
      "nightly-sync",
      "storage-pressure",
      "voice-turn",
    ]);
  });

  for (const name of ALL_FIXTURES) {
    test(`${name} parses, and every frame in it decodes`, () => {
      const trace = load(name);
      assert.ok(trace.events.length > 0, "a trace with no events replays nothing");

      for (const [i, frame] of (trace.frames ?? []).entries()) {
        // decodeFrame validates prefix, length and CRC-16/MODBUS. A fixture that
        // was hand-edited into the wrong shape dies here rather than in an app.
        const { data, consumed } = decodeFrame(bytes(frame.hex));
        assert.equal(consumed * 2, frame.hex.length, `${name} frames[${i}] has trailing bytes`);

        const packet = decodePacket(data);
        assert.ok(
          isKnownCommand(packet.command),
          `${name} frames[${i}] carries unknown command 0x${packet.command.toString(16)}`,
        );
        assert.ok(
          frame.note && frame.note.length > 0,
          `${name} frames[${i}] (${commandName(packet.command)}) needs a note`,
        );
      }
    });
  }
});

describe("command-surface — the device console", () => {
  test("it reaches for the commands the console screen needs", () => {
    const trace = load("command-surface");
    const sent = new Set(
      (trace.frames ?? [])
        .filter((f) => f.dir === "tx")
        .map((f) => decodePacket(decodeFrame(bytes(f.hex)).data).command),
    );

    for (const id of [
      Command.GET_SUPPORTED_FEATURES,
      Command.GET_BATTERY,
      Command.GET_DISK_INFO,
      Command.GET_FILE_COUNT,
      Command.GET_WAKEWORD_LIST,
      Command.SET_WAKEWORD_SETTING,
      Command.GET_RECORDING_PROMPT,
      Command.SET_CALL_AUTO_RECORD,
      Command.GET_KEY_FUNCTIONS,
      Command.FIND_DEVICE,
    ]) {
      assert.ok(sent.has(id), `console never sent ${commandName(id)}`);
    }
  });

  test("the wake word response decodes to the firmware-fixed list", () => {
    const trace = load("command-surface");
    const frame = (trace.frames ?? []).find(
      (f) =>
        f.dir === "rx" &&
        decodePacket(decodeFrame(bytes(f.hex)).data).command === Command.GET_WAKEWORD_LIST,
    );
    assert.ok(frame, "0x0F01 must have a response to decode");

    const packet = decodePacket(decodeFrame(bytes(frame!.hex)).data);
    const words = decodeWakeWordList(packet.payload);
    assert.equal(words[0]!.phrase, "hey chatgpt", "the spec's own worked example");
    assert.equal(words[0]!.kind, 0, "type 0 is an AI wake phrase");
    assert.ok(words.length >= 2);
  });

  test("it replays into the events a settings screen binds to", async () => {
    const { seen } = await replay(load("command-surface"), [
      "battery",
      "diskInfo",
      "mediaCounts",
      "wakeWordSettingsChanged",
      "firmwareVersion",
      "findDeviceChanged",
    ]);
    assert.deepEqual(
      seen.map((s) => s.event),
      [
        "firmwareVersion",
        "battery",
        "diskInfo",
        "mediaCounts",
        "wakeWordSettingsChanged",
        "findDeviceChanged",
      ],
    );
  });
});

describe("voice-turn — every verb belongs to the app", () => {
  test("the device reports, then we start recognising, then we produce the text", async () => {
    const { seen } = await replay(load("voice-turn"), [
      "aiInterfaceEvent",
      "recognitionChanged",
      "transcriptText",
      "speakModeChanged",
    ]);

    const order = seen.map((s) => s.event);
    assert.equal(order[0], "aiInterfaceEvent", "the glasses go first, with an event");
    assert.equal(order[1], "recognitionChanged", "then the app starts its own recogniser");

    const transcriptAt = seen.findIndex((s) => s.event === "transcriptText");
    const recognitionStart = seen.findIndex((s) => s.event === "recognitionChanged");
    assert.ok(
      transcriptAt > recognitionStart,
      "a transcript cannot precede the recogniser that produced it",
    );
    assert.deepEqual(
      seen.filter((s) => s.event === "speakModeChanged").map((s) => s.payload),
      [1, 2, 3],
      "start, hold, stop — the hold is what keeps a long reply from truncating",
    );
  });

  test("the mic arrives at the documented ~3 KB/s", async () => {
    const trace = load("voice-turn");
    const chunks = trace.events
      .filter((e) => e.event === "audioChunk")
      .map((e) => e.payload as AudioChunk);

    assert.ok(chunks.length > 10);
    const spanS = (chunks.at(-1)!.deviceTimeMs - chunks[0]!.deviceTimeMs) / 1000;
    const total = chunks.reduce((n, c) => n + c.data.byteLength, 0);
    const perSecond = total / spanS;
    assert.ok(
      perSecond > 2_700 && perSecond < 3_400,
      `fixture mic rate was ${perSecond.toFixed(0)} B/s`,
    );
    assert.deepEqual(
      chunks.map((c) => c.sequence),
      chunks.map((_, i) => i),
    );
  });

  test("the reply goes down the same 0x0A03 the mic came up", () => {
    const trace = load("voice-turn");
    const audioFrames = (trace.frames ?? []).filter(
      (f) => decodePacket(decodeFrame(bytes(f.hex)).data).command === Command.AUDIO_DATA,
    );
    assert.ok(
      audioFrames.some((f) => f.dir === "rx") && audioFrames.some((f) => f.dir === "tx"),
      "0x0A03 must appear in both directions — it is bidirectional",
    );
  });
});

describe("nightly-sync — the two-phase ritual, at real speed", () => {
  test("a day of audio moves over the AP in about ninety seconds", async () => {
    const { seen } = await replay(load("nightly-sync"), [
      "wifiAccessPointChanged",
      "fileTransferProgress",
    ]);

    const opened = seen.find(
      (s) => s.event === "wifiAccessPointChanged" && (s.payload as { open: boolean }).open,
    );
    const progress = seen.filter((s) => s.event === "fileTransferProgress");
    const last = progress.at(-1)!;

    assert.ok(opened, "the AP has to open before anything transfers");
    assert.ok(opened!.at < progress[0]!.at, "no bytes may move before the AP is up");

    const elapsedS = (last.at - opened!.at) / 1000;
    assert.ok(elapsedS > 60 && elapsedS < 120, `sync took ${elapsedS.toFixed(0)} s`);

    const payload = last.payload as { receivedBytes: number; totalBytes: number; via: string };
    assert.equal(payload.receivedBytes, payload.totalBytes);
    assert.equal(payload.via, "wifiAp", "this cannot ride BLE — that is the whole point");
    assert.ok(payload.totalBytes > 150 * 1024 ** 2, "a 16 h Opus day is ~173 MB");
  });

  test("the phone has no uplink while the AP is open, and gets it back after", async () => {
    const { seen } = await replay(load("nightly-sync"), ["wifiAccessPointChanged"]);
    const flags = seen.map((s) => (s.payload as { phoneUplinkSuspended: boolean }).phoneUplinkSuspended);
    assert.deepEqual(flags, [true, false], "ARCHITECTURE.md §2.1 — two phases, never one");
  });
});

describe("storage-pressure — video evicts audio", () => {
  test("free space collapses while video runs", async () => {
    const { seen } = await replay(load("storage-pressure"), ["diskInfo", "clearResult"]);
    const free = seen
      .filter((s) => s.event === "diskInfo")
      .map((s) => (s.payload as { freeBytes: number }).freeBytes);

    assert.ok(free.length > 3);
    assert.deepEqual(free, [...free].sort((a, b) => b - a), "free space must fall monotonically");
    assert.ok(free.at(-1)! < free[0]! / 2, "a couple of minutes of 1080p halves a 4 GB device");

    const cleared = seen.find((s) => s.event === "clearResult");
    assert.ok(cleared, "the recovery path is 0x0911, and it is destructive");
  });
});

describe("the legacy fixtures still work", () => {
  test("desk-session replays with its original timing", async () => {
    const { seen } = await replay(load("desk-session"), ["wear", "recordingState"]);
    assert.deepEqual(
      seen.map((s) => `${s.event}@${s.at}`),
      ["recordingState@2500", "wear@2010", "recordingState@45500", "wear@45010"].sort(
        (a, b) => Number(a.split("@")[1]) - Number(b.split("@")[1]),
      ),
    );
  });

  test("flaky-link still ends connected after a drop", async () => {
    const trace = load("flaky-link");
    const states = trace.events.filter((e) => e.event === "connectionChanged").map((e) => e.payload);
    assert.deepEqual(states, ["connected", "disconnected", "reconnecting", "connected"]);
  });
});
