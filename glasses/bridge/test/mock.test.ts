import assert from "node:assert/strict";
import { describe, test } from "node:test";

import { FakeClock } from "../src/clock.ts";
import { MOCK_DEFAULTS, MockTransport } from "../src/mock.ts";
import { ConnectionState, GlassesError, TouchAction } from "../src/types.ts";
import type { ConnectionState as ConnectionStateType, PhotoProgress } from "../src/types.ts";

/** Connect and settle, returning the transport and its clock. */
async function connected(options: ConstructorParameters<typeof MockTransport>[0] = {}) {
  const clock = new FakeClock();
  const glasses = new MockTransport({ clock, ...options });
  const pending = glasses.connect();
  await clock.advance(MOCK_DEFAULTS.connectDelayMs);
  await pending;
  return { glasses, clock };
}

describe("connection lifecycle", () => {
  test("moves through connecting to connected and reports each transition", async () => {
    const clock = new FakeClock();
    const glasses = new MockTransport({ clock });
    const seen: ConnectionStateType[] = [];
    glasses.on("connectionChanged", (s) => seen.push(s));

    const pending = glasses.connect();
    assert.equal(glasses.state, ConnectionState.Connecting);

    await clock.advance(MOCK_DEFAULTS.connectDelayMs);
    await pending;

    assert.equal(glasses.state, ConnectionState.Connected);
    assert.deepEqual(seen, [ConnectionState.Connecting, ConnectionState.Connected]);
  });

  test("nothing happens until the clock is advanced", async () => {
    const clock = new FakeClock();
    const glasses = new MockTransport({ clock });
    void glasses.connect();
    await clock.advance(MOCK_DEFAULTS.connectDelayMs - 1);
    assert.equal(glasses.state, ConnectionState.Connecting);
  });

  test("connect failure leaves the transport disconnected", async () => {
    const clock = new FakeClock();
    const glasses = new MockTransport({ clock, faults: { connectFails: true } });
    // Attach the rejection handler before advancing, or the rejection surfaces
    // as unhandled while the fake clock is between macrotasks.
    const assertion = assert.rejects(
      glasses.connect(),
      (e: GlassesError) => e.code === "connectFailed",
    );
    await clock.advance(MOCK_DEFAULTS.connectDelayMs);
    await assertion;

    assert.equal(glasses.state, ConnectionState.Disconnected);
  });

  test("a timeout shorter than the connect delay rejects", async () => {
    const clock = new FakeClock();
    const glasses = new MockTransport({ clock });
    const assertion = assert.rejects(
      glasses.connect({ timeoutMs: 10 }),
      (e: GlassesError) => e.code === "timeout",
    );
    await clock.advance(MOCK_DEFAULTS.connectDelayMs);
    await assertion;
  });

  test("commands reject when not connected", async () => {
    const glasses = new MockTransport({ clock: new FakeClock() });
    await assert.rejects(glasses.getBattery(), (e: GlassesError) => e.code === "notConnected");
    await assert.rejects(glasses.takePhoto(), (e: GlassesError) => e.code === "notConnected");
    await assert.rejects(glasses.listFiles(), (e: GlassesError) => e.code === "notConnected");
  });

  test("the link can drop on its own and reports an error", async () => {
    const { glasses, clock } = await connected({ faults: { dropAfterMs: 5_000 } });
    const errors: GlassesError[] = [];
    glasses.on("error", (e) => errors.push(e));

    await clock.advance(5_000);

    assert.equal(glasses.state, ConnectionState.Disconnected);
    assert.equal(errors.length, 1);
    assert.equal(errors[0]!.code, "notConnected");
  });
});

describe("photo transfer — resolution is a latency dial", () => {
  test("a small photo completes in seconds and reports progress throughout", async () => {
    const { glasses, clock } = await connected();
    const progress: PhotoProgress[] = [];
    glasses.on("photoProgress", (p) => progress.push(p));

    const started = clock.now();
    let finishedAt = -1;
    const pending = glasses.takePhoto({ maxWidth: 320, maxHeight: 240 }).then((p) => {
      finishedAt = clock.now(); // captured when it settles, not when the advance ends
      return p;
    });
    await clock.advance(5_000);
    const photo = await pending;

    // 320 x 240 x 0.08 = 6144 bytes at 3000 B/s ~= 2.05 s
    const elapsed = finishedAt - started;
    assert.ok(elapsed >= 2_000 && elapsed <= 2_200, `elapsed was ${elapsed}ms`);

    assert.ok(progress.length > 1, "should report incremental progress, not one jump");
    assert.equal(progress.at(-1)!.receivedBytes, progress.at(-1)!.totalBytes);
    assert.equal(photo.mimeType, "image/jpeg");
    assert.deepEqual([...photo.data.slice(0, 2)], [0xff, 0xd8]);
  });

  test("a full-size photo takes far longer over the same link", async () => {
    const { glasses, clock } = await connected();

    const pending = glasses.takePhoto();
    let settled = false;
    void pending.then(() => {
      settled = true;
    });

    await clock.advance(5_000);
    assert.equal(settled, false, "full-size photo should not be done in 5s over BLE");

    await clock.advance(90_000);
    const photo = await pending;
    // 2048 x 1536 x 0.08 ~= 252 KB, ~84 s at 3 KB/s.
    assert.ok(photo.data.byteLength > 200_000);
  });

  test("photo transfer can fail partway", async () => {
    const { glasses, clock } = await connected({ faults: { photoFails: true } });
    const assertion = assert.rejects(
      glasses.takePhoto({ maxWidth: 320, maxHeight: 240 }),
      (e: GlassesError) => e.code === "transferFailed",
    );
    await clock.advance(5_000);
    await assertion;
  });

  test("disconnecting mid-transfer fails the in-flight photo", async () => {
    const { glasses, clock } = await connected();
    const assertion = assert.rejects(
      glasses.takePhoto({ maxWidth: 320, maxHeight: 240 }),
      (e: GlassesError) => e.code === "notConnected",
    );

    await clock.advance(500);
    glasses.simulateDisconnect();
    await clock.advance(5_000);

    await assertion;
  });
});

describe("photos that stay on the glasses", () => {
  test("capturePhoto returns immediately and transfers nothing", async () => {
    const { glasses, clock } = await connected();
    const startedAt = clock.now();

    const file = await glasses.capturePhoto();

    assert.equal(clock.now(), startedAt, "the shutter must not wait on a radio");
    assert.match(file.name, /\.jpg$/);
    assert.equal(file.uploaded, false);
    // Full resolution, not the downscaled thing BLE would have forced.
    assert.ok(file.sizeBytes > 200_000, `expected full-res, got ${file.sizeBytes} bytes`);
  });

  test("captured photos join the sync queue alongside recordings", async () => {
    const { glasses, clock } = await connected();
    await glasses.startLocalRecording();
    await clock.advance(60_000);
    await glasses.stopLocalRecording();
    await glasses.capturePhoto();

    const files = await glasses.listFiles();
    assert.deepEqual(
      files.map((f) => f.name.split(".").pop()),
      ["opus", "jpg"],
      "audio and stills share one queue and one nightly sync",
    );
  });

  test("a full-resolution photo syncs in seconds over the access point", async () => {
    const { glasses, clock } = await connected();
    const file = await glasses.capturePhoto();
    await glasses.openWifiAccessPoint();

    let finishedAt = -1;
    const pending = glasses.fetchFile(file.name).then((r) => {
      finishedAt = clock.now();
      return r;
    });
    await clock.advance(60_000);
    await pending;

    assert.ok(finishedAt / 1000 < 5, "full-res over WiFi should be seconds, not minutes");
  });

  test("thumbnail clarity trades sharpness against transfer time", async () => {
    const { glasses, clock } = await connected();
    const file = await glasses.capturePhoto();

    const timeAt = async (clarity: number) => {
      const from = clock.now();
      let at = -1;
      const pending = glasses.fetchThumbnail(file.name, { clarity }).then((p) => {
        at = clock.now();
        return p;
      });
      await clock.advance(120_000);
      const photo = await pending;
      return { ms: at - from, bytes: photo.data.byteLength };
    };

    const low = await timeAt(0);
    const high = await timeAt(6);

    assert.ok(high.ms > low.ms, "higher clarity must cost more time");
    assert.ok(high.bytes > low.bytes);
    assert.ok(low.ms < 2_000, `clarity 0 should be sub-2s, was ${low.ms}ms`);
  });

  test("a thumbnail is far cheaper than delivering the photo over BLE", async () => {
    const { glasses, clock } = await connected();
    const file = await glasses.capturePhoto();

    let thumbAt = -1;
    const thumb = glasses.fetchThumbnail(file.name, { clarity: 2 }).then((p) => {
      thumbAt = clock.now();
      return p;
    });
    await clock.advance(120_000);
    await thumb;

    // takePhoto at full size is ~84 s over BLE; a clarity-2 thumbnail is ~2 s.
    assert.ok(thumbAt < 10_000, `thumbnail took ${thumbAt}ms`);
  });

  test("capturePhoto refuses when storage is exhausted", async () => {
    const { glasses } = await connected({ totalStorageBytes: 1_000, usedStorageBytes: 1_000 });
    await assert.rejects(glasses.capturePhoto(), (e: GlassesError) => e.code === "storageFull");
  });
});

describe("local recording and storage", () => {
  test("recording consumes storage and produces a file", async () => {
    const { glasses, clock } = await connected();

    await glasses.startLocalRecording();
    await clock.advance(60 * 60 * 1000); // one hour
    await glasses.stopLocalRecording();

    const files = await glasses.listFiles();
    assert.equal(files.length, 1);
    assert.ok(Math.abs(files[0]!.durationS! - 3600) < 1);
    // 3600 s x 3000 B/s ~= 10.8 MB
    assert.ok(files[0]!.sizeBytes > 10_000_000 && files[0]!.sizeBytes < 11_500_000);
    assert.equal(files[0]!.uploaded, false);
  });

  test("free space reflects a recording already in progress", async () => {
    const { glasses, clock } = await connected();
    const before = await glasses.getDiskInfo();

    await glasses.startLocalRecording();
    await clock.advance(10 * 60 * 1000);
    const during = await glasses.getDiskInfo();

    assert.ok(during.freeBytes < before.freeBytes, "in-flight recording should consume space");
  });

  test("4 GB holds well over a day of audio", async () => {
    const { glasses, clock } = await connected();
    await glasses.startLocalRecording();
    await clock.advance(16 * 60 * 60 * 1000); // a 16-hour day
    await glasses.stopLocalRecording();

    const disk = await glasses.getDiskInfo();
    const usedFraction = 1 - disk.freeBytes / disk.totalBytes;
    assert.ok(usedFraction < 0.06, `a 16h day should be a few percent of 4 GB, was ${usedFraction}`);
  });

  test("recording refuses to start when storage is exhausted", async () => {
    const { glasses } = await connected({
      totalStorageBytes: 1_000,
      usedStorageBytes: 1_000,
    });
    await assert.rejects(glasses.startLocalRecording(), (e: GlassesError) => e.code === "storageFull");
  });

  test("deleting a file returns its space", async () => {
    const { glasses, clock } = await connected();
    await glasses.startLocalRecording();
    await clock.advance(60_000);
    await glasses.stopLocalRecording();

    const [file] = await glasses.listFiles();
    const before = await glasses.getDiskInfo();
    await glasses.deleteFile(file!.name);
    const after = await glasses.getDiskInfo();

    assert.ok(after.freeBytes > before.freeBytes);
    assert.equal((await glasses.listFiles()).length, 0);
  });

  test("deleting an unknown file is an error, not a silent no-op", async () => {
    const { glasses } = await connected();
    await assert.rejects(glasses.deleteFile("nope.opus"));
  });
});

describe("bulk transfer — why sync rides WiFi", () => {
  async function recordOneHour() {
    const ctx = await connected();
    await ctx.glasses.startLocalRecording();
    await ctx.clock.advance(60 * 60 * 1000);
    await ctx.glasses.stopLocalRecording();
    const [file] = await ctx.glasses.listFiles();
    return { ...ctx, file: file! };
  }

  test("fetching over BLE is unusably slow", async () => {
    const { glasses, clock, file } = await recordOneHour();

    const started = clock.now();
    let finishedAt = -1;
    const pending = glasses.fetchFile(file.name).then((r) => {
      finishedAt = clock.now();
      return r;
    });

    await clock.advance(30 * 60 * 1000); // half an hour
    assert.equal(finishedAt, -1, "one hour of audio should not transfer over BLE in 30 min");

    await clock.advance(60 * 60 * 1000);
    await pending;
    // ~10.8 MB at 3 KB/s is about an hour — as long as it took to record.
    const elapsedMin = (finishedAt - started) / 60_000;
    assert.ok(elapsedMin > 55, `BLE fetch took ${elapsedMin.toFixed(1)} min`);
  });

  test("the same file over the access point takes seconds", async () => {
    const { glasses, clock, file } = await recordOneHour();
    await glasses.openWifiAccessPoint();

    const started = clock.now();
    let finishedAt = -1;
    const pending = glasses.fetchFile(file.name).then((r) => {
      finishedAt = clock.now();
      return r;
    });
    await clock.advance(60_000);
    await pending;

    const elapsedS = (finishedAt - started) / 1000;
    assert.ok(elapsedS < 10, `AP fetch took ${elapsedS.toFixed(1)}s, expected seconds`);
  });

  test("a completed fetch marks the file uploaded", async () => {
    const { glasses, clock, file } = await recordOneHour();
    await glasses.openWifiAccessPoint();
    const pending = glasses.fetchFile(file.name);
    await clock.advance(60_000);
    await pending;

    const [after] = await glasses.listFiles();
    assert.equal(after!.uploaded, true);
  });

  test("progress is reported during a fetch", async () => {
    const { glasses, clock, file } = await recordOneHour();
    await glasses.openWifiAccessPoint();

    const seen: number[] = [];
    const pending = glasses.fetchFile(file.name, (p) => seen.push(p.receivedBytes));
    await clock.advance(60_000);
    await pending;

    assert.ok(seen.length > 1);
    assert.equal(seen.at(-1), file.sizeBytes);
    assert.deepEqual(seen, [...seen].sort((a, b) => a - b), "progress must be monotonic");
  });
});

describe("battery", () => {
  test("drains over time while unplugged", async () => {
    const { glasses, clock } = await connected({ batteryPercent: 80 });
    await clock.advance(2 * 60 * 60 * 1000);
    const battery = await glasses.getBattery();
    assert.equal(battery.charging, false);
    assert.ok(battery.percent < 80, `expected drain, got ${battery.percent}`);
  });

  test("charges while plugged in — the desk case", async () => {
    const { glasses, clock } = await connected({ batteryPercent: 40, charging: true });
    await clock.advance(60 * 60 * 1000);
    const battery = await glasses.getBattery();
    assert.equal(battery.charging, true);
    assert.ok(battery.percent > 40, `expected charge, got ${battery.percent}`);
  });

  test("never leaves 0-100", async () => {
    const { glasses, clock } = await connected({ batteryPercent: 5 });
    await clock.advance(48 * 60 * 60 * 1000);
    const empty = await glasses.getBattery();
    assert.equal(empty.percent, 0);

    glasses.setCharging(true);
    await clock.advance(48 * 60 * 60 * 1000);
    const full = await glasses.getBattery();
    assert.equal(full.percent, 100);
  });
});

describe("voice sessions and input", () => {
  test("audio only flows while a session is open", async () => {
    const { glasses } = await connected();
    const chunks: number[] = [];
    glasses.on("audioChunk", (c) => chunks.push(c.sequence));

    assert.equal(glasses.emitAudioChunk(), false, "no audio before the session opens");

    await glasses.startVoiceSession();
    assert.equal(glasses.emitAudioChunk(), true);
    assert.equal(glasses.emitAudioChunk(), true);
    await glasses.stopVoiceSession();
    assert.equal(glasses.emitAudioChunk(), false, "no audio after the session closes");

    assert.deepEqual(chunks, [0, 1], "sequence numbers must be contiguous within a session");
  });

  test("session state changes are observable", async () => {
    const { glasses } = await connected();
    const states: boolean[] = [];
    glasses.on("voiceSessionChanged", (open) => states.push(open));

    await glasses.startVoiceSession();
    await glasses.startVoiceSession(); // idempotent
    await glasses.stopVoiceSession();

    assert.deepEqual(states, [true, false]);
  });

  test("wear and remove derive a wear event as well as a touch", async () => {
    const { glasses } = await connected();
    const touches: string[] = [];
    const wear: boolean[] = [];
    glasses.on("touch", (t) => touches.push(t));
    glasses.on("wear", (w) => wear.push(w));

    glasses.emitTouch(TouchAction.Wear);
    glasses.emitTouch(TouchAction.DoubleTap);
    glasses.emitTouch(TouchAction.Remove);

    assert.deepEqual(touches, ["wear", "doubleTap", "remove"]);
    assert.deepEqual(wear, [true, false], "taps must not be mistaken for wear changes");
  });

  test("disconnecting closes an open voice session", async () => {
    const { glasses } = await connected();
    await glasses.startVoiceSession();
    assert.equal(glasses.isVoiceSessionOpen, true);
    glasses.simulateDisconnect();
    assert.equal(glasses.isVoiceSessionOpen, false);
  });
});

describe("features and preview", () => {
  test("unsupported features are refused rather than faked", async () => {
    const { glasses } = await connected({ features: { livePreview: false, localRecording: false } });
    await assert.rejects(glasses.startPreview(), (e: GlassesError) => e.code === "unsupported");
    await assert.rejects(
      glasses.startLocalRecording(),
      (e: GlassesError) => e.code === "unsupported",
    );
  });

  test("preview yields the documented RTSP endpoint and opens the AP", async () => {
    const { glasses } = await connected();
    const url = await glasses.startPreview();
    assert.match(url, /^rtsp:\/\/192\.168\.31\.1:8554\//);
    assert.equal(glasses.isAccessPointOpen, true, "preview requires the device AP");
  });
});
