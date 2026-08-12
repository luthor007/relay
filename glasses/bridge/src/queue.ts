/**
 * Store-and-forward, and the nightly sync ritual that feeds it.
 *
 * `docs/SYSTEM.md` §2 puts a queue between the BLE bridge and the uplink, and
 * `docs/APPS-SCOPE.md` §4.2 says why: subway, plane, out of range. The rules that
 * follow are all consequences of one fact — for a memory product, the queue holds
 * the *only* copy of something that already happened.
 *
 * ## The eviction policy, stated
 *
 * | Situation | What happens | Why |
 * |---|---|---|
 * | Queue full | the **newest** record is refused, with a reason | dropping the oldest silently loses last Tuesday; a refusal is a thing the UI can honestly report |
 * | Record larger than the whole capacity | refused as `tooLarge` immediately | it will never fit, and pretending otherwise blocks the queue forever |
 * | Same id enqueued twice | accepted, not duplicated | a retry after a failed flush must not double-upload |
 * | Id already delivered | accepted as a no-op | replaying a sync must not re-upload the day |
 * | Flush hits an error | stops at that record, keeps order | skipping past a stuck record quietly reorders someone's day, and the box segments episodes by time |
 *
 * Nothing is ever dropped silently. Every refusal returns a reason *and* emits
 * `refused`, because the two callers that matter are a UI and a log.
 *
 * ## Parity
 *
 * `apps/android/.../connector/StoreAndForwardQueue.kt` and
 * `connector/src/client.ts` implement the same queue. Those four semantics —
 * idempotent enqueue, refuse-newest, FIFO, stop-at-first-failure — are identical
 * here on purpose, and `test/queue.test.ts` ports the Kotlin tests verbatim so the
 * two cannot drift. Two implementations of one queue that disagree is a bug that
 * only appears on a bad network, which is the worst place to find one.
 *
 * Two things this one adds, and the other two owe (`docs/APPS-SCOPE.md` §4.2):
 * durability across a crash, and delivered-id memory for replay dedupe.
 */

import type { Clock } from "./clock.ts";
import { systemClock } from "./clock.ts";
import { TypedEmitter } from "./emitter.ts";
import type { GlassesTransport } from "./transport.ts";
import type { RemoteFile, Unsubscribe, WifiAccessPoint } from "./types.ts";

const GIGABYTE = 1024 * 1024 * 1024;

/** 2 GB. Half the glasses' storage: enough for a day, bounded on the phone. */
export const DEFAULT_QUEUE_CAPACITY_BYTES = 2 * GIGABYTE;

/** Ids remembered after delivery. ~40 files a day, so this is weeks of replay cover. */
export const DEFAULT_DELIVERED_MEMORY = 1024;

// --- records ----------------------------------------------------------------

export interface QueueRecord {
  /**
   * Stable and derived from the source, not random — the device filename for a
   * pulled recording, the envelope id for a control message. Dedupe is only as
   * good as this being the same across a restart.
   */
  id: string;
  /** "audio" | "photo" | "envelope". Free-form; the box reads the meta. */
  kind: string;
  /**
   * Owned by the queue from `enqueue` onward — do not mutate it afterwards.
   * The store takes a durable copy, but the in-memory entry aliases what was
   * handed in, because copying a 10 MB recording twice on a phone is a real cost
   * and the caller has no reason to keep writing to it.
   */
  body: Uint8Array;
  /** Manifest fields the uploader needs. Must survive a JSON round trip. */
  meta?: Record<string, unknown>;
}

export interface StoredRecord extends QueueRecord {
  enqueuedAtMs: number;
  /** Monotonic. This, not insertion into an array, is what preserves order. */
  sequence: number;
}

/**
 * Durable backing for the queue.
 *
 * `docs/APPS-SCOPE.md` §4.5: a crash or restart must not lose the day's capture.
 * That makes persistence part of the contract rather than an optimisation, so it
 * is an interface with a platform implementation behind it — files under
 * `NSFileManager` / `Context.filesDir`, one per record plus an index.
 *
 * `append` must not resolve until the bytes are durable. Resolving early turns a
 * crash into silent data loss, which is exactly the case this exists to prevent.
 */
export interface QueueStore {
  load(): Promise<{ pending: StoredRecord[]; delivered: string[] }>;
  append(record: StoredRecord): Promise<void>;
  remove(id: string): Promise<void>;
  markDelivered(id: string): Promise<void>;
}

export interface MemoryQueueStoreFaults {
  /** Simulate a full or failing disk. Enqueue must surface it, not swallow it. */
  appendFails?: boolean;
}

/** In-memory store for tests and the simulator. Loses everything on restart. */
export class MemoryQueueStore implements QueueStore {
  #pending = new Map<string, StoredRecord>();
  #delivered: string[] = [];
  #faults: MemoryQueueStoreFaults;

  constructor(faults: MemoryQueueStoreFaults = {}) {
    this.#faults = faults;
  }

  setFaults(faults: MemoryQueueStoreFaults): void {
    this.#faults = { ...this.#faults, ...faults };
  }

  async load(): Promise<{ pending: StoredRecord[]; delivered: string[] }> {
    return {
      pending: [...this.#pending.values()].sort((a, b) => a.sequence - b.sequence),
      delivered: [...this.#delivered],
    };
  }

  async append(record: StoredRecord): Promise<void> {
    if (this.#faults.appendFails) throw new Error("store: append failed");
    this.#pending.set(record.id, { ...record, body: record.body.slice() });
  }

  async remove(id: string): Promise<void> {
    this.#pending.delete(id);
  }

  async markDelivered(id: string): Promise<void> {
    this.#delivered.push(id);
  }

  /** Survive a simulated restart by handing the same store to a new queue. */
  get persistedIds(): string[] {
    return [...this.#pending.keys()];
  }
}

// --- results ----------------------------------------------------------------

export const QueueRefusal = {
  /** Capacity reached. The newest record is the one refused. */
  Full: "full",
  /** More records than `capacityItems`. */
  ItemLimit: "itemLimit",
  /** Bigger than the entire capacity — it can never fit, so say so now. */
  TooLarge: "tooLarge",
  /** The disk said no. Never swallowed: the caller must not delete its source. */
  StoreFailed: "storeFailed",
} as const;

export type QueueRefusal = (typeof QueueRefusal)[keyof typeof QueueRefusal];

export interface EnqueueResult {
  accepted: boolean;
  /** Already pending under this id — accepted, not duplicated. */
  duplicate?: boolean;
  /** Already delivered in a previous run — accepted as a no-op. */
  alreadyDelivered?: boolean;
  reason?: QueueRefusal;
  message?: string;
}

export interface FlushResult {
  sent: number;
  remaining: number;
  /** Present when the flush stopped early. The queue is intact and ordered. */
  error?: Error;
}

export interface QueueEvents {
  enqueued: StoredRecord;
  /** Never silent. Both a UI and a log want this. */
  refused: { record: QueueRecord; reason: QueueRefusal; message: string };
  sent: StoredRecord;
  flushFailed: { record: StoredRecord; error: Error };
  /** How much survived a restart. */
  restored: { records: number; bytes: number };
}

export interface QueueOptions {
  store?: QueueStore;
  clock?: Clock;
  capacityBytes?: number;
  capacityItems?: number;
  /** Ids kept after delivery, for replay dedupe. 0 disables. */
  deliveredMemory?: number;
}

export type QueueSend = (record: StoredRecord) => Promise<void>;

/**
 * Holds capture while the box is unreachable.
 *
 * Construct with `open()` rather than `new`: restoring what a previous run left on
 * disk is not optional, and a constructor cannot await.
 */
export class StoreAndForwardQueue {
  #emitter = new TypedEmitter<QueueEvents>();
  #store: QueueStore;
  #clock: Clock;
  #capacityBytes: number;
  #capacityItems: number;
  #deliveredMemory: number;

  #pending: StoredRecord[] = [];
  #delivered: string[] = [];
  #deliveredSet = new Set<string>();
  #sequence = 0;

  constructor(options: QueueOptions = {}) {
    this.#store = options.store ?? new MemoryQueueStore();
    this.#clock = options.clock ?? systemClock;
    this.#capacityBytes = options.capacityBytes ?? DEFAULT_QUEUE_CAPACITY_BYTES;
    this.#capacityItems = options.capacityItems ?? Number.POSITIVE_INFINITY;
    this.#deliveredMemory = options.deliveredMemory ?? DEFAULT_DELIVERED_MEMORY;
  }

  /** Build and restore in one step. */
  static async open(options: QueueOptions = {}): Promise<StoreAndForwardQueue> {
    const queue = new StoreAndForwardQueue(options);
    await queue.restore();
    return queue;
  }

  /**
   * Reload whatever the last run left behind.
   *
   * This is the whole of `docs/APPS-SCOPE.md` §4.5 from the queue's side: the app
   * being killed mid-day costs nothing but the in-flight record, which is re-pulled
   * because the glasses still have it.
   */
  async restore(): Promise<void> {
    const { pending, delivered } = await this.#store.load();
    this.#delivered = delivered.slice(-this.#deliveredMemory);
    this.#deliveredSet = new Set(this.#delivered);
    // A record marked delivered but not yet removed is the crash window in
    // `flush`. Trusting the pending list alone would re-upload it every restart.
    this.#pending = [...pending]
      .filter((record) => !this.#deliveredSet.has(record.id))
      .sort((a, b) => a.sequence - b.sequence);
    this.#sequence = pending.reduce((max, r) => Math.max(max, r.sequence + 1), 0);
    this.#emitter.emit("restored", { records: this.#pending.length, bytes: this.usedBytes });
  }

  on<K extends keyof QueueEvents>(
    event: K,
    handler: (payload: QueueEvents[K]) => void,
  ): Unsubscribe {
    return this.#emitter.on(event, handler);
  }

  get size(): number {
    return this.#pending.length;
  }

  get usedBytes(): number {
    return this.#pending.reduce((total, record) => total + record.body.byteLength, 0);
  }

  get capacityBytes(): number {
    return this.#capacityBytes;
  }

  get freeBytes(): number {
    return Math.max(0, this.#capacityBytes - this.usedBytes);
  }

  /** In order. Named `ids` where the Kotlin says `sessionIds`; same list. */
  get ids(): string[] {
    return this.#pending.map((record) => record.id);
  }

  get deliveredIds(): string[] {
    return [...this.#delivered];
  }

  has(id: string): boolean {
    return this.#pending.some((record) => record.id === id);
  }

  /**
   * Accept a record, or refuse it with a reason.
   *
   * Async only because durability is: the record is on disk before this resolves,
   * so a caller may delete its source the moment it does. The Kotlin and connector
   * versions are synchronous and in-memory; every accept/refuse decision here
   * matches them exactly.
   */
  async enqueue(record: QueueRecord): Promise<EnqueueResult> {
    if (this.#deliveredSet.has(record.id)) {
      return { accepted: true, alreadyDelivered: true };
    }
    if (this.has(record.id)) {
      return { accepted: true, duplicate: true };
    }

    const size = record.body.byteLength;
    if (size > this.#capacityBytes) {
      return this.#refuse(
        record,
        QueueRefusal.TooLarge,
        `record is ${size} bytes and the queue holds ${this.#capacityBytes}`,
      );
    }
    if (this.usedBytes + size > this.#capacityBytes) {
      return this.#refuse(
        record,
        QueueRefusal.Full,
        `queue full: ${this.usedBytes} + ${size} exceeds ${this.#capacityBytes}`,
      );
    }
    if (this.#pending.length + 1 > this.#capacityItems) {
      return this.#refuse(
        record,
        QueueRefusal.ItemLimit,
        `queue full: ${this.#pending.length} records is the limit`,
      );
    }

    const stored: StoredRecord = {
      ...record,
      enqueuedAtMs: this.#clock.now(),
      sequence: this.#sequence++,
    };

    try {
      await this.#store.append(stored);
    } catch (error) {
      // The caller still owns its bytes. Saying "accepted" here is how a day of
      // audio gets deleted off the glasses and never arrives.
      return this.#refuse(record, QueueRefusal.StoreFailed, String(error));
    }

    this.#pending.push(stored);
    this.#emitter.emit("enqueued", stored);
    return { accepted: true };
  }

  /**
   * Send everything, oldest first, stopping at the first failure.
   *
   * Stopping rather than skipping is deliberate: the box segments episodes by
   * time, so a queue that steps over a stuck record reorders someone's day.
   */
  async flush(send: QueueSend): Promise<FlushResult> {
    let sent = 0;
    while (this.#pending.length > 0) {
      const next = this.#pending[0]!;
      try {
        await send(next);
      } catch (error) {
        const failure = error instanceof Error ? error : new Error(String(error));
        this.#emitter.emit("flushFailed", { record: next, error: failure });
        return { sent, remaining: this.#pending.length, error: failure };
      }
      // Mark delivered *before* removing. A crash between the two leaves a
      // record that is both pending and delivered, which `restore` resolves in
      // favour of delivered — the alternative order loses that and re-uploads.
      await this.#remember(next.id);
      await this.#store.remove(next.id);
      this.#pending.shift();
      this.#emitter.emit("sent", next);
      sent += 1;
    }
    return { sent, remaining: 0 };
  }

  #refuse(record: QueueRecord, reason: QueueRefusal, message: string): EnqueueResult {
    this.#emitter.emit("refused", { record, reason, message });
    return { accepted: false, reason, message };
  }

  async #remember(id: string): Promise<void> {
    if (this.#deliveredMemory <= 0) return;
    this.#delivered.push(id);
    this.#deliveredSet.add(id);
    while (this.#delivered.length > this.#deliveredMemory) {
      const dropped = this.#delivered.shift();
      if (dropped !== undefined) this.#deliveredSet.delete(dropped);
    }
    await this.#store.markDelivered(id);
  }
}

// --- the nightly ritual -----------------------------------------------------
//
// This lives next to the queue because it is the thing that fills it, and because
// the constraint it exists to encode is the same one: bytes that already happened
// must not be lost between the glasses and the box.
//
// `docs/ARCHITECTURE.md` §2.1 and §5.3, `docs/APPS-SCOPE.md` §3.1, `docs/SYSTEM.md`
// §7. Three facts drive the whole state machine:
//
//   1. A day of audio cannot ride BLE — ~173 MB at ~3 KB/s is ~16 hours, longer
//      than the day took to record. It goes over the glasses' own access point.
//   2. The phone cannot hold the glasses' AP and its own uplink at once. So this
//      is two phases with a network change between them, not one transfer.
//   3. The day's audio never crosses the rendezvous relay. If the box is not on
//      the same LAN, sync **waits and says so**, rather than spending someone's
//      cellular plan on a gigabyte.

export const BoxReachability = {
  /** Same LAN. The only state in which bulk sync moves. */
  Lan: "lan",
  /** Through our rendezvous relay: control traffic only, never the day's audio. */
  Relay: "relay",
  None: "none",
} as const;

export type BoxReachability = (typeof BoxReachability)[keyof typeof BoxReachability];

/**
 * The phone's radios, injected.
 *
 * `joinAccessPoint` costs the uplink. An implementation that could hold both would
 * be a different product; this interface exists so the state machine cannot
 * accidentally assume one.
 */
export interface SyncNetwork {
  reachBox(): Promise<BoxReachability>;
  joinAccessPoint(ap: WifiAccessPoint): Promise<void>;
  leaveAccessPoint(): Promise<void>;
}

export const SyncPhase = {
  Idle: "idle",
  /** Stopped before doing anything. The `deferred` event carries the reason. */
  Waiting: "waiting",
  OpeningAccessPoint: "openingAccessPoint",
  /** Uplink lost from here until `RejoiningUplink` completes. */
  JoiningAccessPoint: "joiningAccessPoint",
  PullingFiles: "pullingFiles",
  LeavingAccessPoint: "leavingAccessPoint",
  RejoiningUplink: "rejoiningUplink",
  Uploading: "uploading",
  Done: "done",
  Failed: "failed",
} as const;

export type SyncPhase = (typeof SyncPhase)[keyof typeof SyncPhase];

export const SyncDeferral = {
  NotCharging: "notCharging",
  /** Reachable, but only through the relay. Bulk waits for the LAN. */
  BoxOnlyViaRelay: "boxOnlyViaRelay",
  BoxUnreachable: "boxUnreachable",
  NotConnected: "notConnected",
  NothingToSync: "nothingToSync",
} as const;

export type SyncDeferral = (typeof SyncDeferral)[keyof typeof SyncDeferral];

export interface SyncResult {
  phase: SyncPhase;
  filesPulled: number;
  bytesPulled: number;
  uploaded: number;
  remaining: number;
  /** Set when the run stopped for a reason the user should be told. */
  deferred?: SyncDeferral;
  error?: Error;
}

export interface SyncEvents {
  phaseChanged: SyncPhase;
  filePulled: { file: RemoteFile; queued: EnqueueResult };
  pullProgress: { name: string; receivedBytes: number; totalBytes: number };
  /** The "it is waiting, and here is why" event. Wire it to the UI. */
  deferred: { reason: SyncDeferral; message: string };
  failed: { phase: SyncPhase; error: Error };
  finished: SyncResult;
}

export interface BulkSyncOptions {
  glasses: GlassesTransport;
  queue: StoreAndForwardQueue;
  network: SyncNetwork;
  upload: QueueSend;
  clock?: Clock;
  /**
   * Sync only while the glasses are charging. The natural trigger: both devices
   * stationary, the user asleep, and nothing competing for the radio.
   */
  requireCharging?: boolean;
  /** How long to wait for the uplink to come back after leaving the AP. */
  rejoinTimeoutMs?: number;
  rejoinPollMs?: number;
}

/**
 * The two-phase WiFi sync, as an explicit state machine.
 *
 * Explicit because the interesting states are the ones where nothing is
 * transferring: waiting for the LAN, holding the AP with no uplink, and rejoining.
 * A design that models sync as one `await` has nowhere to put those, and ends up
 * discovering them as bugs.
 *
 * It never deletes anything from the glasses. The firmware tracks un-uploaded
 * files itself (`0x0911`), and deleting on the strength of our own bookkeeping is
 * how the only copy disappears.
 */
export class BulkSync {
  #emitter = new TypedEmitter<SyncEvents>();
  #glasses: GlassesTransport;
  #queue: StoreAndForwardQueue;
  #network: SyncNetwork;
  #upload: QueueSend;
  #clock: Clock;
  #requireCharging: boolean;
  #rejoinTimeoutMs: number;
  #rejoinPollMs: number;

  #phase: SyncPhase = SyncPhase.Idle;
  #holdingAp = false;

  constructor(options: BulkSyncOptions) {
    this.#glasses = options.glasses;
    this.#queue = options.queue;
    this.#network = options.network;
    this.#upload = options.upload;
    this.#clock = options.clock ?? systemClock;
    this.#requireCharging = options.requireCharging ?? true;
    this.#rejoinTimeoutMs = options.rejoinTimeoutMs ?? 60_000;
    this.#rejoinPollMs = options.rejoinPollMs ?? 2_000;
  }

  get phase(): SyncPhase {
    return this.#phase;
  }

  /**
   * False exactly while the phone is on the glasses' network.
   *
   * `docs/ARCHITECTURE.md` §2.1: joining the glasses' AP costs the phone its WiFi
   * uplink. Anything that wants the box — including the relayd link — has to check
   * this rather than discover it as a timeout.
   */
  get uplinkAvailable(): boolean {
    return !this.#holdingAp;
  }

  on<K extends keyof SyncEvents>(event: K, handler: (payload: SyncEvents[K]) => void): Unsubscribe {
    return this.#emitter.on(event, handler);
  }

  async run(): Promise<SyncResult> {
    let filesPulled = 0;
    let bytesPulled = 0;
    this.#setPhase(SyncPhase.Idle);

    // --- preconditions, before anything costs a radio ------------------------
    const precondition = await this.#checkPreconditions();
    if (precondition) return this.#defer(precondition.reason, precondition.message);

    // `listFiles` is a BLE command and costs nothing, so ask before paying for a
    // network change. A run with nothing new to pull and nothing left over is a
    // run that should not have touched the WiFi radio at all.
    let outstanding: RemoteFile[];
    try {
      outstanding = (await this.#glasses.listFiles()).filter(
        (file) =>
          !file.uploaded &&
          !this.#queue.has(file.name) &&
          !this.#queue.deliveredIds.includes(file.name),
      );
    } catch (error) {
      return this.#fail(error);
    }

    if (outstanding.length === 0 && this.#queue.size === 0) {
      return this.#defer(SyncDeferral.NothingToSync, "nothing on the glasses that the box lacks");
    }

    // --- phase 1: the glasses' network, with no uplink -----------------------
    if (outstanding.length > 0) {
      try {
        this.#setPhase(SyncPhase.OpeningAccessPoint);
        const ap = await this.#glasses.openWifiAccessPoint();
        this.#setPhase(SyncPhase.JoiningAccessPoint);
        await this.#network.joinAccessPoint(ap);
        this.#holdingAp = true;
      } catch (error) {
        return this.#fail(error);
      }

      try {
        this.#setPhase(SyncPhase.PullingFiles);
        for (const file of outstanding) {
          const body = await this.#glasses.fetchFile(file.name, (progress) => {
            this.#emitter.emit("pullProgress", {
              name: file.name,
              receivedBytes: progress.receivedBytes,
              totalBytes: progress.totalBytes,
            });
          });

          const queued = await this.#queue.enqueue({
            id: file.name,
            kind: file.name.endsWith(".jpg") ? "photo" : "audio",
            body,
            meta: {
              sourceName: file.name,
              sizeBytes: file.sizeBytes,
              durationS: file.durationS,
              pulledAtMs: this.#clock.now(),
            },
          });
          this.#emitter.emit("filePulled", { file, queued });

          if (!queued.accepted) {
            // The phone is out of room. Stop pulling — the glasses still hold the
            // file, which is the correct place for it to be.
            break;
          }
          filesPulled += 1;
          bytesPulled += body.byteLength;
        }
      } catch (error) {
        // Always release the AP first: a phone left on the glasses' network with
        // no uplink is worse than a failed sync.
        const failedIn = this.#phase;
        await this.#releaseAccessPoint();
        return this.#fail(error, failedIn);
      }

      await this.#releaseAccessPoint();
    }

    // --- phase 2: back on the real network -----------------------------------
    try {
      this.#setPhase(SyncPhase.RejoiningUplink);
      if (!(await this.#waitForLan())) {
        // The bytes are safe in the queue; the upload is simply not now.
        return this.#defer(
          SyncDeferral.BoxUnreachable,
          "pulled the day, but the box is not on this network yet — the upload stays queued",
          { filesPulled, bytesPulled },
        );
      }

      this.#setPhase(SyncPhase.Uploading);
      const flushed = await this.#queue.flush(this.#upload);
      if (flushed.error) {
        return this.#fail(flushed.error, SyncPhase.Uploading, {
          filesPulled,
          bytesPulled,
          uploaded: flushed.sent,
          remaining: flushed.remaining,
        });
      }

      this.#setPhase(SyncPhase.Done);
      return this.#finish({
        phase: SyncPhase.Done,
        filesPulled,
        bytesPulled,
        uploaded: flushed.sent,
        remaining: 0,
      });
    } catch (error) {
      return this.#fail(error);
    }
  }

  async #checkPreconditions(): Promise<{ reason: SyncDeferral; message: string } | null> {
    try {
      if (this.#requireCharging) {
        const battery = await this.#glasses.getBattery();
        if (!battery.charging) {
          return {
            reason: SyncDeferral.NotCharging,
            message: "waiting until the glasses are charging — sync is a plugged-in ritual",
          };
        }
      }
    } catch (error) {
      return {
        reason: SyncDeferral.NotConnected,
        message: `glasses not reachable: ${String(error)}`,
      };
    }

    const reach = await this.#network.reachBox();
    if (reach === BoxReachability.Relay) {
      return {
        reason: SyncDeferral.BoxOnlyViaRelay,
        message:
          "the box is only reachable through the relay — the day's audio waits for home WiFi " +
          "rather than riding your data plan",
      };
    }
    if (reach === BoxReachability.None) {
      return { reason: SyncDeferral.BoxUnreachable, message: "the box is not reachable" };
    }
    return null;
  }

  async #waitForLan(): Promise<boolean> {
    const deadline = this.#clock.now() + this.#rejoinTimeoutMs;
    for (;;) {
      if ((await this.#network.reachBox()) === BoxReachability.Lan) return true;
      if (this.#clock.now() >= deadline) return false;
      await this.#clock.sleep(this.#rejoinPollMs);
    }
  }

  async #releaseAccessPoint(): Promise<void> {
    if (!this.#holdingAp) return;
    this.#setPhase(SyncPhase.LeavingAccessPoint);
    this.#holdingAp = false;
    try {
      await this.#network.leaveAccessPoint();
    } finally {
      // Closing the device AP is best-effort: the phone has already left it, and
      // a device that keeps its hotspot up costs battery but loses nothing.
      await this.#glasses.closeWifiAccessPoint().catch(() => {});
    }
  }

  #setPhase(phase: SyncPhase): void {
    if (this.#phase === phase) return;
    this.#phase = phase;
    this.#emitter.emit("phaseChanged", phase);
  }

  #defer(
    reason: SyncDeferral,
    message: string,
    counts: { filesPulled: number; bytesPulled: number } = { filesPulled: 0, bytesPulled: 0 },
  ): SyncResult {
    this.#setPhase(SyncPhase.Waiting);
    this.#emitter.emit("deferred", { reason, message });
    return this.#finish({
      phase: SyncPhase.Waiting,
      filesPulled: counts.filesPulled,
      bytesPulled: counts.bytesPulled,
      uploaded: 0,
      remaining: this.#queue.size,
      deferred: reason,
    });
  }

  #fail(
    error: unknown,
    failedIn: SyncPhase = this.#phase,
    counts: Pick<SyncResult, "filesPulled" | "bytesPulled" | "uploaded" | "remaining"> = {
      filesPulled: 0,
      bytesPulled: 0,
      uploaded: 0,
      remaining: this.#queue.size,
    },
  ): SyncResult {
    const failure = error instanceof Error ? error : new Error(String(error));
    this.#setPhase(SyncPhase.Failed);
    this.#emitter.emit("failed", { phase: failedIn, error: failure });
    return this.#finish({ phase: SyncPhase.Failed, ...counts, error: failure });
  }

  #finish(result: SyncResult): SyncResult {
    this.#emitter.emit("finished", result);
    return result;
  }
}

/**
 * A phone's radios, faked — including the part where it only has one.
 *
 * `reachBox` throws while the glasses' access point is held, because that is what
 * the hardware does (`docs/ARCHITECTURE.md` §2.1) and a mock that quietly answered
 * would let the state machine grow a dependency the real phone cannot satisfy.
 */
export class MockSyncNetwork implements SyncNetwork {
  #reachability: BoxReachability;
  #holdingAp = false;
  joined: WifiAccessPoint[] = [];

  constructor(reachability: BoxReachability = BoxReachability.Lan) {
    this.#reachability = reachability;
  }

  get holdingAccessPoint(): boolean {
    return this.#holdingAp;
  }

  setReachability(reachability: BoxReachability): void {
    this.#reachability = reachability;
  }

  async reachBox(): Promise<BoxReachability> {
    if (this.#holdingAp) {
      throw new Error("the phone cannot reach the box while it holds the glasses' access point");
    }
    return this.#reachability;
  }

  async joinAccessPoint(ap: WifiAccessPoint): Promise<void> {
    this.#holdingAp = true;
    this.joined.push(ap);
  }

  async leaveAccessPoint(): Promise<void> {
    this.#holdingAp = false;
  }
}
