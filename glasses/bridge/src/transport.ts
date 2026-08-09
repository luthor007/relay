/**
 * The transport contract.
 *
 * One interface, three implementations:
 *
 *   MockTransport      — this package; no hardware, deterministic, fault-injecting
 *   AndroidTransport   — native module wrapping LIB_GLASSES_SDK (com.glasses.*)
 *   IosTransport       — native module wrapping QCSDK.framework
 *
 * The whole product surface — onboarding, memory, sessions, connectors — is
 * written against this and nothing else, so it can be built and UI-tested with
 * no glasses present. That matters concretely: the iOS frameworks are arm64
 * device-only, so linking them removes the Simulator as an option (see
 * docs/APPS-SCOPE.md §5).
 *
 * Method names are deliberately product-level, not protocol-level. The command
 * IDs each one maps to are noted so the native adapters have an unambiguous
 * target; the spec lives in glasses/protocol and docs/APPS-SCOPE.md.
 */

import type {
  BatteryStatus,
  ConnectionState,
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

export interface ConnectOptions {
  /** Opaque per-platform handle: a CoreBluetooth UUID on iOS, a MAC on Android. */
  deviceId?: string;
  /** Give up after this long. */
  timeoutMs?: number;
}

export interface FetchProgress {
  receivedBytes: number;
  totalBytes: number;
}

export interface GlassesTransport {
  readonly state: ConnectionState;

  connect(options?: ConnectOptions): Promise<void>;
  disconnect(): Promise<void>;

  on<K extends keyof GlassesEvents>(
    event: K,
    handler: (payload: GlassesEvents[K]) => void,
  ): Unsubscribe;

  // --- device ---------------------------------------------------------------

  /** Protocol 0x0005. Call before anything else; firmware revisions differ. */
  getFeatures(): Promise<Features>;
  /** Protocol 0x0101. */
  getBattery(): Promise<BatteryStatus>;
  /** Protocol 0x0909 / 0x091C. */
  getDiskInfo(): Promise<DiskInfo>;
  /** Protocol 0x0903. Align the device clock before any capture. */
  setTime(date: Date): Promise<void>;

  // --- interactive voice (capture Path A) -----------------------------------

  /**
   * Open the live microphone stream; `audioChunk` events follow until stopped.
   * Expensive on both batteries — open on user intent, close immediately after.
   * Protocol 0x0805.
   */
  startVoiceSession(): Promise<void>;
  stopVoiceSession(): Promise<void>;

  // --- camera ---------------------------------------------------------------
  //
  // Three calls, because photos have the same split as audio: most of them are
  // for the memory and nobody is waiting on them, while a few are answering a
  // question right now.
  //
  //   capturePhoto     shutter to device storage, full resolution, returns at
  //                    once. Syncs later over WiFi with the day's audio. Use
  //                    this for everything passive.
  //   fetchThumbnail   cheap preview of a stored photo over BLE.
  //   takePhoto        capture AND deliver immediately over BLE. Only when the
  //                    agent has to see it to answer.
  //
  // Reaching for takePhoto by default is the mistake: it pays tens of seconds
  // of BLE transfer for an image that could have ridden the nightly sync at
  // full resolution for free.

  /**
   * Capture at full resolution to the glasses' own storage. Returns as soon as
   * the shutter fires; nothing transfers. The file appears in `listFiles` and
   * syncs over the access point with everything else.
   */
  capturePhoto(): Promise<RemoteFile>;

  /**
   * Pull a low-resolution preview of a stored photo over BLE.
   * Backed by the vendor's thumbnail path, whose clarity scale (0-6) trades
   * sharpness against transfer time directly.
   */
  fetchThumbnail(name: string, options?: ThumbnailOptions): Promise<Photo>;

  /**
   * Capture and deliver in one call, chunked over BLE. Emits `photoProgress`
   * throughout — at a few KB/s this is seconds, not milliseconds, so callers
   * must show progress rather than block. Protocol 0x0906 / 0x0907.
   *
   * Prefer `capturePhoto` unless something is genuinely blocked on the pixels.
   */
  takePhoto(options?: PhotoOptions): Promise<Photo>;

  // --- passive capture (Path B) ---------------------------------------------

  /**
   * Record to the glasses' own storage. This is the all-day path: no sustained
   * radio, and it survives the phone being out of range. Protocol 0x0E04.
   */
  startLocalRecording(): Promise<void>;
  stopLocalRecording(): Promise<void>;
  /** Protocol 0x0E01. */
  listFiles(): Promise<RemoteFile[]>;
  /** Protocol 0x0E02. Check `uploaded` first. */
  deleteFile(name: string): Promise<void>;

  // --- bulk transfer --------------------------------------------------------

  /**
   * Open the device's WiFi access point for bulk file transfer. The host must
   * then join that network, which costs it its own WiFi uplink — so this is a
   * deliberate, foregrounded operation. Protocol 0x090B.
   */
  openWifiAccessPoint(): Promise<WifiAccessPoint>;
  closeWifiAccessPoint(): Promise<void>;

  /**
   * Pull a recording. Only viable over the WiFi AP: a day of audio is 173 MB to
   * 1.8 GB, which over BLE would take longer than the day took to record.
   */
  fetchFile(name: string, onProgress?: (p: FetchProgress) => void): Promise<Uint8Array>;

  // --- video ----------------------------------------------------------------

  /** Start RTSP preview and resolve the stream URL. Protocol 0x090A → 0x0908. */
  startPreview(): Promise<string>;
  stopPreview(): Promise<void>;
}
