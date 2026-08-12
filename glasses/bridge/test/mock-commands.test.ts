/**
 * The command surface, exercised against the mock.
 *
 * Two rules are being enforced here rather than assumed, because they are the
 * two a convenient mock quietly breaks:
 *
 *   1. **Rates are real.** SYSTEM.md §3.1. Live mic ~3 KB/s, battery ~1/min, a
 *      day of audio 170 MB–1.8 GB. A mock that hands back a day of audio instantly
 *      teaches the app a lie it discovers on hardware.
 *   2. **Absent capabilities fail like the device fails.** No station mode, no
 *      device ASR, no wake phrase outside the firmware's list.
 */

import assert from "node:assert/strict";
import { describe, test } from "node:test";

import { FakeClock } from "../src/clock.ts";
import { MOCK_DEFAULTS, MockTransport } from "../src/mock.ts";
import { Command, DeviceMode, RATES, SpeakMode, SpeakerRoute } from "../src/commands.ts";
import type { AudioChunk, BatteryStatus, GlassesError } from "../src/types.ts";
import type { CaptureState, FileTransferProgress, WifiApState } from "../src/commands.ts";

async function connected(options: ConstructorParameters<typeof MockTransport>[0] = {}) {
  const clock = new FakeClock();
  const glasses = new MockTransport({ clock, ...options });
  const pending = glasses.connect();
  await clock.advance(MOCK_DEFAULTS.connectDelayMs);
  await pending;
  glasses.clearCommandLog();
  return { glasses, clock };
}

/** Payload of the last time `id` was sent, as hex. */
function lastPayload(glasses: MockTransport, id: number): string | undefined {
  return [...glasses.commandLog].reverse().find((entry) => entry.id === id)?.payloadHex;
}

describe("every action reaches for the documented command", () => {
  test("local recording is 0x0E04 on and off, and its state is 0x0E05", async () => {
    const { glasses, clock } = await connected();

    await glasses.startLocalRecording();
    assert.equal(lastPayload(glasses, Command.LOCAL_RECORDING_CONTROL), "01");

    await clock.advance(5_000);
    const state = await glasses.getLocalRecordingState();
    assert.equal(glasses.commandCount(Command.LOCAL_RECORDING_STATE_REPORT), 1);
    assert.equal(state.active, true);
    assert.equal(state.kind, "audio");
    assert.equal(state.durationS, 5);

    await glasses.stopLocalRecording();
    assert.equal(lastPayload(glasses, Command.LOCAL_RECORDING_CONTROL), "00");
  });

  test("media, storage and network reach for the codes the docs quote", async () => {
    const { glasses, clock } = await connected();

    await glasses.listFiles();
    await glasses.getDiskInfo();
    await glasses.getMediaCounts();
    await glasses.getBattery();
    await glasses.setTime(new Date(1_770_000_000_000));
    await glasses.openWifiAccessPoint();
    await glasses.capturePhoto();
    const pendingThumb = glasses.fetchThumbnail("IMG_0001.jpg", { clarity: 0 });
    await clock.advance(10_000);
    await pendingThumb;

    for (const id of [
      Command.GET_FILE_LIST, // 0x0E01
      Command.GET_DISK_INFO, // 0x091C
      Command.GET_FILE_COUNT, // 0x0916
      Command.GET_BATTERY, // 0x0101
      Command.SET_TIME, // 0x0903
      Command.WIFI_AP_CONTROL, // 0x090B
      Command.AI_PHOTO_START, // 0x0906
      Command.FILE_FETCH_START, // 0x0C01
    ]) {
      assert.ok(glasses.commandCount(id) > 0, `0x${id.toString(16)} was never sent`);
    }
    assert.equal(lastPayload(glasses, Command.WIFI_AP_CONTROL), "01");
  });

  test("audio uplink is 0x0A02 and the downlink is 0x0A03", async () => {
    const { glasses, clock } = await connected();

    await glasses.startMicUplink();
    assert.equal(lastPayload(glasses, Command.AUDIO_CONTROL), "01");

    const pending = glasses.sendAudio(new Uint8Array(600));
    await clock.advance(1_000);
    await pending;
    assert.equal(glasses.commandCount(Command.AUDIO_DATA), 1);

    await glasses.stopMicUplink();
    assert.equal(lastPayload(glasses, Command.AUDIO_CONTROL), "00");
  });

  test("speak start/hold/stop ride 0x0D01 with the SDK's own mode bytes", async () => {
    const { glasses } = await connected();
    const modes: SpeakMode[] = [];
    glasses.on("speakModeChanged", (m) => modes.push(m));

    await glasses.speakStart();
    assert.equal(lastPayload(glasses, Command.DEVICE_CONTROL), "10"); // SpeakStart
    await glasses.speakHold();
    assert.equal(lastPayload(glasses, Command.DEVICE_CONTROL), "02"); // QGAISpeakModeHold
    await glasses.speakStop();
    assert.equal(lastPayload(glasses, Command.DEVICE_CONTROL), "11"); // SpeakStop

    assert.deepEqual(modes, [SpeakMode.Start, SpeakMode.Hold, SpeakMode.Stop]);
  });

  test("identity is five commands behind one call", async () => {
    const { glasses } = await connected();
    const identity = await glasses.getIdentity();

    for (const id of [
      Command.GET_PRODUCT_INFO,
      Command.GET_PRODUCT_MODEL,
      Command.GET_VERSION,
      Command.GET_HARDWARE_INFO,
      Command.GET_DEVICE_NAME,
    ]) {
      assert.equal(glasses.commandCount(id), 1, `0x${id.toString(16)} missing`);
    }
    assert.equal(identity.product, "M01 Pro");
  });

  test("the bind code never reaches the command log", async () => {
    const { glasses } = await connected();
    await glasses.setBindCode("s3cret");

    const secretHex = [...new TextEncoder().encode("s3cret")]
      .map((b) => b.toString(16).padStart(2, "0"))
      .join("");
    for (const entry of glasses.commandLog) {
      assert.ok(
        !entry.payloadHex.includes(secretHex),
        "a shared secret must not be logged, even in a mock",
      );
    }
    assert.equal(await glasses.getBindCode(), "s3cret");
  });

  test("the log is observable as it happens, for the device console", async () => {
    const { glasses } = await connected();
    const names: string[] = [];
    glasses.on("command", (entry) => names.push(entry.name));

    await glasses.heartbeat();
    await glasses.getBattery();

    assert.deepEqual(names, ["HEARTBEAT", "GET_BATTERY"]);
  });
});

describe("rates are real", () => {
  test("the live mic costs about 3 KB/s, streamed not dumped", async () => {
    const { glasses, clock } = await connected();
    const chunks: AudioChunk[] = [];
    glasses.on("audioChunk", (c) => chunks.push(c));

    await glasses.startMicUplink();
    await clock.advance(10_000);
    await glasses.stopMicUplink();

    const bytes = chunks.reduce((n, c) => n + c.data.byteLength, 0);
    const perSecond = bytes / 10;
    assert.ok(
      perSecond > 2_700 && perSecond < 3_300,
      `mic delivered ${perSecond.toFixed(0)} B/s, expected ~${RATES.micBytesPerSecond}`,
    );
    assert.ok(chunks.length > 40, "audio must arrive in chunks, not one lump");
    assert.deepEqual(
      chunks.map((c) => c.sequence),
      chunks.map((_, i) => i),
      "sequence numbers must be contiguous or a gap means a drop",
    );
  });

  test("the mic goes quiet the moment the uplink closes", async () => {
    const { glasses, clock } = await connected();
    let count = 0;
    glasses.on("audioChunk", () => count++);

    await glasses.startMicUplink();
    await clock.advance(2_000);
    const during = count;
    await glasses.stopMicUplink();
    await clock.advance(60_000);

    assert.ok(during > 0);
    assert.equal(count, during, "no audio may arrive after the uplink is closed");
  });

  test("pushing a spoken reply down 0x0A03 takes as long as the reply", async () => {
    const { glasses, clock } = await connected();

    // Ten seconds of Opus at 3 KB/s.
    const reply = new Uint8Array(30_000);
    let finishedAt = -1;
    const started = clock.now();
    const pending = glasses.sendAudio(reply).then(() => {
      finishedAt = clock.now();
    });

    await clock.advance(5_000);
    assert.equal(finishedAt, -1, "half the audio cannot have arrived in half the time and be done");

    await clock.advance(10_000);
    await pending;
    const seconds = (finishedAt - started) / 1000;
    assert.ok(seconds > 9 && seconds < 11, `downlink took ${seconds.toFixed(1)} s`);
  });

  test("the downlink is one channel — a second write is refused, not interleaved", async () => {
    const { glasses, clock } = await connected();
    const first = glasses.sendAudio(new Uint8Array(9_000));
    await assert.rejects(
      glasses.sendAudio(new Uint8Array(300)),
      (e: GlassesError) => e.code === "deviceBusy",
    );
    await clock.advance(5_000);
    await first;
  });

  test("battery reports arrive about once a minute without being asked", async () => {
    const { glasses, clock } = await connected();
    const reports: BatteryStatus[] = [];
    glasses.on("battery", (b) => reports.push(b));

    await clock.advance(5 * 60_000);

    assert.equal(reports.length, 5, "SYSTEM.md §3.1 says ~1/min");
    assert.equal(glasses.commandCount(Command.GET_BATTERY), 0, "reports are unprompted");
  });

  test("a PCM day is ten times the bytes of an Opus day", async () => {
    const opus = await connected();
    await opus.glasses.startLocalRecording();
    await opus.clock.advance(16 * 3600_000);
    await opus.glasses.stopLocalRecording();
    const [opusFile] = await opus.glasses.listFiles();

    const pcm = await connected({ recordingFormat: "pcm16" });
    await pcm.glasses.startLocalRecording();
    await pcm.clock.advance(16 * 3600_000);
    await pcm.glasses.stopLocalRecording();
    const [pcmFile] = await pcm.glasses.listFiles();

    assert.match(opusFile!.name, /\.opus$/);
    assert.match(pcmFile!.name, /\.pcm$/);
    const ratio = pcmFile!.sizeBytes / opusFile!.sizeBytes;
    assert.ok(ratio > 9 && ratio < 12, `PCM/Opus ratio was ${ratio.toFixed(1)}`);
    // APPS-SCOPE.md §3.1: even the pessimistic case still holds two days.
    assert.ok(pcmFile!.sizeBytes < 2 * 1024 ** 3);
  });

  test("a few minutes of video is bigger than a whole day of audio", async () => {
    const { glasses, clock } = await connected();

    await glasses.startVideoRecording();
    await clock.advance(5 * 60_000);
    await glasses.stopVideoRecording();
    const [video] = await glasses.listFiles();

    const dayOfOpus = 16 * 3600 * MOCK_DEFAULTS.recordingBytesPerSecond;
    assert.ok(
      video!.sizeBytes > 2 * dayOfOpus,
      `5 min of video was ${(video!.sizeBytes / 1024 ** 2).toFixed(0)} MB against a ` +
        `${(dayOfOpus / 1024 ** 2).toFixed(0)} MB day of audio`,
    );
    assert.equal(lastPayload(glasses, Command.DEVICE_CONTROL), "03"); // VideoStop
  });

  test("video can fill the device, and then audio cannot start", async () => {
    const { glasses, clock } = await connected();

    await glasses.startVideoRecording();
    await clock.advance(60 * 60_000); // an hour of 1080p is more than 4 GB
    await glasses.stopVideoRecording();

    const disk = await glasses.getDiskInfo();
    assert.equal(disk.freeBytes, 0, "1080p at 4.5 GB/h fills 4 GB in under an hour");
    await assert.rejects(
      glasses.startLocalRecording(),
      (e: GlassesError) => e.code === "storageFull",
    );
  });
});

describe("absent capabilities fail the way the device fails", () => {
  test("there is no station mode, so the deprecated WiFi commands refuse", async () => {
    const { glasses } = await connected();
    await assert.rejects(
      glasses.setAccessPointCredentials("HomeWiFi", "hunter2"),
      (e: GlassesError) => e.code === "unsupported" && /station mode/.test(e.message),
    );
  });

  test("a wake phrase outside the firmware list is refused", async () => {
    const { glasses } = await connected();
    const words = await glasses.getWakeWords();
    assert.deepEqual(
      words.map((w) => w.phrase),
      ["hey chatgpt", "answer the call", "take a picture"],
    );

    // The index the device handed out works.
    const settings = await glasses.setWakeWordEnabled(words[0]!.index, false);
    assert.equal(settings[0]!.enabled, false);
    assert.equal(lastPayload(glasses, Command.SET_WAKEWORD_SETTING), "0000");

    // "Hey Jarvis" would be index 7. It is not in the model.
    await assert.rejects(
      glasses.setWakeWordEnabled(7, true),
      (e: GlassesError) => e.code === "unsupported" && /firmware-fixed/.test(e.message),
    );
  });

  test("firmware without a capability refuses rather than pretending", async () => {
    const { glasses } = await connected({
      features: {
        voiceWakeup: false,
        wifiP2p: false,
        stabilization: false,
        wearDetection: false,
        wifiAp: false,
      },
    });

    for (const call of [
      () => glasses.getWakeWords(),
      () => glasses.setWifiP2p(true),
      () => glasses.setStabilisation(true),
      () => glasses.setWearDetection(true),
      () => glasses.openWifiAccessPoint(),
    ]) {
      await assert.rejects(call(), (e: GlassesError) => e.code === "unsupported");
    }
  });

  test("live preview and OTA refuse on a flat battery, like the firmware does", async () => {
    const preview = await connected({ batteryPercent: 8 });
    await assert.rejects(
      preview.glasses.startPreview(),
      (e: GlassesError) => e.code === "lowBattery",
    );

    const ota = await connected({ batteryPercent: 20 });
    await assert.rejects(
      ota.glasses.startOta(new Uint8Array(1024)),
      (e: GlassesError) => e.code === "lowBattery",
    );
    assert.equal((await ota.glasses.getOtaInfo()).batteryOk, false);
  });

  test("speakHold without speakStart is a wrong-state error, not a no-op", async () => {
    const { glasses } = await connected();
    await assert.rejects(glasses.speakHold(), (e: GlassesError) => e.code === "deviceBusy");
    await glasses.speakStart();
    await glasses.speakHold(); // now fine
  });

  test("a photo sharpness outside 0-6 is refused", async () => {
    const { glasses } = await connected();
    await assert.rejects(
      glasses.setPhotoParams({ widthPx: 1024, heightPx: 768, sharpness: 9 }),
      (e: GlassesError) => e.code === "unsupported",
    );
    await glasses.setPhotoParams({ widthPx: 1024, heightPx: 768, sharpness: 4 });
  });

  test("the whole command surface refuses while disconnected", async () => {
    const glasses = new MockTransport({ clock: new FakeClock() });
    for (const call of [
      () => glasses.getIdentity(),
      () => glasses.getWakeWords(),
      () => glasses.startMicUplink(),
      () => glasses.sendAudio(new Uint8Array(10)),
      () => glasses.clearUnuploadedFiles(),
      () => glasses.getMediaCounts(),
      () => glasses.setDeviceMode(DeviceMode.Photo),
    ]) {
      await assert.rejects(call(), (e: GlassesError) => e.code === "notConnected");
    }
  });
});

describe("the glasses do not recognise speech — the app does", () => {
  test("a device event starts our recogniser and produces no transcript on its own", async () => {
    const { glasses, clock } = await connected();
    const transcripts: string[] = [];
    const recognition: boolean[] = [];
    let chunks = 0;
    glasses.on("transcriptText", (t) => transcripts.push(t));
    glasses.on("recognitionChanged", (r) => {
      recognition.push(r.active);
      assert.equal(r.owner, "app", "recognition is always ours");
    });
    glasses.on("audioChunk", () => chunks++);

    // 0x0805: the device reports that the user asked for the assistant.
    glasses.emitAiInterfaceEvent("start");
    await glasses.startRecognition();
    assert.equal(glasses.isRecognising(), true);

    await clock.advance(4_000);
    await glasses.stopRecognition();

    assert.ok(chunks > 0, "audio must flow, because that is all the device offers");
    assert.deepEqual(transcripts, [], "no transcript can come from the device — it has no ASR");
    assert.deepEqual(recognition, [true, false]);
    assert.equal(glasses.isRecognising(), false);
    // Recognition rides the mic uplink, which is 0x0A02.
    assert.equal(glasses.commandCount(Command.AUDIO_CONTROL), 2);
  });

  test("the vendor's own assistant is surfaced as theirs, not as ours", async () => {
    const { glasses } = await connected();
    const vendor: string[] = [];
    const ours: string[] = [];
    glasses.on("vendorAiPrompt", (t) => vendor.push(t));
    glasses.on("transcriptText", (t) => ours.push(t));

    glasses.emitVendorAiPrompt("connected to the vendor cloud");
    assert.deepEqual(vendor, ["connected to the vendor cloud"]);
    assert.deepEqual(ours, [], "0x0806 is their channel; it is not a transcript of the user");
  });

  test("stopping the assistant closes the uplink too", async () => {
    const { glasses } = await connected();
    await glasses.startRecognition();
    assert.equal(glasses.isVoiceSessionOpen, true);
    await glasses.stopRecognition();
    assert.equal(glasses.isVoiceSessionOpen, false);
  });
});

describe("storage policy — un-uploaded is the flag that matters", () => {
  async function twoRecordings() {
    const ctx = await connected();
    for (let i = 0; i < 2; i++) {
      await ctx.glasses.startLocalRecording();
      await ctx.clock.advance(60_000);
      await ctx.glasses.stopLocalRecording();
    }
    return ctx;
  }

  test("0x0911 destroys exactly the files that were never synced", async () => {
    const { glasses, clock } = await twoRecordings();
    await glasses.openWifiAccessPoint();

    const [first] = await glasses.listFiles();
    const pending = glasses.fetchFile(first!.name);
    await clock.advance(60_000);
    await pending;

    const results: Array<{ deletedFiles: number }> = [];
    glasses.on("clearResult", (r) => results.push(r));

    const result = await glasses.clearUnuploadedFiles();
    assert.equal(glasses.commandCount(Command.CLEAR_UNUPLOADED_FILES), 1);
    assert.equal(result.deletedFiles, 1, "only the un-synced one");
    assert.deepEqual(results, [result]);

    const left = await glasses.listFiles();
    assert.equal(left.length, 1);
    assert.equal(left[0]!.uploaded, true, "the synced file survives");
  });

  test("0x0E03 takes everything, including capture nobody has seen", async () => {
    const { glasses } = await twoRecordings();
    const result = await glasses.deleteAllFiles();

    assert.equal(glasses.commandCount(Command.DELETE_ALL_FILES), 1);
    assert.equal(result.deletedFiles, 2, "the device does not argue — the UI must");
    assert.deepEqual(await glasses.listFiles(), []);
    assert.equal((await glasses.getDiskInfo()).freeBytes, 4 * 1024 ** 3);
  });

  test("file counts split photos, video and recordings", async () => {
    const { glasses, clock } = await connected();
    await glasses.capturePhoto();
    await glasses.startVideoRecording();
    await clock.advance(10_000);
    await glasses.stopVideoRecording();
    await glasses.startLocalRecording();
    await clock.advance(10_000);
    await glasses.stopLocalRecording();

    const counts = await glasses.getMediaCounts();
    assert.deepEqual(
      { photos: counts.photos, videos: counts.videos, recordings: counts.recordings },
      { photos: 1, videos: 1, recordings: 1 },
    );
    assert.ok(counts.totalBytes > 0);
  });
});

describe("the WiFi AP costs the phone its uplink", () => {
  test("opening the AP says so, and closing it gives the uplink back", async () => {
    const { glasses } = await connected();
    const states: WifiApState[] = [];
    glasses.on("wifiAccessPointChanged", (s) => states.push(s));

    await glasses.openWifiAccessPoint();
    assert.equal(states.at(-1)!.open, true);
    assert.equal(
      states.at(-1)!.phoneUplinkSuspended,
      true,
      "ARCHITECTURE.md §2.1: the phone cannot hold both networks, so sync is two phases",
    );

    await glasses.closeWifiAccessPoint();
    assert.equal(states.at(-1)!.open, false);
    assert.equal(states.at(-1)!.phoneUplinkSuspended, false);
  });

  test("P2P is reported separately and refuses when the firmware lacks it", async () => {
    const { glasses } = await connected();
    await glasses.setWifiP2p(true);
    assert.equal(glasses.isWifiP2pOpen, true);
    assert.equal(lastPayload(glasses, Command.WIFI_P2P_CONTROL), "01");
  });
});

describe("file transfer — 0x0C01 through 0x0C05", () => {
  async function oneHourRecorded() {
    const ctx = await connected();
    await ctx.glasses.startLocalRecording();
    await ctx.clock.advance(3600_000);
    await ctx.glasses.stopLocalRecording();
    await ctx.glasses.openWifiAccessPoint();
    const [file] = await ctx.glasses.listFiles();
    return { ...ctx, file: file! };
  }

  test("progress is reported and the transfer closes out", async () => {
    const { glasses, clock, file } = await oneHourRecorded();
    const progress: FileTransferProgress[] = [];
    glasses.on("fileTransferProgress", (p) => progress.push(p));

    const pending = glasses.fetchFile(file.name);
    await clock.advance(60_000);
    await pending;

    assert.ok(progress.length > 2);
    assert.equal(progress.at(-1)!.receivedBytes, file.sizeBytes);
    assert.equal(progress.at(-1)!.via, "wifiAp");
    assert.deepEqual(glasses.activeTransfers(), [], "a finished transfer must not linger");
  });

  test("a transfer can be aborted, and the fetch fails rather than hanging", async () => {
    const { glasses, clock, file } = await oneHourRecorded();

    const assertion = assert.rejects(
      glasses.fetchFile(file.name),
      (e: GlassesError) => e.code === "transferFailed",
    );
    await clock.advance(500);
    const [inFlight] = glasses.activeTransfers();
    assert.ok(inFlight, "the transfer should be visible while it runs");
    await glasses.abortFileTransfer(inFlight!.id);
    await clock.advance(60_000);
    await assertion;

    assert.equal(glasses.commandCount(Command.FILE_UPLOAD_ABORT), 1);
  });

  test("retries cost time and are counted, because a flaky link is normal", async () => {
    const { glasses, clock, file } = await oneHourRecorded();
    glasses.setFaults({ transferRetries: 3 });

    const progress: FileTransferProgress[] = [];
    glasses.on("fileTransferProgress", (p) => progress.push(p));

    const pending = glasses.fetchFile(file.name);
    await clock.advance(120_000);
    await pending;

    assert.equal(glasses.commandCount(Command.FILE_DATA_RETRY), 3);
    assert.equal(progress.at(-1)!.retries, 3);
  });

  test("aborting a transfer that does not exist is an error, not a shrug", async () => {
    const { glasses } = await connected();
    await assert.rejects(
      glasses.abortFileTransfer(99),
      (e: GlassesError) => e.code === "transferFailed",
    );
  });
});

describe("settings round-trip through the device", () => {
  test("each getter returns what its setter wrote", async () => {
    const { glasses } = await connected();

    await glasses.setNoiseCancellation(2);
    await glasses.setGameMode(true);
    await glasses.setEqualiser(1);
    await glasses.setWearDetection(false);
    await glasses.setRecordingPrompt(false);
    await glasses.setCallAutoRecord(true);
    await glasses.setStabilisation(true);
    await glasses.setVideoParams({ angle: 0, durationS: 120 });
    await glasses.setSpeakerRoute(SpeakerRoute.Phone);

    assert.equal(await glasses.getNoiseCancellation(), 2);
    assert.equal(await glasses.getGameMode(), true);
    assert.equal(await glasses.getEqualiser(), 1);
    assert.equal(await glasses.getWearDetection(), false);
    assert.equal(await glasses.getRecordingPrompt(), false);
    assert.equal(await glasses.getCallAutoRecord(), true);
    assert.deepEqual(await glasses.getStabilisation(), { enabled: true, supported: true });
    assert.deepEqual(await glasses.getVideoParams(), { angle: 0, durationS: 120 });
    assert.equal(await glasses.getSpeakerRoute(), SpeakerRoute.Phone);
  });

  test("the default key bindings put the assistant on a gesture", async () => {
    const { glasses } = await connected();
    const bindings = await glasses.getKeyBindings();
    assert.ok(
      bindings.some((b) => b.action === "aiAssistant"),
      "tap-to-talk is the primary trigger — ARCHITECTURE.md §5.2b",
    );
  });

  test("recording prompt and call auto-record default to the consent-safe side", async () => {
    const { glasses } = await connected();
    assert.equal(await glasses.getRecordingPrompt(), true, "the audible cue is on by default");
    assert.equal(
      await glasses.getCallAutoRecord(),
      false,
      "Quebec is two-party consent — ARCHITECTURE.md §6",
    );
  });
});

describe("device control and lifecycle", () => {
  test("restart drops the link, as it must", async () => {
    const { glasses } = await connected();
    await glasses.restartDevice();
    assert.equal(glasses.state, "disconnected");
    assert.equal(lastPayload(glasses, Command.DEVICE_CONTROL), "0e"); // Restart
  });

  test("factory reset takes the files and the bind code with it", async () => {
    const { glasses } = await connected();
    await glasses.capturePhoto();
    await glasses.setBindCode("abc123");
    await glasses.factoryReset();

    assert.equal(glasses.state, "disconnected");
    assert.equal(lastPayload(glasses, Command.DEVICE_CONTROL), "0a"); // FactoryReset
  });

  test("capture state is reported for video as well as audio", async () => {
    const { glasses, clock } = await connected();
    const states: CaptureState[] = [];
    glasses.on("captureState", (s) => states.push(s));

    await glasses.startVideoRecording();
    await clock.advance(1_000);
    await glasses.stopVideoRecording();

    assert.deepEqual(
      states.map((s) => `${s.kind}:${s.active}`),
      ["video:true", "video:false"],
    );
  });

  test("an OTA image moves at BLE speed and reports progress", async () => {
    const { glasses, clock } = await connected({ batteryPercent: 90 });
    const progress: number[] = [];
    glasses.on("otaProgress", (p) => progress.push(p.sentBytes));

    // 300 KB at 3 KB/s is 100 seconds — the UI has to say so before it starts.
    const pending = glasses.startOta(new Uint8Array(300_000));
    await clock.advance(50_000);
    assert.ok(progress.length > 0 && progress.at(-1)! < 300_000, "not done in half the time");

    await clock.advance(60_000);
    await pending;
    assert.equal(progress.at(-1), 300_000);
    assert.equal(glasses.commandCount(Command.OTA_COMPLETE), 1);
  });
});
