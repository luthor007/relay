/**
 * Regenerates the synthetic fixtures in this directory.
 *
 *     node fixtures/build-fixtures.ts
 *
 * These stand in until real captures from `tools/capture_trace.py` replace them.
 * They are built through TraceBuilder rather than hand-written JSON so they
 * cannot drift out of schema, and the timings match the figures in
 * docs/ARCHITECTURE.md §5 rather than being invented.
 *
 * The command-surface fixtures additionally carry `frames`: complete wire bytes,
 * prefix through CRC, built by the same encoder the bridge uses. Those are
 * decodable by `glasses/protocol` and by `decodeFrame`/`decodePacket` here, so a
 * fixture that drifts out of protocol fails a test rather than teaching the app
 * something false. `test/fixtures.test.ts` decodes every one of them.
 */

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

import { TraceBuilder, serialiseTrace } from "../src/trace.ts";
import type { Trace } from "../src/trace.ts";
import { ConnectionState, TouchAction } from "../src/types.ts";
import type { GlassesEventName } from "../src/types.ts";
import type { AllGlassesEventName, AllGlassesEvents } from "../src/commands.ts";
import {
  AiInterfaceEvent,
  Command,
  CommandType,
  DeviceMode,
  RATES,
  RecognitionOwner,
  SpeakMode,
  encodeCommandFrame,
  encodeWakeWordList,
  encodeWakeWordSettings,
  commandName,
} from "../src/commands.ts";
import { MOCK_WAKE_WORDS } from "../src/mock.ts";

const here = dirname(fileURLToPath(import.meta.url));

const MEGABYTE = 1024 * 1024;

/**
 * Appends events to an already-built `Trace`. `TraceBuilder` is generic over its
 * event map, so building needs no cast — but `Trace.events` itself is typed to
 * the base `GlassesEventName`, and widening the stored record would change the
 * on-disk shape every reader parses. `parseTrace` accepts any string, so one
 * narrow cast here is cheaper than that.
 */
function extend(
  trace: Trace,
  records: Array<{ tMs: number; event: AllGlassesEventName; payload: unknown }>,
): Trace {
  for (const record of records) {
    trace.events.push({
      tMs: record.tMs,
      event: record.event as GlassesEventName,
      payload: record.payload,
    });
  }
  trace.events.sort((a, b) => a.tMs - b.tMs);
  return trace;
}

let seq = 0;

/** One request going out, as it appears on the write characteristic. */
function tx(
  builder: TraceBuilder,
  tMs: number,
  command: number,
  payload: Uint8Array = new Uint8Array(),
  note?: string,
): void {
  const frame = encodeCommandFrame({
    command,
    type: CommandType.Request,
    seq: seq++ & 0xff,
    payload,
  });
  builder.frame(tMs, "tx", hex(frame), note ?? commandName(command));
}

/** One packet arriving on the notify characteristic. */
function rx(
  builder: TraceBuilder,
  tMs: number,
  command: number,
  payload: Uint8Array = new Uint8Array(),
  type: 1 | 2 | 3 = CommandType.Response,
  note?: string,
): void {
  const frame = encodeCommandFrame({ command, type, seq: seq++ & 0xff, payload });
  builder.frame(tMs, "rx", hex(frame), note ?? commandName(command));
}

function hex(bytes: Uint8Array): string {
  let out = "";
  for (const byte of bytes) out += byte.toString(16).padStart(2, "0");
  return out;
}

/**
 * A desk session: glasses go on, the user dictates something, the agent
 * answers, a photo is taken, the glasses come off. This is the shape of the
 * interactive loop, at realistic timings.
 */
function deskSession() {
  const b = new TraceBuilder<AllGlassesEvents>({
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
  let chunk = 0;
  for (let t = 8_400; t <= 11_000; t += 200) {
    b.event(t, "audioChunk", {
      data: new Uint8Array(48).fill(chunk & 0xff),
      format: "opus",
      sampleRate: 16_000,
      channels: 1,
      sequence: chunk++,
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
  const b = new TraceBuilder<AllGlassesEvents>({
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

/**
 * The device console: what the app does when someone opens the settings screen
 * and touches every control. This is the "by hand" surface of ORCHESTRATOR.md §5
 * in trace form — identity, capabilities, storage, wake words, settings.
 */
function commandSurface() {
  const b = new TraceBuilder<AllGlassesEvents>({
    device: { model: "M01 Pro", firmware: "synthetic" },
    notes:
      "Synthetic device-console session. Every frame is a real encoded packet " +
      "(CRC-16/MODBUS, init 0xFFFF) so glasses/protocol can decode it. Response " +
      "payload layouts are only filled in where the spec attests them — the wake " +
      "word list (Index/Type/Len/Value) and the settings pairs. Everything else " +
      "is an ack with an empty payload and says so.",
  });

  b.event(0, "connectionChanged", ConnectionState.Connected);

  // Capabilities first: firmware revisions differ in what they honour.
  tx(b, 100, Command.GET_SUPPORTED_FEATURES);
  rx(b, 180, Command.GET_SUPPORTED_FEATURES, new Uint8Array(), CommandType.Response, "ack — capability bitmap layout unverified");

  for (const [t, id] of [
    [300, Command.GET_PRODUCT_INFO],
    [400, Command.GET_PRODUCT_MODEL],
    [500, Command.GET_VERSION],
    [600, Command.GET_HARDWARE_INFO],
    [700, Command.GET_DEVICE_NAME],
  ] as const) {
    tx(b, t, id);
    rx(b, t + 60, id, new Uint8Array(), CommandType.Response, "ack — string payload unverified");
  }
  b.event(760, "firmwareVersion", "2.00.04");

  tx(b, 900, Command.GET_BATTERY);
  b.event(980, "battery", { percent: 92, charging: false });

  tx(b, 1_100, Command.GET_DISK_INFO);
  b.event(1_180, "diskInfo", { totalBytes: 4 * 1024 ** 3, freeBytes: 3.4 * 1024 ** 3 });

  tx(b, 1_300, Command.GET_FILE_COUNT);

  // Wake words: the list is firmware, the setting is ours. There is no command
  // that accepts a phrase — ARCHITECTURE.md §5.2b.
  tx(b, 1_600, Command.GET_WAKEWORD_LIST);
  rx(
    b,
    1_700,
    Command.GET_WAKEWORD_LIST,
    encodeWakeWordList(MOCK_WAKE_WORDS),
    CommandType.Response,
    "Index/Type/Len/Value entries — the firmware-fixed list",
  );
  tx(b, 1_800, Command.GET_WAKEWORD_SETTING);
  rx(
    b,
    1_880,
    Command.GET_WAKEWORD_SETTING,
    encodeWakeWordSettings([
      { index: 0, enabled: true },
      { index: 1, enabled: false },
      { index: 2, enabled: false },
    ]),
  );
  tx(b, 2_000, Command.SET_WAKEWORD_SETTING, new Uint8Array([0, 0]), "disable 'hey chatgpt'");

  // Settings the console exposes.
  tx(b, 2_300, Command.GET_WEAR_DETECTION);
  tx(b, 2_400, Command.SET_WEAR_DETECTION, new Uint8Array([1]));
  tx(b, 2_500, Command.GET_RECORDING_PROMPT);
  tx(b, 2_600, Command.SET_RECORDING_PROMPT, new Uint8Array([1]), "audible consent cue on");
  tx(b, 2_700, Command.GET_CALL_AUTO_RECORD);
  tx(b, 2_800, Command.SET_CALL_AUTO_RECORD, new Uint8Array([0]), "off — two-party consent");
  tx(b, 2_900, Command.GET_KEY_FUNCTIONS);
  tx(b, 3_000, Command.SET_KEY_FUNCTIONS, new Uint8Array([6]), "double-tap -> aiAssistant");
  tx(b, 3_200, Command.FIND_DEVICE, new Uint8Array([1]));
  rx(b, 3_400, Command.FIND_DEVICE_REPORT, new Uint8Array([1]), CommandType.Notify);

  const trace = b.build();
  return extend(trace, [
    { tMs: 1_360, event: "mediaCounts", payload: { photos: 12, videos: 1, recordings: 4, totalBytes: 640 * MEGABYTE } },
    { tMs: 2_060, event: "wakeWordSettingsChanged", payload: [
      { index: 0, enabled: false },
      { index: 1, enabled: false },
      { index: 2, enabled: false },
    ] },
    { tMs: 3_460, event: "findDeviceChanged", payload: true },
  ]);
}

/**
 * One voice turn, with the ownership made explicit.
 *
 * The device reports 0x0805 and then does nothing else: **the app** starts
 * recognition, **the app** produces the transcript, **the app** pushes the reply
 * back down 0x0A03. There is no device-side ASR anywhere in this trace because
 * there is none on the hardware (SYSTEM.md §7b).
 *
 * Audio is at the real rate: 3 KB/s Opus, 200 ms chunks, 600 bytes each.
 */
function voiceTurn() {
  const b = new TraceBuilder<AllGlassesEvents>({
    device: { model: "M01 Pro", firmware: "synthetic" },
    notes:
      "Synthetic voice turn. Every verb belongs to the app: the glasses report " +
      "an event and carry audio, nothing more. Mic uplink and speech downlink " +
      "share 0x0A03 at ~3 KB/s each way.",
  });

  b.event(0, "connectionChanged", ConnectionState.Connected);
  b.event(1_200, "touch", TouchAction.DoubleTap);

  // The device says the user asked for the assistant. That is all it says.
  rx(b, 1_250, Command.AI_CHAT_TRIGGER, new Uint8Array([0x01]), CommandType.Notify, "AI_CHAT_TRIGGER start");

  // The app opens the uplink and starts its own recogniser.
  tx(b, 1_320, Command.AUDIO_CONTROL, new Uint8Array([0x01]), "start mic uplink");
  b.event(1_400, "voiceSessionChanged", true);

  const chunkBytes = Math.round((RATES.micBytesPerSecond * RATES.audioChunkMs) / 1000);
  let chunk = 0;
  for (let t = 1_600; t <= 4_200; t += RATES.audioChunkMs) {
    b.event(t, "audioChunk", {
      data: new Uint8Array(chunkBytes).fill(chunk & 0xff),
      format: "opus",
      sampleRate: 16_000,
      channels: 1,
      sequence: chunk++,
      deviceTimeMs: t,
    });
  }
  rx(b, 1_600, Command.AUDIO_DATA, new Uint8Array(8), CommandType.Notify, "mic uplink, first chunk (truncated)");

  tx(b, 4_400, Command.AUDIO_CONTROL, new Uint8Array([0x00]), "stop mic uplink");
  b.event(4_450, "voiceSessionChanged", false);
  b.event(4_600, "transcriptText", "add that to the payments refactor");

  // Reply: speak mode on, Opus down the same channel, hold while it streams.
  tx(b, 5_200, Command.DEVICE_CONTROL, new Uint8Array([DeviceMode.SpeakStart]), "speak start");
  tx(b, 5_300, Command.AUDIO_DATA, new Uint8Array(8), "speech downlink (truncated)");
  tx(b, 6_300, Command.DEVICE_CONTROL, new Uint8Array([SpeakMode.Hold]), "speak hold");
  tx(b, 8_400, Command.DEVICE_CONTROL, new Uint8Array([DeviceMode.SpeakStop]), "speak stop");

  const trace = b.build();
  return extend(trace, [
    { tMs: 1_260, event: "aiInterfaceEvent", payload: AiInterfaceEvent.Start },
    { tMs: 1_380, event: "recognitionChanged", payload: { active: true, owner: RecognitionOwner.App } },
    { tMs: 4_500, event: "recognitionChanged", payload: { active: false, owner: RecognitionOwner.App } },
    { tMs: 5_260, event: "speakModeChanged", payload: SpeakMode.Start },
    { tMs: 6_360, event: "speakModeChanged", payload: SpeakMode.Hold },
    { tMs: 8_460, event: "speakModeChanged", payload: SpeakMode.Stop },
  ]);
}

/**
 * The nightly sync, in the two phases ARCHITECTURE.md §2.1 forces.
 *
 * 173 MB of Opus — one 16-hour day — over the glasses' access point at 2 MB/s is
 * about 87 seconds. Over BLE the same file is about 16 hours, which is longer
 * than the day took to record, and is the entire reason this ritual exists.
 *
 * Phase 1 is here: the phone joins the glasses' AP and *loses its own uplink*
 * while it does. Phase 2 — rejoin home WiFi, push to the box — cannot overlap.
 */
function nightlySync() {
  const dayBytes = 173 * MEGABYTE;
  const durationMs = (dayBytes / 2_000_000) * 1000;

  const b = new TraceBuilder<AllGlassesEvents>({
    device: { model: "M01 Pro", firmware: "synthetic" },
    notes:
      "Synthetic nightly sync, phase 1 of 2. While the AP is open the phone has " +
      "no uplink of its own, so nothing can be pushed to relayd until it closes. " +
      "173 MB at 2 MB/s is ~87 s; the same file over BLE would be ~16 h.",
  });

  b.event(0, "connectionChanged", ConnectionState.Connected);
  b.event(200, "battery", { percent: 41, charging: true });
  b.event(400, "diskInfo", { totalBytes: 4 * 1024 ** 3, freeBytes: 4 * 1024 ** 3 - dayBytes });

  tx(b, 800, Command.GET_FILE_LIST);
  tx(b, 1_000, Command.WIFI_AP_CONTROL, new Uint8Array([0x01]), "open the glasses' AP");
  rx(b, 1_200, Command.AP_SSID_REPORT, new TextEncoder().encode("QCGlasses-MOCK"), CommandType.Notify);
  rx(b, 1_260, Command.AP_PASSWORD_REPORT, new TextEncoder().encode("12345678"), CommandType.Notify);
  tx(b, 1_400, Command.FILE_FETCH_START, new Uint8Array([0x00]), "REC_0001.opus");

  const steps = 18;
  const progress: Array<{ tMs: number; event: AllGlassesEventName; payload: unknown }> = [];
  for (let i = 1; i <= steps; i++) {
    progress.push({
      tMs: Math.round(1_400 + (durationMs * i) / steps),
      event: "fileTransferProgress",
      payload: {
        id: 1,
        name: "REC_0001.opus",
        totalBytes: dayBytes,
        receivedBytes: Math.round((dayBytes * i) / steps),
        via: "wifiAp",
        retries: 0,
      },
    });
  }

  const closedAt = Math.round(1_400 + durationMs + 600);
  tx(b, closedAt, Command.WIFI_AP_CONTROL, new Uint8Array([0x00]), "close the AP — phone gets its uplink back");

  const trace = b.build();
  return extend(trace, [
    {
      tMs: 1_100,
      event: "wifiAccessPointChanged",
      payload: {
        ssid: "QCGlasses-MOCK",
        password: "12345678",
        host: "192.168.31.1",
        macAddress: "02:00:00:00:00:00",
        open: true,
        phoneUplinkSuspended: true,
      },
    },
    { tMs: 1_150, event: "wifiOperation", payload: { operation: "apOpen", ok: true } },
    ...progress,
    {
      tMs: closedAt + 80,
      event: "wifiAccessPointChanged",
      payload: {
        ssid: "QCGlasses-MOCK",
        password: "12345678",
        host: "192.168.31.1",
        macAddress: "02:00:00:00:00:00",
        open: false,
        phoneUplinkSuspended: false,
      },
    },
    { tMs: closedAt + 120, event: "wifiOperation", payload: { operation: "apClose", ok: true } },
  ]);
}

/**
 * Storage under pressure — the case APPS-SCOPE.md §3.2 warns about. A few
 * minutes of 1080p video evicts a whole day of audio, and the un-uploaded flag
 * is the only thing standing between a full device and lost capture.
 */
function storagePressure() {
  const b = new TraceBuilder<AllGlassesEvents>({
    device: { model: "M01 Pro", firmware: "synthetic" },
    notes:
      "Synthetic storage-pressure session. Video at ~4.5 GB/h fills 4 GB in " +
      "well under an hour; the app has to notice before the day's audio is at " +
      "risk. 0x0911 clears exactly the files that have not been synced.",
  });

  b.event(0, "connectionChanged", ConnectionState.Connected);
  b.event(200, "diskInfo", { totalBytes: 4 * 1024 ** 3, freeBytes: 3.6 * 1024 ** 3 });

  tx(b, 1_000, Command.DEVICE_CONTROL, new Uint8Array([DeviceMode.Video]), "start video");
  rx(b, 1_100, Command.LOCAL_VIDEO_STATE_REPORT, new Uint8Array([0x01]), CommandType.Notify);

  // ~4.5 GB/h. Forty minutes of it takes a 4 GB device from comfortable to
  // nearly full, which is the failure the app has to see coming.
  const videoRate = (4.5 * 1024 ** 3) / 3600;
  const videoMs = 40 * 60_000;
  for (let t = 5 * 60_000; t <= videoMs; t += 5 * 60_000) {
    const used = 0.4 * 1024 ** 3 + (t / 1000) * videoRate;
    b.event(1_000 + t, "diskInfo", {
      totalBytes: 4 * 1024 ** 3,
      freeBytes: Math.max(0, Math.round(4 * 1024 ** 3 - used)),
    });
  }

  const stoppedAt = 1_000 + videoMs + 500;
  tx(b, stoppedAt, Command.DEVICE_CONTROL, new Uint8Array([DeviceMode.VideoStop]), "stop video");
  tx(b, stoppedAt + 500, Command.GET_DISK_INFO);
  tx(b, stoppedAt + 1_000, Command.CLEAR_UNUPLOADED_FILES, new Uint8Array(), "destructive — confirm first");
  rx(b, stoppedAt + 1_500, Command.CLEAR_RESULT_REPORT, new Uint8Array(), CommandType.Notify);

  const trace = b.build();
  return extend(trace, [
    { tMs: 1_160, event: "captureState", payload: { kind: "video", active: true, durationS: 0 } },
    { tMs: stoppedAt + 60, event: "captureState", payload: { kind: "video", active: false, durationS: videoMs / 1000 } },
    { tMs: stoppedAt + 1_560, event: "clearResult", payload: { deletedFiles: 3, freedBytes: 1_612_349_440 } },
  ]);
}

for (const [name, trace] of [
  ["desk-session", deskSession()],
  ["flaky-link", flakySession()],
  ["command-surface", commandSurface()],
  ["voice-turn", voiceTurn()],
  ["nightly-sync", nightlySync()],
  ["storage-pressure", storagePressure()],
] as const) {
  const path = join(here, `${name}.trace.json`);
  writeFileSync(path, serialiseTrace(trace));
  console.log(
    `wrote ${name}.trace.json — ${trace.events.length} events, ${trace.frames?.length ?? 0} frames`,
  );
}
