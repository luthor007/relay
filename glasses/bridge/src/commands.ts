/**
 * Every glasses command, by hand.
 *
 * `ORCHESTRATOR.md` §5 gives the reason, and it is not a completeness fetish:
 * *"Voice is the point, but a product where the only input is speech fails in a
 * quiet room, a loud room, and on a bad day."* So everything the voice loop can
 * do has to be tappable, which means the app needs a typed handle on all 92
 * commands rather than the six the demo happened to use.
 *
 * **`glasses/protocol/commands.py` is the source of truth for the IDs below.**
 * They were extracted from it mechanically, not transcribed from the PDF, and
 * `test/commands.test.ts` re-parses that file on every run and fails if the two
 * ever drift. If they disagree, the Python is right and this file is wrong.
 *
 * Three things this file is deliberate about:
 *
 *   1. **The glasses do not recognise speech.** `0x0803`/`0x0805` are the device
 *      reporting an *event*; every verb in the spec's own description belongs to
 *      the app (`SYSTEM.md` §7b). So the AI-interface surface here is an inbound
 *      event plus app-owned `startRecognition`/`stopRecognition`. There is no
 *      method that asks the device to transcribe, because there is no such
 *      command.
 *   2. **Wake words come from a firmware-fixed list.** `0x0F01` returns what the
 *      DSP model was trained on; `0x0F02`/`0x0F03` read and set which entries are
 *      active (`ARCHITECTURE.md` §5.2b). Nothing here accepts a phrase — the
 *      selection API takes an index that must have come from the device.
 *   3. **`0x0A03` is bidirectional.** Mic up *and* audio down, same channel, same
 *      ~3 KB/s Opus budget (`SYSTEM.md` §7b). `0x0A02` controls the uplink only.
 *
 * No `enum`, no parameter properties, no namespaces: Node strips types natively
 * and any of those is a parse error rather than a lint warning.
 */

import type {
  BatteryStatus,
  DiskInfo,
  Features,
  GlassesEvents,
  Photo,
  RemoteFile,
  WifiAccessPoint,
} from "./types.ts";
import type { GlassesTransport } from "./transport.ts";

// ---------------------------------------------------------------------------
// 1. Packet type
// ---------------------------------------------------------------------------

/** Byte 2 of the command header. Mirrors `CommandType` in commands.py. */
export const CommandType = {
  Request: 1,
  Response: 2,
  Notify: 3,
} as const;

export type CommandType = (typeof CommandType)[keyof typeof CommandType];

// ---------------------------------------------------------------------------
// 2. Command IDs — all 92, generated from glasses/protocol/commands.py
// ---------------------------------------------------------------------------

/**
 * Every command ID in 通信协议 v2.0.17, including the ones the spec marks
 * 已弃用 (deprecated) and 未使用 (unused): a device on older firmware may still
 * emit them, and a bridge that cannot name an incoming frame logs a hex number
 * at 3 a.m. instead of a cause.
 *
 * Direction annotations are the spec's own. `(DEV->APP)` means a report.
 */
export const Command = {
  // 1. 获取信息 — device identity
  GET_PRODUCT_INFO: 0x0001, // 获取产品信息
  GET_PRODUCT_MODEL: 0x0002, // 获取产品型号
  GET_VERSION: 0x0003, // 获取版本号
  GET_HARDWARE_INFO: 0x0004, // 获取硬件信息
  GET_SUPPORTED_FEATURES: 0x0005, // 获取支持功能 — capability bitmap; call this first
  GET_DEVICE_NAME: 0x0006, // 获取设备名称
  HEARTBEAT: 0x0007, // 心跳包

  // 2. 电量显示
  GET_BATTERY: 0x0101, // 获取电量
  BATTERY_REPORT: 0x0102, // 电量上报 (DEV->APP)

  // 3. 降噪 — active noise cancellation
  GET_ANC_STATE: 0x0201, // 获取降噪状态
  SET_ANC: 0x0202, // 降噪切换
  ANC_REPORT: 0x0203, // 降噪状态上报 (DEV->APP)

  // 4. 佩戴检测 — wear detection
  GET_WEAR_DETECTION: 0x0301, // 获取佩戴检测状态
  SET_WEAR_DETECTION: 0x0302, // 佩戴检测开关

  // 5. 游戏模式 — low-latency audio mode
  GET_GAME_MODE: 0x0401, // 获取游戏模式状态
  SET_GAME_MODE: 0x0402, // 游戏模式开关
  GAME_MODE_REPORT: 0x0403, // 游戏模式上报 (DEV->APP)

  // 6. EQ 音效
  GET_EQ: 0x0501, // 获取当前 EQ 音效
  SET_EQ: 0x0502, // EQ 音效设置

  // 7. 按键设置 — remappable touch gestures
  GET_KEY_FUNCTIONS: 0x0601, // 获取按键功能
  SET_KEY_FUNCTIONS: 0x0602, // 按键功能设置

  // 8. 设备查找
  FIND_DEVICE: 0x0701, // 查找设备
  FIND_DEVICE_REPORT: 0x0702, // 查找耳机状态上报 (DEV->APP)

  // 9. AI 对话 — 0x0801..0x0804 are marked 未使用 (unused) in v2.0.17
  AI_CHAT_MODE_UNUSED: 0x0801, // 对话模式 (未使用)
  AI_CHAT_DEVICE_MODE_UNUSED: 0x0802, // 设备 AI 对话模式 (未使用)
  AI_CHAT_EVENT_UNUSED: 0x0803, // 对话事件触发 (未使用)
  AI_CHAT_ASR_START_UNUSED: 0x0804, // 对话语音识别开始提示 (未使用)
  AI_CHAT_TRIGGER: 0x0805, // AI 实时语音对话事件触发 — 0x00 stop, 0x01 start
  AI_CHAT_PROMPT: 0x0806, // AI 实时语音对话提示 (DEV->APP)

  // 10. WiFi / camera / storage
  SET_WIFI_SSID_DEPRECATED: 0x0901, // 设置 WIFI SSID (已弃用) — sets the glasses' own hotspot
  SET_WIFI_PASSWORD_DEPRECATED: 0x0902, // 设置 WIFI 密码 (已弃用)
  SET_TIME: 0x0903, // 设置时间 — sync device clock to phone
  CONNECTION_STATE_REPORT: 0x0904, // 上报连接状态 (DEV->APP)
  FILE_COUNT_UPDATE: 0x0905, // 文件个数更新 (DEV->APP)
  AI_PHOTO_START: 0x0906, // 图像识别拍照开始
  AI_PHOTO_COMPLETE: 0x0907, // 图像识别拍照完成 (DEV->APP)
  RTSP_URL: 0x0908, // 实时视频 API (DEV->APP) — returns the RTSP stream URL
  DISK_INFO: 0x0909, // 磁盘容量 API
  PREVIEW_CONTROL: 0x090a, // 视频预览控制 — start/stop; success is followed by 0x0908
  WIFI_AP_CONTROL: 0x090b, // WIFI AP 控制 — open/close the glasses' access point
  AP_SSID_REPORT: 0x090c, // 上报 AP SSID (DEV->APP)
  AP_PASSWORD_REPORT: 0x090d, // 上报 AP 密码 (DEV->APP)
  WIFI_OPERATION_REPORT: 0x090e, // 上报 wifi 操作 API (DEV->APP)
  SET_BIND_CODE: 0x090f, // 设置绑定码
  GET_BIND_CODE: 0x0910, // 获取绑定码
  CLEAR_UNUPLOADED_FILES: 0x0911, // 清除未上传文件
  CLEAR_RESULT_REPORT: 0x0912, // 清除结果上报 (DEV->APP)
  RUN_STATE_REPORT: 0x0913, // 运行状态上报 (DEV->APP)
  SET_STABILIZATION: 0x0914, // 设置防抖
  GET_STABILIZATION: 0x0915, // 获取防抖设置
  GET_FILE_COUNT: 0x0916, // 获取文件个数
  AP_MAC_REPORT: 0x0917, // 上报 AP MAC 地址 (DEV->APP)
  WIFI_P2P_SUPPORT_REPORT: 0x0918, // 上报 WIFI P2P 功能支持 (DEV->APP)
  WIFI_P2P_CONTROL: 0x0919, // WIFI P2P 控制
  WIFI_P2P_NAME_REPORT: 0x091a, // 上报 WIFI P2P 名称 (DEV->APP)
  WIFI_P2P_MAC_REPORT: 0x091b, // 上报 WIFI P2P MAC 地址 (DEV->APP)
  GET_DISK_INFO: 0x091c, // 获取磁盘容量信息
  SET_VIDEO_PARAMS: 0x091d, // 设置视频录制参数
  GET_VIDEO_PARAMS: 0x091e, // 获取视频录制参数
  SET_PHOTO_PARAMS: 0x091f, // 设置拍照参数
  GET_PHOTO_PARAMS: 0x0920, // 获取拍照参数
  SET_VIDEO_RESOLUTION: 0x0921, // 设置视频分辨率
  VIDEO_RESOLUTION_REPORT: 0x0922, // 上报视频分辨率 (DEV->APP)
  STABILIZATION_SUPPORT_REPORT: 0x0923, // 上报防抖处理支持 (DEV->APP)

  // 11. 通话 / 音频
  GET_CALL_STATE: 0x0a01, // 获取通话状态
  AUDIO_CONTROL: 0x0a02, // 音频控制
  AUDIO_DATA: 0x0a03, // 音频数据 — mic stream (Opus / PCM 16 kHz mono)

  // 12. OTA
  GET_OTA_INFO: 0x0b01, // 获取 OTA 升级信息
  FIRMWARE_VERSION_REPORT: 0x0b02, // 上报固件版本号 (DEV->APP)
  OTA_START: 0x0b03, // 开始 OTA 升级
  OTA_COMPLETE: 0x0b04, // 升级完成

  // 13. 文件传输
  FILE_FETCH_START: 0x0c01, // 开始获取文件
  FILE_DATA_UPLOAD: 0x0c02, // 文件数据上传 (DEV->APP)
  FILE_UPLOAD_END: 0x0c03, // 上传文件结束
  FILE_DATA_RETRY: 0x0c04, // 重新获取文件数据
  FILE_UPLOAD_ABORT: 0x0c05, // 终止文件上传

  // 14. 设备控制
  DEVICE_CONTROL: 0x0d01, // 设备控制命令
  LOCAL_VIDEO_STATE_REPORT: 0x0d02, // 本地录像状态上报 (DEV->APP)
  LOCAL_AUDIO_STATE_REPORT: 0x0d03, // 本地录音状态上报 (DEV->APP)

  // 15. 文件管理 / 录音
  GET_FILE_LIST: 0x0e01, // 获取文件列表、磁盘信息文件
  DELETE_FILE: 0x0e02, // 删除文件
  DELETE_ALL_FILES: 0x0e03, // 删除所有文件
  LOCAL_RECORDING_CONTROL: 0x0e04, // 本地录音控制
  LOCAL_RECORDING_STATE_REPORT: 0x0e05, // 本地录音状态上报 (DEV->APP)
  SET_RECORDING_PROMPT: 0x0e06, // 本地录音提示设置
  GET_RECORDING_PROMPT: 0x0e07, // 获取本地录音提示状态
  RECORDING_FILE_COUNT_REPORT: 0x0e08, // 本地录音文件数量上报 (DEV->APP)
  SET_CALL_AUTO_RECORD: 0x0e09, // 通话自动录音设置
  GET_CALL_AUTO_RECORD: 0x0e0a, // 获取通话自动录音状态

  // 16. 语音唤醒 — wake word
  GET_WAKEWORD_LIST: 0x0f01, // 获取语音唤醒功能列表
  GET_WAKEWORD_SETTING: 0x0f02, // 获取语音唤醒设置
  SET_WAKEWORD_SETTING: 0x0f03, // 设置语音唤醒设置
} as const;

export type CommandName = keyof typeof Command;
export type CommandId = (typeof Command)[CommandName];

const NAME_BY_ID = new Map<number, CommandName>(
  (Object.entries(Command) as Array<[CommandName, number]>).map(([name, id]) => [id, name]),
);

/**
 * Human-readable name for a command ID, or `UNKNOWN_0xNNNN` when the device
 * sends something this build has never heard of. Matches `Packet.name` in
 * commands.py byte for byte, so a TypeScript log line and a Python one can be
 * diffed directly.
 */
export function commandName(id: number): string {
  return NAME_BY_ID.get(id) ?? `UNKNOWN_0x${id.toString(16).toUpperCase().padStart(4, "0")}`;
}

/** Whether this build recognises the ID at all. */
export function isKnownCommand(id: number): boolean {
  return NAME_BY_ID.has(id);
}

// ---------------------------------------------------------------------------
// 3. Wire codec — CRC-16/MODBUS, frame, packet
// ---------------------------------------------------------------------------

/**
 * CRC-16/MODBUS: reflected poly 0xA001, **init 0xFFFF**, no final XOR.
 *
 * Not CRC-16/ARC. The vendor spec publishes the Linux `lib/crc16.c` table
 * without an initial value, and the shipping `QCSDK.framework` disassembly shows
 * `mov w8, #0xffff`. `glasses/protocol/crc.py` is the authority and this is a
 * port of it; `test/commands.test.ts` checks both against the same vectors.
 */
export function crc16(data: Uint8Array): number {
  let crc = 0xffff;
  for (const byte of data) {
    let x = (crc ^ byte) & 0xff;
    for (let bit = 0; bit < 8; bit++) {
      x = x & 1 ? (x >>> 1) ^ 0xa001 : x >>> 1;
    }
    crc = (crc >>> 8) ^ x;
  }
  return crc & 0xffff;
}

export const FRAME_PREFIX = 0xa5;
export const PACKET_HEADER_LEN = 6;

export class FrameError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "FrameError";
  }
}

/** Wrap a `data` block in prefix, little-endian length and CRC. */
export function encodeFrame(data: Uint8Array): Uint8Array {
  const crc = crc16(data);
  const out = new Uint8Array(3 + data.length + 2);
  out[0] = FRAME_PREFIX;
  out[1] = data.length & 0xff;
  out[2] = (data.length >>> 8) & 0xff;
  out.set(data, 3);
  out[3 + data.length] = crc & 0xff;
  out[4 + data.length] = (crc >>> 8) & 0xff;
  return out;
}

/** Unwrap one frame from the front of `buf`, returning its data block. */
export function decodeFrame(buf: Uint8Array): { data: Uint8Array; consumed: number } {
  if (buf.length < 3) throw new FrameError(`need 3 header bytes, have ${buf.length}`);
  if (buf[0] !== FRAME_PREFIX) {
    throw new FrameError(`expected prefix 0xa5, got 0x${buf[0]!.toString(16)}`);
  }
  const n = buf[1]! | (buf[2]! << 8);
  const total = 3 + n + 2;
  if (buf.length < total) throw new FrameError(`frame needs ${total} bytes, have ${buf.length}`);

  const data = buf.slice(3, 3 + n);
  const carried = buf[3 + n]! | (buf[4 + n]! << 8);
  const computed = crc16(data);
  if (carried !== computed) {
    throw new FrameError(
      `CRC mismatch: frame carries 0x${carried.toString(16)}, computed 0x${computed.toString(16)}`,
    );
  }
  return { data, consumed: total };
}

export interface Packet {
  command: number;
  type: CommandType;
  /** 0-255, echoed by the device so a reply can be matched to its request. */
  seq: number;
  payload: Uint8Array;
}

/** Serialise a packet to the `data` block (no prefix, no CRC). */
export function encodePacket(packet: Packet): Uint8Array {
  const { command, type, seq, payload } = packet;
  const out = new Uint8Array(PACKET_HEADER_LEN + payload.length);
  out[0] = command & 0xff;
  out[1] = (command >>> 8) & 0xff;
  out[2] = type;
  out[3] = seq & 0xff;
  out[4] = payload.length & 0xff;
  out[5] = (payload.length >>> 8) & 0xff;
  out.set(payload, PACKET_HEADER_LEN);
  return out;
}

export function decodePacket(data: Uint8Array): Packet {
  if (data.length < PACKET_HEADER_LEN) {
    throw new FrameError(`packet needs ${PACKET_HEADER_LEN} header bytes, got ${data.length}`);
  }
  const declared = data[4]! | (data[5]! << 8);
  const payload = data.slice(PACKET_HEADER_LEN);
  if (payload.length !== declared) {
    throw new FrameError(
      `payload length field says ${declared}, frame carries ${payload.length}`,
    );
  }
  const rawType = data[2]!;
  if (rawType !== 1 && rawType !== 2 && rawType !== 3) {
    throw new FrameError(`unknown command type ${rawType}`);
  }
  return {
    command: data[0]! | (data[1]! << 8),
    type: rawType,
    seq: data[3]!,
    payload,
  };
}

/** Packet straight to wire bytes — the thing the write characteristic wants. */
export function encodeCommandFrame(packet: Packet): Uint8Array {
  return encodeFrame(encodePacket(packet));
}

/** Request sequence numbers: 0-255, wrapping. Mirrors `SequenceCounter`. */
export class SequenceCounter {
  #next: number;

  constructor(start = 0) {
    if (!Number.isInteger(start) || start < 0 || start > 0xff) {
      throw new RangeError(`start must be 0-255, got ${start}`);
    }
    this.#next = start;
  }

  next(): number {
    const value = this.#next;
    this.#next = (this.#next + 1) & 0xff;
    return value;
  }
}

// ---------------------------------------------------------------------------
// 4. Payload vocabularies
// ---------------------------------------------------------------------------

/**
 * `DEVICE_CONTROL` (0x0D01) payload byte. Values are `QCOperatorDeviceMode` from
 * the shipping `QGDFU_Utils.h`, not guesses — this is the one command that does
 * a dozen unrelated jobs, which is why the API wraps the useful ones by name.
 */
export const DeviceMode = {
  Unknown: 0x00,
  Photo: 0x01,
  Video: 0x02,
  VideoStop: 0x03,
  Transfer: 0x04,
  Ota: 0x05,
  AiPhoto: 0x06,
  SpeechRecognition: 0x07,
  Audio: 0x08,
  TransferStop: 0x09,
  FactoryReset: 0x0a,
  SpeechRecognitionStop: 0x0b,
  AudioStop: 0x0c,
  FindDevice: 0x0d,
  Restart: 0x0e,
  NoPowerP2p: 0x0f,
  SpeakStart: 0x10,
  SpeakStop: 0x11,
  TranslateStart: 0x12,
  TranslateStop: 0x13,
  LiveStart: 0x14,
  LiveStop: 0x15,
  Shipping: 0x16,
  ContinuousChatStart: 0x17,
  ContinuousChatStop: 0x18,
  ContinuousChatPhoto: 0x19,
  ContinuousChatAiPhoto: 0x1a,
  AgingStart: 0x1b,
  AgingStop: 0x1c,
  GyroCalibration: 0x1d,
  WifiFactoryCalibration: 0x1e,
  SpeechTranscriptionStart: 0x1f,
  SpeechTranscriptionStop: 0x20,
  AovStart: 0x21,
  AovStop: 0x22,
  AovSwitchVideo: 0x23,
  TeleprompterStart: 0x24,
  TeleprompterStop: 0x25,
  Empty: 0xff,
} as const;

export type DeviceMode = (typeof DeviceMode)[keyof typeof DeviceMode];

/**
 * `QGAISpeakMode` — what the glasses show and do while the assistant talks.
 *
 * Note `Hold`: the device wants a keep-alive while a long reply streams, which
 * is why `speakHold()` exists as a separate call rather than being folded into
 * `speakStart()`. Dropping it mid-reply is how a spoken answer gets truncated.
 */
export const SpeakMode = {
  Start: 0x01,
  Hold: 0x02,
  Stop: 0x03,
  ThinkingStart: 0x04,
  ThinkingHold: 0x05,
  ThinkingStop: 0x06,
  NoNetwork: 0xf1,
} as const;

export type SpeakMode = (typeof SpeakMode)[keyof typeof SpeakMode];

/** `QGSpeakerPlaybackStatus` — where Bluetooth audio comes out. */
export const SpeakerRoute = {
  Unknown: 0x00,
  Glasses: 0x01,
  Phone: 0x02,
} as const;

export type SpeakerRoute = (typeof SpeakerRoute)[keyof typeof SpeakerRoute];

/** `AUDIO_CONTROL` (0x0A02) — mic uplink only. The downlink is 0x0A03. */
export const AudioControl = {
  StopUplink: 0x00,
  StartUplink: 0x01,
} as const;

export type AudioControl = (typeof AudioControl)[keyof typeof AudioControl];

/** Generic on/off control byte shared by 0x090A, 0x090B, 0x0919, 0x0E04. */
export const Toggle = {
  Off: 0x00,
  On: 0x01,
} as const;

export type Toggle = (typeof Toggle)[keyof typeof Toggle];

/**
 * `0x0F01` entry type, per `ARCHITECTURE.md` §5.2b: `Index, Type, Len, Value`.
 * Type 0 is an AI wake phrase, 1 a Bluetooth control, 2 a device control.
 */
export const WakeWordKind = {
  AiPhrase: 0,
  BluetoothControl: 1,
  DeviceControl: 2,
} as const;

export type WakeWordKind = (typeof WakeWordKind)[keyof typeof WakeWordKind];

/** `QGAIImageSharpnessLevel` 0-6 — the device's own latency dial for photos. */
export const SHARPNESS_MIN = 0;
export const SHARPNESS_MAX = 6;

/** ANC state (0x0201/0x0202/0x0203). */
export const NoiseCancellation = {
  Off: 0x00,
  On: 0x01,
  Transparency: 0x02,
} as const;

export type NoiseCancellation =
  (typeof NoiseCancellation)[keyof typeof NoiseCancellation];

/** EQ preset (0x0501/0x0502). */
export const EqPreset = {
  Standard: 0x00,
  Bass: 0x01,
  Treble: 0x02,
  Vocal: 0x03,
} as const;

export type EqPreset = (typeof EqPreset)[keyof typeof EqPreset];

/** Which physical gesture a key binding covers (0x0601/0x0602). */
export const KeyGesture = {
  SingleTap: "singleTap",
  DoubleTap: "doubleTap",
  TripleTap: "tripleTap",
  LongPress: "longPress",
  SwipeForward: "swipeForward",
  SwipeBackward: "swipeBackward",
} as const;

export type KeyGesture = (typeof KeyGesture)[keyof typeof KeyGesture];

/** What a gesture is bound to do. `aiAssistant` is the one Relay cares about. */
export const KeyAction = {
  None: "none",
  PlayPause: "playPause",
  NextTrack: "nextTrack",
  PreviousTrack: "previousTrack",
  VolumeUp: "volumeUp",
  VolumeDown: "volumeDown",
  AiAssistant: "aiAssistant",
  Photo: "photo",
  Video: "video",
  Recording: "recording",
} as const;

export type KeyAction = (typeof KeyAction)[keyof typeof KeyAction];

/**
 * What the device is recording locally. The 0x0E04/0x0E05 pair covers audio;
 * 0x0D02/0x0D03 report video and audio state respectively.
 */
export const CaptureKind = {
  Audio: "audio",
  Video: "video",
} as const;

export type CaptureKind = (typeof CaptureKind)[keyof typeof CaptureKind];

/**
 * On-device recording format. Still an open question in `APPS-SCOPE.md` §3.1 and
 * it moves storage and sync duration by ~10x, so the mock models both rather
 * than picking one and pretending.
 */
export const RecordingFormat = {
  /** ~24 kbps mono — ~10.8 MB/h, a 16 h day is ~173 MB. */
  Opus: "opus",
  /** 16 kHz/16-bit mono — ~115 MB/h, a 16 h day is ~1.84 GB. */
  Pcm16: "pcm16",
} as const;

export type RecordingFormat = (typeof RecordingFormat)[keyof typeof RecordingFormat];

/**
 * The AI-interface event the device reports on `0x0803`/`0x0805`.
 *
 * Read the spec text carefully (`SYSTEM.md` §7b): every verb is the app's.
 *
 *   非 AI 界面：直接进入 AI 对话界面，开始语言识别  → the APP starts recognition
 *   语言识别中：结束语言识别                        → the APP ends recognition
 *   获取 AI 对话内容、播报中：停止获取、播报         → the APP stops fetching and speaking
 *
 * So this type is what the *device saw*, never what it decided.
 */
export const AiInterfaceEvent = {
  /** 0x01 — user asked for the assistant. The app now starts recognising. */
  Start: "start",
  /** 0x00 — user dismissed it. The app stops recognising and stops speaking. */
  Stop: "stop",
} as const;

export type AiInterfaceEvent = (typeof AiInterfaceEvent)[keyof typeof AiInterfaceEvent];

/** Who is running speech recognition. Always the app; never the glasses. */
export const RecognitionOwner = {
  App: "app",
} as const;

export type RecognitionOwner = (typeof RecognitionOwner)[keyof typeof RecognitionOwner];

// ---------------------------------------------------------------------------
// 5. Result shapes
// ---------------------------------------------------------------------------

export interface DeviceIdentity {
  /** 0x0001 获取产品信息 */
  product: string;
  /** 0x0002 获取产品型号 */
  model: string;
  /** 0x0003 获取版本号 */
  firmwareVersion: string;
  /** 0x0004 获取硬件信息 */
  hardwareVersion: string;
  /** 0x0006 获取设备名称 */
  name: string;
}

/** 0x0916 获取文件个数 / 0x0905 文件个数更新 / 0x0E08 录音文件数量. */
export interface MediaCounts {
  photos: number;
  videos: number;
  recordings: number;
  totalBytes: number;
}

/** 0x0E05 / 0x0D02 / 0x0D03. `durationS` is 0 when idle. */
export interface CaptureState {
  kind: CaptureKind;
  active: boolean;
  durationS: number;
}

/** One entry of the firmware-fixed wake word list (0x0F01). */
export interface WakeWord {
  /** Firmware index. The only thing 0x0F03 will accept. */
  index: number;
  kind: WakeWordKind;
  /** The phrase as the firmware spells it, e.g. "hey chatgpt". Read-only. */
  phrase: string;
}

/** 0x0F02 / 0x0F03 — which entries are live. */
export interface WakeWordSetting {
  index: number;
  enabled: boolean;
}

export interface KeyBinding {
  gesture: KeyGesture;
  action: KeyAction;
}

/** 0x091D / 0x091E. */
export interface VideoParams {
  /** Reserved by the vendor; 0 in every sample. */
  angle: number;
  /** Maximum clip length in seconds. */
  durationS: number;
}

/** 0x091F / 0x0920. */
export interface PhotoParams {
  widthPx: number;
  heightPx: number;
  /** 0-6, `QGAIImageSharpnessLevel`. */
  sharpness: number;
}

/** 0x0921 / 0x0922. */
export interface VideoResolution {
  widthPx: number;
  heightPx: number;
  fps: number;
}

/** 0x0A01 获取通话状态. */
export interface CallState {
  active: boolean;
  /** True while the device is recording the call (see 0x0E09). */
  recording: boolean;
}

/** 0x0B01 获取 OTA 升级信息. */
export interface OtaInfo {
  currentVersion: string;
  /** Bytes the device will accept in one firmware image. */
  maxImageBytes: number;
  batteryOk: boolean;
}

export interface OtaProgress {
  sentBytes: number;
  totalBytes: number;
  done: boolean;
}

/** 0x0912 清除结果上报 — the answer to 0x0911. */
export interface ClearResult {
  deletedFiles: number;
  freedBytes: number;
}

/** 0x0913 运行状态上报. */
export interface RunState {
  mode: DeviceMode;
  busy: boolean;
}

/** 0x090E 上报 wifi 操作. */
export interface WifiOperationReport {
  operation: "apOpen" | "apClose" | "p2pOpen" | "p2pClose";
  ok: boolean;
}

/**
 * What the phone should believe about its own radios while the glasses' AP is
 * open.
 *
 * `ARCHITECTURE.md` §2.1: the phone cannot hold the glasses' access point and
 * its own uplink at the same time. That is why the nightly sync is two phases
 * and not a background trickle, and it is the single most surprising thing for
 * anyone writing sync code, so the transport says it out loud.
 */
export interface WifiApState extends WifiAccessPoint {
  open: boolean;
  macAddress: string;
  /** True whenever `open` is true: joining this AP costs the phone its uplink. */
  phoneUplinkSuspended: boolean;
}

/** 0x0C01–0x0C05. One handle per in-flight transfer. */
export interface FileTransfer {
  id: number;
  name: string;
  totalBytes: number;
  receivedBytes: number;
  /** Which radio is carrying it — decides whether this takes seconds or hours. */
  via: "ble" | "wifiAp";
}

export interface FileTransferProgress extends FileTransfer {
  /** Chunks the device had to resend (0x0C04). */
  retries: number;
}

/** One command as it went out or came in — the app's developer console. */
export interface CommandRecord {
  at: number;
  id: number;
  name: string;
  type: CommandType;
  seq: number;
  /** Payload bytes, hex, so a log line can be pasted straight into a decoder. */
  payloadHex: string;
}

// ---------------------------------------------------------------------------
// 6. Events beyond the base transport
// ---------------------------------------------------------------------------

/**
 * Events the command surface adds on top of `GlassesEvents`.
 *
 * Kept separate rather than merged into `types.ts` so the base transport
 * contract stays small: an adapter that only does the capture loop implements
 * `GlassesTransport`, and one that drives the whole command set implements
 * `GlassesCommands`.
 */
export interface GlassesCommandEvents {
  /** Every frame the bridge sent or received. Drives the debug screen. */
  command: CommandRecord;

  /** 0x0805 (and 0x0803 on older firmware). The device saw something. */
  aiInterfaceEvent: AiInterfaceEvent;
  /**
   * The app's own recogniser starting or stopping. Emitted by us, never by the
   * device — the glasses have no ASR.
   */
  recognitionChanged: { active: boolean; owner: RecognitionOwner };
  /** 0x0806 — the vendor's cloud assistant talking through their own channel. */
  vendorAiPrompt: string;
  /** Echo of the last 0x0D01 speak-mode write. */
  speakModeChanged: SpeakMode;

  /** 0x0905 / 0x0916 / 0x0E08. */
  mediaCounts: MediaCounts;
  /** 0x0D02 本地录像状态 and 0x0D03 本地录音状态. */
  captureState: CaptureState;
  /** 0x0912. */
  clearResult: ClearResult;
  /** 0x0913. */
  runState: RunState;

  /** 0x090C / 0x090D / 0x0917 collapsed into one usable state. */
  wifiAccessPointChanged: WifiApState;
  /** 0x090E. */
  wifiOperation: WifiOperationReport;
  /** 0x0918 / 0x091A / 0x091B. */
  wifiP2pChanged: { supported: boolean; name: string; macAddress: string; open: boolean };

  /** 0x0C02 arriving, 0x0C04 retried, 0x0C03 finished. */
  fileTransferProgress: FileTransferProgress;

  /** 0x0203. */
  noiseCancellationChanged: NoiseCancellation;
  /** 0x0403. */
  gameModeChanged: boolean;
  /** 0x0702. */
  findDeviceChanged: boolean;
  /** 0x0922. */
  videoResolutionChanged: VideoResolution;
  /** 0x0923. */
  stabilisationSupportChanged: boolean;
  /** 0x0F03 accepted. */
  wakeWordSettingsChanged: WakeWordSetting[];
  /** 0x0B02. */
  firmwareVersion: string;
  /** 0x0B03 progress, 0x0B04 completion. */
  otaProgress: OtaProgress;
}

/** Every event name a fully-featured adapter can emit. */
export type AllGlassesEvents = GlassesEvents & GlassesCommandEvents;
export type AllGlassesEventName = keyof AllGlassesEvents;

// ---------------------------------------------------------------------------
// 7. The command surface
// ---------------------------------------------------------------------------

/**
 * Every command the app can issue, typed.
 *
 * This is the "by hand" half of `ORCHESTRATOR.md` §5: the UI binds a control to
 * each of these, so nothing in the product is reachable only by speaking. It
 * composes with `GlassesTransport` rather than replacing it — the transport is
 * the capture loop, this is the rest of the device.
 *
 * Where the byte layout of a payload is not attested by the spec or the shipping
 * SDK headers, the method takes a typed object and the *adapter* owns the
 * encoding. Inventing a layout here would put a guess somewhere it looks like a
 * fact.
 */
export interface GlassesCommandSet {
  // --- identity -------------------------------------------------------------

  /** 0x0001-0x0004 + 0x0006, gathered in one round trip where the SDK allows. */
  getIdentity(): Promise<DeviceIdentity>;
  /** 0x0007 心跳包. The vendor also wants `sendVoiceHeartbeat` during AI mode. */
  heartbeat(): Promise<void>;

  // --- settings -------------------------------------------------------------

  /** 0x0201 / 0x0202. */
  getNoiseCancellation(): Promise<NoiseCancellation>;
  setNoiseCancellation(mode: NoiseCancellation): Promise<void>;
  /** 0x0301 / 0x0302. Capture gating depends on this being on. */
  getWearDetection(): Promise<boolean>;
  setWearDetection(enabled: boolean): Promise<void>;
  /** 0x0401 / 0x0402. */
  getGameMode(): Promise<boolean>;
  setGameMode(enabled: boolean): Promise<void>;
  /** 0x0501 / 0x0502. */
  getEqualiser(): Promise<EqPreset>;
  setEqualiser(preset: EqPreset): Promise<void>;
  /** 0x0601 / 0x0602. Bind a gesture to `aiAssistant` and tap-to-talk works. */
  getKeyBindings(): Promise<KeyBinding[]>;
  setKeyBindings(bindings: KeyBinding[]): Promise<void>;
  /** 0x0701. Makes the glasses chirp so they can be found down the sofa. */
  findDevice(on: boolean): Promise<void>;
  /** 0x090F / 0x0910 绑定码 — the pairing secret, never logged. */
  setBindCode(code: string): Promise<void>;
  getBindCode(): Promise<string>;
  /** 0x0914 / 0x0915 / 0x0923. */
  getStabilisation(): Promise<{ enabled: boolean; supported: boolean }>;
  setStabilisation(enabled: boolean): Promise<void>;
  /** 0x091D / 0x091E. */
  getVideoParams(): Promise<VideoParams>;
  setVideoParams(params: VideoParams): Promise<void>;
  /** 0x091F / 0x0920. */
  getPhotoParams(): Promise<PhotoParams>;
  setPhotoParams(params: PhotoParams): Promise<void>;
  /** 0x0921 / 0x0922. */
  setVideoResolution(resolution: VideoResolution): Promise<void>;
  /** 0x0E06 / 0x0E07 — the audible "recording started" prompt. Consent surface. */
  getRecordingPrompt(): Promise<boolean>;
  setRecordingPrompt(enabled: boolean): Promise<void>;
  /** 0x0E09 / 0x0E0A. Two-party-consent jurisdictions: default this off. */
  getCallAutoRecord(): Promise<boolean>;
  setCallAutoRecord(enabled: boolean): Promise<void>;

  // --- device control (0x0D01) ---------------------------------------------

  /**
   * Raw 0x0D01. Every convenience below is a wrapper; this exists because the
   * mode list grows with firmware and the app should not need a release to
   * reach a new one.
   */
  setDeviceMode(mode: DeviceMode): Promise<void>;
  /** 0x0D01 restart / factory reset / shipping mode. */
  restartDevice(): Promise<void>;
  factoryReset(): Promise<void>;

  // --- capture --------------------------------------------------------------

  /** 0x0D01 Video / VideoStop, reported on 0x0D02. */
  startVideoRecording(): Promise<void>;
  stopVideoRecording(): Promise<void>;
  /** 0x0E05 本地录音状态 — poll, in addition to the report. */
  getLocalRecordingState(): Promise<CaptureState>;
  /** 0x0916 获取文件个数. */
  getMediaCounts(): Promise<MediaCounts>;

  // --- media ----------------------------------------------------------------

  /** 0x0E03 删除所有文件. Refuses while anything is still un-uploaded. */
  deleteAllFiles(): Promise<ClearResult>;
  /**
   * 0x0911 清除未上传文件.
   *
   * **This deletes the files that have not been pulled yet** — the firmware
   * tracks the distinction, which is exactly why `APPS-SCOPE.md` §3.2 says never
   * to reach for it casually. It is here because the day the device is wedged
   * full it is the only way out, and it must be an explicit act.
   */
  clearUnuploadedFiles(): Promise<ClearResult>;
  /** 0x0C04 — ask for a chunk again after a CRC failure or a dropout. */
  retryFileChunk(transferId: number, chunkIndex: number): Promise<void>;
  /** 0x0C05 终止文件上传. */
  abortFileTransfer(transferId: number): Promise<void>;
  /** In-flight transfers, so the UI can show and cancel them. */
  activeTransfers(): FileTransfer[];

  // --- network --------------------------------------------------------------

  /** The AP as last reported by 0x090C / 0x090D / 0x0917. */
  getWifiAccessPointState(): Promise<WifiApState>;
  /** 0x0919 WIFI P2P 控制. */
  setWifiP2p(open: boolean): Promise<void>;
  /**
   * 0x0901 / 0x0902, both 已弃用.
   *
   * These set the glasses' **own hotspot** credentials, not a network to join —
   * the glasses have no station mode at all (`ARCHITECTURE.md` §2). Kept in the
   * surface so an adapter can name them when old firmware sends one, and made to
   * fail the way the device fails rather than quietly succeeding.
   */
  setAccessPointCredentials(ssid: string, password: string): Promise<void>;

  // --- audio ----------------------------------------------------------------

  /**
   * 0x0A02 音频控制 — open the mic uplink. `audioChunk` events follow at roughly
   * 3 KB/s of Opus until stopped (`SYSTEM.md` §3.1). Expensive on both
   * batteries: open on intent, close immediately after.
   */
  startMicUplink(): Promise<void>;
  stopMicUplink(): Promise<void>;
  /**
   * 0x0A03 音频数据, downstream. Same channel as the mic and the same ~3 KB/s,
   * so this resolves when the bytes have actually gone — a 10-second reply is
   * ~30 KB and takes ~10 seconds to push. Callers that need the reply to start
   * sooner should stream it in pieces rather than buffering the whole thing.
   */
  sendAudio(opus: Uint8Array): Promise<void>;
  /** 0x0D01 SpeakStart / SpeakStop plus the vendor's `QGAISpeakMode` hold. */
  speakStart(): Promise<void>;
  speakHold(): Promise<void>;
  speakStop(): Promise<void>;
  /** `QGAISpeakMode` thinking states — the device's own "working on it". */
  setSpeakMode(mode: SpeakMode): Promise<void>;
  /** Vendor operation 0x52; no v2.0.17 command ID. Glasses or phone. */
  getSpeakerRoute(): Promise<SpeakerRoute>;
  setSpeakerRoute(route: SpeakerRoute): Promise<void>;
  /** 0x0A01 获取通话状态. */
  getCallState(): Promise<CallState>;

  // --- AI interface (app-owned verbs) --------------------------------------

  /**
   * Start the app's recogniser after an `aiInterfaceEvent`.
   *
   * Opens the mic uplink and marks recognition active. It does **not** ask the
   * glasses to transcribe, because no such command exists — the device is a
   * microphone and a button (`SYSTEM.md` §7b).
   */
  startRecognition(): Promise<void>;
  stopRecognition(): Promise<void>;
  isRecognising(): boolean;

  // --- wake word ------------------------------------------------------------

  /**
   * 0x0F01 — the firmware-fixed list. There is no command that accepts a new
   * phrase: the spotter is a trained DSP model. A per-user phrase needs either a
   * supplier firmware build or a phone-side spotter with the mic held open
   * (`ARCHITECTURE.md` §5.2b), and neither is free.
   */
  getWakeWords(): Promise<WakeWord[]>;
  /** 0x0F02. */
  getWakeWordSettings(): Promise<WakeWordSetting[]>;
  /** 0x0F03. `index` must be one the device listed; anything else is refused. */
  setWakeWordEnabled(index: number, enabled: boolean): Promise<WakeWordSetting[]>;

  // --- OTA ------------------------------------------------------------------

  /** 0x0B01. */
  getOtaInfo(): Promise<OtaInfo>;
  /** 0x0B03 / 0x0B04. Reports on `otaProgress`. */
  startOta(image: Uint8Array): Promise<void>;
}

/** The full contract: capture loop plus every command. */
export interface GlassesCommands extends GlassesTransport, GlassesCommandSet {
  on<K extends keyof AllGlassesEvents>(
    event: K,
    handler: (payload: AllGlassesEvents[K]) => void,
  ): () => void;
}

// ---------------------------------------------------------------------------
// 8. Catalog — the coverage proof
// ---------------------------------------------------------------------------

/**
 * What a command is for, from the app's point of view.
 *
 *   `command`    APP -> DEV, driven by at least one method
 *   `report`     DEV -> APP, surfaced as at least one event
 *   `both`       has both directions worth naming
 *   `unused`     spec marks it 未使用; refused rather than sent
 *   `deprecated` spec marks it 已弃用; refused rather than sent
 */
export const CommandRole = {
  Command: "command",
  Report: "report",
  Both: "both",
  Unused: "unused",
  Deprecated: "deprecated",
} as const;

export type CommandRole = (typeof CommandRole)[keyof typeof CommandRole];

export interface CommandDescriptor {
  readonly name: CommandName;
  readonly id: number;
  readonly role: CommandRole;
  /** Methods on `GlassesCommands` that issue it. */
  readonly methods: readonly string[];
  /** Events it produces. */
  readonly events: readonly (keyof AllGlassesEvents)[];
  /** Destroys user data. The UI must confirm before calling. */
  readonly destructive?: boolean;
  readonly note?: string;
}

/**
 * Every command mapped to the surface that drives it.
 *
 * `test/commands.test.ts` asserts three things about this table, which together
 * are the machine-checkable version of "every glasses command, by hand":
 *
 *   1. It covers exactly the IDs in `glasses/protocol/commands.py` — no more, no
 *      fewer, same values.
 *   2. Every method it names exists and is callable on `MockTransport`.
 *   3. Every event it names is one the mock can actually emit.
 */
export const COMMAND_CATALOG: readonly CommandDescriptor[] = [
  // identity
  { name: "GET_PRODUCT_INFO", id: Command.GET_PRODUCT_INFO, role: CommandRole.Command, methods: ["getIdentity"], events: [] },
  { name: "GET_PRODUCT_MODEL", id: Command.GET_PRODUCT_MODEL, role: CommandRole.Command, methods: ["getIdentity"], events: [] },
  { name: "GET_VERSION", id: Command.GET_VERSION, role: CommandRole.Command, methods: ["getIdentity"], events: [] },
  { name: "GET_HARDWARE_INFO", id: Command.GET_HARDWARE_INFO, role: CommandRole.Command, methods: ["getIdentity"], events: [] },
  { name: "GET_SUPPORTED_FEATURES", id: Command.GET_SUPPORTED_FEATURES, role: CommandRole.Command, methods: ["getFeatures"], events: [], note: "call first; gates everything else" },
  { name: "GET_DEVICE_NAME", id: Command.GET_DEVICE_NAME, role: CommandRole.Command, methods: ["getIdentity"], events: [] },
  { name: "HEARTBEAT", id: Command.HEARTBEAT, role: CommandRole.Command, methods: ["heartbeat"], events: [] },

  // battery
  { name: "GET_BATTERY", id: Command.GET_BATTERY, role: CommandRole.Command, methods: ["getBattery"], events: ["battery"] },
  { name: "BATTERY_REPORT", id: Command.BATTERY_REPORT, role: CommandRole.Report, methods: [], events: ["battery"], note: "~1/min unprompted" },

  // ANC
  { name: "GET_ANC_STATE", id: Command.GET_ANC_STATE, role: CommandRole.Command, methods: ["getNoiseCancellation"], events: [] },
  { name: "SET_ANC", id: Command.SET_ANC, role: CommandRole.Command, methods: ["setNoiseCancellation"], events: ["noiseCancellationChanged"] },
  { name: "ANC_REPORT", id: Command.ANC_REPORT, role: CommandRole.Report, methods: [], events: ["noiseCancellationChanged"] },

  // wear
  { name: "GET_WEAR_DETECTION", id: Command.GET_WEAR_DETECTION, role: CommandRole.Command, methods: ["getWearDetection"], events: [] },
  { name: "SET_WEAR_DETECTION", id: Command.SET_WEAR_DETECTION, role: CommandRole.Command, methods: ["setWearDetection"], events: ["wear"] },

  // game mode
  { name: "GET_GAME_MODE", id: Command.GET_GAME_MODE, role: CommandRole.Command, methods: ["getGameMode"], events: [] },
  { name: "SET_GAME_MODE", id: Command.SET_GAME_MODE, role: CommandRole.Command, methods: ["setGameMode"], events: ["gameModeChanged"] },
  { name: "GAME_MODE_REPORT", id: Command.GAME_MODE_REPORT, role: CommandRole.Report, methods: [], events: ["gameModeChanged"] },

  // EQ
  { name: "GET_EQ", id: Command.GET_EQ, role: CommandRole.Command, methods: ["getEqualiser"], events: [] },
  { name: "SET_EQ", id: Command.SET_EQ, role: CommandRole.Command, methods: ["setEqualiser"], events: [] },

  // keys
  { name: "GET_KEY_FUNCTIONS", id: Command.GET_KEY_FUNCTIONS, role: CommandRole.Command, methods: ["getKeyBindings"], events: [] },
  { name: "SET_KEY_FUNCTIONS", id: Command.SET_KEY_FUNCTIONS, role: CommandRole.Command, methods: ["setKeyBindings"], events: [], note: "bind a gesture to aiAssistant for tap-to-talk" },

  // find
  { name: "FIND_DEVICE", id: Command.FIND_DEVICE, role: CommandRole.Command, methods: ["findDevice"], events: [] },
  { name: "FIND_DEVICE_REPORT", id: Command.FIND_DEVICE_REPORT, role: CommandRole.Report, methods: [], events: ["findDeviceChanged"] },

  // AI interface
  { name: "AI_CHAT_MODE_UNUSED", id: Command.AI_CHAT_MODE_UNUSED, role: CommandRole.Unused, methods: [], events: [], note: "未使用 in v2.0.17" },
  { name: "AI_CHAT_DEVICE_MODE_UNUSED", id: Command.AI_CHAT_DEVICE_MODE_UNUSED, role: CommandRole.Unused, methods: [], events: [], note: "未使用 in v2.0.17" },
  { name: "AI_CHAT_EVENT_UNUSED", id: Command.AI_CHAT_EVENT_UNUSED, role: CommandRole.Report, methods: [], events: ["aiInterfaceEvent"], note: "未使用 in v2.0.17 but older firmware still emits it; same meaning as 0x0805" },
  { name: "AI_CHAT_ASR_START_UNUSED", id: Command.AI_CHAT_ASR_START_UNUSED, role: CommandRole.Unused, methods: [], events: [], note: "未使用; a prompt that the APP should start recognising, not a device transcript" },
  { name: "AI_CHAT_TRIGGER", id: Command.AI_CHAT_TRIGGER, role: CommandRole.Report, methods: ["startRecognition", "stopRecognition"], events: ["aiInterfaceEvent", "recognitionChanged"], note: "device reports; the APP recognises" },
  { name: "AI_CHAT_PROMPT", id: Command.AI_CHAT_PROMPT, role: CommandRole.Report, methods: [], events: ["vendorAiPrompt"], note: "the vendor's own cloud assistant, not ours" },

  // wifi / camera / storage
  { name: "SET_WIFI_SSID_DEPRECATED", id: Command.SET_WIFI_SSID_DEPRECATED, role: CommandRole.Deprecated, methods: ["setAccessPointCredentials"], events: [], note: "已弃用; sets the glasses' own hotspot — there is no station mode" },
  { name: "SET_WIFI_PASSWORD_DEPRECATED", id: Command.SET_WIFI_PASSWORD_DEPRECATED, role: CommandRole.Deprecated, methods: ["setAccessPointCredentials"], events: [] },
  { name: "SET_TIME", id: Command.SET_TIME, role: CommandRole.Command, methods: ["setTime"], events: [], note: "align before any capture or timestamps are useless" },
  { name: "CONNECTION_STATE_REPORT", id: Command.CONNECTION_STATE_REPORT, role: CommandRole.Report, methods: [], events: ["connectionChanged"] },
  { name: "FILE_COUNT_UPDATE", id: Command.FILE_COUNT_UPDATE, role: CommandRole.Report, methods: [], events: ["mediaCounts"] },
  { name: "AI_PHOTO_START", id: Command.AI_PHOTO_START, role: CommandRole.Command, methods: ["takePhoto", "capturePhoto"], events: ["photoProgress"] },
  { name: "AI_PHOTO_COMPLETE", id: Command.AI_PHOTO_COMPLETE, role: CommandRole.Report, methods: [], events: ["photo"] },
  { name: "RTSP_URL", id: Command.RTSP_URL, role: CommandRole.Report, methods: [], events: ["rtspUrl"] },
  { name: "DISK_INFO", id: Command.DISK_INFO, role: CommandRole.Command, methods: ["getDiskInfo"], events: ["diskInfo"] },
  { name: "PREVIEW_CONTROL", id: Command.PREVIEW_CONTROL, role: CommandRole.Command, methods: ["startPreview", "stopPreview"], events: ["rtspUrl"] },
  { name: "WIFI_AP_CONTROL", id: Command.WIFI_AP_CONTROL, role: CommandRole.Command, methods: ["openWifiAccessPoint", "closeWifiAccessPoint"], events: ["wifiAccessPointChanged", "wifiOperation"] },
  { name: "AP_SSID_REPORT", id: Command.AP_SSID_REPORT, role: CommandRole.Report, methods: ["getWifiAccessPointState"], events: ["wifiAccessPointChanged"] },
  { name: "AP_PASSWORD_REPORT", id: Command.AP_PASSWORD_REPORT, role: CommandRole.Report, methods: ["getWifiAccessPointState"], events: ["wifiAccessPointChanged"] },
  { name: "WIFI_OPERATION_REPORT", id: Command.WIFI_OPERATION_REPORT, role: CommandRole.Report, methods: [], events: ["wifiOperation"] },
  { name: "SET_BIND_CODE", id: Command.SET_BIND_CODE, role: CommandRole.Command, methods: ["setBindCode"], events: [], note: "a secret; never logged" },
  { name: "GET_BIND_CODE", id: Command.GET_BIND_CODE, role: CommandRole.Command, methods: ["getBindCode"], events: [] },
  { name: "CLEAR_UNUPLOADED_FILES", id: Command.CLEAR_UNUPLOADED_FILES, role: CommandRole.Command, methods: ["clearUnuploadedFiles"], events: ["clearResult", "diskInfo"], destructive: true, note: "deletes exactly the audio that has not been synced yet" },
  { name: "CLEAR_RESULT_REPORT", id: Command.CLEAR_RESULT_REPORT, role: CommandRole.Report, methods: [], events: ["clearResult"] },
  { name: "RUN_STATE_REPORT", id: Command.RUN_STATE_REPORT, role: CommandRole.Report, methods: [], events: ["runState"] },
  { name: "SET_STABILIZATION", id: Command.SET_STABILIZATION, role: CommandRole.Command, methods: ["setStabilisation"], events: [] },
  { name: "GET_STABILIZATION", id: Command.GET_STABILIZATION, role: CommandRole.Command, methods: ["getStabilisation"], events: [] },
  { name: "GET_FILE_COUNT", id: Command.GET_FILE_COUNT, role: CommandRole.Command, methods: ["getMediaCounts"], events: ["mediaCounts"] },
  { name: "AP_MAC_REPORT", id: Command.AP_MAC_REPORT, role: CommandRole.Report, methods: ["getWifiAccessPointState"], events: ["wifiAccessPointChanged"] },
  { name: "WIFI_P2P_SUPPORT_REPORT", id: Command.WIFI_P2P_SUPPORT_REPORT, role: CommandRole.Report, methods: [], events: ["wifiP2pChanged"] },
  { name: "WIFI_P2P_CONTROL", id: Command.WIFI_P2P_CONTROL, role: CommandRole.Command, methods: ["setWifiP2p"], events: ["wifiP2pChanged"] },
  { name: "WIFI_P2P_NAME_REPORT", id: Command.WIFI_P2P_NAME_REPORT, role: CommandRole.Report, methods: [], events: ["wifiP2pChanged"] },
  { name: "WIFI_P2P_MAC_REPORT", id: Command.WIFI_P2P_MAC_REPORT, role: CommandRole.Report, methods: [], events: ["wifiP2pChanged"] },
  { name: "GET_DISK_INFO", id: Command.GET_DISK_INFO, role: CommandRole.Command, methods: ["getDiskInfo"], events: ["diskInfo"], note: "0x0909 and 0x091C are the same question; adapters use whichever the firmware answers" },
  { name: "SET_VIDEO_PARAMS", id: Command.SET_VIDEO_PARAMS, role: CommandRole.Command, methods: ["setVideoParams"], events: [] },
  { name: "GET_VIDEO_PARAMS", id: Command.GET_VIDEO_PARAMS, role: CommandRole.Command, methods: ["getVideoParams"], events: [] },
  { name: "SET_PHOTO_PARAMS", id: Command.SET_PHOTO_PARAMS, role: CommandRole.Command, methods: ["setPhotoParams"], events: [] },
  { name: "GET_PHOTO_PARAMS", id: Command.GET_PHOTO_PARAMS, role: CommandRole.Command, methods: ["getPhotoParams"], events: [] },
  { name: "SET_VIDEO_RESOLUTION", id: Command.SET_VIDEO_RESOLUTION, role: CommandRole.Command, methods: ["setVideoResolution"], events: ["videoResolutionChanged"] },
  { name: "VIDEO_RESOLUTION_REPORT", id: Command.VIDEO_RESOLUTION_REPORT, role: CommandRole.Report, methods: [], events: ["videoResolutionChanged"] },
  { name: "STABILIZATION_SUPPORT_REPORT", id: Command.STABILIZATION_SUPPORT_REPORT, role: CommandRole.Report, methods: [], events: ["stabilisationSupportChanged"] },

  // audio / call
  { name: "GET_CALL_STATE", id: Command.GET_CALL_STATE, role: CommandRole.Command, methods: ["getCallState"], events: [] },
  { name: "AUDIO_CONTROL", id: Command.AUDIO_CONTROL, role: CommandRole.Command, methods: ["startMicUplink", "stopMicUplink", "startVoiceSession", "stopVoiceSession"], events: ["voiceSessionChanged"], note: "uplink only" },
  { name: "AUDIO_DATA", id: Command.AUDIO_DATA, role: CommandRole.Both, methods: ["sendAudio"], events: ["audioChunk"], note: "APP<->DEVICE: mic up and speech down, ~3 KB/s each way" },

  // OTA
  { name: "GET_OTA_INFO", id: Command.GET_OTA_INFO, role: CommandRole.Command, methods: ["getOtaInfo"], events: [] },
  { name: "FIRMWARE_VERSION_REPORT", id: Command.FIRMWARE_VERSION_REPORT, role: CommandRole.Report, methods: [], events: ["firmwareVersion"] },
  { name: "OTA_START", id: Command.OTA_START, role: CommandRole.Command, methods: ["startOta"], events: ["otaProgress"] },
  { name: "OTA_COMPLETE", id: Command.OTA_COMPLETE, role: CommandRole.Both, methods: ["startOta"], events: ["otaProgress"] },

  // file transfer
  { name: "FILE_FETCH_START", id: Command.FILE_FETCH_START, role: CommandRole.Command, methods: ["fetchFile", "fetchThumbnail"], events: ["fileTransferProgress"] },
  { name: "FILE_DATA_UPLOAD", id: Command.FILE_DATA_UPLOAD, role: CommandRole.Report, methods: [], events: ["fileTransferProgress"] },
  { name: "FILE_UPLOAD_END", id: Command.FILE_UPLOAD_END, role: CommandRole.Report, methods: [], events: ["fileTransferProgress"] },
  { name: "FILE_DATA_RETRY", id: Command.FILE_DATA_RETRY, role: CommandRole.Command, methods: ["retryFileChunk"], events: ["fileTransferProgress"] },
  { name: "FILE_UPLOAD_ABORT", id: Command.FILE_UPLOAD_ABORT, role: CommandRole.Command, methods: ["abortFileTransfer"], events: ["fileTransferProgress"] },

  // device control
  { name: "DEVICE_CONTROL", id: Command.DEVICE_CONTROL, role: CommandRole.Command, methods: ["setDeviceMode", "restartDevice", "factoryReset", "startVideoRecording", "stopVideoRecording", "speakStart", "speakHold", "speakStop", "setSpeakMode"], events: ["speakModeChanged", "runState"], note: "one command, forty jobs — QCOperatorDeviceMode" },
  { name: "LOCAL_VIDEO_STATE_REPORT", id: Command.LOCAL_VIDEO_STATE_REPORT, role: CommandRole.Report, methods: [], events: ["captureState"] },
  { name: "LOCAL_AUDIO_STATE_REPORT", id: Command.LOCAL_AUDIO_STATE_REPORT, role: CommandRole.Report, methods: [], events: ["captureState"] },

  // files / recording
  { name: "GET_FILE_LIST", id: Command.GET_FILE_LIST, role: CommandRole.Command, methods: ["listFiles"], events: [] },
  { name: "DELETE_FILE", id: Command.DELETE_FILE, role: CommandRole.Command, methods: ["deleteFile"], events: ["diskInfo"], destructive: true },
  { name: "DELETE_ALL_FILES", id: Command.DELETE_ALL_FILES, role: CommandRole.Command, methods: ["deleteAllFiles"], events: ["clearResult", "diskInfo"], destructive: true },
  { name: "LOCAL_RECORDING_CONTROL", id: Command.LOCAL_RECORDING_CONTROL, role: CommandRole.Command, methods: ["startLocalRecording", "stopLocalRecording"], events: ["recordingState", "captureState"], note: "capture Path B — the all-day pipeline" },
  { name: "LOCAL_RECORDING_STATE_REPORT", id: Command.LOCAL_RECORDING_STATE_REPORT, role: CommandRole.Report, methods: ["getLocalRecordingState"], events: ["recordingState", "captureState"] },
  { name: "SET_RECORDING_PROMPT", id: Command.SET_RECORDING_PROMPT, role: CommandRole.Command, methods: ["setRecordingPrompt"], events: [], note: "audible consent cue — ARCHITECTURE.md §6" },
  { name: "GET_RECORDING_PROMPT", id: Command.GET_RECORDING_PROMPT, role: CommandRole.Command, methods: ["getRecordingPrompt"], events: [] },
  { name: "RECORDING_FILE_COUNT_REPORT", id: Command.RECORDING_FILE_COUNT_REPORT, role: CommandRole.Report, methods: [], events: ["mediaCounts"] },
  { name: "SET_CALL_AUTO_RECORD", id: Command.SET_CALL_AUTO_RECORD, role: CommandRole.Command, methods: ["setCallAutoRecord"], events: [], note: "off by default in two-party-consent jurisdictions" },
  { name: "GET_CALL_AUTO_RECORD", id: Command.GET_CALL_AUTO_RECORD, role: CommandRole.Command, methods: ["getCallAutoRecord"], events: [] },

  // wake word
  { name: "GET_WAKEWORD_LIST", id: Command.GET_WAKEWORD_LIST, role: CommandRole.Command, methods: ["getWakeWords"], events: [], note: "firmware-fixed; selection only, never a phrase" },
  { name: "GET_WAKEWORD_SETTING", id: Command.GET_WAKEWORD_SETTING, role: CommandRole.Command, methods: ["getWakeWordSettings"], events: [] },
  { name: "SET_WAKEWORD_SETTING", id: Command.SET_WAKEWORD_SETTING, role: CommandRole.Command, methods: ["setWakeWordEnabled"], events: ["wakeWordSettingsChanged"] },
];

const CATALOG_BY_ID = new Map(COMMAND_CATALOG.map((entry) => [entry.id, entry]));

export function describeCommand(id: number): CommandDescriptor | undefined {
  return CATALOG_BY_ID.get(id);
}

/** Commands that destroy user data. The UI must confirm before any of these. */
export function destructiveCommands(): readonly CommandDescriptor[] {
  return COMMAND_CATALOG.filter((entry) => entry.destructive === true);
}

// ---------------------------------------------------------------------------
// 9. Wake word list codec
// ---------------------------------------------------------------------------

/**
 * Decode a 0x0F01 payload: repeating `Index(1) Type(1) Len(1) Value(Len)`,
 * per `ARCHITECTURE.md` §5.2b. Values are UTF-8 phrases as the firmware spells
 * them — the spec's worked example is `"hey chatgpt"`.
 */
export function decodeWakeWordList(payload: Uint8Array): WakeWord[] {
  const decoder = new TextDecoder();
  const out: WakeWord[] = [];
  let offset = 0;
  while (offset + 3 <= payload.length) {
    const index = payload[offset]!;
    const kind = payload[offset + 1]!;
    const len = payload[offset + 2]!;
    if (offset + 3 + len > payload.length) {
      throw new FrameError(
        `wake word entry ${index} declares ${len} bytes but only ` +
          `${payload.length - offset - 3} remain`,
      );
    }
    out.push({
      index,
      kind: kind as WakeWordKind,
      phrase: decoder.decode(payload.subarray(offset + 3, offset + 3 + len)),
    });
    offset += 3 + len;
  }
  if (offset !== payload.length) {
    throw new FrameError(`wake word list has ${payload.length - offset} trailing bytes`);
  }
  return out;
}

/** Encode the same structure — used to build fixtures and to test the decoder. */
export function encodeWakeWordList(words: readonly WakeWord[]): Uint8Array {
  const parts = words.map((word) => {
    const value = new TextEncoder().encode(word.phrase);
    if (value.length > 0xff) throw new RangeError(`phrase too long: ${word.phrase}`);
    const entry = new Uint8Array(3 + value.length);
    entry[0] = word.index & 0xff;
    entry[1] = word.kind & 0xff;
    entry[2] = value.length;
    entry.set(value, 3);
    return entry;
  });
  const total = parts.reduce((n, part) => n + part.length, 0);
  const out = new Uint8Array(total);
  let offset = 0;
  for (const part of parts) {
    out.set(part, offset);
    offset += part.length;
  }
  return out;
}

/** 0x0F02 / 0x0F03 payload: repeating `Index(1) Enabled(1)`. */
export function encodeWakeWordSettings(settings: readonly WakeWordSetting[]): Uint8Array {
  const out = new Uint8Array(settings.length * 2);
  settings.forEach((setting, i) => {
    out[i * 2] = setting.index & 0xff;
    out[i * 2 + 1] = setting.enabled ? 1 : 0;
  });
  return out;
}

export function decodeWakeWordSettings(payload: Uint8Array): WakeWordSetting[] {
  if (payload.length % 2 !== 0) {
    throw new FrameError(`wake word settings must be index/enabled pairs, got ${payload.length}`);
  }
  const out: WakeWordSetting[] = [];
  for (let i = 0; i < payload.length; i += 2) {
    out.push({ index: payload[i]!, enabled: payload[i + 1] !== 0 });
  }
  return out;
}

// ---------------------------------------------------------------------------
// 10. Rate constants — measured figures, in one place
// ---------------------------------------------------------------------------

/**
 * The numbers from `SYSTEM.md` §3.1 and `APPS-SCOPE.md` §3.1, as constants.
 *
 * They are here rather than inline in the mock because the app needs them too:
 * a sync screen that cannot say "this will take about four minutes" before it
 * starts is a screen that looks broken.
 */
export const RATES = {
  /** Practical BLE application throughput, both directions. */
  bleBytesPerSecond: 3_000,
  /** Live Opus mic uplink. Same order as the link itself, which is the point. */
  micBytesPerSecond: 3_000,
  /** Over the glasses' own access point. */
  wifiApBytesPerSecond: 2_000_000,
  /** On-device recording, Opus ~24 kbps mono. */
  recordingOpusBytesPerSecond: 3_000,
  /** On-device recording, PCM 16 kHz 16-bit mono. */
  recordingPcm16BytesPerSecond: 32_000,
  /** Unprompted battery reports, `SYSTEM.md` §3.1. */
  batteryReportIntervalMs: 60_000,
  /** One Opus frame per 20 ms; the transport batches ~10 into a chunk. */
  audioChunkMs: 200,
} as const;

/** Bytes a recording of this length will occupy on the device. */
export function recordingBytes(durationS: number, format: RecordingFormat): number {
  const rate =
    format === RecordingFormat.Pcm16
      ? RATES.recordingPcm16BytesPerSecond
      : RATES.recordingOpusBytesPerSecond;
  return Math.round(durationS * rate);
}

/**
 * How long moving `bytes` will actually take. The reason the nightly sync is a
 * WiFi ritual is visible directly in this function: a 16 h Opus day is ~173 MB,
 * which is ~16 hours over BLE and ~90 seconds over the AP.
 */
export function transferMs(bytes: number, via: "ble" | "wifiAp"): number {
  const rate = via === "wifiAp" ? RATES.wifiApBytesPerSecond : RATES.bleBytesPerSecond;
  return (bytes / rate) * 1000;
}

// Re-exported so a consumer of the command surface does not have to import from
// two places to describe a device.
export type { BatteryStatus, DiskInfo, Features, Photo, RemoteFile, WifiAccessPoint };
