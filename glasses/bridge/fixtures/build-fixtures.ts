/**
 * Regenerates the synthetic fixtures in this directory.
 *
 *     node fixtures/build-fixtures.ts
 *
 * These stand in until real captures from `tools/capture_trace.py` replace them.
 * They are built through TraceBuilder rather than hand-written JSON so they
 * cannot drift out of schema, and the timings match the figures in
 * docs/ARCHITECTURE.md §5 rather than being invented.
 */

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

import { TraceBuilder, serialiseTrace } from "../src/trace.ts";
import { ConnectionState, TouchAction } from "../src/types.ts";

const here = dirname(fileURLToPath(import.meta.url));

/**
 * A desk session: glasses go on, the user dictates something, the agent
 * answers, a photo is taken, the glasses come off. This is the shape of the
 * interactive loop, at realistic timings.
 */
function deskSession() {
  const b = new TraceBuilder({
    device: { model: "M01 Pro", firmware: "synthetic" },
    notes:
      "Synthetic desk session — wear, voice query, photo, removal. " +
      "Replace with a real capture from tools/capture_trace.py.",
  });

  b.event(0, "connectionChanged", ConnectionState.Connected);
  b.event(150, "battery", { percent: 88, charging: true });
  b.event(300, "diskInfo", { totalBytes: 4 * 1024 ** 3, freeBytes: 3.6 * 1024 ** 3 });

  // Put them on — capture gating keys off this.
  b.event(2_000, "touch", TouchAction.Wear);
  b.event(2_010, "wear", true);
  b.event(2_500, "recordingState", { recording: true, durationS: 0 });

  // Double-tap opens the interactive voice loop.
  b.event(8_000, "touch", TouchAction.DoubleTap);
  b.event(8_100, "voiceSessionChanged", true);

  // ~200 ms of Opus per chunk while speaking.
  let seq = 0;
  for (let t = 8_400; t <= 11_000; t += 200) {
    b.event(t, "audioChunk", {
      data: new Uint8Array(48).fill(seq & 0xff),
      format: "opus",
      sampleRate: 16_000,
      channels: 1,
      sequence: seq++,
      deviceTimeMs: t,
    });
  }

  b.event(11_400, "transcriptText", "what did I decide about the CRC yesterday");
  b.event(11_600, "voiceSessionChanged", false);

  // A small photo: 320x240 over BLE is roughly two seconds, reported in steps.
  b.event(20_000, "touch", TouchAction.SingleTap);
  for (let i = 1; i <= 8; i++) {
    b.event(20_000 + i * 250, "photoProgress", {
      receivedBytes: Math.round((6_144 * i) / 8),
      totalBytes: 6_144,
      chunkIndex: i,
      chunkCount: 8,
    });
  }

  b.event(28_000, "battery", { percent: 89, charging: true });

  // Off they come — capture stops.
  b.event(45_000, "touch", TouchAction.Remove);
  b.event(45_010, "wear", false);
  b.event(45_500, "recordingState", { recording: false, durationS: 43 });

  return b.build();
}

/** A flaky link: connects, drops, and errors — for exercising error states. */
function flakySession() {
  const b = new TraceBuilder({
    device: { model: "M01 Pro", firmware: "synthetic" },
    notes: "Synthetic unstable link — for building reconnect and error UI.",
  });

  b.event(0, "connectionChanged", ConnectionState.Connected);
  b.event(500, "battery", { percent: 14, charging: false });
  b.event(3_000, "touch", TouchAction.Wear);
  b.event(3_010, "wear", true);
  b.event(9_000, "connectionChanged", ConnectionState.Disconnected);
  b.event(12_000, "connectionChanged", ConnectionState.Reconnecting);
  b.event(15_000, "connectionChanged", ConnectionState.Connected);
  b.event(15_500, "battery", { percent: 11, charging: false });

  return b.build();
}

for (const [name, trace] of [
  ["desk-session", deskSession()],
  ["flaky-link", flakySession()],
] as const) {
  const path = join(here, `${name}.trace.json`);
  writeFileSync(path, serialiseTrace(trace));
  console.log(`wrote ${name}.trace.json — ${trace.events.length} events`);
}
