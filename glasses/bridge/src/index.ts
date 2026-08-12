/**
 * @uulab/glasses-bridge — the seam between the app and the glasses.
 *
 * Zero dependencies, no React Native imports, no Node built-ins. It runs in RN,
 * in a browser, and under `node --test` unchanged.
 *
 *     import { MockTransport, FakeClock } from "@uulab/glasses-bridge"
 *
 *     const clock = new FakeClock()
 *     const glasses = new MockTransport({ clock })
 *     await clock.advance(1000)   // nothing happens until time moves
 *
 * Swap `MockTransport` for `AndroidTransport` / `IosTransport` on device; every
 * caller above this line is unchanged.
 */

export type { Clock, CancelTimer } from "./clock.ts";
export { FakeClock, systemClock } from "./clock.ts";

export { TypedEmitter } from "./emitter.ts";
export type { Handler } from "./emitter.ts";

export type { ConnectOptions, FetchProgress, GlassesTransport } from "./transport.ts";

export { MOCK_DEFAULTS, MockTransport, syntheticJpeg } from "./mock.ts";
export type { MockFaults, MockOptions } from "./mock.ts";

export {
  TRACE_VERSION,
  TraceBuilder,
  TraceFormatError,
  base64ToBytes,
  bytesToBase64,
  decodeEventPayload,
  encodeEventPayload,
  parseTrace,
  serialiseTrace,
  traceDurationMs,
} from "./trace.ts";
export type { Trace, TraceEventRecord, TraceFrame } from "./trace.ts";

export {
  PAIRING_VERSION,
  TEST_ONLY_PAKE_NAME,
  PairingClient,
  PairingError,
  PairingErrorCode,
  PairingHost,
  PairingHostState,
  PairingRole,
  SealedChannel,
  bytesToHex,
  completePairing,
  countingRandom,
  equalBytes,
  field,
  formatPairingCode,
  fromUtf8,
  hexToBytes,
  hkdf,
  hmacSha256,
  linkSealingKey,
  newPairingCode,
  openSealed,
  parsePairingCode,
  seal,
  sha256,
  // Not a PAKE, and named so. It refuses to build without an explicit opt-in and
  // refuses to build at all in a production environment — see pairing.ts.
  unsafeTestOnlyPake,
  utf8,
  webCryptoRandom,
} from "./pairing.ts";
export type {
  DeviceCredential,
  PairAccept,
  PairConfirm,
  PairGrant,
  PairHello,
  PairingClientOptions,
  PairingCode,
  PairingHostOptions,
  PairingMessage,
  PairingRun,
  PakeEngine,
  PakeSession,
  RandomSource,
  SealedFrame,
  TestOnlyPakeOptions,
} from "./pairing.ts";

export {
  AUTH_WINDOW_MS,
  LINK_SUBPROTOCOL,
  LINK_VERSION,
  LinkError,
  LinkErrorCode,
  LinkState,
  MockRelaydSocket,
  PhoneMessage,
  RelaydLink,
  ServerErrorCode,
  ServerMessage,
  authHeader,
  backoffMs,
  isProductServerMessage,
  isServerMessage,
  mockSocketFactory,
  newEnvelopeId,
  parseEnvelope,
  serialiseEnvelope,
  verifyAuthHeader,
} from "./relayd.ts";
export type {
  AckFrame,
  AuthProof,
  BackoffOptions,
  ConfirmResolvedFrame,
  NotifyFrame,
  ProductServerMessage,
  RelaydEnvelope,
  RelaydLinkEvents,
  RelaydLinkOptions,
  RelaydSocket,
  ServerErrorFrame,
  SocketFactory,
} from "./relayd.ts";

export {
  DEFAULT_DELIVERED_MEMORY,
  DEFAULT_QUEUE_CAPACITY_BYTES,
  BoxReachability,
  BulkSync,
  MemoryQueueStore,
  MockSyncNetwork,
  QueueRefusal,
  StoreAndForwardQueue,
  SyncDeferral,
  SyncPhase,
} from "./queue.ts";
export type {
  BulkSyncOptions,
  EnqueueResult,
  FlushResult,
  QueueEvents,
  QueueOptions,
  QueueRecord,
  QueueSend,
  QueueStore,
  StoredRecord,
  SyncEvents,
  SyncNetwork,
  SyncResult,
} from "./queue.ts";

export { ConnectionState, GlassesError, GlassesErrorCode, TouchAction, AudioFormat } from "./types.ts";
export type {
  AudioChunk,
  BatteryStatus,
  DiskInfo,
  Features,
  GlassesEventName,
  GlassesEvents,
  Photo,
  PhotoOptions,
  PhotoProgress,
  RecordingState,
  RemoteFile,
  Unsubscribe,
  WifiAccessPoint,
} from "./types.ts";

// --- the full command surface ------------------------------------------------
// Every glasses command, by hand — ORCHESTRATOR.md §5. IDs are taken from
// glasses/protocol/commands.py, which is the source of truth.

export { MOCK_WAKE_WORDS } from "./mock.ts";

export {
  AiInterfaceEvent,
  AudioControl,
  COMMAND_CATALOG,
  CaptureKind,
  Command,
  CommandRole,
  CommandType,
  DeviceMode,
  EqPreset,
  FRAME_PREFIX,
  FrameError,
  KeyAction,
  KeyGesture,
  NoiseCancellation,
  PACKET_HEADER_LEN,
  RATES,
  RecognitionOwner,
  RecordingFormat,
  SHARPNESS_MAX,
  SHARPNESS_MIN,
  SequenceCounter,
  SpeakMode,
  SpeakerRoute,
  Toggle,
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
} from "./commands.ts";

export type {
  AllGlassesEventName,
  AllGlassesEvents,
  CallState,
  CaptureState,
  ClearResult,
  CommandDescriptor,
  CommandId,
  CommandName,
  CommandRecord,
  DeviceIdentity,
  FileTransfer,
  FileTransferProgress,
  GlassesCommandEvents,
  GlassesCommandSet,
  GlassesCommands,
  KeyBinding,
  MediaCounts,
  OtaInfo,
  OtaProgress,
  Packet,
  PhotoParams,
  RunState,
  VideoParams,
  VideoResolution,
  WakeWord,
  WakeWordSetting,
  WifiApState,
  WifiOperationReport,
} from "./commands.ts";
