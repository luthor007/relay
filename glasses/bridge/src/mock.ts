/**
 * MockTransport — the glasses, without the glasses.
 *
 * The point is not to make calls resolve. It is to make them resolve *the way
 * the hardware does*, so the app is built against real constraints:
 *
 *   - photos arrive over BLE at a few KB/s, so `takePhoto` takes seconds and
 *     reports progress; asking for a smaller image is genuinely faster
 *   - `fetchFile` over BLE is unusably slow and fast over the WiFi AP, which is
 *     why the sync design is what it is
 *   - local recording consumes the 4 GB budget, and storage can fill
 *   - the link drops, and commands in flight fail when it does
 *
 * A mock that returns instantly and never fails produces a UI that lies about
 * latency and has no error states. This one is deliberately inconvenient.
 *
 * Drive it with a FakeClock in tests: nothing happens until time is advanced.
 */

import type { Clock } from "./clock.ts";
import { systemClock } from "./clock.ts";
import { TypedEmitter } from "./emitter.ts";
import type { ConnectOptions, FetchProgress, GlassesTransport } from "./transport.ts";
import type { Trace } from "./trace.ts";
import type {
  BatteryStatus,
  DiskInfo,
  Features,
  GlassesEvents,
  Photo,
  PhotoOptions,
  RemoteFile,
  ThumbnailOptions,
  Unsubscribe,
  WifiAccessPoint,
} from "./types.ts";
import { ConnectionState, GlassesError, GlassesErrorCode, TouchAction } from "./types.ts";

const GIGABYTE = 1024 * 1024 * 1024;

/** Measured-ish defaults; replace with real figures once hardware is measured. */
export const MOCK_DEFAULTS = {
  connectDelayMs: 800,
  /** Practical BLE application throughput. */
  bleBytesPerSecond: 3_000,
  /** Over the glasses' own access point. */
  wifiBytesPerSecond: 2_000_000,
  /** 4 GB device, per supplier. */
  totalStorageBytes: 4 * GIGABYTE,
  /** Opus ~24 kbps mono. */
  recordingBytesPerSecond: 3_000,
  batteryPercent: 92,
  batteryDrainPerHour: 12,
  batteryChargePerHour: 45,
  /** Rough JPEG bytes per pixel at the device's default quality. */
  jpegBytesPerPixel: 0.08,
  defaultPhotoWidth: 2048,
  defaultPhotoHeight: 1536,
  photoProgressIntervalMs: 250,
} as const;

export interface MockFaults {
  /** Reject every connect attempt. */
  connectFails?: boolean;
  /** Fail photo capture partway through the transfer. */
  photoFails?: boolean;
  /** Drop the link this long after connecting. */
  dropAfterMs?: number;
  /** Reject writes once storage is exhausted rather than silently wrapping. */
  storageFull?: boolean;
}

export interface MockOptions {
  clock?: Clock;
  connectDelayMs?: number;
  bleBytesPerSecond?: number;
  wifiBytesPerSecond?: number;
  totalStorageBytes?: number;
  usedStorageBytes?: number;
  recordingBytesPerSecond?: number;
  batteryPercent?: number;
  charging?: boolean;
  features?: Partial<Features>;
  faults?: MockFaults;
  /** Replay a recorded session instead of synthesising events. */
  trace?: Trace;
}

const DEFAULT_FEATURES: Features = {
  localRecording: true,
  wifiAp: true,
  wifiP2p: true,
  livePreview: true,
  voiceWakeup: true,
  wearDetection: true,
  stabilization: true,
  unknownBits: [],
};

export class MockTransport implements GlassesTransport {
  #emitter = new TypedEmitter<GlassesEvents>();
  #clock: Clock;
  #opts: Required<Omit<MockOptions, "clock" | "features" | "faults" | "trace" | "usedStorageBytes">>;
  #features: Features;
  #faults: MockFaults;
  #trace?: Trace;

  #state: ConnectionState = ConnectionState.Disconnected;
  #batteryPercent: number;
  #charging: boolean;
  #batteryAtMs: number;
  #usedStorageBytes: number;
  #apOpen = false;
  #previewing = false;
  #voiceOpen = false;
  #recordingSinceMs: number | null = null;
  #files: RemoteFile[] = [];
  #audioSeq = 0;
  #fileCounter = 0;
  #cancelDrop: (() => void) | null = null;

  constructor(options: MockOptions = {}) {
    this.#clock = options.clock ?? systemClock;
    this.#opts = {
      connectDelayMs: options.connectDelayMs ?? MOCK_DEFAULTS.connectDelayMs,
      bleBytesPerSecond: options.bleBytesPerSecond ?? MOCK_DEFAULTS.bleBytesPerSecond,
      wifiBytesPerSecond: options.wifiBytesPerSecond ?? MOCK_DEFAULTS.wifiBytesPerSecond,
      totalStorageBytes: options.totalStorageBytes ?? MOCK_DEFAULTS.totalStorageBytes,
      recordingBytesPerSecond:
        options.recordingBytesPerSecond ?? MOCK_DEFAULTS.recordingBytesPerSecond,
      batteryPercent: options.batteryPercent ?? MOCK_DEFAULTS.batteryPercent,
      charging: options.charging ?? false,
    };
    this.#features = { ...DEFAULT_FEATURES, ...options.features };
    this.#faults = { ...options.faults };
    this.#trace = options.trace;
    this.#batteryPercent = this.#opts.batteryPercent;
    this.#charging = this.#opts.charging;
    this.#batteryAtMs = this.#clock.now();
    this.#usedStorageBytes = options.usedStorageBytes ?? 0;
  }

  // --- lifecycle ------------------------------------------------------------

  get state(): ConnectionState {
    return this.#state;
  }

  on<K extends keyof GlassesEvents>(
    event: K,
    handler: (payload: GlassesEvents[K]) => void,
  ): Unsubscribe {
    return this.#emitter.on(event, handler);
  }

  async connect(options: ConnectOptions = {}): Promise<void> {
    if (this.#state === ConnectionState.Connected) return;

    this.#setState(ConnectionState.Connecting);
    await this.#clock.sleep(this.#opts.connectDelayMs);

    if (this.#faults.connectFails) {
      this.#setState(ConnectionState.Disconnected);
      throw new GlassesError(GlassesErrorCode.ConnectFailed, "mock: connect failed");
    }
    if (options.timeoutMs !== undefined && options.timeoutMs < this.#opts.connectDelayMs) {
      this.#setState(ConnectionState.Disconnected);
      throw new GlassesError(GlassesErrorCode.Timeout, "mock: connect timed out");
    }

    this.#setState(ConnectionState.Connected);

    if (this.#faults.dropAfterMs !== undefined) {
      this.#cancelDrop = this.#clock.setTimeout(() => {
        this.simulateDisconnect("mock: link dropped");
      }, this.#faults.dropAfterMs);
    }
    if (this.#trace) void this.#playTrace(this.#trace);
  }

  async disconnect(): Promise<void> {
    this.#cancelDrop?.();
    this.#cancelDrop = null;
    this.#voiceOpen = false;
    this.#previewing = false;
    this.#apOpen = false;
    this.#setState(ConnectionState.Disconnected);
  }

  // --- device ---------------------------------------------------------------

  async getFeatures(): Promise<Features> {
    this.#requireConnected();
    return { ...this.#features };
  }

  async getBattery(): Promise<BatteryStatus> {
    this.#requireConnected();
    const status = this.#battery();
    this.#emitter.emit("battery", status);
    return status;
  }

  async getDiskInfo(): Promise<DiskInfo> {
    this.#requireConnected();
    const info = this.#disk();
    this.#emitter.emit("diskInfo", info);
    return info;
  }

  async setTime(_date: Date): Promise<void> {
    this.#requireConnected();
  }

  // --- voice ----------------------------------------------------------------

  async startVoiceSession(): Promise<void> {
    this.#requireConnected();
    if (this.#voiceOpen) return;
    this.#voiceOpen = true;
    this.#audioSeq = 0;
    this.#emitter.emit("voiceSessionChanged", true);
  }

  async stopVoiceSession(): Promise<void> {
    this.#requireConnected();
    if (!this.#voiceOpen) return;
    this.#voiceOpen = false;
    this.#emitter.emit("voiceSessionChanged", false);
  }

  // --- camera ---------------------------------------------------------------

  /**
   * Shutter to device storage. Returns immediately — the whole point is that
   * nothing crosses the radio until the nightly sync.
   */
  async capturePhoto(): Promise<RemoteFile> {
    this.#requireConnected();

    const sizeBytes = Math.round(
      MOCK_DEFAULTS.defaultPhotoWidth *
        MOCK_DEFAULTS.defaultPhotoHeight *
        MOCK_DEFAULTS.jpegBytesPerPixel,
    );
    if (this.#disk().freeBytes < sizeBytes) {
      throw new GlassesError(GlassesErrorCode.StorageFull, "mock: storage full");
    }
    this.#usedStorageBytes += sizeBytes;

    const file: RemoteFile = {
      name: `IMG_${String(++this.#fileCounter).padStart(4, "0")}.jpg`,
      sizeBytes,
      uploaded: false,
    };
    this.#files.push(file);
    this.#emitter.emit("diskInfo", this.#disk());
    return { ...file };
  }

  /**
   * Preview of a stored photo over BLE. Clarity 0-6 scales the payload, so
   * higher settings cost proportionally more transfer time — the device's own
   * latency dial rather than one we invented.
   */
  async fetchThumbnail(name: string, options: ThumbnailOptions = {}): Promise<Photo> {
    this.#requireConnected();
    const file = this.#files.find((f) => f.name === name);
    if (!file) {
      throw new GlassesError(GlassesErrorCode.TransferFailed, `mock: no such file ${name}`);
    }

    const clarity = Math.max(0, Math.min(6, options.clarity ?? 2));
    // 2 KB at clarity 0 up to ~50 KB at clarity 6.
    const totalBytes = Math.round(2_048 * Math.pow(1.75, clarity));
    const totalMs = (totalBytes / this.#opts.bleBytesPerSecond) * 1000;

    await this.#clock.sleep(totalMs);
    this.#requireConnected();

    return {
      data: syntheticJpeg(totalBytes),
      mimeType: "image/jpeg",
      widthPx: 160 * (clarity + 1),
      heightPx: 120 * (clarity + 1),
    };
  }

  /**
   * Transfer time is derived from the requested size, because that is how the
   * device behaves — resolution is a latency dial, not a quality knob.
   */
  async takePhoto(options: PhotoOptions = {}): Promise<Photo> {
    this.#requireConnected();

    const width = options.maxWidth ?? MOCK_DEFAULTS.defaultPhotoWidth;
    const height = options.maxHeight ?? MOCK_DEFAULTS.defaultPhotoHeight;
    const totalBytes = Math.max(
      1024,
      Math.round(width * height * MOCK_DEFAULTS.jpegBytesPerPixel),
    );

    const rate = this.#opts.bleBytesPerSecond;
    const totalMs = (totalBytes / rate) * 1000;
    const step = MOCK_DEFAULTS.photoProgressIntervalMs;
    const chunkCount = Math.max(1, Math.ceil(totalMs / step));

    for (let chunk = 1; chunk <= chunkCount; chunk++) {
      await this.#clock.sleep(Math.min(step, totalMs - step * (chunk - 1)));
      this.#requireConnected();

      if (this.#faults.photoFails && chunk === Math.ceil(chunkCount / 2)) {
        throw new GlassesError(GlassesErrorCode.TransferFailed, "mock: photo transfer failed");
      }

      const receivedBytes = Math.min(totalBytes, Math.round((totalBytes * chunk) / chunkCount));
      this.#emitter.emit("photoProgress", {
        receivedBytes,
        totalBytes,
        chunkIndex: chunk,
        chunkCount,
      });
    }

    const photo: Photo = {
      data: syntheticJpeg(totalBytes),
      mimeType: "image/jpeg",
      widthPx: width,
      heightPx: height,
    };
    this.#emitter.emit("photo", photo);
    return photo;
  }

  // --- local recording ------------------------------------------------------

  async startLocalRecording(): Promise<void> {
    this.#requireConnected();
    if (!this.#features.localRecording) {
      throw new GlassesError(GlassesErrorCode.Unsupported, "mock: local recording unsupported");
    }
    if (this.#recordingSinceMs !== null) return;
    if (this.#disk().freeBytes <= 0) {
      throw new GlassesError(GlassesErrorCode.StorageFull, "mock: storage full");
    }
    this.#recordingSinceMs = this.#clock.now();
    this.#emitter.emit("recordingState", { recording: true, durationS: 0 });
  }

  async stopLocalRecording(): Promise<void> {
    this.#requireConnected();
    if (this.#recordingSinceMs === null) return;

    const durationS = (this.#clock.now() - this.#recordingSinceMs) / 1000;
    this.#recordingSinceMs = null;

    const wanted = Math.round(durationS * this.#opts.recordingBytesPerSecond);
    const free = this.#disk().freeBytes;
    if (wanted > free && this.#faults.storageFull) {
      throw new GlassesError(GlassesErrorCode.StorageFull, "mock: storage full during write");
    }
    const sizeBytes = Math.min(wanted, free);
    this.#usedStorageBytes += sizeBytes;

    this.#files.push({
      name: `REC_${String(++this.#fileCounter).padStart(4, "0")}.opus`,
      sizeBytes,
      uploaded: false,
      durationS,
    });
    this.#emitter.emit("recordingState", { recording: false, durationS: 0 });
    this.#emitter.emit("diskInfo", this.#disk());
  }

  async listFiles(): Promise<RemoteFile[]> {
    this.#requireConnected();
    return this.#files.map((f) => ({ ...f }));
  }

  async deleteFile(name: string): Promise<void> {
    this.#requireConnected();
    const index = this.#files.findIndex((f) => f.name === name);
    if (index === -1) {
      throw new GlassesError(GlassesErrorCode.TransferFailed, `mock: no such file ${name}`);
    }
    this.#usedStorageBytes = Math.max(0, this.#usedStorageBytes - this.#files[index]!.sizeBytes);
    this.#files.splice(index, 1);
    this.#emitter.emit("diskInfo", this.#disk());
  }

  // --- bulk transfer --------------------------------------------------------

  async openWifiAccessPoint(): Promise<WifiAccessPoint> {
    this.#requireConnected();
    if (!this.#features.wifiAp) {
      throw new GlassesError(GlassesErrorCode.Unsupported, "mock: no WiFi AP");
    }
    this.#apOpen = true;
    return { ssid: "QCGlasses-MOCK", password: "12345678", host: "192.168.31.1" };
  }

  async closeWifiAccessPoint(): Promise<void> {
    this.#requireConnected();
    this.#apOpen = false;
  }

  /**
   * Rate depends on whether the access point is open — the whole reason the sync
   * design is a nightly WiFi ritual rather than a background BLE trickle.
   */
  async fetchFile(name: string, onProgress?: (p: FetchProgress) => void): Promise<Uint8Array> {
    this.#requireConnected();
    const file = this.#files.find((f) => f.name === name);
    if (!file) {
      throw new GlassesError(GlassesErrorCode.TransferFailed, `mock: no such file ${name}`);
    }

    const rate = this.#apOpen ? this.#opts.wifiBytesPerSecond : this.#opts.bleBytesPerSecond;
    const totalMs = (file.sizeBytes / rate) * 1000;
    const steps = Math.max(1, Math.min(50, Math.ceil(totalMs / 200)));

    for (let step = 1; step <= steps; step++) {
      await this.#clock.sleep(totalMs / steps);
      this.#requireConnected();
      onProgress?.({
        receivedBytes: Math.round((file.sizeBytes * step) / steps),
        totalBytes: file.sizeBytes,
      });
    }

    file.uploaded = true;
    return new Uint8Array(file.sizeBytes);
  }

  // --- video ----------------------------------------------------------------

  async startPreview(): Promise<string> {
    this.#requireConnected();
    if (!this.#features.livePreview) {
      throw new GlassesError(GlassesErrorCode.Unsupported, "mock: no live preview");
    }
    this.#apOpen = true;
    this.#previewing = true;
    const url = "rtsp://192.168.31.1:8554/live";
    this.#emitter.emit("rtspUrl", url);
    return url;
  }

  async stopPreview(): Promise<void> {
    this.#requireConnected();
    this.#previewing = false;
  }

  // --- test controls --------------------------------------------------------
  // Not part of GlassesTransport; the app never sees these.

  /** Push a touch event. Wear and Remove additionally emit `wear`. */
  emitTouch(action: TouchAction): void {
    this.#emitter.emit("touch", action);
    if (action === TouchAction.Wear) this.#emitter.emit("wear", true);
    if (action === TouchAction.Remove) this.#emitter.emit("wear", false);
  }

  /** Push one microphone chunk. Ignored unless a voice session is open. */
  emitAudioChunk(bytes = 320, format: "opus" | "pcm16" = "opus"): boolean {
    if (!this.#voiceOpen) return false;
    this.#emitter.emit("audioChunk", {
      data: new Uint8Array(bytes),
      format,
      sampleRate: 16_000,
      channels: 1,
      sequence: this.#audioSeq++,
      deviceTimeMs: this.#clock.now(),
    });
    return true;
  }

  emitTranscript(text: string): void {
    this.#emitter.emit("transcriptText", text);
  }

  setCharging(charging: boolean): void {
    this.#battery(); // settle accumulated drain at the old rate first
    this.#charging = charging;
  }

  setFaults(faults: MockFaults): void {
    this.#faults = { ...this.#faults, ...faults };
  }

  simulateDisconnect(reason = "mock: disconnected"): void {
    if (this.#state === ConnectionState.Disconnected) return;
    this.#voiceOpen = false;
    this.#previewing = false;
    this.#apOpen = false;
    this.#recordingSinceMs = null;
    this.#setState(ConnectionState.Disconnected);
    this.#emitter.emit("error", new GlassesError(GlassesErrorCode.NotConnected, reason));
  }

  get isAccessPointOpen(): boolean {
    return this.#apOpen;
  }

  get isPreviewing(): boolean {
    return this.#previewing;
  }

  get isVoiceSessionOpen(): boolean {
    return this.#voiceOpen;
  }

  // --- internals ------------------------------------------------------------

  #setState(state: ConnectionState): void {
    if (this.#state === state) return;
    this.#state = state;
    this.#emitter.emit("connectionChanged", state);
  }

  #requireConnected(): void {
    if (this.#state !== ConnectionState.Connected) {
      throw new GlassesError(GlassesErrorCode.NotConnected, "mock: not connected");
    }
  }

  #battery(): BatteryStatus {
    const now = this.#clock.now();
    const hours = (now - this.#batteryAtMs) / 3_600_000;
    const delta = this.#charging
      ? hours * MOCK_DEFAULTS.batteryChargePerHour
      : -hours * MOCK_DEFAULTS.batteryDrainPerHour;
    this.#batteryPercent = Math.max(0, Math.min(100, this.#batteryPercent + delta));
    this.#batteryAtMs = now;
    return { percent: Math.round(this.#batteryPercent), charging: this.#charging };
  }

  #disk(): DiskInfo {
    let used = this.#usedStorageBytes;
    if (this.#recordingSinceMs !== null) {
      const seconds = (this.#clock.now() - this.#recordingSinceMs) / 1000;
      used += seconds * this.#opts.recordingBytesPerSecond;
    }
    const total = this.#opts.totalStorageBytes;
    return { totalBytes: total, freeBytes: Math.max(0, Math.round(total - used)) };
  }

  async #playTrace(trace: Trace): Promise<void> {
    let previous = 0;
    for (const record of trace.events) {
      await this.#clock.sleep(Math.max(0, record.tMs - previous));
      previous = record.tMs;
      if (this.#state === ConnectionState.Disconnected) return;
      this.#emitter.emit(
        record.event,
        record.payload as GlassesEvents[typeof record.event] & never,
      );
    }
  }
}

/** Bytes shaped like a JPEG — correct SOI/EOI markers, filler between. */
export function syntheticJpeg(totalBytes: number): Uint8Array {
  const size = Math.max(4, totalBytes);
  const bytes = new Uint8Array(size);
  bytes[0] = 0xff;
  bytes[1] = 0xd8; // SOI
  for (let i = 2; i < size - 2; i++) bytes[i] = i & 0xff;
  bytes[size - 2] = 0xff;
  bytes[size - 1] = 0xd9; // EOI
  return bytes;
}
