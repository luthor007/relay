import assert from "node:assert/strict";
import { describe, test } from "node:test";

import { FakeClock } from "../src/clock.ts";
import { MOCK_DEFAULTS, MockTransport } from "../src/mock.ts";
import {
  TRACE_VERSION,
  TraceBuilder,
  TraceFormatError,
  base64ToBytes,
  bytesToBase64,
  parseTrace,
  serialiseTrace,
  traceDurationMs,
} from "../src/trace.ts";
import { ConnectionState, TouchAction } from "../src/types.ts";

describe("base64", () => {
  test("round-trips arbitrary bytes", () => {
    const original = new Uint8Array(256).map((_, i) => i);
    assert.deepEqual([...base64ToBytes(bytesToBase64(original))], [...original]);
  });

  test("handles empty input and lengths either side of a 3-byte group", () => {
    for (const size of [0, 1, 2, 3, 4, 5]) {
      const bytes = new Uint8Array(size).map((_, i) => (i * 37) & 0xff);
      assert.deepEqual([...base64ToBytes(bytesToBase64(bytes))], [...bytes], `size ${size}`);
    }
  });
});

describe("trace serialisation", () => {
  test("round-trips through JSON with binary payloads intact", () => {
    const audio = new Uint8Array([1, 2, 3, 250, 251, 252]);
    const trace = new TraceBuilder({ notes: "synthetic" })
      .event(0, "connectionChanged", ConnectionState.Connected)
      .event(120, "battery", { percent: 87, charging: false })
      .event(300, "audioChunk", {
        data: audio,
        format: "opus",
        sampleRate: 16_000,
        channels: 1,
        sequence: 0,
        deviceTimeMs: 300,
      })
      .build();

    const restored = parseTrace(JSON.parse(serialiseTrace(trace)));

    assert.equal(restored.version, TRACE_VERSION);
    assert.equal(restored.notes, "synthetic");
    assert.equal(restored.events.length, 3);

    const chunk = restored.events[2]!.payload as { data: Uint8Array; sampleRate: number };
    assert.ok(chunk.data instanceof Uint8Array, "binary must decode back to bytes, not a string");
    assert.deepEqual([...chunk.data], [...audio]);
    assert.equal(chunk.sampleRate, 16_000);
  });

  test("builder sorts events by time regardless of insertion order", () => {
    const trace = new TraceBuilder()
      .event(500, "touch", TouchAction.SingleTap)
      .event(100, "touch", TouchAction.Wear)
      .event(300, "touch", TouchAction.DoubleTap)
      .build();

    assert.deepEqual(
      trace.events.map((e) => e.tMs),
      [100, 300, 500],
    );
  });

  test("keeps raw frames alongside decoded events", () => {
    const trace = new TraceBuilder()
      .event(0, "connectionChanged", ConnectionState.Connected)
      .frame(0, "tx", "a5060005000100000001b2", "GET_SUPPORTED_FEATURES")
      .build();

    const restored = parseTrace(JSON.parse(serialiseTrace(trace)));
    assert.equal(restored.frames?.length, 1);
    assert.equal(restored.frames![0]!.hex, "a5060005000100000001b2");
    assert.equal(restored.frames![0]!.dir, "tx");
  });

  test("duration is the last event timestamp", () => {
    const trace = new TraceBuilder()
      .event(0, "touch", TouchAction.Wear)
      .event(9_000, "touch", TouchAction.Remove)
      .build();
    assert.equal(traceDurationMs(trace), 9_000);
  });
});

describe("trace validation", () => {
  test("rejects a wrong version", () => {
    assert.throws(() => parseTrace({ version: 99, events: [] }), TraceFormatError);
  });

  test("rejects a non-object", () => {
    assert.throws(() => parseTrace(null), TraceFormatError);
    assert.throws(() => parseTrace("nope"), TraceFormatError);
  });

  test("rejects missing events", () => {
    assert.throws(() => parseTrace({ version: 1 }), TraceFormatError);
  });

  test("rejects out-of-order events", () => {
    assert.throws(
      () =>
        parseTrace({
          version: 1,
          events: [
            { tMs: 100, event: "touch", payload: "wear" },
            { tMs: 50, event: "touch", payload: "remove" },
          ],
        }),
      /ordered by tMs/,
    );
  });

  test("rejects a malformed timestamp", () => {
    assert.throws(
      () => parseTrace({ version: 1, events: [{ tMs: "soon", event: "touch" }] }),
      /finite number/,
    );
  });

  test("rejects a frame with a bad direction", () => {
    assert.throws(
      () =>
        parseTrace({
          version: 1,
          events: [],
          frames: [{ tMs: 0, dir: "sideways", hex: "a5" }],
        }),
      /"tx" or "rx"/,
    );
  });
});

describe("trace playback", () => {
  test("the mock replays a recorded session with its original timing", async () => {
    const trace = new TraceBuilder()
      .event(0, "battery", { percent: 91, charging: false })
      .event(1_000, "touch", TouchAction.Wear)
      .event(2_500, "transcriptText", "start the deploy")
      .build();

    const clock = new FakeClock();
    const glasses = new MockTransport({ clock, trace });

    const seen: Array<{ at: number; event: string }> = [];
    glasses.on("battery", () => seen.push({ at: clock.now(), event: "battery" }));
    glasses.on("touch", () => seen.push({ at: clock.now(), event: "touch" }));
    glasses.on("transcriptText", () => seen.push({ at: clock.now(), event: "transcriptText" }));

    const pending = glasses.connect();
    await clock.advance(MOCK_DEFAULTS.connectDelayMs);
    await pending;

    const base = clock.now();
    await clock.advance(3_000);

    assert.deepEqual(
      seen.map((s) => s.event),
      ["battery", "touch", "transcriptText"],
    );
    assert.deepEqual(
      seen.map((s) => s.at - base),
      [0, 1_000, 2_500],
      "playback must preserve the recorded inter-event gaps",
    );
  });

  test("playback stops when the link drops", async () => {
    const trace = new TraceBuilder()
      .event(0, "touch", TouchAction.Wear)
      .event(10_000, "touch", TouchAction.Remove)
      .build();

    const clock = new FakeClock();
    const glasses = new MockTransport({ clock, trace });
    const touches: string[] = [];
    glasses.on("touch", (t) => touches.push(t));

    const pending = glasses.connect();
    await clock.advance(MOCK_DEFAULTS.connectDelayMs);
    await pending;

    await clock.advance(1_000);
    glasses.simulateDisconnect();
    await clock.advance(20_000);

    assert.deepEqual(touches, ["wear"], "no events should replay after a disconnect");
  });
});
