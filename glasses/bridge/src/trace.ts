/**
 * Session traces — recorded device behaviour, replayable without hardware.
 *
 * A trace carries two parallel views of the same session:
 *
 *   `frames`  raw wire bytes, hex, exactly as captured. Ground truth. Decodable
 *             by glasses/protocol, and the thing to diff against when firmware
 *             changes or a codec assumption turns out wrong.
 *   `events`  the decoded, product-level view. What MockTransport replays.
 *
 * `tools/capture_trace.py` writes both from a real device. Until hardware is in
 * hand, `fixtures/` holds synthetic traces built to the same schema, so code
 * written against them keeps working when real captures replace them.
 *
 * Binary payloads are base64 in JSON — `audioChunk.data` and `photo.data`.
 */

import type { GlassesEventName, GlassesEvents } from "./types.ts";

export const TRACE_VERSION = 1;

export interface TraceFrame {
  /** Milliseconds from the start of the trace. */
  tMs: number;
  dir: "tx" | "rx";
  /** Complete wire frame including 0xA5 prefix, length and CRC. */
  hex: string;
  note?: string;
}

export interface TraceEventRecord {
  tMs: number;
  event: GlassesEventName;
  /** Serialised payload; binary fields are base64. */
  payload: unknown;
}

export interface Trace {
  version: typeof TRACE_VERSION;
  recordedAt?: string;
  device?: { model?: string; firmware?: string; hardware?: string };
  notes?: string;
  frames?: TraceFrame[];
  events: TraceEventRecord[];
}

// --- base64 -----------------------------------------------------------------
// atob/btoa exist in Node 16+, React Native, and browsers. Buffer does not exist
// in RN, so it is deliberately not used here.

export function bytesToBase64(bytes: Uint8Array): string {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}

export function base64ToBytes(b64: string): Uint8Array {
  const binary = atob(b64);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}

/** Event payload fields that are binary and therefore base64 on disk. */
const BINARY_FIELDS: Partial<Record<GlassesEventName, readonly string[]>> = {
  audioChunk: ["data"],
  photo: ["data"],
};

export function encodeEventPayload(event: GlassesEventName, payload: unknown): unknown {
  const fields = BINARY_FIELDS[event];
  if (!fields || payload === null || typeof payload !== "object") return payload;

  const out: Record<string, unknown> = { ...(payload as Record<string, unknown>) };
  for (const field of fields) {
    const value = out[field];
    if (value instanceof Uint8Array) out[field] = bytesToBase64(value);
  }
  return out;
}

export function decodeEventPayload(event: GlassesEventName, payload: unknown): unknown {
  const fields = BINARY_FIELDS[event];
  if (!fields || payload === null || typeof payload !== "object") return payload;

  const out: Record<string, unknown> = { ...(payload as Record<string, unknown>) };
  for (const field of fields) {
    const value = out[field];
    if (typeof value === "string") out[field] = base64ToBytes(value);
  }
  return out;
}

// --- parsing ----------------------------------------------------------------

export class TraceFormatError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "TraceFormatError";
  }
}

/**
 * Validate and normalise a parsed JSON trace.
 *
 * Strict on purpose: a malformed fixture that silently plays back half a session
 * produces a passing test suite and a broken app.
 */
export function parseTrace(input: unknown): Trace {
  if (input === null || typeof input !== "object") {
    throw new TraceFormatError("trace must be an object");
  }
  const raw = input as Record<string, unknown>;

  if (raw.version !== TRACE_VERSION) {
    throw new TraceFormatError(
      `unsupported trace version ${String(raw.version)}, expected ${TRACE_VERSION}`,
    );
  }
  if (!Array.isArray(raw.events)) {
    throw new TraceFormatError("trace.events must be an array");
  }

  const events: TraceEventRecord[] = raw.events.map((entry, i) => {
    if (entry === null || typeof entry !== "object") {
      throw new TraceFormatError(`events[${i}] must be an object`);
    }
    const record = entry as Record<string, unknown>;
    if (typeof record.tMs !== "number" || !Number.isFinite(record.tMs)) {
      throw new TraceFormatError(`events[${i}].tMs must be a finite number`);
    }
    if (typeof record.event !== "string") {
      throw new TraceFormatError(`events[${i}].event must be a string`);
    }
    return {
      tMs: record.tMs,
      event: record.event as GlassesEventName,
      payload: decodeEventPayload(record.event as GlassesEventName, record.payload),
    };
  });

  for (let i = 1; i < events.length; i++) {
    if (events[i]!.tMs < events[i - 1]!.tMs) {
      throw new TraceFormatError(
        `events must be ordered by tMs; events[${i}] (${events[i]!.tMs}) precedes ` +
          `events[${i - 1}] (${events[i - 1]!.tMs})`,
      );
    }
  }

  const frames: TraceFrame[] | undefined = Array.isArray(raw.frames)
    ? raw.frames.map((entry, i) => {
        const record = entry as Record<string, unknown>;
        if (typeof record?.hex !== "string") {
          throw new TraceFormatError(`frames[${i}].hex must be a string`);
        }
        if (record.dir !== "tx" && record.dir !== "rx") {
          throw new TraceFormatError(`frames[${i}].dir must be "tx" or "rx"`);
        }
        return {
          tMs: typeof record.tMs === "number" ? record.tMs : 0,
          dir: record.dir,
          hex: record.hex,
          note: typeof record.note === "string" ? record.note : undefined,
        };
      })
    : undefined;

  return {
    version: TRACE_VERSION,
    recordedAt: typeof raw.recordedAt === "string" ? raw.recordedAt : undefined,
    device: (raw.device as Trace["device"]) ?? undefined,
    notes: typeof raw.notes === "string" ? raw.notes : undefined,
    frames,
    events,
  };
}

export function serialiseTrace(trace: Trace): string {
  return JSON.stringify(
    {
      ...trace,
      events: trace.events.map((e) => ({
        tMs: e.tMs,
        event: e.event,
        payload: encodeEventPayload(e.event, e.payload),
      })),
    },
    null,
    2,
  );
}

/** Build a trace programmatically — used by fixtures and by tests. */
/**
 * Generic over the event map so a caller that knows about a wider set — the
 * command surface adds events on top of `GlassesEvents` — can build traces
 * without a cast. Defaults to `GlassesEvents`, so every existing caller is
 * unchanged.
 *
 * The parameter is here rather than a direct import of the command map because
 * this is the low-level trace module: it must not depend on the layer above it.
 */
export class TraceBuilder<Events extends object = GlassesEvents> {
  #events: TraceEventRecord[] = [];
  #frames: TraceFrame[] = [];
  // Explicit field, not a constructor parameter property: parameter properties
  // emit runtime code and Node's strip-only TypeScript mode rejects them.
  readonly #meta: Omit<Trace, "version" | "events" | "frames">;

  constructor(meta: Omit<Trace, "version" | "events" | "frames"> = {}) {
    this.#meta = meta;
  }

  event<K extends keyof Events & string>(tMs: number, event: K, payload: Events[K]): this {
    this.#events.push({ tMs, event: event as GlassesEventName, payload });
    return this;
  }

  frame(tMs: number, dir: "tx" | "rx", hex: string, note?: string): this {
    this.#frames.push({ tMs, dir, hex, note });
    return this;
  }

  build(): Trace {
    const events = [...this.#events].sort((a, b) => a.tMs - b.tMs);
    return {
      version: TRACE_VERSION,
      ...this.#meta,
      frames: this.#frames.length > 0 ? [...this.#frames].sort((a, b) => a.tMs - b.tMs) : undefined,
      events,
    };
  }
}

export function traceDurationMs(trace: Trace): number {
  const last = trace.events[trace.events.length - 1];
  return last ? last.tMs : 0;
}
