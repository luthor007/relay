/**
 * The phone ↔ `relayd` link.
 *
 * `docs/SYSTEM.md` §6.1, exactly: one authenticated WebSocket, JSON envelopes,
 * both directions.
 *
 *     { v: 1, id: "<uuid>", type: "<name>", at: <unix_ms>, payload: {...} }
 *
 * The reason it is a socket and not gRPC or HTTP is in that section too — "a
 * WebSocket survives a phone that sleeps and wakes far more gracefully than a
 * stream that has to be re-established with state" — and it is the single most
 * load-bearing sentence for this file. **Sleep/wake is the normal path, not the
 * exception.** A phone in a pocket loses this socket dozens of times a day: screen
 * off, cell handover, WiFi to LTE, the OS suspending the process, and — see
 * `queue.ts` — the phone deliberately joining the glasses' access point, which
 * costs it the uplink outright.
 *
 * So the three properties that matter are all about the gap:
 *
 *   reconnect     exponential backoff with jitter, and an immediate retry when the
 *                 OS says the network is back rather than waiting out the timer
 *   resume        queued work survives the gap, in order
 *   never drop    a message handed to `send` during a reconnect is delivered later,
 *                 and a message that cannot be accepted is refused out loud
 *
 * ## At-least-once, and `ack` is what ends it
 *
 * A `send` that the socket accepted may still have died in a buffer when the link
 * dropped. So anything sent on a connection that then closed abnormally goes back
 * to the *head* of the outbox and is sent again. That makes delivery at-least-once
 * and duplicates possible — which is what the envelope's `id` is for, and why the
 * server side dedupes on it.
 *
 * The frame that stops the redelivery is `ack`, and its shape is not ours to
 * choose. `relayd/internal/api/wire.go` sends `{ re: "<envelope id>", ok: true }`,
 * one per accepted frame, and `docs/SYSTEM.md` §6.1 documents that. An earlier
 * version of this file invented a batched `link.ack` with `{ ids: [...] }`; a
 * batched form may well be better, but a client that unilaterally speaks a
 * different acknowledgement than the daemon and the doc is simply a client whose
 * queue never drains. The daemon and the doc win; a batch is a later milestone's
 * conversation.
 *
 * A refusal comes back on the same channel as an `error` frame carrying a code
 * and, for anything not built yet, the milestone that will build it. That is not
 * decoration either: `error` also **ends** the redelivery, because a frame the
 * daemon has refused will be refused again on every reconnect for the life of the
 * queue. `not_implemented, M4` is how the phone learns to keep the audio on the
 * device rather than delete it.
 *
 * Unknown inbound types are still surfaced, never fatal: this link has to
 * tolerate a server that is newer than it is.
 *
 * ## Authentication
 *
 * The credential comes from `pairing.ts` and is presented in the WebSocket
 * subprotocol header rather than the URL — query strings end up in proxy logs and
 * crash reports, and this one would be a bearer token. The proof is an HMAC over
 * `deviceId | timestamp | nonce` under the signing key, so a captured header is
 * useless outside its window.
 *
 * When the link runs through our rendezvous relay (`docs/SYSTEM.md` §7) the
 * envelopes are additionally sealed with `SealedChannel`, so "the relay pipes bytes
 * it cannot read" is enforced by the phone rather than promised by the operator.
 */

import type { Clock, CancelTimer } from "./clock.ts";
import { systemClock } from "./clock.ts";
import { TypedEmitter } from "./emitter.ts";
import type { DeviceCredential, RandomSource, SealedFrame } from "./pairing.ts";
import {
  SealedChannel,
  PairingRole,
  bytesToHex,
  equalBytes,
  hexToBytes,
  hmacSha256,
  linkSealingKey,
  utf8,
  webCryptoRandom,
} from "./pairing.ts";
import type { Unsubscribe } from "./types.ts";

export const LINK_VERSION = 1;

/** The subprotocol both ends must agree on. Bump with the envelope version. */
export const LINK_SUBPROTOCOL = "relay.v1";

/** How long a link auth header stays valid. Matches connector's signature window. */
export const AUTH_WINDOW_MS = 5 * 60 * 1000;

// --- the vocabulary ---------------------------------------------------------

/** Phone → server. `docs/SYSTEM.md` §6.1. */
export const PhoneMessage = {
  Utterance: "utterance",
  Touch: "touch",
  Wear: "wear",
  AudioChunk: "audio.chunk",
  Photo: "photo",
  SessionCommand: "session.command",
  ConsentDecision: "consent.decision",
  SyncOffer: "sync.offer",
} as const;

export type PhoneMessage = (typeof PhoneMessage)[keyof typeof PhoneMessage];

/**
 * Server → phone. `docs/SYSTEM.md` §6.1 — all ten, in the order the doc lists
 * them.
 *
 * The first six are the *product* messages. The last four are the ones the
 * transport turned out to need, and each was added because leaving it out made a
 * documented behaviour unimplementable:
 *
 *   ack               every phone→server frame carries an `id`; without something
 *                     that says it landed, the store-and-forward queue cannot tell
 *                     "delivered" from "socket up, daemon dropped it"
 *   error             refusals on the same channel, with a code and — for anything
 *                     unbuilt — the milestone that will build it
 *   notify            `docs/ADAPTERS.md` §7's notification *without* speech: quiet
 *                     hours hold the speech and keep the notification
 *   confirm.resolved  retracts a `confirm.request` whose question is gone, so a
 *                     ping does not outlive it
 */
export const ServerMessage = {
  Speak: "speak",
  UiRender: "ui.render",
  SessionList: "session.list",
  ConfirmRequest: "confirm.request",
  ConnectorProposal: "connector.proposal",
  Digest: "digest",
  Ack: "ack",
  Error: "error",
  Notify: "notify",
  ConfirmResolved: "confirm.resolved",
} as const;

export type ServerMessage = (typeof ServerMessage)[keyof typeof ServerMessage];

/**
 * The six that carry product payloads, each of which reaches a listener named
 * after it. The other four are transport frames and are routed to shaped events
 * instead — see [RelaydLinkEvents].
 */
export type ProductServerMessage =
  | typeof ServerMessage.Speak
  | typeof ServerMessage.UiRender
  | typeof ServerMessage.SessionList
  | typeof ServerMessage.ConfirmRequest
  | typeof ServerMessage.ConnectorProposal
  | typeof ServerMessage.Digest;

/**
 * The `code` on an `error` frame. Mirrors `relayd/internal/api/wire.go`.
 *
 * Open rather than closed — the link accepts a code it does not know and passes
 * it through, because a daemon that grows a new refusal must not become a daemon
 * whose refusals are invisible.
 */
export const ServerErrorCode = {
  BadEnvelope: "bad_envelope",
  UnsupportedVersion: "unsupported_version",
  UnknownType: "unknown_type",
  BadPayload: "bad_payload",
  /** Named with a milestone. The phone keeps the data rather than deleting it. */
  NotImplemented: "not_implemented",
  NoSuchSession: "no_such_session",
  Unsupported: "unsupported",
  Failed: "failed",
} as const;

export type ServerErrorCode = (typeof ServerErrorCode)[keyof typeof ServerErrorCode];

export interface RelaydEnvelope {
  v: number;
  id: string;
  type: string;
  at: number;
  payload: unknown;
}

export class LinkError extends Error {
  readonly code: LinkErrorCode;

  constructor(code: LinkErrorCode, message: string) {
    super(message);
    this.name = "LinkError";
    this.code = code;
  }
}

export const LinkErrorCode = {
  /** Inbound text was not a valid envelope. Reported, never fatal. */
  Malformed: "malformed",
  VersionMismatch: "versionMismatch",
  /** The outbox is full. The caller still owns the data. */
  OutboxFull: "outboxFull",
  /** Sealing was expected and the frame was not sealed, or failed to open. */
  SealFailed: "sealFailed",
  SocketFailed: "socketFailed",
  Closed: "closed",
} as const;

export type LinkErrorCode = (typeof LinkErrorCode)[keyof typeof LinkErrorCode];

/**
 * Strict on purpose. An envelope that half-parses produces a UI that acts on a
 * field it invented — and `docs/SYSTEM.md` §6.1 is a contract two languages have
 * to implement identically.
 */
export function parseEnvelope(text: string): RelaydEnvelope {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch (error) {
    throw new LinkError(LinkErrorCode.Malformed, `envelope is not JSON: ${String(error)}`);
  }
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    throw new LinkError(LinkErrorCode.Malformed, "envelope must be a JSON object");
  }
  const record = raw as Record<string, unknown>;

  if (record.v !== LINK_VERSION) {
    throw new LinkError(
      LinkErrorCode.VersionMismatch,
      `envelope v=${String(record.v)}, this link speaks v=${LINK_VERSION}`,
    );
  }
  if (typeof record.id !== "string" || record.id.length === 0) {
    throw new LinkError(LinkErrorCode.Malformed, "envelope.id must be a non-empty string");
  }
  if (typeof record.type !== "string" || record.type.length === 0) {
    throw new LinkError(LinkErrorCode.Malformed, "envelope.type must be a non-empty string");
  }
  if (typeof record.at !== "number" || !Number.isFinite(record.at)) {
    throw new LinkError(LinkErrorCode.Malformed, "envelope.at must be a finite number");
  }
  return {
    v: LINK_VERSION,
    id: record.id,
    type: record.type,
    at: record.at,
    payload: record.payload,
  };
}

export function serialiseEnvelope(envelope: RelaydEnvelope): string {
  return JSON.stringify(envelope);
}

/**
 * RFC 4122 v4 from an injected random source.
 *
 * Not `crypto.randomUUID`: React Native does not have it, and this package has to
 * run there unchanged. The bytes come from the same source pairing uses, so a test
 * gets stable ids for free.
 */
export function newEnvelopeId(random: RandomSource): string {
  const bytes = random(16);
  bytes[6] = (bytes[6]! & 0x0f) | 0x40;
  bytes[8] = (bytes[8]! & 0x3f) | 0x80;
  const hex = bytesToHex(bytes);
  return [
    hex.slice(0, 8),
    hex.slice(8, 12),
    hex.slice(12, 16),
    hex.slice(16, 20),
    hex.slice(20),
  ].join("-");
}

// --- the socket seam --------------------------------------------------------

/**
 * The subset of WebSocket this link uses.
 *
 * Deliberately assignment-style handlers rather than `addEventListener`, because
 * that is the shape React Native, browsers and `ws` all agree on. The real
 * implementation is three lines; the fake below is what the tests drive.
 */
export interface RelaydSocket {
  send(data: string): void;
  close(code?: number, reason?: string): void;
  onOpen?: () => void;
  onMessage?: (data: string) => void;
  onClose?: (info: { code: number; reason: string; clean: boolean }) => void;
  onError?: (error: Error) => void;
}

export type SocketFactory = (url: string, protocols: string[]) => RelaydSocket;

export const LinkState = {
  Idle: "idle",
  Connecting: "connecting",
  Open: "open",
  /** Waiting out a backoff. Work still queues. */
  Reconnecting: "reconnecting",
  /** Deliberately shut. Only `connect()` reopens it. */
  Closed: "closed",
} as const;

export type LinkState = (typeof LinkState)[keyof typeof LinkState];

// --- the four transport frames, shaped -------------------------------------
//
// Payload shapes are `relayd/internal/api/wire.go`'s, field for field. The
// product frames stay opaque on purpose — the link is not the place to decide
// what a `ui.render` node means — but these four the link itself acts on, so it
// reads them rather than handing a caller an `unknown` and hoping.

/** `Ack` in wire.go. */
export interface AckFrame {
  envelope: RelaydEnvelope;
  /** The id of the phone→server envelope this acknowledges. */
  re: string;
  ok: boolean;
  /** True when this ack retired something the link was still holding. */
  pruned: boolean;
}

/** `ErrorPayload` in wire.go. */
export interface ServerErrorFrame {
  envelope: RelaydEnvelope;
  /** The phone→server envelope refused, or null for an unsolicited error. */
  re: string | null;
  code: ServerErrorCode | string;
  message: string;
  /** Where the unbuilt half lives, e.g. `"M4 — capture and memory"`. */
  milestone: string | null;
  /** True when the refusal retired an envelope the link would have redelivered. */
  cancelled: boolean;
}

/** `Notify` in wire.go. */
export interface NotifyFrame {
  envelope: RelaydEnvelope;
  title: string;
  body: string;
  sessions: string[];
  /**
   * Quiet hours: present, soundless. Never a reason to drop it — a held
   * notification that is also silently discarded is the quiet-hours behaviour
   * failing with nothing in the log.
   */
  silent: boolean;
  ping: string | null;
}

/** `ConfirmResolved` in wire.go. */
export interface ConfirmResolvedFrame {
  envelope: RelaydEnvelope;
  actionId: string;
  /** Why: answered in a terminal, turn cancelled, deadline passed. */
  reason: string;
  /**
   * False when this link never saw the question, or already retracted it. The
   * UI still needs the event — a resolution that arrives before the request did
   * must not leave a ping on screen — but it tells a duplicate from a retraction.
   */
  wasOutstanding: boolean;
}

export interface RelaydLinkEvents {
  stateChanged: LinkState;
  /** Every inbound envelope, whatever its type. */
  message: RelaydEnvelope;
  speak: RelaydEnvelope;
  "ui.render": RelaydEnvelope;
  "session.list": RelaydEnvelope;
  "confirm.request": RelaydEnvelope;
  "connector.proposal": RelaydEnvelope;
  digest: RelaydEnvelope;
  /** A notification that must reach the phone whether or not anything is spoken. */
  notify: NotifyFrame;
  /** The question is gone. Take the ping down. */
  "confirm.resolved": ConfirmResolvedFrame;
  /** A frame landed. Emitted after the outbox has been pruned. */
  ack: AckFrame;
  /**
   * The server refused a frame.
   *
   * Named apart from `error`, which is *this link's* own faults — a UI that
   * conflates "the daemon will not do this" with "the socket broke" retries the
   * first forever.
   */
  serverError: ServerErrorFrame;
  /** A type this build does not know. Forward compatibility, not an error. */
  unknownType: RelaydEnvelope;
  /** Bad inbound frame, refused outbox, failed seal. Never thrown at the caller. */
  error: LinkError;
  /** Envelopes put back after an abnormal close, and therefore sent twice. */
  redelivered: { count: number; ids: string[] };
  sent: RelaydEnvelope;
}

export interface BackoffOptions {
  baseMs?: number;
  maxMs?: number;
  /** Fraction of the interval that is random. 0 = none, 0.5 = half. */
  jitter?: number;
}

/**
 * Exponential with jitter, `roll` in [0, 1] supplying the randomness.
 *
 * The jitter subtracts rather than adds, so `maxMs` is a real ceiling. It is not
 * decoration either: every phone in a building loses WiFi at the same moment when
 * an access point reboots, and a fleet that all retries at exactly 1 s, 2 s, 4 s
 * is a self-inflicted thundering herd on the one box.
 */
export function backoffMs(attempt: number, options: BackoffOptions = {}, roll = 0): number {
  const base = options.baseMs ?? 500;
  const max = options.maxMs ?? 30_000;
  const jitter = Math.min(1, Math.max(0, options.jitter ?? 0.5));
  const exponential = Math.min(max, base * Math.pow(2, Math.max(0, attempt)));
  return Math.round(exponential * (1 - jitter + jitter * Math.min(1, Math.max(0, roll))));
}

export interface RelaydLinkOptions {
  url: string;
  credential: DeviceCredential;
  socketFactory: SocketFactory;
  clock?: Clock;
  random?: RandomSource;
  /**
   * Seal every envelope. Set when the route is the rendezvous relay rather than
   * the LAN — see `docs/SYSTEM.md` §7.
   */
  sealed?: boolean;
  backoff?: BackoffOptions;
  /** Bounded, because an unbounded outbox is a memory leak with a good excuse. */
  outboxLimit?: number;
  /**
   * Close and reconnect if nothing arrives for this long. Default 0 (off):
   * liveness belongs in WebSocket ping/pong (RFC 6455 §5.5.2), which the platform
   * handles beneath this layer. The knob exists for platforms that do not expose it.
   */
  idleTimeoutMs?: number;
}

interface OutboxEntry {
  envelope: RelaydEnvelope;
  text: string;
}

/**
 * One authenticated WebSocket to `relayd`, built for a phone that keeps vanishing.
 */
export class RelaydLink {
  #emitter = new TypedEmitter<RelaydLinkEvents>();
  #clock: Clock;
  #random: RandomSource;
  #url: string;
  #credential: DeviceCredential;
  #factory: SocketFactory;
  #backoff: BackoffOptions;
  #outboxLimit: number;
  #idleTimeoutMs: number;
  #sealed: boolean;

  #state: LinkState = LinkState.Idle;
  #socket: RelaydSocket | null = null;
  #channel: SealedChannel | null = null;
  #attempt = 0;
  #cancelRetry: CancelTimer | null = null;
  #cancelIdle: CancelTimer | null = null;

  /** Waiting to go out. */
  #outbox: OutboxEntry[] = [];
  /** Handed to a socket, not yet known to have landed. */
  #inFlight: OutboxEntry[] = [];
  /** `action_id`s asked and not yet answered or retracted. */
  #confirmations = new Set<string>();

  constructor(options: RelaydLinkOptions) {
    this.#clock = options.clock ?? systemClock;
    this.#random = options.random ?? webCryptoRandom;
    this.#url = options.url;
    this.#credential = options.credential;
    this.#factory = options.socketFactory;
    this.#backoff = options.backoff ?? {};
    this.#outboxLimit = options.outboxLimit ?? 1_000;
    this.#idleTimeoutMs = options.idleTimeoutMs ?? 0;
    this.#sealed = options.sealed ?? false;
  }

  get state(): LinkState {
    return this.#state;
  }

  /** Envelopes accepted and not yet delivered, including any in flight. */
  get pending(): number {
    return this.#outbox.length + this.#inFlight.length;
  }

  get pendingIds(): string[] {
    return [...this.#inFlight, ...this.#outbox].map((entry) => entry.envelope.id);
  }

  get attempt(): number {
    return this.#attempt;
  }

  /**
   * Questions the box has asked and neither this phone nor the box has closed.
   *
   * The list exists so a `confirm.resolved` can retract a specific ping rather
   * than clearing the screen: waking someone to approve what is already approved
   * is the failure that frame was added for.
   */
  get outstandingConfirmations(): string[] {
    return [...this.#confirmations];
  }

  on<K extends keyof RelaydLinkEvents>(
    event: K,
    handler: (payload: RelaydLinkEvents[K]) => void,
  ): Unsubscribe {
    return this.#emitter.on(event, handler);
  }

  connect(): void {
    if (this.#state === LinkState.Connecting || this.#state === LinkState.Open) return;
    this.#open();
  }

  /**
   * Deliberate shutdown. Anything queued stays queued: the app being closed is not
   * a reason to throw away an utterance the user already said.
   */
  close(): void {
    this.#cancelRetry?.();
    this.#cancelRetry = null;
    this.#cancelIdle?.();
    this.#cancelIdle = null;
    const socket = this.#socket;
    this.#socket = null;
    this.#channel = null;
    // Even a clean close can lose what is still in the socket's buffer, so
    // in-flight work goes back to the outbox and waits for the next connect.
    this.#requeueInFlight();
    this.#setState(LinkState.Closed);
    socket?.close(1000, "client closed");
  }

  /**
   * The OS says the network is back, or the app came to the foreground.
   *
   * Collapses the backoff instead of waiting it out. Without this a phone that
   * wakes into good WiFi sits idle for up to `maxMs` for no reason, which is the
   * whole sleep/wake case behaving badly.
   */
  wake(): void {
    if (
      this.#state === LinkState.Open ||
      this.#state === LinkState.Connecting ||
      this.#state === LinkState.Closed
    ) {
      return;
    }
    this.#cancelRetry?.();
    this.#cancelRetry = null;
    this.#attempt = 0;
    this.#open();
  }

  /**
   * Queue an envelope for delivery.
   *
   * Returns the id, which is also the dedupe key on the server. Never throws for
   * a closed or reconnecting link — that is the normal state of a phone, and a
   * caller that has to check first is a caller that will forget.
   */
  send(type: PhoneMessage | string, payload: unknown): string | null {
    const envelope: RelaydEnvelope = {
      v: LINK_VERSION,
      id: newEnvelopeId(this.#random),
      type,
      at: this.#clock.now(),
      payload,
    };

    if (this.pending >= this.#outboxLimit) {
      // Refuse the newest and say so, exactly as `queue.ts` does. Silently
      // dropping the oldest here would discard the utterance the user is waiting
      // on an answer to.
      this.#emitter.emit(
        "error",
        new LinkError(
          LinkErrorCode.OutboxFull,
          `link outbox full at ${this.#outboxLimit} envelopes; "${type}" refused`,
        ),
      );
      return null;
    }

    // Answering closes the question here, not when the box gets around to saying
    // so. Otherwise a `confirm.resolved` that arrives seconds later reads as a
    // retraction of something still open.
    if (type === PhoneMessage.ConsentDecision) {
      const actionId = readString(payload, "action_id");
      if (actionId !== undefined) this.#confirmations.delete(actionId);
    }

    this.#outbox.push({ envelope, text: serialiseEnvelope(envelope) });
    this.#flush();
    return envelope.id;
  }

  // --- internals ------------------------------------------------------------

  #open(): void {
    this.#setState(LinkState.Connecting);

    const nonce = this.#random(16);
    const header = authHeader(this.#credential, this.#clock.now(), nonce);
    this.#channel = this.#sealed
      ? new SealedChannel(linkSealingKey(this.#credential, nonce), PairingRole.Client)
      : null;

    let socket: RelaydSocket;
    try {
      socket = this.#factory(this.#url, [LINK_SUBPROTOCOL, header]);
    } catch (error) {
      this.#emitter.emit(
        "error",
        new LinkError(LinkErrorCode.SocketFailed, `could not open socket: ${String(error)}`),
      );
      this.#scheduleRetry();
      return;
    }

    this.#socket = socket;

    socket.onOpen = () => {
      if (this.#socket !== socket) return;
      this.#attempt = 0;
      this.#setState(LinkState.Open);
      this.#armIdleTimer();
      this.#flush();
    };

    socket.onMessage = (data) => {
      if (this.#socket !== socket) return;
      this.#armIdleTimer();
      this.#receive(data);
    };

    socket.onError = (error) => {
      if (this.#socket !== socket) return;
      this.#emitter.emit("error", new LinkError(LinkErrorCode.SocketFailed, error.message));
    };

    socket.onClose = (info) => {
      if (this.#socket !== socket) return;
      this.#socket = null;
      this.#channel = null;
      this.#cancelIdle?.();
      this.#cancelIdle = null;
      // A clean close still loses whatever was in the socket's buffer, so
      // everything in flight goes back either way.
      this.#requeueInFlight();
      if (this.#state !== LinkState.Closed) this.#scheduleRetry();
    };
  }

  #receive(data: string): void {
    let text = data;
    if (this.#channel) {
      try {
        text = this.#channel.openText(JSON.parse(data) as SealedFrame);
      } catch (error) {
        this.#emitter.emit(
          "error",
          new LinkError(LinkErrorCode.SealFailed, `could not open sealed frame: ${String(error)}`),
        );
        return;
      }
    }

    let envelope: RelaydEnvelope;
    try {
      envelope = parseEnvelope(text);
    } catch (error) {
      this.#emitter.emit(
        "error",
        error instanceof LinkError
          ? error
          : new LinkError(LinkErrorCode.Malformed, String(error)),
      );
      return;
    }

    // Everything documented goes on the firehose first, including the transport
    // frames: a dropped `notify` is the quiet-hours behaviour failing with no log
    // line, which is precisely the bug that made this list ten instead of six.
    this.#emitter.emit("message", envelope);

    switch (envelope.type) {
      case ServerMessage.Ack:
        this.#handleAck(envelope);
        return;
      case ServerMessage.Error:
        this.#handleServerError(envelope);
        return;
      case ServerMessage.Notify:
        this.#handleNotify(envelope);
        return;
      case ServerMessage.ConfirmResolved:
        this.#handleConfirmResolved(envelope);
        return;
      case ServerMessage.ConfirmRequest:
        // Remembered so the matching `confirm.resolved` can retract it.
        this.#rememberConfirmRequest(envelope);
        break;
      default:
        break;
    }

    if (isProductServerMessage(envelope.type)) {
      this.#emitter.emit(envelope.type, envelope);
    } else {
      this.#emitter.emit("unknownType", envelope);
    }
  }

  /**
   * One ack, one envelope — `{ re, ok }`, exactly `relayd/internal/api/wire.go`.
   *
   * `ok: false` deliberately does *not* prune: it says the frame did not land, so
   * the entry stays held and goes out again on the next connection.
   */
  #handleAck(envelope: RelaydEnvelope): void {
    const re = readString(envelope.payload, "re") ?? "";
    const ok = readBoolean(envelope.payload, "ok") ?? true;
    let pruned = false;
    if (re !== "" && ok) {
      const before = this.#inFlight.length;
      this.#inFlight = this.#inFlight.filter((entry) => entry.envelope.id !== re);
      pruned = this.#inFlight.length !== before;
    }
    this.#emitter.emit("ack", { envelope, re, ok, pruned });
  }

  /**
   * A refusal is an answer, so it retires the envelope it names.
   *
   * Leaving it in flight would redeliver it on every reconnect for the life of
   * the queue, and it would be refused every time — `not_implemented` does not
   * become implemented because a phone asked again. The caller gets the code and
   * the milestone and decides what to do with the data it still owns.
   */
  #handleServerError(envelope: RelaydEnvelope): void {
    const re = readString(envelope.payload, "re") ?? null;
    const code = readString(envelope.payload, "code") ?? ServerErrorCode.Failed;
    const message = readString(envelope.payload, "message") ?? "";
    const milestone = readString(envelope.payload, "milestone") ?? null;

    let cancelled = false;
    if (re !== null) {
      const before = this.#inFlight.length + this.#outbox.length;
      this.#inFlight = this.#inFlight.filter((entry) => entry.envelope.id !== re);
      this.#outbox = this.#outbox.filter((entry) => entry.envelope.id !== re);
      cancelled = this.#inFlight.length + this.#outbox.length !== before;
    }

    this.#emitter.emit("serverError", { envelope, re, code, message, milestone, cancelled });
  }

  #handleNotify(envelope: RelaydEnvelope): void {
    this.#emitter.emit("notify", {
      envelope,
      title: readString(envelope.payload, "title") ?? "",
      body: readString(envelope.payload, "body") ?? "",
      sessions: readStrings(envelope.payload, "sessions"),
      silent: readBoolean(envelope.payload, "silent") ?? false,
      ping: readString(envelope.payload, "ping") ?? null,
    });
  }

  #rememberConfirmRequest(envelope: RelaydEnvelope): void {
    const actionId = readString(envelope.payload, "action_id");
    if (actionId !== undefined) this.#confirmations.add(actionId);
  }

  #handleConfirmResolved(envelope: RelaydEnvelope): void {
    const actionId = readString(envelope.payload, "action_id") ?? "";
    const reason = readString(envelope.payload, "reason") ?? "";
    const wasOutstanding = actionId !== "" && this.#confirmations.delete(actionId);
    this.#emitter.emit("confirm.resolved", { envelope, actionId, reason, wasOutstanding });
  }

  #flush(): void {
    const socket = this.#socket;
    if (!socket || this.#state !== LinkState.Open) return;

    while (this.#outbox.length > 0) {
      const entry = this.#outbox[0]!;
      try {
        socket.send(this.#channel ? JSON.stringify(this.#channel.sealText(entry.text)) : entry.text);
      } catch (error) {
        // The socket refused it. Leave it at the head of the outbox — the close
        // that follows will trigger a reconnect and it goes out then.
        this.#emitter.emit(
          "error",
          new LinkError(LinkErrorCode.SocketFailed, `send failed: ${String(error)}`),
        );
        return;
      }
      this.#outbox.shift();
      this.#inFlight.push(entry);
      this.#emitter.emit("sent", entry.envelope);
    }
  }

  /**
   * Put everything unacknowledged back at the *head*, oldest first.
   *
   * At the head rather than the tail because `relayd` segments by time and a
   * reordered utterance lands in the wrong session. Duplicates are the price, and
   * the envelope `id` is what pays it.
   */
  #requeueInFlight(): void {
    if (this.#inFlight.length === 0) return;
    const returned = this.#inFlight;
    this.#inFlight = [];
    this.#outbox = [...returned, ...this.#outbox];
    this.#emitter.emit("redelivered", {
      count: returned.length,
      ids: returned.map((entry) => entry.envelope.id),
    });
  }

  #scheduleRetry(): void {
    if (this.#state === LinkState.Closed) return;
    this.#setState(LinkState.Reconnecting);
    const delay = backoffMs(this.#attempt, this.#backoff, this.#random(1)[0]! / 255);
    this.#attempt += 1;
    this.#cancelRetry?.();
    this.#cancelRetry = this.#clock.setTimeout(() => {
      this.#cancelRetry = null;
      if (this.#state === LinkState.Closed) return;
      this.#open();
    }, delay);
  }

  #armIdleTimer(): void {
    this.#cancelIdle?.();
    this.#cancelIdle = null;
    if (this.#idleTimeoutMs <= 0) return;
    this.#cancelIdle = this.#clock.setTimeout(() => {
      this.#cancelIdle = null;
      const socket = this.#socket;
      if (!socket) return;
      this.#emitter.emit(
        "error",
        new LinkError(
          LinkErrorCode.SocketFailed,
          `nothing inbound for ${this.#idleTimeoutMs}ms; assuming the link is half-open`,
        ),
      );
      // Close it rather than fake a close event: a half-open socket that is
      // never closed is a file descriptor and a wake lock held forever.
      socket.close(4000, "idle timeout");
    }, this.#idleTimeoutMs);
  }

  #setState(state: LinkState): void {
    if (this.#state === state) return;
    this.#state = state;
    this.#emitter.emit("stateChanged", state);
  }
}

const SERVER_MESSAGE_TYPES = new Set<string>(Object.values(ServerMessage));

export function isServerMessage(type: string): type is ServerMessage {
  return SERVER_MESSAGE_TYPES.has(type);
}

const PRODUCT_SERVER_MESSAGE_TYPES = new Set<string>([
  ServerMessage.Speak,
  ServerMessage.UiRender,
  ServerMessage.SessionList,
  ServerMessage.ConfirmRequest,
  ServerMessage.ConnectorProposal,
  ServerMessage.Digest,
]);

export function isProductServerMessage(type: string): type is ProductServerMessage {
  return PRODUCT_SERVER_MESSAGE_TYPES.has(type);
}

// --- reading a payload without trusting it ----------------------------------
//
// Inbound payloads are `unknown` because they crossed a network. A missing
// `title` must produce an empty string and a delivered notification, not a
// `TypeError` inside the socket callback that takes the link down.

function payloadRecord(payload: unknown): Record<string, unknown> {
  return payload !== null && typeof payload === "object" && !Array.isArray(payload)
    ? (payload as Record<string, unknown>)
    : {};
}

function readString(payload: unknown, key: string): string | undefined {
  const value = payloadRecord(payload)[key];
  return typeof value === "string" && value.length > 0 ? value : undefined;
}

function readBoolean(payload: unknown, key: string): boolean | undefined {
  const value = payloadRecord(payload)[key];
  return typeof value === "boolean" ? value : undefined;
}

function readStrings(payload: unknown, key: string): string[] {
  const value = payloadRecord(payload)[key];
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
}

// --- authentication ---------------------------------------------------------

export const AUTH_PREFIX = "relay.auth.";

export interface AuthProof {
  deviceId: string;
  deviceToken: string;
  timestampMs: number;
  /** Hex. Also the salt for this connection's sealing key. */
  nonce: string;
  signature: string;
}

/**
 * `relay.auth.<deviceId>.<token>.<timestamp>.<nonce>.<hmac>`, carried as a
 * WebSocket subprotocol.
 *
 * The signature covers the timestamp and nonce as well as the token, so replaying
 * a captured header outside its five-minute window fails, and replaying it inside
 * the window is detectable by nonce. The token alone is never the proof.
 *
 * Everything in it is hex, digits, or percent-encoded: a subprotocol value is an
 * RFC 7230 token, which excludes the `/` and `=` that base64 would have brought.
 */
export function authHeader(
  credential: DeviceCredential,
  timestampMs: number,
  nonce: Uint8Array,
): string {
  const nonceHex = bytesToHex(nonce);
  const signature = bytesToHex(
    hmacSha256(
      hexToBytes(credential.signingKey),
      utf8(
        `relay/link/v1\n${credential.deviceId}\n${credential.deviceToken}\n${timestampMs}\n${nonceHex}`,
      ),
    ),
  );
  return (
    AUTH_PREFIX +
    [
      encodeURIComponent(credential.deviceId),
      credential.deviceToken,
      String(timestampMs),
      nonceHex,
      signature,
    ].join(".")
  );
}

/**
 * The box's half. `relayd` owns the production check; this is the reference it has
 * to agree with, and it is what lets the phone's auth be tested with no server.
 */
export function verifyAuthHeader(
  header: string,
  credential: DeviceCredential,
  nowMs: number,
  windowMs = AUTH_WINDOW_MS,
): AuthProof {
  if (!header.startsWith(AUTH_PREFIX)) {
    throw new LinkError(LinkErrorCode.Malformed, "auth header is not a relay.auth header");
  }
  const parts = header.slice(AUTH_PREFIX.length).split(".");
  if (parts.length !== 5) {
    throw new LinkError(LinkErrorCode.Malformed, "auth header has the wrong number of fields");
  }
  const deviceId = decodeURIComponent(parts[0]!);
  const deviceToken = parts[1]!;
  const timestampMs = Number(parts[2]);
  const nonce = parts[3]!;
  const signature = parts[4]!;

  if (!Number.isFinite(timestampMs) || Math.abs(nowMs - timestampMs) > windowMs) {
    throw new LinkError(LinkErrorCode.Malformed, "auth header is outside its freshness window");
  }
  if (deviceId !== credential.deviceId || deviceToken !== credential.deviceToken) {
    throw new LinkError(LinkErrorCode.Malformed, "auth header is for a different device");
  }

  if (!/^[0-9a-f]{64}$/.test(signature)) {
    throw new LinkError(LinkErrorCode.Malformed, "auth header signature is not a SHA-256 HMAC");
  }
  const expected = hmacSha256(
    hexToBytes(credential.signingKey),
    utf8(`relay/link/v1\n${deviceId}\n${deviceToken}\n${timestampMs}\n${nonce}`),
  );
  // Constant time: a fast-exit compare here leaks the signature one byte at a
  // time to anyone who can measure how long the upgrade takes to fail.
  if (!equalBytes(expected, hexToBytes(signature))) {
    throw new LinkError(LinkErrorCode.Malformed, "auth header signature does not verify");
  }

  return { deviceId, deviceToken, timestampMs, nonce, signature };
}

// --- a socket that is not a socket ------------------------------------------

/**
 * Fake WebSocket: nothing opens until a test says so.
 *
 * Same discipline as `FakeClock` — no real sockets, no real timers, no network.
 * `MockRelaydSocket` records everything it was told to send, so a test can assert
 * ordering, redelivery and sealing on the exact bytes the relay would have seen.
 */
export class MockRelaydSocket implements RelaydSocket {
  readonly url: string;
  readonly protocols: string[];
  readonly sent: string[] = [];

  onOpen?: () => void;
  onMessage?: (data: string) => void;
  onClose?: (info: { code: number; reason: string; clean: boolean }) => void;
  onError?: (error: Error) => void;

  #open = false;
  #closed = false;
  /** Reject sends, the way a socket that is already dead does. */
  sendFails = false;

  constructor(url: string, protocols: string[] = []) {
    this.url = url;
    this.protocols = protocols;
  }

  get isOpen(): boolean {
    return this.#open;
  }

  get authHeader(): string | undefined {
    return this.protocols.find((protocol) => protocol.startsWith("relay.auth."));
  }

  send(data: string): void {
    if (this.sendFails) throw new Error("mock socket: send failed");
    this.sent.push(data);
  }

  close(code = 1000, reason = ""): void {
    if (this.#closed) return;
    this.#closed = true;
    this.#open = false;
    this.onClose?.({ code, reason, clean: code === 1000 });
  }

  // --- test controls --------------------------------------------------------

  /** The server accepted the upgrade. */
  acceptOpen(): void {
    if (this.#closed) return;
    this.#open = true;
    this.onOpen?.();
  }

  /** Deliver an inbound frame exactly as the wire would. */
  deliver(text: string): void {
    this.onMessage?.(text);
  }

  deliverEnvelope(envelope: Partial<RelaydEnvelope> & { type: string }): void {
    this.deliver(
      serialiseEnvelope({
        v: LINK_VERSION,
        id: envelope.id ?? `srv-${this.sent.length}-${envelope.type}`,
        type: envelope.type,
        at: envelope.at ?? 0,
        payload: envelope.payload ?? {},
      }),
    );
  }

  /** The tunnel died: no close frame, no flush. The normal phone case. */
  dropAbruptly(reason = "network lost"): void {
    if (this.#closed) return;
    this.#closed = true;
    this.#open = false;
    this.onClose?.({ code: 1006, reason, clean: false });
  }

  fail(message: string): void {
    this.onError?.(new Error(message));
  }
}

/** Hands back every socket it made, newest last. */
export function mockSocketFactory(): SocketFactory & { sockets: MockRelaydSocket[] } {
  const sockets: MockRelaydSocket[] = [];
  const create: SocketFactory = (url, protocols) => {
    const socket = new MockRelaydSocket(url, protocols);
    sockets.push(socket);
    return socket;
  };
  return Object.assign(create, { sockets });
}
