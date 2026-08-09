/**
 * Domain types for the glasses bridge.
 *
 * These mirror what the vendor SDKs actually deliver — `QCDeviceTouchAction` and
 * the `QCSDKManagerDelegate` callbacks on iOS, the `GlassesControl` listeners on
 * Android — rather than inventing a parallel vocabulary. Where the two platforms
 * disagree, the shape here is the union, and the platform adapter normalises.
 *
 * No `enum` anywhere: Node's native type stripping only accepts erasable syntax,
 * and `enum` emits runtime code. Const objects plus derived unions give the same
 * ergonomics and survive stripping.
 */

// --- connection -------------------------------------------------------------

export const ConnectionState = {
  Disconnected: "disconnected",
  Connecting: "connecting",
  Connected: "connected",
  Reconnecting: "reconnecting",
} as const;

export type ConnectionState = (typeof ConnectionState)[keyof typeof ConnectionState];

// --- input ------------------------------------------------------------------

/**
 * Mirrors `QCDeviceTouchAction`. Wear and remove arrive on the same channel as
 * taps, which is a vendor quirk — the transport re-emits them as `wear` too,
 * because capture gating cares about them and nothing else does.
 */
export const TouchAction = {
  Wear: "wear",
  Remove: "remove",
  Forward: "forward",
  Backward: "backward",
  LongPress: "longPress",
  SingleTap: "singleTap",
  DoubleTap: "doubleTap",
  TripleTap: "tripleTap",
} as const;

export type TouchAction = (typeof TouchAction)[keyof typeof TouchAction];

// --- audio ------------------------------------------------------------------

export const AudioFormat = {
  /** What the device streams natively; do not transcode without a reason. */
  Opus: "opus",
  /** 16-bit signed little-endian. */
  Pcm16: "pcm16",
} as const;

export type AudioFormat = (typeof AudioFormat)[keyof typeof AudioFormat];

export interface AudioChunk {
  data: Uint8Array;
  format: AudioFormat;
  /** 16000 on this hardware. */
  sampleRate: number;
  channels: number;
  /** Monotonic per voice session; gaps mean dropped chunks. */
  sequence: number;
  /** Device clock, milliseconds. Aligned by `setTime` (protocol 0x0903). */
  deviceTimeMs: number;
}

// --- camera -----------------------------------------------------------------

export interface Photo {
  data: Uint8Array;
  mimeType: "image/jpeg";
  widthPx?: number;
  heightPx?: number;
}

export interface PhotoProgress {
  receivedBytes: number;
  /** Null until the device reports a total. */
  totalBytes: number | null;
  chunkIndex: number;
  chunkCount: number | null;
}

export interface PhotoOptions {
  /**
   * Requested pixel dimensions. This is a latency dial, not a quality setting:
   * photos arrive over BLE at a few KB/s, so a full-size JPEG takes tens of
   * seconds. See docs/ARCHITECTURE.md §5.2.
   *
   * Only relevant to `takePhoto`, which delivers immediately. `capturePhoto`
   * writes full resolution to device storage and costs nothing to call.
   */
  maxWidth?: number;
  maxHeight?: number;
}

export interface ThumbnailOptions {
  /**
   * Vendor thumbnail quality, 0-6. The Android SDK's own note: "the higher the
   * number, the clearer the image, but the slower the transmission speed."
   * The device exposes the latency dial directly — use it rather than pulling a
   * full image and downscaling.
   */
  clarity?: number;
}

// --- device state -----------------------------------------------------------

export interface BatteryStatus {
  /** 0-100. */
  percent: number;
  charging: boolean;
}

export interface DiskInfo {
  totalBytes: number;
  freeBytes: number;
}

export interface RecordingState {
  recording: boolean;
  /** Seconds elapsed in the current recording, 0 when stopped. */
  durationS: number;
}

export interface RemoteFile {
  name: string;
  sizeBytes: number;
  /**
   * Whether the device considers this file already pulled. The firmware tracks
   * it — protocol 0x0911 is "clear un-uploaded files" — so never delete on the
   * basis of our own bookkeeping alone.
   */
  uploaded: boolean;
  durationS?: number;
}

export interface WifiAccessPoint {
  ssid: string;
  password: string;
  /** 192.168.31.1 on this hardware. */
  host: string;
}

/**
 * Capability bitmap from protocol 0x0005 获取支持功能. Query before issuing
 * anything else — firmware revisions differ in what they honour.
 */
export interface Features {
  localRecording: boolean;
  wifiAp: boolean;
  wifiP2p: boolean;
  livePreview: boolean;
  voiceWakeup: boolean;
  wearDetection: boolean;
  stabilization: boolean;
  /** Anything the adapter did not recognise, by raw bit index. */
  unknownBits: number[];
}

// --- errors -----------------------------------------------------------------

export const GlassesErrorCode = {
  NotConnected: "notConnected",
  Timeout: "timeout",
  DeviceBusy: "deviceBusy",
  Unsupported: "unsupported",
  TransferFailed: "transferFailed",
  LowBattery: "lowBattery",
  StorageFull: "storageFull",
  ConnectFailed: "connectFailed",
} as const;

export type GlassesErrorCode = (typeof GlassesErrorCode)[keyof typeof GlassesErrorCode];

export class GlassesError extends Error {
  readonly code: GlassesErrorCode;

  constructor(code: GlassesErrorCode, message: string) {
    super(message);
    this.name = "GlassesError";
    this.code = code;
  }
}

// --- events -----------------------------------------------------------------

/** Event name to payload. Keys are the wire vocabulary for traces. */
export interface GlassesEvents {
  connectionChanged: ConnectionState;
  battery: BatteryStatus;
  touch: TouchAction;
  /** Derived from Wear/Remove touch actions; true when the glasses go on. */
  wear: boolean;
  /** Live microphone, only between startVoiceSession and stopVoiceSession. */
  audioChunk: AudioChunk;
  voiceSessionChanged: boolean;
  /** Device-side speech recognition, when the firmware provides it. */
  transcriptText: string;
  photoProgress: PhotoProgress;
  photo: Photo;
  recordingState: RecordingState;
  diskInfo: DiskInfo;
  rtspUrl: string;
  error: GlassesError;
}

export type GlassesEventName = keyof GlassesEvents;

export type Unsubscribe = () => void;
