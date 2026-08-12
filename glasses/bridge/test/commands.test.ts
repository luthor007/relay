/**
 * The command table, checked against the thing that owns it.
 *
 * `glasses/protocol/commands.py` is the source of truth for command IDs — it was
 * built from the spec and is covered by 92 Python tests. This suite re-parses
 * that file on every run rather than trusting a transcription, so the two cannot
 * drift without something going red.
 */

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, test } from "node:test";

import {
  COMMAND_CATALOG,
  Command,
  CommandRole,
  CommandType,
  DeviceMode,
  FrameError,
  RATES,
  SHARPNESS_MAX,
  SequenceCounter,
  SpeakMode,
  WakeWordKind,
  commandName,
  crc16,
  decodeFrame,
  decodePacket,
  decodeWakeWordList,
  decodeWakeWordSettings,
  describeCommand,
  destructiveCommands,
  encodeCommandFrame,
  encodeFrame,
  encodePacket,
  encodeWakeWordList,
  encodeWakeWordSettings,
  isKnownCommand,
  recordingBytes,
  transferMs,
} from "../src/commands.ts";
import type { AllGlassesEventName, CommandName, GlassesCommands } from "../src/commands.ts";
import { MockTransport } from "../src/mock.ts";

const here = dirname(fileURLToPath(import.meta.url));
const PROTOCOL_COMMANDS_PY = join(here, "..", "..", "protocol", "commands.py");

/** Pull `NAME = 0xNNNN` out of the `Command(IntEnum)` block in commands.py. */
function pythonCommandTable(): Map<string, number> {
  const source = readFileSync(PROTOCOL_COMMANDS_PY, "utf8");
  const start = source.indexOf("class Command(IntEnum):");
  assert.ok(start >= 0, "commands.py must still define class Command(IntEnum)");
  const body = source.slice(start).split("\n\n\n")[0]!;

  const table = new Map<string, number>();
  for (const line of body.split("\n")) {
    const match = /^ {4}([A-Z0-9_]+) = (0x[0-9A-Fa-f]{4})\b/.exec(line);
    if (match) table.set(match[1]!, Number.parseInt(match[2]!, 16));
  }
  return table;
}

// Compile-time exhaustiveness for the event list below. If a new event is added
// to GlassesCommandEvents and not listed, this fails to typecheck.
type AssertNever<T extends never> = T;
type _NoMissingEvents = AssertNever<Exclude<AllGlassesEventName, (typeof EVENT_NAMES)[number]>>;
type _NoExtraEvents = AssertNever<Exclude<(typeof EVENT_NAMES)[number], AllGlassesEventName>>;

const EVENT_NAMES = [
  // base transport (types.ts)
  "connectionChanged",
  "battery",
  "touch",
  "wear",
  "audioChunk",
  "voiceSessionChanged",
  "transcriptText",
  "photoProgress",
  "photo",
  "recordingState",
  "diskInfo",
  "rtspUrl",
  "error",
  // command surface (commands.ts)
  "command",
  "aiInterfaceEvent",
  "recognitionChanged",
  "vendorAiPrompt",
  "speakModeChanged",
  "mediaCounts",
  "captureState",
  "clearResult",
  "runState",
  "wifiAccessPointChanged",
  "wifiOperation",
  "wifiP2pChanged",
  "fileTransferProgress",
  "noiseCancellationChanged",
  "gameModeChanged",
  "findDeviceChanged",
  "videoResolutionChanged",
  "stabilisationSupportChanged",
  "wakeWordSettingsChanged",
  "firmwareVersion",
  "otaProgress",
] as const;

describe("command IDs come from glasses/protocol/commands.py", () => {
  test("every Python command exists here with the same value", () => {
    const python = pythonCommandTable();
    assert.equal(python.size, 92, "commands.py should still carry all 92 spec commands");

    for (const [name, id] of python) {
      const ours = (Command as Record<string, number | undefined>)[name];
      assert.equal(ours, id, `${name}: Python says 0x${id.toString(16)}, TypeScript says ${ours}`);
    }
  });

  test("this file invents nothing the Python does not have", () => {
    const python = pythonCommandTable();
    for (const [name, id] of Object.entries(Command)) {
      assert.ok(python.has(name), `${name} (0x${id.toString(16)}) is not in commands.py`);
    }
    assert.equal(Object.keys(Command).length, python.size);
  });

  test("the checklist commands are present with the codes the docs quote", () => {
    // BUILD-PROMPT's row, spelled out. Each of these is quoted by name in the
    // task and in APPS-SCOPE.md; if the Python ever moved one, the test above
    // catches it, and this one says which product feature broke.
    assert.equal(Command.LOCAL_RECORDING_CONTROL, 0x0e04);
    assert.equal(Command.LOCAL_RECORDING_STATE_REPORT, 0x0e05);
    assert.equal(Command.FILE_FETCH_START, 0x0c01);
    assert.equal(Command.FILE_DATA_UPLOAD, 0x0c02);
    assert.equal(Command.FILE_UPLOAD_END, 0x0c03);
    assert.equal(Command.FILE_DATA_RETRY, 0x0c04);
    assert.equal(Command.FILE_UPLOAD_ABORT, 0x0c05);
    assert.equal(Command.CLEAR_UNUPLOADED_FILES, 0x0911);
    assert.equal(Command.DISK_INFO, 0x0909);
    assert.equal(Command.GET_DISK_INFO, 0x091c);
    assert.equal(Command.WIFI_AP_CONTROL, 0x090b);
    assert.equal(Command.AUDIO_CONTROL, 0x0a02);
    assert.equal(Command.AUDIO_DATA, 0x0a03);
    assert.equal(Command.GET_BATTERY, 0x0101);
    assert.equal(Command.GET_WAKEWORD_LIST, 0x0f01);
    assert.equal(Command.GET_WAKEWORD_SETTING, 0x0f02);
    assert.equal(Command.SET_WAKEWORD_SETTING, 0x0f03);
    assert.equal(Command.AI_CHAT_EVENT_UNUSED, 0x0803);
    assert.equal(Command.AI_CHAT_TRIGGER, 0x0805);
  });

  test("names round-trip, and an unknown ID is labelled rather than dropped", () => {
    assert.equal(commandName(0x0e04), "LOCAL_RECORDING_CONTROL");
    assert.equal(commandName(0x0a03), "AUDIO_DATA");
    assert.equal(isKnownCommand(0x0a03), true);
    // Matches Packet.name in commands.py exactly, so logs from both sides diff.
    assert.equal(commandName(0x1234), "UNKNOWN_0x1234");
    assert.equal(isKnownCommand(0x1234), false);
  });
});

describe("catalog — every command is reachable by hand", () => {
  test("covers exactly the command table, once each", () => {
    const seen = new Set<number>();
    for (const entry of COMMAND_CATALOG) {
      assert.equal(
        (Command as Record<string, number>)[entry.name],
        entry.id,
        `${entry.name} has the wrong id in the catalog`,
      );
      assert.ok(!seen.has(entry.id), `${entry.name} appears twice`);
      seen.add(entry.id);
    }
    assert.equal(seen.size, Object.keys(Command).length);
  });

  test("MockTransport satisfies the whole contract, capture loop included", () => {
    // The assignment is the assertion: `GlassesCommands` is GlassesTransport plus
    // GlassesCommandSet, so this fails to compile if either half is incomplete or
    // if the widened `on()` stops accepting the base event names.
    const glasses: GlassesCommands = new MockTransport();
    assert.equal(typeof glasses.startMicUplink, "function");
    assert.equal(typeof glasses.takePhoto, "function");
    const off = glasses.on("battery", () => {});
    off();
  });

  test("every method the catalog names exists on the transport", () => {
    const mock = new MockTransport() as unknown as Record<string, unknown>;
    for (const entry of COMMAND_CATALOG) {
      for (const method of entry.methods) {
        assert.equal(
          typeof mock[method],
          "function",
          `${entry.name} claims ${method}(), which does not exist`,
        );
      }
    }
  });

  test("every event the catalog names is one the transport can emit", () => {
    const known = new Set<string>(EVENT_NAMES);
    for (const entry of COMMAND_CATALOG) {
      for (const event of entry.events) {
        assert.ok(known.has(event), `${entry.name} claims event "${event}", which is not defined`);
      }
    }
  });

  test("nothing is left without a way to reach it", () => {
    for (const entry of COMMAND_CATALOG) {
      if (entry.role === CommandRole.Unused) {
        assert.equal(entry.methods.length, 0, `${entry.name} is 未使用 and must not be sent`);
        continue;
      }
      const reachable = entry.methods.length + entry.events.length;
      assert.ok(reachable > 0, `${entry.name} has neither a method nor an event`);
    }
  });

  test("reports are surfaced as events, not silently swallowed", () => {
    for (const entry of COMMAND_CATALOG) {
      if (entry.role !== CommandRole.Report) continue;
      assert.ok(entry.events.length > 0, `${entry.name} is a DEV->APP report with no event`);
    }
  });

  test("destructive commands are marked so the UI can confirm first", () => {
    const names = destructiveCommands().map((entry) => entry.name);
    assert.deepEqual(
      [...names].sort(),
      ["CLEAR_UNUPLOADED_FILES", "DELETE_ALL_FILES", "DELETE_FILE"],
      "anything that can destroy un-synced capture must be flagged",
    );
  });

  test("describeCommand answers for a real ID and stays quiet for a fake one", () => {
    const entry = describeCommand(Command.CLEAR_UNUPLOADED_FILES);
    assert.equal(entry?.name, "CLEAR_UNUPLOADED_FILES" satisfies CommandName);
    assert.equal(entry?.destructive, true);
    assert.equal(describeCommand(0x1234), undefined);
  });

  test("0x0A03 is documented as bidirectional and 0x0A02 as uplink only", () => {
    const data = describeCommand(Command.AUDIO_DATA)!;
    assert.equal(data.role, CommandRole.Both, "0x0A03 carries the mic up AND audio down");
    assert.ok(data.methods.includes("sendAudio"), "the downlink needs a method");
    assert.ok(data.events.includes("audioChunk"), "the uplink needs an event");

    const control = describeCommand(Command.AUDIO_CONTROL)!;
    assert.equal(control.role, CommandRole.Command);
    assert.match(control.note ?? "", /uplink only/);
  });

  test("the AI-interface commands are modelled as device reports, not device ASR", () => {
    const trigger = describeCommand(Command.AI_CHAT_TRIGGER)!;
    assert.equal(trigger.role, CommandRole.Report, "the glasses report; the app recognises");
    assert.deepEqual([...trigger.methods].sort(), ["startRecognition", "stopRecognition"]);

    const legacy = describeCommand(Command.AI_CHAT_EVENT_UNUSED)!;
    assert.ok(legacy.events.includes("aiInterfaceEvent"));

    // Nothing anywhere claims the device transcribes.
    for (const entry of COMMAND_CATALOG) {
      assert.ok(
        !entry.methods.includes("transcribe"),
        `${entry.name} must not offer device-side transcription`,
      );
    }
  });
});

describe("wire codec — the same bytes glasses/protocol produces", () => {
  // Golden values printed by glasses/protocol (python3, crc16 + Packet.to_frame).
  test("CRC-16/MODBUS, init 0xFFFF — not ARC", () => {
    assert.equal(crc16(new Uint8Array()), 0xffff, "empty input returns the init value");
    assert.equal(
      crc16(new TextEncoder().encode("123456789")),
      0x4b37,
      "the standard MODBUS check value; ARC would give 0xBB3D",
    );
    assert.equal(crc16(new Uint8Array(16).map((_, i) => i)), 0xe7b4);
  });

  test("golden frames match the Python byte for byte", () => {
    const cases: Array<[string, number, number, number, number[]]> = [
      ["a5060005000100000001b2", Command.GET_SUPPORTED_FEATURES, CommandType.Request, 0, []],
      ["a506000101010700008c37", Command.GET_BATTERY, CommandType.Request, 7, []],
      ["a507000b0901030100010cdd", Command.WIFI_AP_CONTROL, CommandType.Request, 3, [1]],
      ["a50700040e010c010001f17e", Command.LOCAL_RECORDING_CONTROL, CommandType.Request, 12, [1]],
      ["a50700050803000100019b48", Command.AI_CHAT_TRIGGER, CommandType.Notify, 0, [1]],
      ["a50700010d010501001067dd", Command.DEVICE_CONTROL, CommandType.Request, 5, [0x10]],
    ];

    for (const [expected, command, type, seq, payload] of cases) {
      const frame = encodeCommandFrame({
        command,
        type: type as CommandType,
        seq,
        payload: new Uint8Array(payload),
      });
      assert.equal(toHex(frame), expected, commandName(command));
    }
  });

  test("a frame round-trips back to the packet that made it", () => {
    const packet = {
      command: Command.SET_TIME,
      type: CommandType.Request,
      seq: 9,
      payload: new Uint8Array([0x00, 0x80, 0x0e, 0x69]),
    };
    const { data, consumed } = decodeFrame(encodeCommandFrame(packet));
    assert.equal(consumed, 3 + 6 + 4 + 2);
    const decoded = decodePacket(data);
    assert.equal(decoded.command, Command.SET_TIME);
    assert.equal(decoded.seq, 9);
    assert.deepEqual([...decoded.payload], [...packet.payload]);
  });

  test("a flipped bit is caught rather than acted on", () => {
    const frame = encodeCommandFrame({
      command: Command.DEVICE_CONTROL,
      type: CommandType.Request,
      seq: 1,
      payload: new Uint8Array([DeviceMode.FactoryReset]),
    });
    frame[7] = frame[7]! ^ 0x01; // corrupt the payload, leave the CRC
    assert.throws(() => decodeFrame(frame), FrameError);
  });

  test("a truncated frame is a wait, and a bad prefix is a fault", () => {
    const frame = encodeCommandFrame({
      command: Command.GET_BATTERY,
      type: CommandType.Request,
      seq: 0,
      payload: new Uint8Array(),
    });
    assert.throws(() => decodeFrame(frame.slice(0, 5)), /needs \d+ bytes/);
    const bad = new Uint8Array(frame);
    bad[0] = 0x5a;
    assert.throws(() => decodeFrame(bad), /prefix/);
  });

  test("the packet header is exactly the spec's table", () => {
    const raw = encodePacket({
      command: Command.GET_SUPPORTED_FEATURES,
      type: CommandType.Request,
      seq: 0x2a,
      payload: new Uint8Array([0xff]),
    });
    assert.deepEqual([...raw.slice(0, 2)], [0x05, 0x00], "command id, little-endian");
    assert.equal(raw[2], 1, "type: request");
    assert.equal(raw[3], 0x2a, "sequence number");
    assert.deepEqual([...raw.slice(4, 6)], [0x01, 0x00], "payload length, little-endian");
    assert.equal(raw[6], 0xff);
  });

  test("a length field that disagrees with the payload is rejected", () => {
    const raw = encodePacket({
      command: Command.GET_BATTERY,
      type: CommandType.Request,
      seq: 0,
      payload: new Uint8Array([1, 2]),
    });
    raw[4] = 99;
    assert.throws(() => decodePacket(raw), /length field/);
  });

  test("sequence numbers wrap at 255, because the device echoes one byte", () => {
    const counter = new SequenceCounter(254);
    assert.deepEqual([counter.next(), counter.next(), counter.next()], [254, 255, 0]);
    assert.throws(() => new SequenceCounter(256), RangeError);
  });

  test("encodeFrame carries the CRC little-endian after the data", () => {
    const data = new Uint8Array([1, 2, 3]);
    const frame = encodeFrame(data);
    const crc = crc16(data);
    assert.equal(frame[frame.length - 2], crc & 0xff);
    assert.equal(frame[frame.length - 1], (crc >>> 8) & 0xff);
  });
});

describe("wake words are a selection, never a phrase", () => {
  test("the 0x0F01 list round-trips through Index/Type/Len/Value", () => {
    const words = [
      { index: 0, kind: WakeWordKind.AiPhrase, phrase: "hey chatgpt" },
      { index: 1, kind: WakeWordKind.DeviceControl, phrase: "take a picture" },
    ];
    assert.deepEqual(decodeWakeWordList(encodeWakeWordList(words)), words);
  });

  test("a truncated entry is an error, not a half-read list", () => {
    const bytes = encodeWakeWordList([
      { index: 0, kind: WakeWordKind.AiPhrase, phrase: "hey chatgpt" },
    ]);
    assert.throws(() => decodeWakeWordList(bytes.slice(0, bytes.length - 3)), FrameError);
  });

  test("settings are index/enabled pairs and reject an odd payload", () => {
    const settings = [
      { index: 0, enabled: true },
      { index: 2, enabled: false },
    ];
    assert.deepEqual(decodeWakeWordSettings(encodeWakeWordSettings(settings)), settings);
    assert.throws(() => decodeWakeWordSettings(new Uint8Array([0, 1, 2])), FrameError);
  });
});

describe("rates are the documented ones", () => {
  test("a 16-hour day is ~173 MB of Opus and ~1.8 GB of PCM", () => {
    const day = 16 * 3600;
    const opusMb = recordingBytes(day, "opus") / 1024 ** 2;
    const pcmGb = recordingBytes(day, "pcm16") / 1024 ** 3;
    assert.ok(opusMb > 160 && opusMb < 180, `Opus day was ${opusMb.toFixed(0)} MB`);
    assert.ok(pcmGb > 1.6 && pcmGb < 1.9, `PCM day was ${pcmGb.toFixed(2)} GB`);
  });

  test("that day is ~16 hours over BLE and ~90 seconds over the access point", () => {
    const dayBytes = recordingBytes(16 * 3600, "opus");
    const bleHours = transferMs(dayBytes, "ble") / 3_600_000;
    const wifiSeconds = transferMs(dayBytes, "wifiAp") / 1000;
    assert.ok(bleHours > 15, `BLE would take ${bleHours.toFixed(1)} h — longer than the day`);
    assert.ok(wifiSeconds < 120, `AP took ${wifiSeconds.toFixed(0)} s`);
  });

  test("the constants themselves match SYSTEM.md §3.1", () => {
    assert.equal(RATES.micBytesPerSecond, 3_000);
    assert.equal(RATES.bleBytesPerSecond, 3_000);
    assert.equal(RATES.batteryReportIntervalMs, 60_000);
    assert.equal(SHARPNESS_MAX, 6);
    assert.equal(SpeakMode.Hold, 0x02);
    assert.equal(DeviceMode.SpeakStart, 0x10);
  });
});

function toHex(bytes: Uint8Array): string {
  let out = "";
  for (const byte of bytes) out += byte.toString(16).padStart(2, "0");
  return out;
}
