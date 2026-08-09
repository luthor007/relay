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
