import assert from "node:assert/strict";
import { describe, test } from "node:test";

import { FakeClock } from "../src/clock.ts";
import { MOCK_DEFAULTS, MockTransport } from "../src/mock.ts";
import {
  BoxReachability,
  BulkSync,
  MemoryQueueStore,
  MockSyncNetwork,
  StoreAndForwardQueue,
  SyncDeferral,
  SyncPhase,
} from "../src/queue.ts";
import type { EnqueueResult, QueueRecord, StoredRecord, SyncResult } from "../src/queue.ts";
import { countingRandom } from "../src/pairing.ts";
import { PhoneMessage, RelaydLink, mockSocketFactory, parseEnvelope } from "../src/relayd.ts";

function record(id: string, bytes: number, kind = "audio"): QueueRecord {
  return { id, kind, body: new Uint8Array(bytes), meta: { sourceName: id } };
}

async function connectedGlasses(options: ConstructorParameters<typeof MockTransport>[0] = {}) {
  const clock = new FakeClock();
  const glasses = new MockTransport({ clock, charging: true, ...options });
  const pending = glasses.connect();
  await clock.advance(MOCK_DEFAULTS.connectDelayMs);
  await pending;
  return { glasses, clock };
}

/** A day on the glasses: an hour of audio and a photo, ready to sync. */
async function glassesWithADay(options: ConstructorParameters<typeof MockTransport>[0] = {}) {
  const ctx = await connectedGlasses(options);
  await ctx.glasses.startLocalRecording();
  await ctx.clock.advance(60 * 60 * 1000);
  await ctx.glasses.stopLocalRecording();
  await ctx.glasses.capturePhoto();
  return ctx;
}

// ---------------------------------------------------------------------------
// The three Kotlin tests, ported verbatim.
//
// `apps/android/.../connector/StoreAndForwardQueueTest.kt` owns these assertions.
// They are here in the same words so that a change to one implementation fails
// the other, which is the only defence against two queues that disagree only on
// a bad network.

describe("parity with the Kotlin queue", () => {
  test("queueing the same session twice does not duplicate it", async () => {
    const queue = new StoreAndForwardQueue();
    assert.equal((await queue.enqueue(record("a", 128))).accepted, true);
    assert.equal((await queue.enqueue(record("a", 128))).accepted, true);
    assert.equal(queue.size, 1);
  });

  test("a full queue refuses new sessions rather than evicting old ones", async () => {
    // Dropping the oldest would silently lose last Tuesday, which for a memory
    // product is the worst available failure. Refusing is visible.
    const queue = new StoreAndForwardQueue({ capacityBytes: 1000 });

    assert.equal((await queue.enqueue(record("keep", 800))).accepted, true);
    assert.equal((await queue.enqueue(record("drop", 800))).accepted, false);

    assert.deepEqual(queue.ids, ["keep"]);
  });

  test("used bytes tracks what is actually held", async () => {
    const queue = new StoreAndForwardQueue();
    await queue.enqueue(record("a", 100));
    await queue.enqueue(record("b", 250));
    assert.equal(queue.usedBytes, 350);
  });

  test("order is preserved, because the box segments episodes by time", async () => {
    const queue = new StoreAndForwardQueue();
    await queue.enqueue(record("first", 10));
    await queue.enqueue(record("second", 10));
    await queue.enqueue(record("third", 10));
    assert.deepEqual(queue.ids, ["first", "second", "third"]);
  });
});

describe("the eviction policy, stated", () => {
  test("a refusal names its reason and is announced, never silent", async () => {
    const queue = new StoreAndForwardQueue({ capacityBytes: 1000 });
    const refusals: string[] = [];
    queue.on("refused", (r) => refusals.push(r.reason));

    await queue.enqueue(record("keep", 900));
    const result = await queue.enqueue(record("drop", 200));

    assert.equal(result.accepted, false);
    assert.equal(result.reason, "full");
    assert.ok(result.message?.includes("queue full"));
    assert.deepEqual(refusals, ["full"]);
  });

  test("a record larger than the whole queue is refused as tooLarge, not full", async () => {
    // It can never fit, so retrying it forever would block everything behind it.
    const queue = new StoreAndForwardQueue({ capacityBytes: 1000 });
    const result = await queue.enqueue(record("huge", 5000));
    assert.equal(result.reason, "tooLarge");
  });

  test("an item limit is enforced as well as a byte limit", async () => {
    const queue = new StoreAndForwardQueue({ capacityItems: 2 });
    await queue.enqueue(record("a", 1));
    await queue.enqueue(record("b", 1));
    assert.equal((await queue.enqueue(record("c", 1))).reason, "itemLimit");
  });

  test("a store that fails refuses the record instead of pretending", async () => {
    // Saying "accepted" here is how a day of audio gets deleted off the glasses
    // and never arrives.
    const store = new MemoryQueueStore({ appendFails: true });
    const queue = new StoreAndForwardQueue({ store });

    const result = await queue.enqueue(record("a", 10));
    assert.equal(result.accepted, false);
    assert.equal(result.reason, "storeFailed");
    assert.equal(queue.size, 0);
  });
});

describe("flushing", () => {
  test("sends oldest first and empties the queue", async () => {
    const queue = new StoreAndForwardQueue();
    await queue.enqueue(record("a", 10));
    await queue.enqueue(record("b", 10));
    await queue.enqueue(record("c", 10));

    const sent: string[] = [];
    const result = await queue.flush(async (r) => {
      sent.push(r.id);
    });

    assert.deepEqual(sent, ["a", "b", "c"]);
    assert.deepEqual(result, { sent: 3, remaining: 0 });
    assert.equal(queue.size, 0);
  });

  test("stops at the first failure and keeps the rest in order", async () => {
    const queue = new StoreAndForwardQueue();
    await queue.enqueue(record("a", 10));
    await queue.enqueue(record("b", 10));
    await queue.enqueue(record("c", 10));

    const result = await queue.flush(async (r) => {
      if (r.id === "b") throw new Error("subway");
    });

    assert.equal(result.sent, 1);
    assert.equal(result.remaining, 2);
    assert.equal(result.error?.message, "subway");
    assert.deepEqual(queue.ids, ["b", "c"], "skipping past b would reorder someone's day");
  });

  test("a retry after a failure resumes where it stopped", async () => {
    const queue = new StoreAndForwardQueue();
    await queue.enqueue(record("a", 10));
    await queue.enqueue(record("b", 10));

    let allow = false;
    await queue.flush(async (r) => {
      if (r.id === "b" && !allow) throw new Error("still underground");
    });
    allow = true;
    const second = await queue.flush(async () => {});

    assert.equal(second.sent, 1);
    assert.equal(queue.size, 0);
  });

  test("re-queuing after a failed flush does not duplicate", async () => {
    const queue = new StoreAndForwardQueue();
    await queue.enqueue(record("a", 10));
    await queue.flush(async () => {
      throw new Error("no uplink");
    });

    const again = await queue.enqueue(record("a", 10));
    assert.equal(again.accepted, true);
    assert.equal(again.duplicate, true);
    assert.equal(queue.size, 1);
  });
});

describe("dedupe on replay", () => {
  test("a delivered id is not re-uploaded when the source is offered again", async () => {
    const queue = new StoreAndForwardQueue();
    await queue.enqueue(record("REC_0001.opus", 10));
    await queue.flush(async () => {});

    const replay = await queue.enqueue(record("REC_0001.opus", 10));
    assert.equal(replay.accepted, true);
    assert.equal(replay.alreadyDelivered, true);
    assert.equal(queue.size, 0, "a replayed sync must not send the day twice");
  });

  test("delivered memory is bounded, so it cannot grow without limit", async () => {
    const queue = new StoreAndForwardQueue({ deliveredMemory: 2 });
    for (const id of ["a", "b", "c"]) {
      await queue.enqueue(record(id, 1));
      await queue.flush(async () => {});
    }
    assert.deepEqual(queue.deliveredIds, ["b", "c"]);
    assert.equal((await queue.enqueue(record("a", 1))).alreadyDelivered, undefined);
  });
});

describe("a crash does not lose the day", () => {
  test("what was enqueued survives a restart, in order", async () => {
    // docs/APPS-SCOPE.md §4.5. The app being killed mid-day is the normal case
    // on iOS, not an exception worth a footnote.
    const store = new MemoryQueueStore();
    const first = await StoreAndForwardQueue.open({ store });
    await first.enqueue(record("REC_0001.opus", 100));
    await first.enqueue(record("REC_0002.opus", 200));
    await first.enqueue(record("IMG_0001.jpg", 50));

    const afterCrash = await StoreAndForwardQueue.open({ store });

    assert.deepEqual(afterCrash.ids, ["REC_0001.opus", "REC_0002.opus", "IMG_0001.jpg"]);
    assert.equal(afterCrash.usedBytes, 350);
  });

  test("a record already sent before the crash is not sent again", async () => {
    const store = new MemoryQueueStore();
    const first = await StoreAndForwardQueue.open({ store });
    await first.enqueue(record("REC_0001.opus", 100));
    await first.enqueue(record("REC_0002.opus", 100));
    await first.flush(async (r) => {
      if (r.id === "REC_0002.opus") throw new Error("uplink died");
    });

    const afterCrash = await StoreAndForwardQueue.open({ store });
    const sent: string[] = [];
    await afterCrash.flush(async (r) => {
      sent.push(r.id);
    });

    assert.deepEqual(sent, ["REC_0002.opus"]);
    assert.equal((await afterCrash.enqueue(record("REC_0001.opus", 100))).alreadyDelivered, true);
  });

  test("a crash between 'delivered' and 'removed' does not re-upload", async () => {
    // flush marks delivered before removing, so this window exists on purpose;
    // restore has to resolve it in favour of delivered.
    const store = new MemoryQueueStore();
    const first = await StoreAndForwardQueue.open({ store });
    await first.enqueue(record("REC_0001.opus", 100));
    await first.flush(async () => {});
    // Put the record back on disk without clearing the delivered mark — exactly
    // what a crash mid-flush leaves behind.
    await store.append({
      ...record("REC_0001.opus", 100),
      body: new Uint8Array(100),
      enqueuedAtMs: 0,
      sequence: 0,
    });

    const afterCrash = await StoreAndForwardQueue.open({ store });
    const sent: string[] = [];
    await afterCrash.flush(async (r) => {
      sent.push(r.id);
    });

    assert.deepEqual(sent, []);
    assert.equal(afterCrash.size, 0);
  });

  test("restoring reports what it found", async () => {
    const store = new MemoryQueueStore();
    const first = await StoreAndForwardQueue.open({ store });
    await first.enqueue(record("a", 64));

    const second = new StoreAndForwardQueue({ store });
    const seen: Array<{ records: number; bytes: number }> = [];
    second.on("restored", (r) => seen.push(r));
    await second.restore();

    assert.deepEqual(seen, [{ records: 1, bytes: 64 }]);
  });

  test("the queue keeps its own copy of the bytes", async () => {
    const store = new MemoryQueueStore();
    const queue = await StoreAndForwardQueue.open({ store });
    const body = new Uint8Array([1, 2, 3]);
    await queue.enqueue({ id: "a", kind: "audio", body });
    body[0] = 99;

    const restored = await StoreAndForwardQueue.open({ store });
    const held: StoredRecord[] = [];
    await restored.flush(async (r) => {
      held.push(r);
    });
    assert.deepEqual([...held[0]!.body], [1, 2, 3]);
  });
});

describe("timestamps", () => {
  test("records carry the clock they were enqueued on", async () => {
    const clock = new FakeClock(1_700_000_000_000);
    const queue = new StoreAndForwardQueue({ clock });
    await queue.enqueue(record("a", 1));
    await clock.advance(5_000);
    await queue.enqueue(record("b", 1));

    const stamps: number[] = [];
    await queue.flush(async (r) => {
      stamps.push(r.enqueuedAtMs);
    });
    assert.deepEqual(stamps, [1_700_000_000_000, 1_700_000_005_000]);
  });
});

// ---------------------------------------------------------------------------

describe("the nightly sync ritual", () => {
  async function syncSetup(
    options: {
      reachability?: BoxReachability;
      requireCharging?: boolean;
      charging?: boolean;
      capacityBytes?: number;
      uploadFails?: (id: string) => boolean;
    } = {},
  ) {
    const { glasses, clock } = await glassesWithADay({ charging: options.charging ?? true });
    const queue = new StoreAndForwardQueue({
      clock,
      ...(options.capacityBytes === undefined ? {} : { capacityBytes: options.capacityBytes }),
    });
    const network = new MockSyncNetwork(options.reachability ?? BoxReachability.Lan);
    const uploaded: string[] = [];
    const sync = new BulkSync({
      glasses,
      queue,
      network,
      clock,
      requireCharging: options.requireCharging ?? true,
      upload: async (r) => {
        if (options.uploadFails?.(r.id)) throw new Error("box said no");
        uploaded.push(r.id);
      },
    });
    return { glasses, clock, queue, network, sync, uploaded };
  }

  /** Sync is a long operation on a fake clock; drive it to completion. */
  async function runSync(sync: BulkSync, clock: FakeClock): Promise<SyncResult> {
    let result: SyncResult | null = null;
    const pending = sync.run().then((r) => {
      result = r;
      return r;
    });
    await clock.advance(10 * 60 * 1000);
    await pending;
    assert.ok(result, "sync did not settle");
    return result;
  }

  test("pulls the day over WiFi and uploads it on the LAN", async () => {
    const { sync, clock, uploaded } = await syncSetup();
    const phases: SyncPhase[] = [];
    sync.on("phaseChanged", (p) => phases.push(p));

    const result = await runSync(sync, clock);

    assert.equal(result.phase, SyncPhase.Done);
    assert.equal(result.filesPulled, 2, "an hour of audio and a photo");
    assert.equal(result.uploaded, 2);
    assert.deepEqual(uploaded, ["REC_0001.opus", "IMG_0002.jpg"]);
    assert.deepEqual(phases, [
      SyncPhase.OpeningAccessPoint,
      SyncPhase.JoiningAccessPoint,
      SyncPhase.PullingFiles,
      SyncPhase.LeavingAccessPoint,
      SyncPhase.RejoiningUplink,
      SyncPhase.Uploading,
      SyncPhase.Done,
    ]);
  });

  test("it really is two phases — the uplink is gone while the AP is held", async () => {
    // docs/ARCHITECTURE.md §2.1: the phone cannot hold the glasses' network and
    // its own at the same time, so this is a network change, not a transfer.
    const { sync, clock, network } = await syncSetup();
    const uplinkDuringPull: boolean[] = [];
    const holdingDuringPull: boolean[] = [];

    sync.on("phaseChanged", (phase) => {
      if (phase === SyncPhase.PullingFiles) {
        uplinkDuringPull.push(sync.uplinkAvailable);
        holdingDuringPull.push(network.holdingAccessPoint);
      }
    });

    await runSync(sync, clock);

    assert.deepEqual(uplinkDuringPull, [false]);
    assert.deepEqual(holdingDuringPull, [true]);
    assert.equal(network.holdingAccessPoint, false, "the AP must be released before uploading");
    assert.equal(sync.uplinkAvailable, true);
  });

  test("the day's audio waits rather than riding the relay", async () => {
    // docs/SYSTEM.md §7: only live voice and control traffic relay. A gigabyte
    // over cellular is a bill, and the app should say so.
    const { sync, clock, queue } = await syncSetup({ reachability: BoxReachability.Relay });
    const deferrals: Array<{ reason: string; message: string }> = [];
    sync.on("deferred", (d) => deferrals.push(d));

    const result = await runSync(sync, clock);

    assert.equal(result.deferred, SyncDeferral.BoxOnlyViaRelay);
    assert.equal(result.phase, SyncPhase.Waiting);
    assert.equal(queue.size, 0, "nothing should have been pulled");
    assert.equal(deferrals.length, 1);
    assert.match(deferrals[0]!.message, /data plan/);
  });

  test("an unreachable box defers without touching a radio", async () => {
    const { sync, clock, network } = await syncSetup({ reachability: BoxReachability.None });
    const result = await runSync(sync, clock);

    assert.equal(result.deferred, SyncDeferral.BoxUnreachable);
    assert.deepEqual(network.joined, [], "never join the glasses' AP for a sync that cannot finish");
  });

  test("unplugged glasses defer — sync is a charging ritual", async () => {
    const { sync, clock } = await syncSetup({ charging: false });
    const result = await runSync(sync, clock);
    assert.equal(result.deferred, SyncDeferral.NotCharging);
  });

  test("requireCharging can be turned off for a manual sync", async () => {
    const { sync, clock } = await syncSetup({ charging: false, requireCharging: false });
    const result = await runSync(sync, clock);
    assert.equal(result.phase, SyncPhase.Done);
  });

  test("a run with nothing to do never opens the access point", async () => {
    const { sync, clock, network } = await syncSetup();
    await runSync(sync, clock);

    const second = await runSync(sync, clock);
    assert.equal(second.deferred, SyncDeferral.NothingToSync);
    assert.equal(network.joined.length, 1, "the second run should not have cost a network change");
  });

  test("bytes pulled but not uploaded stay in the queue and go next time", async () => {
    const { sync, clock, queue, network, uploaded } = await syncSetup();
    // The home WiFi drops while the phone is on the glasses' network.
    sync.on("phaseChanged", (phase) => {
      if (phase === SyncPhase.PullingFiles) network.setReachability(BoxReachability.None);
    });

    const first = await runSync(sync, clock);
    assert.equal(first.deferred, SyncDeferral.BoxUnreachable);
    assert.equal(first.filesPulled, 2);
    assert.equal(queue.size, 2, "the day is safe on the phone");
    assert.deepEqual(uploaded, []);

    network.setReachability(BoxReachability.Lan);
    const second = await runSync(sync, clock);
    assert.equal(second.phase, SyncPhase.Done);
    assert.equal(second.uploaded, 2);
    assert.equal(queue.size, 0);
  });

  test("a full phone stops pulling and leaves the file on the glasses", async () => {
    const { sync, clock, queue, glasses } = await syncSetup({ capacityBytes: 1000 });
    const refusals: EnqueueResult[] = [];
    sync.on("filePulled", ({ queued }) => {
      if (!queued.accepted) refusals.push(queued);
    });

    await runSync(sync, clock);

    assert.equal(refusals.length, 1);
    assert.equal(refusals[0]!.reason, "tooLarge");
    assert.equal(queue.size, 0);
    // The only copy is still where it was.
    assert.equal((await glasses.listFiles()).length, 2);
  });

  test("the access point is released even when the pull fails", async () => {
    const { sync, clock, network, glasses } = await syncSetup();
    sync.on("phaseChanged", (phase) => {
      if (phase === SyncPhase.PullingFiles) glasses.simulateDisconnect("cable pulled");
    });

    const result = await runSync(sync, clock);

    assert.equal(result.phase, SyncPhase.Failed);
    assert.equal(
      network.holdingAccessPoint,
      false,
      "a phone left on the glasses' network with no uplink is worse than a failed sync",
    );
  });

  test("an upload failure is reported with what did land", async () => {
    const { sync, clock, queue } = await syncSetup({
      uploadFails: (id) => id.endsWith(".jpg"),
    });
    const result = await runSync(sync, clock);

    assert.equal(result.phase, SyncPhase.Failed);
    assert.equal(result.uploaded, 1);
    assert.equal(result.remaining, 1);
    assert.deepEqual(queue.ids, ["IMG_0002.jpg"]);
  });

  test("progress is reported while pulling, monotonically", async () => {
    const { sync, clock } = await syncSetup();
    const seen: number[] = [];
    sync.on("pullProgress", (p) => {
      if (p.name.endsWith(".opus")) seen.push(p.receivedBytes);
    });

    await runSync(sync, clock);

    assert.ok(seen.length > 1, "a multi-megabyte pull must not be one silent jump");
    assert.deepEqual(seen, [...seen].sort((a, b) => a - b));
  });

  test("nothing is ever deleted from the glasses", async () => {
    const { sync, clock, glasses } = await syncSetup();
    await runSync(sync, clock);

    const files = await glasses.listFiles();
    assert.equal(files.length, 2, "the firmware tracks un-uploaded files; deletion is not ours");
    assert.deepEqual(
      files.map((f) => f.uploaded),
      [true, true],
    );
  });

  test("the mock network refuses to answer while it holds the glasses' AP", async () => {
    const network = new MockSyncNetwork(BoxReachability.Lan);
    await network.joinAccessPoint({ ssid: "QCGlasses-MOCK", password: "12345678", host: "1.2.3.4" });
    await assert.rejects(network.reachBox(), /cannot reach the box/);
  });

  test("the relayd link loses its socket during the pull and loses no work", async () => {
    // The two halves of this milestone meeting: joining the glasses' network
    // kills the uplink, so the link drops mid-sync. Anything the user says while
    // that is happening has to arrive afterwards, in order.
    const { sync, clock, network } = await syncSetup();
    const factory = mockSocketFactory();
    const link = new RelaydLink({
      url: "wss://relay.glass/v1/link",
      credential: {
        deviceId: "phone-abc",
        boxId: "box-mini-01",
        deviceToken: "a".repeat(64),
        signingKey: "b".repeat(64),
        protocolVersion: 1,
        pairedAtMs: 0,
      },
      socketFactory: factory,
      clock,
      random: countingRandom(3),
      backoff: { baseMs: 1_000, maxMs: 1_000, jitter: 0 },
    });

    link.connect();
    factory.sockets[0]!.acceptOpen();
    link.send(PhoneMessage.Utterance, { text: "before the sync" });

    sync.on("phaseChanged", (phase) => {
      if (phase === SyncPhase.JoiningAccessPoint) {
        factory.sockets.at(-1)!.dropAbruptly("joined the glasses' AP");
        link.send(PhoneMessage.Touch, { action: "doubleTap" });
      }
    });

    const result = await runSync(sync, clock);
    assert.equal(result.phase, SyncPhase.Done);
    assert.equal(network.holdingAccessPoint, false);

    link.wake();
    factory.sockets.at(-1)!.acceptOpen();

    const delivered = factory.sockets
      .at(-1)!
      .sent.map((text) => (parseEnvelope(text).payload as Record<string, unknown>));
    assert.deepEqual(delivered, [{ text: "before the sync" }, { action: "doubleTap" }]);
    assert.equal(link.pending, 2, "sent, but still unacknowledged — the server dedupes on id");
  });
});
