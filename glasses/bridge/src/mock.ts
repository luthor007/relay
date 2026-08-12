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
 *   - the live mic costs ~3 KB/s in each direction, so pushing a spoken reply
 *     down `0x0A03` takes about as long as the reply lasts
 *   - local recording consumes the 4 GB budget, and video evicts audio fast
 *   - the link drops, and commands in flight fail when it does
 *   - **absent capabilities fail.** No station mode, no device-side ASR, no wake
 *     phrase that is not in the firmware's list. Each of those refuses here the
 *     way it refuses on hardware, rather than succeeding quietly and surprising
 *     someone in Quebec with a device in their hand.
 *
 * A mock that returns instantly and never fails produces a UI that lies about
 * latency and has no error states. This one is deliberately inconvenient.
 *
 * It also keeps a `commandLog`: every protocol command the call issued, with its
 * ID from `glasses/protocol/commands.py`. That is what lets a test assert the
 * app reached for `0x0E04` rather than merely that a promise resolved.
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
  GlassesEventName,
  Photo,
  PhotoOptions,
  RemoteFile,
  ThumbnailOptions,
  Unsubscribe,
  WifiAccessPoint,
} from "./types.ts";
import { ConnectionState, GlassesError, GlassesErrorCode, TouchAction } from "./types.ts";
import type {
  AllGlassesEvents,
  CallState,
  CaptureState,
  ClearResult,
  CommandRecord,
  DeviceIdentity,
  FileTransfer,
  FileTransferProgress,
  GlassesCommandSet,
  KeyBinding,
  MediaCounts,
  OtaInfo,
  PhotoParams,
  RecordingFormat,
  VideoParams,
  VideoResolution,
  WakeWord,
  WakeWordSetting,
  WifiApState,
} from "./commands.ts";
import {
  AiInterfaceEvent,
  AudioControl,
  Command,
  CommandType,
  CaptureKind,
  DeviceMode,
  EqPreset,
  KeyAction,
  KeyGesture,
  NoiseCancellation,
  RATES,
  RecognitionOwner,
  RecordingFormat as RecordingFormatValues,
  SHARPNESS_MAX,
  SHARPNESS_MIN,
  SpeakMode,
  SpeakerRoute,
  SequenceCounter,
  Toggle,
  WakeWordKind,
  commandName,
  recordingBytes,
} from "./commands.ts";

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
  /** 1080p video, ~4.5 GB/h — APPS-SCOPE.md §3.2. This is what evicts audio. */
  videoBytesPerSecond: Math.round((4.5 * GIGABYTE) / 3600),
  /** Live Opus mic uplink, SYSTEM.md §3.1. */
  micBytesPerSecond: RATES.micBytesPerSecond,
  /** Unprompted battery reports, ~1/min. */
  batteryReportIntervalMs: RATES.batteryReportIntervalMs,
  batteryPercent: 92,
  batteryDrainPerHour: 12,
  batteryChargePerHour: 45,
  /** Rough JPEG bytes per pixel at the device's default quality. */
  jpegBytesPerPixel: 0.08,
  defaultPhotoWidth: 2048,
  defaultPhotoHeight: 1536,
  photoProgressIntervalMs: 250,
  /** Live preview refuses below this, mirroring QGLivePreviewErrorCodeLowBattery. */
  previewMinBatteryPercent: 15,
  /** OTA refuses below this, mirroring QG_DFU_OperationStatus_NotEnoughPower. */
  otaMinBatteryPercent: 30,
} as const;

/**
 * The wake phrases a stock unit ships with.
 *
 * `ARCHITECTURE.md` §5.2b: the list is firmware, built into a trained DSP model,
 * and the spec's own worked example is `"hey chatgpt"`. Nothing accepts a new
 * phrase, so the mock refuses indices it did not hand out — which is the failure
 * a per-user "Hey Jarvis" will hit on hardware.
 */
export const MOCK_WAKE_WORDS: readonly WakeWord[] = [
  { index: 0, kind: WakeWordKind.AiPhrase, phrase: "hey chatgpt" },
  { index: 1, kind: WakeWordKind.BluetoothControl, phrase: "answer the call" },
  { index: 2, kind: WakeWordKind.DeviceControl, phrase: "take a picture" },
];

export interface MockFaults {
  /** Reject every connect attempt. */
  connectFails?: boolean;
  /** Fail photo capture partway through the transfer. */
  photoFails?: boolean;
  /** Drop the link this long after connecting. */
  dropAfterMs?: number;
  /** Reject writes once storage is exhausted rather than silently wrapping. */
  storageFull?: boolean;
  /** Make the device resend this many chunks per file transfer (0x0C04). */
  transferRetries?: number;
}

export interface MockOptions {
  clock?: Clock;
  connectDelayMs?: number;
  bleBytesPerSecond?: number;
  wifiBytesPerSecond?: number;
  totalStorageBytes?: number;
  usedStorageBytes?: number;
  recordingBytesPerSecond?: number;
  videoBytesPerSecond?: number;
  micBytesPerSecond?: number;
  /**
   * How often the device reports battery unprompted. `SYSTEM.md` §3.1 says
   * ~1/min. Set 0 to silence it.
   */
  batteryReportIntervalMs?: number;
  /**
   * Emit `audioChunk` on a timer while the uplink is open, at the real rate.
   * On by default: a mock whose microphone is silent teaches an app that never
   * has to deal with backpressure.
   */
  streamMicAudio?: boolean;
  /**
   * Opus or PCM on the device. Still an open question (`APPS-SCOPE.md` §3.1) and
   * it moves storage and sync duration by ~10x, so both are modellable.
   */
  recordingFormat?: RecordingFormat;
  batteryPercent?: number;
  charging?: boolean;
  features?: Partial<Features>;
  faults?: MockFaults;
  wakeWords?: readonly WakeWord[];
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

const DEFAULT_IDENTITY: DeviceIdentity = {
  product: "M01 Pro",
  model: "A02Q8DP",
  firmwareVersion: "2.00.04",
  hardwareVersion: "HW-1.2",
  name: "Relay Glasses",
};

const DEFAULT_KEY_BINDINGS: KeyBinding[] = [
  { gesture: KeyGesture.SingleTap, action: KeyAction.PlayPause },
  { gesture: KeyGesture.DoubleTap, action: KeyAction.AiAssistant },
  { gesture: KeyGesture.TripleTap, action: KeyAction.Photo },
  { gesture: KeyGesture.LongPress, action: KeyAction.Recording },
  { gesture: KeyGesture.SwipeForward, action: KeyAction.VolumeUp },
  { gesture: KeyGesture.SwipeBackward, action: KeyAction.VolumeDown },
];

interface TransferSlot {
  handle: FileTransfer;
  retries: number;
  aborted: boolean;
}

export class MockTransport implements GlassesTransport, GlassesCommandSet {
  #emitter = new TypedEmitter<AllGlassesEvents>();
  #clock: Clock;
  #opts: {
    connectDelayMs: number;
    bleBytesPerSecond: number;
    wifiBytesPerSecond: number;
    totalStorageBytes: number;
    recordingBytesPerSecond: number;
    videoBytesPerSecond: number;
    micBytesPerSecond: number;
    batteryReportIntervalMs: number;
    streamMicAudio: boolean;
    batteryPercent: number;
    charging: boolean;
  };
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

  // --- command surface state -------------------------------------------------

  #seq = new SequenceCounter();
  #log: CommandRecord[] = [];
  #identity: DeviceIdentity = { ...DEFAULT_IDENTITY };
  #recordingFormat: RecordingFormat;
  #videoSinceMs: number | null = null;
  #anc: NoiseCancellation = NoiseCancellation.Off;
  #wearDetection = true;
  #gameMode = false;
  #eq: EqPreset = EqPreset.Standard;
  #keyBindings: KeyBinding[] = DEFAULT_KEY_BINDINGS.map((b) => ({ ...b }));
  #bindCode = "";
  #stabilisation = false;
  #videoParams: VideoParams = { angle: 0, durationS: 600 };
  #photoParams: PhotoParams = { widthPx: 2048, heightPx: 1536, sharpness: 2 };
  #videoResolution: VideoResolution = { widthPx: 1920, heightPx: 1080, fps: 30 };
  #recordingPrompt = true;
  #callAutoRecord = false;
  #callState: CallState = { active: false, recording: false };
  #deviceMode: DeviceMode = DeviceMode.Empty;
  #speakMode: SpeakMode | null = null;
  #speakerRoute: SpeakerRoute = SpeakerRoute.Glasses;
  #recognising = false;
  #wakeWords: WakeWord[];
  #wakeWordSettings: WakeWordSetting[];
  #p2pOpen = false;
  #transfers = new Map<number, TransferSlot>();
  #transferSeq = 0;
  #audioDownlinkBusy = false;
  #cancelBattery: (() => void) | null = null;
  #cancelMic: (() => void) | null = null;

  constructor(options: MockOptions = {}) {
    this.#clock = options.clock ?? systemClock;
    this.#recordingFormat = options.recordingFormat ?? RecordingFormatValues.Opus;
    this.#opts = {
      connectDelayMs: options.connectDelayMs ?? MOCK_DEFAULTS.connectDelayMs,
      bleBytesPerSecond: options.bleBytesPerSecond ?? MOCK_DEFAULTS.bleBytesPerSecond,
      wifiBytesPerSecond: options.wifiBytesPerSecond ?? MOCK_DEFAULTS.wifiBytesPerSecond,
      totalStorageBytes: options.totalStorageBytes ?? MOCK_DEFAULTS.totalStorageBytes,
      recordingBytesPerSecond:
        options.recordingBytesPerSecond ?? recordingBytes(1, this.#recordingFormat),
      videoBytesPerSecond: options.videoBytesPerSecond ?? MOCK_DEFAULTS.videoBytesPerSecond,
      micBytesPerSecond: options.micBytesPerSecond ?? MOCK_DEFAULTS.micBytesPerSecond,
      batteryReportIntervalMs:
        options.batteryReportIntervalMs ?? MOCK_DEFAULTS.batteryReportIntervalMs,
      streamMicAudio: options.streamMicAudio ?? true,
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
    this.#wakeWords = (options.wakeWords ?? MOCK_WAKE_WORDS).map((w) => ({ ...w }));
    this.#wakeWordSettings = this.#wakeWords.map((w) => ({
      index: w.index,
      enabled: w.kind === WakeWordKind.AiPhrase,
    }));
  }

  // --- lifecycle ------------------------------------------------------------

  get state(): ConnectionState {
    return this.#state;
  }

  on<K extends keyof AllGlassesEvents>(
    event: K,
    handler: (payload: AllGlassesEvents[K]) => void,
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
    this.#armBatteryReports();

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
    this.#teardownRadios();
    this.#setState(ConnectionState.Disconnected);
  }

  // --- device ---------------------------------------------------------------

  async getFeatures(): Promise<Features> {
    this.#requireConnected();
    this.#record(Command.GET_SUPPORTED_FEATURES);
    return { ...this.#features };
  }

  async getBattery(): Promise<BatteryStatus> {
    this.#requireConnected();
    this.#record(Command.GET_BATTERY);
    const status = this.#battery();
    this.#emitter.emit("battery", status);
    return status;
  }

  async getDiskInfo(): Promise<DiskInfo> {
    this.#requireConnected();
    this.#record(Command.GET_DISK_INFO);
    const info = this.#disk();
    this.#emitter.emit("diskInfo", info);
    return info;
  }

  async setTime(date: Date): Promise<void> {
    this.#requireConnected();
    const seconds = Math.floor(date.getTime() / 1000);
    this.#record(Command.SET_TIME, u32le(seconds));
  }

  /** 0x0001-0x0004 + 0x0006, as one call. Five commands, one screen. */
  async getIdentity(): Promise<DeviceIdentity> {
    this.#requireConnected();
    for (const id of [
      Command.GET_PRODUCT_INFO,
      Command.GET_PRODUCT_MODEL,
      Command.GET_VERSION,
      Command.GET_HARDWARE_INFO,
      Command.GET_DEVICE_NAME,
    ]) {
      this.#record(id);
    }
    this.#emitter.emit("firmwareVersion", this.#identity.firmwareVersion);
    return { ...this.#identity };
  }

  async heartbeat(): Promise<void> {
    this.#requireConnected();
    this.#record(Command.HEARTBEAT);
  }

  // --- settings -------------------------------------------------------------

  async getNoiseCancellation(): Promise<NoiseCancellation> {
    this.#requireConnected();
    this.#record(Command.GET_ANC_STATE);
    return this.#anc;
  }

  async setNoiseCancellation(mode: NoiseCancellation): Promise<void> {
    this.#requireConnected();
    this.#record(Command.SET_ANC, u8(mode));
    this.#anc = mode;
    this.#emitter.emit("noiseCancellationChanged", mode);
  }

  async getWearDetection(): Promise<boolean> {
    this.#requireConnected();
    this.#record(Command.GET_WEAR_DETECTION);
    return this.#wearDetection;
  }

  async setWearDetection(enabled: boolean): Promise<void> {
    this.#requireConnected();
    if (!this.#features.wearDetection) {
      throw new GlassesError(GlassesErrorCode.Unsupported, "mock: no wear detection");
    }
    this.#record(Command.SET_WEAR_DETECTION, u8(enabled ? 1 : 0));
    this.#wearDetection = enabled;
  }

  async getGameMode(): Promise<boolean> {
    this.#requireConnected();
    this.#record(Command.GET_GAME_MODE);
    return this.#gameMode;
  }

  async setGameMode(enabled: boolean): Promise<void> {
    this.#requireConnected();
    this.#record(Command.SET_GAME_MODE, u8(enabled ? 1 : 0));
    this.#gameMode = enabled;
    this.#emitter.emit("gameModeChanged", enabled);
  }

  async getEqualiser(): Promise<EqPreset> {
    this.#requireConnected();
    this.#record(Command.GET_EQ);
    return this.#eq;
  }

  async setEqualiser(preset: EqPreset): Promise<void> {
    this.#requireConnected();
    this.#record(Command.SET_EQ, u8(preset));
    this.#eq = preset;
  }

  async getKeyBindings(): Promise<KeyBinding[]> {
    this.#requireConnected();
    this.#record(Command.GET_KEY_FUNCTIONS);
    return this.#keyBindings.map((b) => ({ ...b }));
  }

  async setKeyBindings(bindings: KeyBinding[]): Promise<void> {
    this.#requireConnected();
    this.#record(Command.SET_KEY_FUNCTIONS, u8(bindings.length));
    this.#keyBindings = bindings.map((b) => ({ ...b }));
  }

  async findDevice(on: boolean): Promise<void> {
    this.#requireConnected();
    this.#record(Command.FIND_DEVICE, u8(on ? 1 : 0));
    this.#emitter.emit("findDeviceChanged", on);
  }

  async setBindCode(code: string): Promise<void> {
    this.#requireConnected();
    // Length only: the bind code is a shared secret and must never reach a log.
    this.#record(Command.SET_BIND_CODE, u8(code.length));
    this.#bindCode = code;
  }

  async getBindCode(): Promise<string> {
    this.#requireConnected();
    this.#record(Command.GET_BIND_CODE);
    return this.#bindCode;
  }

  async getStabilisation(): Promise<{ enabled: boolean; supported: boolean }> {
    this.#requireConnected();
    this.#record(Command.GET_STABILIZATION);
    this.#emitter.emit("stabilisationSupportChanged", this.#features.stabilization);
    return { enabled: this.#stabilisation, supported: this.#features.stabilization };
  }

  async setStabilisation(enabled: boolean): Promise<void> {
    this.#requireConnected();
    if (!this.#features.stabilization) {
      throw new GlassesError(GlassesErrorCode.Unsupported, "mock: no stabilisation");
    }
    this.#record(Command.SET_STABILIZATION, u8(enabled ? 1 : 0));
    this.#stabilisation = enabled;
  }

  async getVideoParams(): Promise<VideoParams> {
    this.#requireConnected();
    this.#record(Command.GET_VIDEO_PARAMS);
    return { ...this.#videoParams };
  }

  async setVideoParams(params: VideoParams): Promise<void> {
    this.#requireConnected();
    this.#record(Command.SET_VIDEO_PARAMS, u32le(params.durationS));
    this.#videoParams = { ...params };
  }

  async getPhotoParams(): Promise<PhotoParams> {
    this.#requireConnected();
    this.#record(Command.GET_PHOTO_PARAMS);
    return { ...this.#photoParams };
  }

  async setPhotoParams(params: PhotoParams): Promise<void> {
    this.#requireConnected();
    if (params.sharpness < SHARPNESS_MIN || params.sharpness > SHARPNESS_MAX) {
      throw new GlassesError(
        GlassesErrorCode.Unsupported,
        `mock: sharpness must be ${SHARPNESS_MIN}-${SHARPNESS_MAX}, got ${params.sharpness}`,
      );
    }
    this.#record(Command.SET_PHOTO_PARAMS, u8(params.sharpness));
    this.#photoParams = { ...params };
  }

  async setVideoResolution(resolution: VideoResolution): Promise<void> {
    this.#requireConnected();
    this.#record(Command.SET_VIDEO_RESOLUTION, u8(resolution.fps));
    this.#videoResolution = { ...resolution };
    this.#emitter.emit("videoResolutionChanged", { ...resolution });
  }

  async getRecordingPrompt(): Promise<boolean> {
    this.#requireConnected();
    this.#record(Command.GET_RECORDING_PROMPT);
    return this.#recordingPrompt;
  }

  async setRecordingPrompt(enabled: boolean): Promise<void> {
    this.#requireConnected();
    this.#record(Command.SET_RECORDING_PROMPT, u8(enabled ? 1 : 0));
    this.#recordingPrompt = enabled;
  }

  async getCallAutoRecord(): Promise<boolean> {
    this.#requireConnected();
    this.#record(Command.GET_CALL_AUTO_RECORD);
    return this.#callAutoRecord;
  }

  async setCallAutoRecord(enabled: boolean): Promise<void> {
    this.#requireConnected();
    this.#record(Command.SET_CALL_AUTO_RECORD, u8(enabled ? 1 : 0));
    this.#callAutoRecord = enabled;
  }

  // --- device control (0x0D01) ---------------------------------------------

  async setDeviceMode(mode: DeviceMode): Promise<void> {
    this.#requireConnected();
    this.#record(Command.DEVICE_CONTROL, u8(mode));
    this.#deviceMode = mode;
    this.#emitter.emit("runState", { mode, busy: mode !== DeviceMode.Empty });
  }

  async restartDevice(): Promise<void> {
    await this.setDeviceMode(DeviceMode.Restart);
    this.simulateDisconnect("mock: device restarting");
  }

  async factoryReset(): Promise<void> {
    await this.setDeviceMode(DeviceMode.FactoryReset);
    this.#files = [];
    this.#usedStorageBytes = 0;
    this.#bindCode = "";
    this.simulateDisconnect("mock: factory reset");
  }

  // --- voice ----------------------------------------------------------------

  /**
   * Kept for the base transport contract. It is `startMicUplink` — the app-side
   * verb — and maps to `AUDIO_CONTROL` 0x0A02, not to 0x0805, which is a report
   * the *device* sends.
   */
  async startVoiceSession(): Promise<void> {
    return this.startMicUplink();
  }

  async stopVoiceSession(): Promise<void> {
    return this.stopMicUplink();
  }

  async startMicUplink(): Promise<void> {
    this.#requireConnected();
    if (this.#voiceOpen) return;
    this.#record(Command.AUDIO_CONTROL, u8(AudioControl.StartUplink));
    this.#voiceOpen = true;
    this.#audioSeq = 0;
    this.#emitter.emit("voiceSessionChanged", true);
    if (this.#opts.streamMicAudio) this.#armMicStream();
  }

  async stopMicUplink(): Promise<void> {
    this.#requireConnected();
    if (!this.#voiceOpen) return;
    this.#record(Command.AUDIO_CONTROL, u8(AudioControl.StopUplink));
    this.#voiceOpen = false;
    this.#cancelMic?.();
    this.#cancelMic = null;
    this.#emitter.emit("voiceSessionChanged", false);
  }

  /**
   * Push Opus down `0x0A03`. Resolves when the bytes have gone, at the same
   * ~3 KB/s the microphone costs — a ten-second reply really does take ten
   * seconds to arrive. Streaming it in pieces is the only way to get audio out
   * sooner, which is exactly the point `SYSTEM.md` §7b makes about perceived
   * latency.
   */
  async sendAudio(opus: Uint8Array): Promise<void> {
    this.#requireConnected();
    if (this.#audioDownlinkBusy) {
      throw new GlassesError(
        GlassesErrorCode.DeviceBusy,
        "mock: 0x0A03 already has a write in flight",
      );
    }
    this.#record(Command.AUDIO_DATA, u16le(opus.length));
    this.#audioDownlinkBusy = true;
    try {
      await this.#clock.sleep((opus.length / this.#opts.bleBytesPerSecond) * 1000);
      this.#requireConnected();
    } finally {
      this.#audioDownlinkBusy = false;
    }
  }

  async setSpeakMode(mode: SpeakMode): Promise<void> {
    this.#requireConnected();
    this.#record(Command.DEVICE_CONTROL, u8(mode));
    this.#speakMode = mode;
    this.#emitter.emit("speakModeChanged", mode);
  }

  async speakStart(): Promise<void> {
    this.#requireConnected();
    this.#record(Command.DEVICE_CONTROL, u8(DeviceMode.SpeakStart));
    this.#speakMode = SpeakMode.Start;
    this.#emitter.emit("speakModeChanged", SpeakMode.Start);
  }

  /**
   * Keep-alive while a long reply streams. The device stops showing the speaking
   * state without it, so a truncated hold truncates the reply.
   */
  async speakHold(): Promise<void> {
    this.#requireConnected();
    if (this.#speakMode !== SpeakMode.Start && this.#speakMode !== SpeakMode.Hold) {
      throw new GlassesError(
        GlassesErrorCode.DeviceBusy,
        "mock: speakHold without speakStart — the device is not in speak mode",
      );
    }
    this.#record(Command.DEVICE_CONTROL, u8(SpeakMode.Hold));
    this.#speakMode = SpeakMode.Hold;
    this.#emitter.emit("speakModeChanged", SpeakMode.Hold);
  }

  async speakStop(): Promise<void> {
    this.#requireConnected();
    this.#record(Command.DEVICE_CONTROL, u8(DeviceMode.SpeakStop));
    this.#speakMode = SpeakMode.Stop;
    this.#emitter.emit("speakModeChanged", SpeakMode.Stop);
  }

  async getSpeakerRoute(): Promise<SpeakerRoute> {
    this.#requireConnected();
    return this.#speakerRoute;
  }

  async setSpeakerRoute(route: SpeakerRoute): Promise<void> {
    this.#requireConnected();
    this.#speakerRoute = route;
  }

  async getCallState(): Promise<CallState> {
    this.#requireConnected();
    this.#record(Command.GET_CALL_STATE);
    return { ...this.#callState };
  }

  // --- AI interface — every verb is the app's -------------------------------

  /**
   * Start *our* recogniser. Opens the mic uplink and nothing else: there is no
   * command that asks the glasses to transcribe, and this mock will never emit
   * `transcriptText` on its own, because the hardware never will either.
   */
  async startRecognition(): Promise<void> {
    this.#requireConnected();
    await this.startMicUplink();
    if (this.#recognising) return;
    this.#recognising = true;
    this.#emitter.emit("recognitionChanged", { active: true, owner: RecognitionOwner.App });
  }

  async stopRecognition(): Promise<void> {
    this.#requireConnected();
    await this.stopMicUplink();
    if (!this.#recognising) return;
    this.#recognising = false;
    this.#emitter.emit("recognitionChanged", { active: false, owner: RecognitionOwner.App });
  }

  isRecognising(): boolean {
    return this.#recognising;
  }

  // --- wake word ------------------------------------------------------------

  async getWakeWords(): Promise<WakeWord[]> {
    this.#requireConnected();
    if (!this.#features.voiceWakeup) {
      throw new GlassesError(GlassesErrorCode.Unsupported, "mock: no voice wakeup");
    }
    this.#record(Command.GET_WAKEWORD_LIST);
    return this.#wakeWords.map((w) => ({ ...w }));
  }

  async getWakeWordSettings(): Promise<WakeWordSetting[]> {
    this.#requireConnected();
    if (!this.#features.voiceWakeup) {
      throw new GlassesError(GlassesErrorCode.Unsupported, "mock: no voice wakeup");
    }
    this.#record(Command.GET_WAKEWORD_SETTING);
    return this.#wakeWordSettings.map((s) => ({ ...s }));
  }

  /**
   * Selection only. An index the firmware never listed is refused — which is
   * what "Hey Jarvis" will get on hardware, and the reason tap-to-talk is the
   * primary trigger (`ARCHITECTURE.md` §5.2b).
   */
  async setWakeWordEnabled(index: number, enabled: boolean): Promise<WakeWordSetting[]> {
    this.#requireConnected();
    if (!this.#features.voiceWakeup) {
      throw new GlassesError(GlassesErrorCode.Unsupported, "mock: no voice wakeup");
    }
    const setting = this.#wakeWordSettings.find((s) => s.index === index);
    if (!setting) {
      throw new GlassesError(
        GlassesErrorCode.Unsupported,
        `mock: no wake word at index ${index} — the list is firmware-fixed`,
      );
    }
    this.#record(Command.SET_WAKEWORD_SETTING, new Uint8Array([index, enabled ? 1 : 0]));
    setting.enabled = enabled;
    const snapshot = this.#wakeWordSettings.map((s) => ({ ...s }));
    this.#emitter.emit("wakeWordSettingsChanged", snapshot);
    return snapshot.map((s) => ({ ...s }));
  }

  // --- camera ---------------------------------------------------------------

  /**
   * Shutter to device storage. Returns immediately — the whole point is that
   * nothing crosses the radio until the nightly sync.
   */
  async capturePhoto(): Promise<RemoteFile> {
    this.#requireConnected();
    this.#record(Command.AI_PHOTO_START, u8(0));

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
    this.#emitCounts();
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

    const clarity = Math.max(SHARPNESS_MIN, Math.min(SHARPNESS_MAX, options.clarity ?? 2));
    // 2 KB at clarity 0 up to ~50 KB at clarity 6.
    const totalBytes = Math.round(2_048 * Math.pow(1.75, clarity));
    const totalMs = (totalBytes / this.#opts.bleBytesPerSecond) * 1000;

    const slot = this.#beginTransfer(name, totalBytes, "ble");
    this.#record(Command.FILE_FETCH_START, u8(clarity));
    await this.#clock.sleep(totalMs);
    this.#requireConnected();
    this.#finishTransfer(slot, totalBytes);

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
    this.#record(Command.AI_PHOTO_START, u8(1));

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
    this.#record(Command.LOCAL_RECORDING_CONTROL, u8(Toggle.On));
    this.#recordingSinceMs = this.#clock.now();
    this.#emitter.emit("recordingState", { recording: true, durationS: 0 });
    this.#emitter.emit("captureState", {
      kind: CaptureKind.Audio,
      active: true,
      durationS: 0,
    });
  }

  async stopLocalRecording(): Promise<void> {
    this.#requireConnected();
    if (this.#recordingSinceMs === null) return;

    const durationS = (this.#clock.now() - this.#recordingSinceMs) / 1000;
    this.#recordingSinceMs = null;
    this.#record(Command.LOCAL_RECORDING_CONTROL, u8(Toggle.Off));

    const wanted = Math.round(durationS * this.#opts.recordingBytesPerSecond);
    const free = this.#disk().freeBytes;
    if (wanted > free && this.#faults.storageFull) {
      throw new GlassesError(GlassesErrorCode.StorageFull, "mock: storage full during write");
    }
    const sizeBytes = Math.min(wanted, free);
    this.#usedStorageBytes += sizeBytes;

    const extension = this.#recordingFormat === RecordingFormatValues.Pcm16 ? "pcm" : "opus";
    this.#files.push({
      name: `REC_${String(++this.#fileCounter).padStart(4, "0")}.${extension}`,
      sizeBytes,
      uploaded: false,
      durationS,
    });
    this.#emitter.emit("recordingState", { recording: false, durationS: 0 });
    this.#emitter.emit("captureState", {
      kind: CaptureKind.Audio,
      active: false,
      durationS: 0,
    });
    this.#emitter.emit("diskInfo", this.#disk());
    this.#emitCounts();
  }

  async getLocalRecordingState(): Promise<CaptureState> {
    this.#requireConnected();
    this.#record(Command.LOCAL_RECORDING_STATE_REPORT, new Uint8Array(), CommandType.Notify);
    const state = this.#audioCaptureState();
    this.#emitter.emit("captureState", state);
    return state;
  }

  /**
   * 1080p at ~4.5 GB/h. Five minutes of it is bigger than a whole 16-hour day of
   * Opus audio, which is why `APPS-SCOPE.md` §3.2 makes storage policy a real
   * requirement rather than a settings toggle.
   */
  async startVideoRecording(): Promise<void> {
    this.#requireConnected();
    if (this.#videoSinceMs !== null) return;
    if (this.#disk().freeBytes <= 0) {
      throw new GlassesError(GlassesErrorCode.StorageFull, "mock: storage full");
    }
    this.#record(Command.DEVICE_CONTROL, u8(DeviceMode.Video));
    this.#deviceMode = DeviceMode.Video;
    this.#videoSinceMs = this.#clock.now();
    this.#emitter.emit("captureState", { kind: CaptureKind.Video, active: true, durationS: 0 });
  }

  async stopVideoRecording(): Promise<void> {
    this.#requireConnected();
    if (this.#videoSinceMs === null) return;

    const durationS = (this.#clock.now() - this.#videoSinceMs) / 1000;
    this.#videoSinceMs = null;
    this.#record(Command.DEVICE_CONTROL, u8(DeviceMode.VideoStop));
    this.#deviceMode = DeviceMode.Empty;

    const wanted = Math.round(durationS * this.#opts.videoBytesPerSecond);
    const sizeBytes = Math.min(wanted, this.#disk().freeBytes);
    this.#usedStorageBytes += sizeBytes;
    this.#files.push({
      name: `VID_${String(++this.#fileCounter).padStart(4, "0")}.mp4`,
      sizeBytes,
      uploaded: false,
      durationS,
    });
    this.#emitter.emit("captureState", { kind: CaptureKind.Video, active: false, durationS: 0 });
    this.#emitter.emit("diskInfo", this.#disk());
    this.#emitCounts();
  }

  async listFiles(): Promise<RemoteFile[]> {
    this.#requireConnected();
    this.#record(Command.GET_FILE_LIST);
    return this.#files.map((f) => ({ ...f }));
  }

  async deleteFile(name: string): Promise<void> {
    this.#requireConnected();
    const index = this.#files.findIndex((f) => f.name === name);
    if (index === -1) {
      throw new GlassesError(GlassesErrorCode.TransferFailed, `mock: no such file ${name}`);
    }
    this.#record(Command.DELETE_FILE, u8(index));
    this.#usedStorageBytes = Math.max(0, this.#usedStorageBytes - this.#files[index]!.sizeBytes);
    this.#files.splice(index, 1);
    this.#emitter.emit("diskInfo", this.#disk());
    this.#emitCounts();
  }

  /**
   * 0x0E03. Deletes everything, including audio that has never been synced.
   * The device does not argue, so the confirmation belongs in the UI — the
   * catalog marks this destructive for exactly that reason.
   */
  async deleteAllFiles(): Promise<ClearResult> {
    this.#requireConnected();
    this.#record(Command.DELETE_ALL_FILES);
    const result = this.#dropFiles(() => true);
    this.#emitter.emit("clearResult", result);
    this.#emitter.emit("diskInfo", this.#disk());
    this.#emitCounts();
    return result;
  }

  /**
   * 0x0911 清除未上传文件 — deletes exactly the files the device believes have
   * *not* been pulled yet. Uploaded files survive. This is the one command that
   * can throw away a day of capture, so `APPS-SCOPE.md` §3.2 says never reach for
   * it casually; it exists because a wedged, full device needs a way out.
   */
  async clearUnuploadedFiles(): Promise<ClearResult> {
    this.#requireConnected();
    this.#record(Command.CLEAR_UNUPLOADED_FILES);
    const result = this.#dropFiles((f) => !f.uploaded);
    this.#emitter.emit("clearResult", result);
    this.#emitter.emit("diskInfo", this.#disk());
    this.#emitCounts();
    return result;
  }

  async getMediaCounts(): Promise<MediaCounts> {
    this.#requireConnected();
    this.#record(Command.GET_FILE_COUNT);
    const counts = this.#counts();
    this.#emitter.emit("mediaCounts", counts);
    return counts;
  }

  // --- bulk transfer --------------------------------------------------------

  async openWifiAccessPoint(): Promise<WifiAccessPoint> {
    this.#requireConnected();
    if (!this.#features.wifiAp) {
      throw new GlassesError(GlassesErrorCode.Unsupported, "mock: no WiFi AP");
    }
    this.#record(Command.WIFI_AP_CONTROL, u8(Toggle.On));
    this.#apOpen = true;
    const state = this.#apState();
    this.#emitter.emit("wifiAccessPointChanged", state);
    this.#emitter.emit("wifiOperation", { operation: "apOpen", ok: true });
    return { ssid: state.ssid, password: state.password, host: state.host };
  }

  async closeWifiAccessPoint(): Promise<void> {
    this.#requireConnected();
    this.#record(Command.WIFI_AP_CONTROL, u8(Toggle.Off));
    this.#apOpen = false;
    this.#emitter.emit("wifiAccessPointChanged", this.#apState());
    this.#emitter.emit("wifiOperation", { operation: "apClose", ok: true });
  }

  async getWifiAccessPointState(): Promise<WifiApState> {
    this.#requireConnected();
    const state = this.#apState();
    this.#emitter.emit("wifiAccessPointChanged", state);
    return state;
  }

  async setWifiP2p(open: boolean): Promise<void> {
    this.#requireConnected();
    if (!this.#features.wifiP2p) {
      throw new GlassesError(GlassesErrorCode.Unsupported, "mock: no WiFi P2P");
    }
    this.#record(Command.WIFI_P2P_CONTROL, u8(open ? Toggle.On : Toggle.Off));
    this.#p2pOpen = open;
    this.#emitter.emit("wifiP2pChanged", {
      supported: true,
      name: "DIRECT-RelayGlasses",
      macAddress: "02:00:00:00:00:01",
      open,
    });
    this.#emitter.emit("wifiOperation", { operation: open ? "p2pOpen" : "p2pClose", ok: true });
  }

  /**
   * 0x0901 / 0x0902, both 已弃用 in v2.0.17.
   *
   * They configure the glasses' *own* hotspot, and the glasses have no station
   * mode at all — nothing here can make them join a network, which is why the
   * phone bridge is structural rather than a convenience (`ARCHITECTURE.md` §2).
   * Refusing is the honest behaviour: the alternative is an app that thinks it
   * put the glasses on WiFi.
   */
  async setAccessPointCredentials(_ssid: string, _password: string): Promise<void> {
    this.#requireConnected();
    throw new GlassesError(
      GlassesErrorCode.Unsupported,
      "mock: 0x0901/0x0902 are deprecated, and the glasses have no station mode — " +
        "the AP name and password are firmware-assigned",
    );
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

    const via = this.#apOpen ? "wifiAp" : "ble";
    const rate = via === "wifiAp" ? this.#opts.wifiBytesPerSecond : this.#opts.bleBytesPerSecond;
    const totalMs = (file.sizeBytes / rate) * 1000;
    const steps = Math.max(1, Math.min(50, Math.ceil(totalMs / 200)));
    const retriesWanted = this.#faults.transferRetries ?? 0;

    const slot = this.#beginTransfer(name, file.sizeBytes, via);
    this.#record(Command.FILE_FETCH_START, u16le(file.sizeBytes & 0xffff));

    for (let step = 1; step <= steps; step++) {
      await this.#clock.sleep(totalMs / steps);
      this.#requireConnected();
      if (slot.aborted) {
        this.#transfers.delete(slot.handle.id);
        throw new GlassesError(GlassesErrorCode.TransferFailed, `mock: transfer aborted (${name})`);
      }
      if (slot.retries < retriesWanted) {
        // The device asks for the chunk again: cost one extra chunk of time.
        await this.retryFileChunk(slot.handle.id, step);
        this.#requireConnected();
      }
      const received = Math.round((file.sizeBytes * step) / steps);
      slot.handle.receivedBytes = received;
      this.#emitTransfer(slot);
      onProgress?.({ receivedBytes: received, totalBytes: file.sizeBytes });
    }

    file.uploaded = true;
    this.#finishTransfer(slot, file.sizeBytes);
    return new Uint8Array(file.sizeBytes);
  }

  async retryFileChunk(transferId: number, chunkIndex: number): Promise<void> {
    this.#requireConnected();
    const slot = this.#transfers.get(transferId);
    if (!slot) {
      throw new GlassesError(
        GlassesErrorCode.TransferFailed,
        `mock: no transfer ${transferId} to retry`,
      );
    }
    this.#record(Command.FILE_DATA_RETRY, u16le(chunkIndex));
    slot.retries += 1;
    const chunkBytes = Math.max(1, Math.round(slot.handle.totalBytes / 50));
    const rate =
      slot.handle.via === "wifiAp" ? this.#opts.wifiBytesPerSecond : this.#opts.bleBytesPerSecond;
    await this.#clock.sleep((chunkBytes / rate) * 1000);
    this.#emitTransfer(slot);
  }

  async abortFileTransfer(transferId: number): Promise<void> {
    this.#requireConnected();
    const slot = this.#transfers.get(transferId);
    if (!slot) {
      throw new GlassesError(
        GlassesErrorCode.TransferFailed,
        `mock: no transfer ${transferId} to abort`,
      );
    }
    this.#record(Command.FILE_UPLOAD_ABORT, u16le(transferId));
    slot.aborted = true;
    this.#emitTransfer(slot);
  }

  activeTransfers(): FileTransfer[] {
    return [...this.#transfers.values()].map((slot) => ({ ...slot.handle }));
  }

  // --- OTA ------------------------------------------------------------------

  async getOtaInfo(): Promise<OtaInfo> {
    this.#requireConnected();
    this.#record(Command.GET_OTA_INFO);
    const battery = this.#battery();
    this.#emitter.emit("firmwareVersion", this.#identity.firmwareVersion);
    return {
      currentVersion: this.#identity.firmwareVersion,
      maxImageBytes: 8 * 1024 * 1024,
      batteryOk: battery.percent >= MOCK_DEFAULTS.otaMinBatteryPercent,
    };
  }

  /**
   * Firmware goes over the same 3 KB/s link as everything else: a 2 MB image is
   * about eleven minutes. That is why the UI has to say so before it starts.
   */
  async startOta(image: Uint8Array): Promise<void> {
    this.#requireConnected();
    const battery = this.#battery();
    if (battery.percent < MOCK_DEFAULTS.otaMinBatteryPercent) {
      throw new GlassesError(
        GlassesErrorCode.LowBattery,
        `mock: OTA needs ${MOCK_DEFAULTS.otaMinBatteryPercent}% battery, have ${battery.percent}%`,
      );
    }
    this.#record(Command.OTA_START, u32le(image.length));

    const steps = 20;
    const totalMs = (image.length / this.#opts.bleBytesPerSecond) * 1000;
    for (let step = 1; step <= steps; step++) {
      await this.#clock.sleep(totalMs / steps);
      this.#requireConnected();
      this.#emitter.emit("otaProgress", {
        sentBytes: Math.round((image.length * step) / steps),
        totalBytes: image.length,
        done: false,
      });
    }
    this.#record(Command.OTA_COMPLETE, u8(0));
    this.#emitter.emit("otaProgress", {
      sentBytes: image.length,
      totalBytes: image.length,
      done: true,
    });
  }

  // --- video ----------------------------------------------------------------

  async startPreview(): Promise<string> {
    this.#requireConnected();
    if (!this.#features.livePreview) {
      throw new GlassesError(GlassesErrorCode.Unsupported, "mock: no live preview");
    }
    const battery = this.#battery();
    if (battery.percent < MOCK_DEFAULTS.previewMinBatteryPercent) {
      // QGLivePreviewErrorCodeLowBattery (0x08) — the device refuses, loudly.
      throw new GlassesError(
        GlassesErrorCode.LowBattery,
        `mock: live preview needs ${MOCK_DEFAULTS.previewMinBatteryPercent}% battery, ` +
          `have ${battery.percent}%`,
      );
    }
    this.#record(Command.PREVIEW_CONTROL, u8(Toggle.On));
    this.#apOpen = true;
    this.#previewing = true;
    const url = "rtsp://192.168.31.1:8554/live";
    this.#emitter.emit("wifiAccessPointChanged", this.#apState());
    this.#emitter.emit("rtspUrl", url);
    return url;
  }

  async stopPreview(): Promise<void> {
    this.#requireConnected();
    this.#record(Command.PREVIEW_CONTROL, u8(Toggle.Off));
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

  /**
   * The app's recogniser produced text.
   *
   * Deliberately a *test control* and not something the mock does on its own:
   * the glasses have no ASR (`SYSTEM.md` §7b), so any transcript in this system
   * came from code we wrote and pay for.
   */
  emitTranscript(text: string): void {
    this.#emitter.emit("transcriptText", text);
  }

  /** The device reporting 0x0805 / 0x0803 — the user asked for the assistant. */
  emitAiInterfaceEvent(event: AiInterfaceEvent): void {
    this.#emitter.emit("aiInterfaceEvent", event);
  }

  /** 0x0806 — the vendor's own cloud assistant, which is not ours. */
  emitVendorAiPrompt(text: string): void {
    this.#emitter.emit("vendorAiPrompt", text);
  }

  setCharging(charging: boolean): void {
    this.#battery(); // settle accumulated drain at the old rate first
    this.#charging = charging;
  }

  setFaults(faults: MockFaults): void {
    this.#faults = { ...this.#faults, ...faults };
  }

  setCallState(state: CallState): void {
    this.#callState = { ...state };
  }

  simulateDisconnect(reason = "mock: disconnected"): void {
    if (this.#state === ConnectionState.Disconnected) return;
    this.#teardownRadios();
    this.#recordingSinceMs = null;
    this.#videoSinceMs = null;
    this.#setState(ConnectionState.Disconnected);
    this.#emitter.emit("error", new GlassesError(GlassesErrorCode.NotConnected, reason));
  }

  /** Every command this transport issued, oldest first. */
  get commandLog(): readonly CommandRecord[] {
    return this.#log;
  }

  /** How many times a given command ID went out. */
  commandCount(id: number): number {
    return this.#log.filter((entry) => entry.id === id).length;
  }

  clearCommandLog(): void {
    this.#log = [];
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

  get isWifiP2pOpen(): boolean {
    return this.#p2pOpen;
  }

  get deviceMode(): DeviceMode {
    return this.#deviceMode;
  }

  get speakMode(): SpeakMode | null {
    return this.#speakMode;
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

  #record(
    id: number,
    payload: Uint8Array = new Uint8Array(),
    type: CommandType = CommandType.Request,
  ): void {
    const entry: CommandRecord = {
      at: this.#clock.now(),
      id,
      name: commandName(id),
      type,
      seq: this.#seq.next(),
      payloadHex: hex(payload),
    };
    this.#log.push(entry);
    this.#emitter.emit("command", entry);
  }

  #teardownRadios(): void {
    this.#voiceOpen = false;
    this.#recognising = false;
    this.#previewing = false;
    this.#apOpen = false;
    this.#p2pOpen = false;
    this.#audioDownlinkBusy = false;
    this.#cancelMic?.();
    this.#cancelMic = null;
    this.#cancelBattery?.();
    this.#cancelBattery = null;
    for (const slot of this.#transfers.values()) slot.aborted = true;
  }

  #armBatteryReports(): void {
    const interval = this.#opts.batteryReportIntervalMs;
    if (interval <= 0) return;
    const tick = (): void => {
      this.#cancelBattery = null;
      if (this.#state !== ConnectionState.Connected) return;
      this.#emitter.emit("battery", this.#battery());
      this.#cancelBattery = this.#clock.setTimeout(tick, interval);
    };
    this.#cancelBattery = this.#clock.setTimeout(tick, interval);
  }

  #armMicStream(): void {
    const period = RATES.audioChunkMs;
    const bytes = Math.max(1, Math.round((this.#opts.micBytesPerSecond * period) / 1000));
    const tick = (): void => {
      this.#cancelMic = null;
      if (!this.#voiceOpen || this.#state !== ConnectionState.Connected) return;
      this.emitAudioChunk(bytes);
      this.#cancelMic = this.#clock.setTimeout(tick, period);
    };
    this.#cancelMic = this.#clock.setTimeout(tick, period);
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
    if (this.#videoSinceMs !== null) {
      const seconds = (this.#clock.now() - this.#videoSinceMs) / 1000;
      used += seconds * this.#opts.videoBytesPerSecond;
    }
    const total = this.#opts.totalStorageBytes;
    return { totalBytes: total, freeBytes: Math.max(0, Math.round(total - used)) };
  }

  #apState(): WifiApState {
    return {
      ssid: "QCGlasses-MOCK",
      password: "12345678",
      host: "192.168.31.1",
      macAddress: "02:00:00:00:00:00",
      open: this.#apOpen,
      // ARCHITECTURE.md §2.1: joining the glasses' AP costs the phone its uplink.
      phoneUplinkSuspended: this.#apOpen,
    };
  }

  #audioCaptureState(): CaptureState {
    const active = this.#recordingSinceMs !== null;
    return {
      kind: CaptureKind.Audio,
      active,
      durationS: active ? (this.#clock.now() - this.#recordingSinceMs!) / 1000 : 0,
    };
  }

  #counts(): MediaCounts {
    let photos = 0;
    let videos = 0;
    let recordings = 0;
    let totalBytes = 0;
    for (const file of this.#files) {
      totalBytes += file.sizeBytes;
      if (file.name.endsWith(".jpg")) photos++;
      else if (file.name.endsWith(".mp4")) videos++;
      else recordings++;
    }
    return { photos, videos, recordings, totalBytes };
  }

  #emitCounts(): void {
    this.#emitter.emit("mediaCounts", this.#counts());
  }

  #dropFiles(predicate: (file: RemoteFile) => boolean): ClearResult {
    const doomed = this.#files.filter(predicate);
    const freedBytes = doomed.reduce((n, f) => n + f.sizeBytes, 0);
    this.#files = this.#files.filter((f) => !predicate(f));
    this.#usedStorageBytes = Math.max(0, this.#usedStorageBytes - freedBytes);
    return { deletedFiles: doomed.length, freedBytes };
  }

  #beginTransfer(name: string, totalBytes: number, via: "ble" | "wifiAp"): TransferSlot {
    const slot: TransferSlot = {
      handle: { id: ++this.#transferSeq, name, totalBytes, receivedBytes: 0, via },
      retries: 0,
      aborted: false,
    };
    this.#transfers.set(slot.handle.id, slot);
    this.#emitTransfer(slot);
    return slot;
  }

  #emitTransfer(slot: TransferSlot): void {
    const progress: FileTransferProgress = { ...slot.handle, retries: slot.retries };
    this.#emitter.emit("fileTransferProgress", progress);
  }

  #finishTransfer(slot: TransferSlot, receivedBytes: number): void {
    slot.handle.receivedBytes = receivedBytes;
    this.#emitTransfer(slot);
    this.#transfers.delete(slot.handle.id);
  }

  async #playTrace(trace: Trace): Promise<void> {
    let previous = 0;
    for (const record of trace.events) {
      await this.#clock.sleep(Math.max(0, record.tMs - previous));
      previous = record.tMs;
      if (this.#state === ConnectionState.Disconnected) return;
      this.#emitter.emit(
        record.event as GlassesEventName,
        record.payload as AllGlassesEvents[GlassesEventName] & never,
      );
    }
  }
}

// --- small helpers -----------------------------------------------------------

function u8(value: number): Uint8Array {
  return new Uint8Array([value & 0xff]);
}

function u16le(value: number): Uint8Array {
  return new Uint8Array([value & 0xff, (value >>> 8) & 0xff]);
}

function u32le(value: number): Uint8Array {
  return new Uint8Array([
    value & 0xff,
    (value >>> 8) & 0xff,
    (value >>> 16) & 0xff,
    (value >>> 24) & 0xff,
  ]);
}

function hex(bytes: Uint8Array): string {
  let out = "";
  for (const byte of bytes) out += byte.toString(16).padStart(2, "0");
  return out;
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
