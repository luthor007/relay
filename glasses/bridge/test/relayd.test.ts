import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, test } from "node:test";
import { fileURLToPath } from "node:url";

import { FakeClock } from "../src/clock.ts";
import {
  PairingRole,
  SealedChannel,
  countingRandom,
  hexToBytes,
  linkSealingKey,
} from "../src/pairing.ts";
import type { DeviceCredential } from "../src/pairing.ts";
import {
  AUTH_WINDOW_MS,
  LINK_SUBPROTOCOL,
  LINK_VERSION,
  LinkError,
  LinkState,
  PhoneMessage,
  RelaydLink,
  ServerErrorCode,
  ServerMessage,
  authHeader,
  backoffMs,
  isProductServerMessage,
  isServerMessage,
  mockSocketFactory,
  parseEnvelope,
  serialiseEnvelope,
  verifyAuthHeader,
} from "../src/relayd.ts";
import type {
  AckFrame,
  ConfirmResolvedFrame,
  MockRelaydSocket,
  NotifyFrame,
  RelaydEnvelope,
  ServerErrorFrame,
} from "../src/relayd.ts";

const CREDENTIAL: DeviceCredential = {
  deviceId: "phone-abc",
  boxId: "box-mini-01",
  deviceToken: "a".repeat(64),
  signingKey: "b".repeat(64),
  protocolVersion: 1,
  pairedAtMs: 1_700_000_000_000,
};

interface Harness {
  link: RelaydLink;
  clock: FakeClock;
  factory: ReturnType<typeof mockSocketFactory>;
  socket(index?: number): MockRelaydSocket;
  errors: LinkError[];
}

function harness(options: Partial<ConstructorParameters<typeof RelaydLink>[0]> = {}): Harness {
  const clock = new FakeClock(1_700_000_000_000);
  const factory = mockSocketFactory();
  const link = new RelaydLink({
    url: "wss://relay.glass/v1/link",
    credential: CREDENTIAL,
    socketFactory: factory,
    clock,
    random: countingRandom(13),
    ...options,
  });
  const errors: LinkError[] = [];
  link.on("error", (e) => errors.push(e));
  return {
    link,
    clock,
    factory,
    errors,
    socket: (index = factory.sockets.length - 1) => factory.sockets[index]!,
  };
}

/** Connect and let the server accept the upgrade. */
function connected(options: Partial<ConstructorParameters<typeof RelaydLink>[0]> = {}): Harness {
  const h = harness(options);
  h.link.connect();
  h.socket().acceptOpen();
  return h;
}

function sentEnvelopes(socket: MockRelaydSocket): RelaydEnvelope[] {
  return socket.sent.map((text) => parseEnvelope(text));
}

// --- the contract, re-read from the things that own it ----------------------
//
// `docs/SYSTEM.md` §6.1 is the contract; `relayd/internal/api/wire.go` is the
// daemon that has to be satisfied; `apps/ios/RelayKit/RelaydLink.swift` is the
// other client. Same trick as `commands.test.ts` re-parsing `commands.py`: a
// transcription that can drift silently is not a contract.

const here = dirname(fileURLToPath(import.meta.url));
const REPO = join(here, "..", "..", "..");
const SYSTEM_MD = join(REPO, "docs", "SYSTEM.md");
const SWIFT_LINK = join(REPO, "apps", "ios", "RelayKit", "RelaydLink.swift");
const GO_WIRE = join(REPO, "relayd", "internal", "api", "wire.go");

function backticked(text: string): string[] {
  return [...text.matchAll(/`([a-z][a-z.]*)`/g)].map((match) => match[1]!);
}

/** §6.1's two lists — the prose, plus the table of frames added underneath it. */
function systemMdVocabulary(): { phone: string[]; server: string[] } {
  const source = readFileSync(SYSTEM_MD, "utf8");
  const start = source.indexOf("### 6.1 Phone ↔ `relayd`");
  assert.ok(start >= 0, "SYSTEM.md must still have a §6.1 Phone ↔ relayd");
  const section = source.slice(start).split("\n### ")[0]!;

  const phoneAt = section.indexOf("Phone → server:");
  const serverAt = section.indexOf("Server → phone:");
  assert.ok(phoneAt >= 0 && serverAt > phoneAt, "§6.1 must still list both directions");

  const phone = backticked(section.slice(phoneAt, serverAt));
  const afterServer = section.slice(serverAt);
  const server = backticked(afterServer.slice(0, afterServer.indexOf("\n\n")));

  // "Four more server→phone frames, added while implementing this", as a table.
  const extra = [...section.matchAll(/^\| `([a-z][a-z.]*)` \|/gm)].map((match) => match[1]!);
  assert.ok(extra.length > 0, "§6.1's table of added frames must still be a table");

  return { phone, server: [...server, ...extra] };
}

/** Raw values of a `String`-backed Swift enum, in declaration order. */
function swiftEnumRawValues(name: string): string[] {
  const source = readFileSync(SWIFT_LINK, "utf8");
  const start = source.indexOf(`public enum ${name}: String`);
  assert.ok(start >= 0, `RelaydLink.swift must still declare ${name}`);
  const body = source.slice(source.indexOf("{", start) + 1);
  const end = body.indexOf("\n}");
  assert.ok(end > 0, `${name} must still be a brace-delimited enum`);

  return [...body.slice(0, end).matchAll(/^\s*case\s+(\w+)(?:\s*=\s*"([^"]+)")?/gm)].map(
    // A case with no `= "..."` is its own raw value, per Swift.
    (match) => match[2] ?? match[1]!,
  );
}

/** The string constants in the `const (…)` block under a named comment. */
function goWireTypes(marker: string): string[] {
  const source = readFileSync(GO_WIRE, "utf8");
  const at = source.indexOf(`// ${marker}`);
  assert.ok(at >= 0, `wire.go must still label its "${marker}" block`);
  const open = source.indexOf("const (", at);
  const close = source.indexOf("\n)", open);
  assert.ok(open > at && close > open, "wire.go must still group its types in a const block");

  return [...source.slice(open, close).matchAll(/^\tType\w+\s*=\s*"([a-z.]+)"/gm)].map(
    (match) => match[1]!,
  );
}

// ---------------------------------------------------------------------------

describe("the envelope — docs/SYSTEM.md §6.1", () => {
  test("round trips exactly the documented shape", () => {
    const envelope: RelaydEnvelope = {
      v: 1,
      id: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
      type: "utterance",
      at: 1_700_000_000_000,
      payload: { text: "what did I say about the CRC" },
    };
    assert.deepEqual(parseEnvelope(serialiseEnvelope(envelope)), envelope);
  });

  test("every field is required, because two languages implement this", () => {
    const base = { v: 1, id: "x", type: "utterance", at: 1, payload: {} };
    for (const missing of ["id", "type", "at"]) {
      const broken: Record<string, unknown> = { ...base };
      delete broken[missing];
      assert.throws(
        () => parseEnvelope(JSON.stringify(broken)),
        (e: LinkError) => e.code === "malformed",
        `a missing ${missing} must be rejected`,
      );
    }
  });

  test("a future envelope version is named, not silently coerced", () => {
    assert.throws(
      () => parseEnvelope(JSON.stringify({ v: 2, id: "x", type: "speak", at: 1, payload: {} })),
      (e: LinkError) => e.code === "versionMismatch",
    );
  });

  test("non-JSON and non-objects are malformed", () => {
    assert.throws(() => parseEnvelope("not json"), (e: LinkError) => e.code === "malformed");
    assert.throws(() => parseEnvelope("[]"), (e: LinkError) => e.code === "malformed");
    assert.throws(() => parseEnvelope("null"), (e: LinkError) => e.code === "malformed");
  });

  test("the vocabulary is exactly what the doc lists", () => {
    const documented = systemMdVocabulary();

    assert.deepEqual(documented.phone, [
      "utterance",
      "touch",
      "wear",
      "audio.chunk",
      "photo",
      "session.command",
      "consent.decision",
      "sync.offer",
    ]);
    assert.deepEqual(documented.server, [
      "speak",
      "ui.render",
      "session.list",
      "confirm.request",
      "connector.proposal",
      "digest",
      // The four the transport turned out to need. Each was added because
      // leaving it out made a documented behaviour unimplementable, and each was
      // dropped on the floor here until it was.
      "ack",
      "error",
      "notify",
      "confirm.resolved",
    ]);

    assert.deepEqual(
      Object.values(PhoneMessage),
      documented.phone,
      "the phone vocabulary has drifted from SYSTEM.md §6.1",
    );
    assert.deepEqual(
      Object.values(ServerMessage),
      documented.server,
      "the server vocabulary has drifted from SYSTEM.md §6.1",
    );
  });

  test("nothing the doc lists falls through to unknownType", () => {
    // The bug this replaced: four documented frames were absent from
    // `ServerMessage`, so `isServerMessage` said no and the link dropped them
    // into `unknownType`. A silently swallowed `notify` is the quiet-hours
    // behaviour failing with nothing in the log.
    for (const type of systemMdVocabulary().server) {
      assert.equal(isServerMessage(type), true, `${type} is documented and must be recognised`);
    }
    assert.equal(isServerMessage("hologram.render"), false);
  });

  test("the transport frames are routed apart from the product ones", () => {
    // `ack` and `error` are bookkeeping and `notify`/`confirm.resolved` are acted
    // on by the link itself, so none of the four reaches a raw-envelope listener.
    for (const type of ["speak", "ui.render", "session.list", "confirm.request", "connector.proposal", "digest"]) {
      assert.equal(isProductServerMessage(type), true);
    }
    for (const type of ["ack", "error", "notify", "confirm.resolved"]) {
      assert.equal(isProductServerMessage(type), false);
      assert.equal(isServerMessage(type), true);
    }
  });

  test("Swift speaks the same vocabulary, and so does relayd", () => {
    // Three implementations of one contract. The Swift cannot be compiled on
    // this machine — no macOS — so re-parsing it here is the only check it gets,
    // and `relayd` is the daemon the phone actually has to satisfy.
    const documented = systemMdVocabulary();

    assert.deepEqual(swiftEnumRawValues("PhoneMessage"), documented.phone);
    assert.deepEqual(swiftEnumRawValues("ServerMessage"), documented.server);

    assert.deepEqual(goWireTypes("Phone → server."), documented.phone);
    assert.deepEqual(goWireTypes("Server → phone."), documented.server);
  });

  test("ids are v4 UUIDs, so the server can dedupe on them", () => {
    const h = connected();
    h.link.send(PhoneMessage.Utterance, { text: "one" });
    h.link.send(PhoneMessage.Utterance, { text: "two" });

    const ids = sentEnvelopes(h.socket()).map((e) => e.id);
    for (const id of ids) {
      assert.match(id, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    }
    assert.notEqual(ids[0], ids[1]);
  });
});

describe("authentication", () => {
  test("the credential rides the subprotocol, never the URL", () => {
    const h = connected();
    const socket = h.socket();

    assert.equal(socket.url, "wss://relay.glass/v1/link");
    assert.equal(socket.protocols[0], LINK_SUBPROTOCOL);
    assert.ok(socket.authHeader, "no auth subprotocol was offered");
    assert.equal(
      socket.url.includes(CREDENTIAL.deviceToken),
      false,
      "a bearer token in a URL ends up in proxy logs and crash reports",
    );
  });

  test("the box can verify what the phone offered", () => {
    const h = connected();
    const proof = verifyAuthHeader(h.socket().authHeader!, CREDENTIAL, h.clock.now());
    assert.equal(proof.deviceId, CREDENTIAL.deviceId);
    assert.equal(proof.deviceToken, CREDENTIAL.deviceToken);
  });

  test("a captured header stops working once its window passes", () => {
    const h = connected();
    const header = h.socket().authHeader!;
    assert.throws(
      () => verifyAuthHeader(header, CREDENTIAL, h.clock.now() + AUTH_WINDOW_MS + 1),
      (e: LinkError) => e.code === "malformed",
    );
  });

  test("the token alone is not the proof", () => {
    const h = connected();
    const parts = h.socket().authHeader!.split(".");
    // Same device, same token, forged signature.
    parts[parts.length - 1] = "f".repeat(64);
    assert.throws(
      () => verifyAuthHeader(parts.join("."), CREDENTIAL, h.clock.now()),
      (e: LinkError) => e.code === "malformed",
    );
  });

  test("a header signed with another box's key does not verify", () => {
    const other: DeviceCredential = { ...CREDENTIAL, signingKey: "c".repeat(64) };
    const header = authHeader(other, 1_700_000_000_000, new Uint8Array(16));
    assert.throws(
      () => verifyAuthHeader(header, CREDENTIAL, 1_700_000_000_000),
      (e: LinkError) => e.code === "malformed",
    );
  });

  test("each connection carries a fresh nonce", async () => {
    const h = harness();
    h.link.connect();
    h.socket().acceptOpen();
    h.socket().dropAbruptly();
    await h.clock.advance(60_000);

    assert.equal(h.factory.sockets.length, 2, "the drop should have produced a second connection");
    const nonces = h.factory.sockets.map(
      (s) => verifyAuthHeader(s.authHeader!, CREDENTIAL, h.clock.now()).nonce,
    );
    assert.equal(new Set(nonces).size, nonces.length, "a reused nonce is a replayable header");
  });
});

describe("sending", () => {
  test("an open link sends immediately", () => {
    const h = connected();
    const id = h.link.send(PhoneMessage.Utterance, { text: "hello" });

    const sent = sentEnvelopes(h.socket());
    assert.equal(sent.length, 1);
    assert.equal(sent[0]!.id, id);
    assert.equal(sent[0]!.type, "utterance");
    assert.deepEqual(sent[0]!.payload, { text: "hello" });
    assert.equal(sent[0]!.at, h.clock.now());
  });

  test("work queued before the socket opens is not lost", () => {
    const h = harness();
    h.link.connect();
    h.link.send(PhoneMessage.Touch, { action: "doubleTap" });
    h.link.send(PhoneMessage.Wear, { worn: true });

    assert.equal(h.socket().sent.length, 0, "nothing goes out before the upgrade completes");
    assert.equal(h.link.pending, 2);

    h.socket().acceptOpen();
    assert.deepEqual(
      sentEnvelopes(h.socket()).map((e) => e.type),
      ["touch", "wear"],
    );
  });

  test("a message handed over mid-reconnect is delivered, not dropped", async () => {
    // The whole point of §6.1: a phone that sleeps and wakes is the normal path.
    const h = connected();
    h.socket().dropAbruptly("screen off");
    assert.equal(h.link.state, LinkState.Reconnecting);

    const id = h.link.send(PhoneMessage.Utterance, { text: "said while underground" });
    assert.ok(id, "send must not refuse just because the socket is between connections");

    await h.clock.advance(60_000);
    h.socket().acceptOpen();

    const delivered = sentEnvelopes(h.socket());
    assert.deepEqual(delivered.map((e) => e.id), [id]);
    assert.equal(h.link.pending, 1, "still unacknowledged, but on the wire");
  });

  test("a full outbox refuses the newest and says so", () => {
    const h = connected({ outboxLimit: 2 });
    h.socket().sendFails = true;

    assert.ok(h.link.send(PhoneMessage.Utterance, { n: 1 }));
    assert.ok(h.link.send(PhoneMessage.Utterance, { n: 2 }));
    const refused = h.link.send(PhoneMessage.Utterance, { n: 3 });

    assert.equal(refused, null);
    assert.equal(h.errors.at(-1)!.code, "outboxFull");
  });

  test("a socket that refuses a send keeps the envelope at the head", async () => {
    const h = connected();
    h.socket().sendFails = true;
    h.link.send(PhoneMessage.Utterance, { text: "first" });
    h.link.send(PhoneMessage.Utterance, { text: "second" });

    assert.equal(h.link.pending, 2);
    assert.equal(h.errors.at(-1)!.code, "socketFailed");

    h.socket().dropAbruptly();
    await h.clock.advance(60_000);
    h.socket().acceptOpen();

    assert.deepEqual(
      sentEnvelopes(h.socket()).map((e) => (e.payload as { text: string }).text),
      ["first", "second"],
    );
  });
});

describe("sleep, wake, and redelivery", () => {
  test("anything in flight when the link dies goes back to the head of the queue", async () => {
    const h = connected();
    h.link.send(PhoneMessage.Utterance, { text: "a" });
    h.link.send(PhoneMessage.Utterance, { text: "b" });
    const first = sentEnvelopes(h.socket()).map((e) => e.id);

    const redelivered: Array<{ count: number; ids: string[] }> = [];
    h.link.on("redelivered", (r) => redelivered.push(r));

    h.socket().dropAbruptly("cell handover");
    await h.clock.advance(60_000);
    h.socket().acceptOpen();

    assert.deepEqual(redelivered, [{ count: 2, ids: first }]);
    assert.deepEqual(
      sentEnvelopes(h.socket()).map((e) => e.id),
      first,
      "same ids, so the server dedupes rather than storing the utterance twice",
    );
  });

  test("redelivered work goes ahead of newer work, because order is not cosmetic", async () => {
    const h = connected();
    h.link.send(PhoneMessage.Utterance, { text: "older" });
    h.socket().dropAbruptly();
    h.link.send(PhoneMessage.Utterance, { text: "newer" });

    await h.clock.advance(60_000);
    h.socket().acceptOpen();

    assert.deepEqual(
      sentEnvelopes(h.socket()).map((e) => (e.payload as { text: string }).text),
      ["older", "newer"],
    );
  });

  test("an ack prunes what is held, so nothing is sent twice", async () => {
    // `{ re, ok }`, one per accepted frame — relayd/internal/api/wire.go's `Ack`.
    // An earlier version of this link invented a batched `{ ids: [...] }`, which
    // no daemon has ever sent, so nothing was ever pruned.
    const h = connected();
    const id = h.link.send(PhoneMessage.Utterance, { text: "landed" });
    assert.equal(h.link.pending, 1);

    h.socket().deliverEnvelope({ type: ServerMessage.Ack, payload: { re: id, ok: true } });
    assert.equal(h.link.pending, 0);

    const redelivered: number[] = [];
    h.link.on("redelivered", (r) => redelivered.push(r.count));
    h.socket().dropAbruptly();
    await h.clock.advance(60_000);
    h.socket().acceptOpen();

    assert.deepEqual(redelivered, []);
    assert.equal(h.socket().sent.length, 0);
  });

  test("an ack for something else prunes nothing", () => {
    const h = connected();
    h.link.send(PhoneMessage.Utterance, { text: "still waiting" });

    const acks: AckFrame[] = [];
    h.link.on("ack", (a) => acks.push(a));
    h.socket().deliverEnvelope({ type: ServerMessage.Ack, payload: { re: "someone-else", ok: true } });

    assert.equal(h.link.pending, 1);
    assert.equal(acks.length, 1);
    assert.equal(acks[0]!.pruned, false);
  });

  test("ok:false says it did not land, so the envelope stays held", async () => {
    const h = connected();
    const id = h.link.send(PhoneMessage.Utterance, { text: "maybe" });
    h.socket().deliverEnvelope({ type: ServerMessage.Ack, payload: { re: id, ok: false } });

    assert.equal(h.link.pending, 1, "a negative ack is not a delivery receipt");

    h.socket().dropAbruptly();
    await h.clock.advance(60_000);
    h.socket().acceptOpen();
    assert.deepEqual(sentEnvelopes(h.socket()).map((e) => e.id), [id]);
  });

  test("an ack is visible, because a frame the doc lists is never swallowed", () => {
    const h = connected();
    const seen: string[] = [];
    const acks: AckFrame[] = [];
    h.link.on("message", (e) => seen.push(e.type));
    h.link.on("unknownType", (e) => seen.push(`unknown:${e.type}`));
    h.link.on("ack", (a) => acks.push(a));

    h.socket().deliverEnvelope({ type: ServerMessage.Ack, payload: { re: "nope", ok: true } });

    assert.deepEqual(seen, ["ack"], "documented frames go on the firehose, unknown ones do not");
    assert.deepEqual(acks.map((a) => [a.re, a.ok, a.pruned]), [["nope", true, false]]);
  });

  test("backoff grows, is capped, and is jittered", () => {
    const noJitter = { baseMs: 500, maxMs: 30_000, jitter: 0 };
    assert.deepEqual(
      [0, 1, 2, 3, 4, 5, 6, 7].map((n) => backoffMs(n, noJitter)),
      [500, 1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000],
    );

    // Jitter spreads a building full of phones whose access point just rebooted,
    // and it subtracts, so maxMs stays a real ceiling.
    const spread = { baseMs: 1_000, maxMs: 30_000, jitter: 0.5 };
    assert.equal(backoffMs(0, spread, 0), 500);
    assert.equal(backoffMs(0, spread, 1), 1_000);
    assert.equal(backoffMs(20, spread, 1), 30_000, "never longer than the cap");
  });

  test("reconnection actually backs off rather than hammering the box", async () => {
    const h = harness({ backoff: { baseMs: 1_000, maxMs: 30_000, jitter: 0 } });
    h.link.connect();

    for (let i = 0; i < 3; i++) {
      h.socket().acceptOpen();
      h.socket().dropAbruptly();
      await h.clock.advance(1);
      assert.equal(h.factory.sockets.length, i + 1, "must not retry instantly");
      await h.clock.advance(1_000);
    }
    assert.equal(h.factory.sockets.length, 4);
  });

  test("wake collapses the backoff instead of waiting it out", async () => {
    const h = harness({ backoff: { baseMs: 30_000, maxMs: 30_000, jitter: 0 } });
    h.link.connect();
    h.socket().acceptOpen();
    h.socket().dropAbruptly("airplane mode");

    await h.clock.advance(100);
    assert.equal(h.factory.sockets.length, 1);

    h.link.wake();
    assert.equal(h.factory.sockets.length, 2, "the OS said the network is back");
    assert.equal(h.link.state, LinkState.Connecting);
  });

  test("a successful connection resets the backoff", async () => {
    const h = harness({ backoff: { baseMs: 1_000, maxMs: 30_000, jitter: 0 } });
    h.link.connect();
    h.socket().acceptOpen();
    h.socket().dropAbruptly();
    await h.clock.advance(1_000);
    assert.equal(h.link.attempt, 1);

    h.socket().acceptOpen();
    assert.equal(h.link.attempt, 0, "a long-lived connection must not inherit yesterday's backoff");
  });

  test("closing keeps queued work for the next connect", async () => {
    const h = connected();
    h.socket().sendFails = true;
    h.link.send(PhoneMessage.Utterance, { text: "said just before the app closed" });

    h.link.close();
    assert.equal(h.link.state, LinkState.Closed);
    assert.equal(h.link.pending, 1, "closing the app is not consent to forget an utterance");

    await h.clock.advance(60_000);
    assert.equal(h.factory.sockets.length, 1, "a deliberate close must not reconnect");

    h.link.connect();
    h.socket().acceptOpen();
    assert.equal(sentEnvelopes(h.socket()).length, 1);
  });

  test("wake does not resurrect a deliberately closed link", async () => {
    const h = connected();
    h.link.close();
    h.link.wake();
    assert.equal(h.factory.sockets.length, 1);
    assert.equal(h.link.state, LinkState.Closed);
  });

  test("a half-open socket is closed and retried when the idle timer fires", async () => {
    const h = connected({ idleTimeoutMs: 90_000 });
    h.socket().deliverEnvelope({ type: ServerMessage.Speak, payload: { text: "hi" } });

    await h.clock.advance(89_999);
    assert.equal(h.factory.sockets.length, 1);

    await h.clock.advance(2);
    assert.equal(h.errors.at(-1)!.code, "socketFailed");
    await h.clock.advance(60_000);
    assert.equal(h.factory.sockets.length, 2);
  });

  test("state transitions are observable", async () => {
    const h = harness({ backoff: { baseMs: 1_000, maxMs: 1_000, jitter: 0 } });
    const states: string[] = [];
    h.link.on("stateChanged", (s) => states.push(s));

    h.link.connect();
    h.socket().acceptOpen();
    h.socket().dropAbruptly();
    await h.clock.advance(1_000);
    h.socket().acceptOpen();
    h.link.close();

    assert.deepEqual(states, [
      LinkState.Connecting,
      LinkState.Open,
      LinkState.Reconnecting,
      LinkState.Connecting,
      LinkState.Open,
      LinkState.Closed,
    ]);
  });
});

describe("receiving", () => {
  test("each product message reaches its own listener and the firehose", () => {
    const h = connected();
    const product = [
      ServerMessage.Speak,
      ServerMessage.UiRender,
      ServerMessage.SessionList,
      ServerMessage.ConfirmRequest,
      ServerMessage.ConnectorProposal,
      ServerMessage.Digest,
    ] as const;

    const typed: string[] = [];
    const all: string[] = [];
    for (const type of product) h.link.on(type, (e) => typed.push(e.type));
    h.link.on("message", (e) => all.push(e.type));

    for (const type of product) h.socket().deliverEnvelope({ type, payload: {} });

    assert.deepEqual(typed, [...product]);
    assert.deepEqual(all, [...product]);
  });

  test("every documented frame reaches a listener — none is dropped", () => {
    // The finding, as a test: all ten of §6.1's server→phone frames must be
    // handled, not merely six. The other four used to fall through to
    // `unknownType` and be discarded.
    const h = connected();
    const routed: string[] = [];

    for (const type of [
      ServerMessage.Speak,
      ServerMessage.UiRender,
      ServerMessage.SessionList,
      ServerMessage.ConfirmRequest,
      ServerMessage.ConnectorProposal,
      ServerMessage.Digest,
    ] as const) {
      h.link.on(type, () => routed.push(type));
    }
    h.link.on("notify", () => routed.push("notify"));
    h.link.on("confirm.resolved", () => routed.push("confirm.resolved"));
    h.link.on("ack", () => routed.push("ack"));
    h.link.on("serverError", () => routed.push("error"));

    const unknown: string[] = [];
    h.link.on("unknownType", (e) => unknown.push(e.type));

    for (const type of Object.values(ServerMessage)) {
      h.socket().deliverEnvelope({ type, payload: { action_id: "a1", re: "r1" } });
    }

    assert.deepEqual(routed, [...Object.values(ServerMessage)]);
    assert.deepEqual(unknown, [], "nothing the doc lists may be treated as unknown");
  });

  test("a type this build does not know is surfaced, not fatal", () => {
    const h = connected();
    const unknown: string[] = [];
    h.link.on("unknownType", (e) => unknown.push(e.type));

    h.socket().deliverEnvelope({ type: "hologram.render", payload: { x: 1 } });

    assert.deepEqual(unknown, ["hologram.render"]);
    assert.equal(h.errors.length, 0, "a newer server is not an error");
  });

  test("a malformed inbound frame is reported and the link stays up", () => {
    const h = connected();
    h.socket().deliver("{ not json");
    h.socket().deliver(JSON.stringify({ v: 1, type: "speak" }));

    assert.deepEqual(h.errors.map((e) => e.code), ["malformed", "malformed"]);
    assert.equal(h.link.state, LinkState.Open);

    h.socket().deliverEnvelope({ type: ServerMessage.Speak, payload: { text: "still here" } });
    const speaks: string[] = [];
    h.link.on("speak", (e) => speaks.push(e.type));
    h.socket().deliverEnvelope({ type: ServerMessage.Speak, payload: { text: "again" } });
    assert.deepEqual(speaks, ["speak"]);
  });

  test("a throwing listener does not take the link down", () => {
    const h = connected();
    const after: string[] = [];
    h.link.on("speak", () => {
      throw new Error("UI bug");
    });
    h.link.on("speak", (e) => after.push(e.type));

    h.socket().deliverEnvelope({ type: ServerMessage.Speak, payload: {} });
    assert.deepEqual(after, ["speak"], "a UI bug must not stop capture");
  });
});

describe("the four transport frames — docs/SYSTEM.md §6.1", () => {
  test("an error retires the frame it names, so it is not refused forever", () => {
    // The M4 case from `relayd`'s own ws.go: capture is not built, so an
    // `audio.chunk` comes back `not_implemented` with the milestone attached.
    // Holding it would redeliver it on every reconnect for the life of the queue.
    const h = connected();
    const id = h.link.send(PhoneMessage.AudioChunk, { seq: 1, codec: "opus" });
    assert.equal(h.link.pending, 1);

    const errors: ServerErrorFrame[] = [];
    h.link.on("serverError", (e) => errors.push(e));

    h.socket().deliverEnvelope({
      type: ServerMessage.Error,
      payload: {
        re: id,
        code: ServerErrorCode.NotImplemented,
        message: "relayd is not capturing yet, so this was not stored — keep it on the device",
        milestone: "M4 — capture and memory (SYSTEM.md §10 steps 5–6)",
      },
    });

    assert.equal(h.link.pending, 0, "a refusal is an answer");
    assert.equal(errors.length, 1);
    assert.equal(errors[0]!.code, "not_implemented");
    assert.equal(errors[0]!.cancelled, true);
    assert.match(
      errors[0]!.milestone!,
      /^M4/,
      "a phone told which milestone will store this keeps the audio instead of deleting it",
    );
  });

  test("a refused frame is not redelivered across a reconnect", async () => {
    const h = connected();
    const id = h.link.send(PhoneMessage.SyncOffer, { files: 12, bytes: 1, on_lan: true });
    h.socket().deliverEnvelope({
      type: ServerMessage.Error,
      payload: { re: id, code: ServerErrorCode.NotImplemented, message: "not yet" },
    });

    h.socket().dropAbruptly();
    await h.clock.advance(60_000);
    h.socket().acceptOpen();

    assert.equal(h.socket().sent.length, 0);
  });

  test("a server error is not confused with the link's own faults", () => {
    const h = connected();
    const linkErrors: LinkError[] = [];
    const serverErrors: ServerErrorFrame[] = [];
    h.link.on("error", (e) => linkErrors.push(e));
    h.link.on("serverError", (e) => serverErrors.push(e));

    h.socket().deliverEnvelope({
      type: ServerMessage.Error,
      payload: { code: ServerErrorCode.NoSuchSession, message: "no such session: abc" },
    });

    assert.equal(serverErrors.length, 1);
    assert.equal(serverErrors[0]!.re, null, "an unsolicited error names nothing");
    assert.equal(
      linkErrors.length,
      0,
      "the daemon refusing an action is not the socket breaking; a UI that conflates "
        + "them retries the first forever",
    );
  });

  test("an unrecognised error code is passed through rather than swallowed", () => {
    const h = connected();
    const errors: ServerErrorFrame[] = [];
    h.link.on("serverError", (e) => errors.push(e));

    h.socket().deliverEnvelope({
      type: ServerMessage.Error,
      payload: { code: "quota_exhausted", message: "later" },
    });

    assert.equal(errors[0]!.code, "quota_exhausted");
  });

  test("a notify arrives without speech, and silent still means present", () => {
    // `docs/ADAPTERS.md` §7: quiet hours hold the speech and keep the
    // notification. Dropping the frame is that behaviour failing with no log line.
    const h = connected();
    const spoken: RelaydEnvelope[] = [];
    const notifications: NotifyFrame[] = [];
    h.link.on("speak", (e) => spoken.push(e));
    h.link.on("notify", (n) => notifications.push(n));

    h.socket().deliverEnvelope({
      type: ServerMessage.Notify,
      payload: {
        title: "auth-refactor finished",
        body: "14 files, tests green",
        sessions: ["s-1", "s-2"],
        silent: true,
        ping: "p-9",
      },
    });

    assert.deepEqual(spoken, [], "a notification is not an utterance");
    assert.equal(notifications.length, 1);
    assert.deepEqual(
      [notifications[0]!.title, notifications[0]!.body, notifications[0]!.silent],
      ["auth-refactor finished", "14 files, tests green", true],
    );
    assert.deepEqual(notifications[0]!.sessions, ["s-1", "s-2"]);
    assert.equal(notifications[0]!.ping, "p-9");
  });

  test("a notify missing its optional fields is still delivered", () => {
    const h = connected();
    const notifications: NotifyFrame[] = [];
    h.link.on("notify", (n) => notifications.push(n));

    h.socket().deliverEnvelope({ type: ServerMessage.Notify, payload: { title: "hi" } });

    assert.equal(notifications.length, 1);
    assert.deepEqual(notifications[0]!.sessions, []);
    assert.equal(notifications[0]!.silent, false);
    assert.equal(notifications[0]!.body, "");
    assert.equal(h.errors.length, 0, "a thin payload is not a malformed envelope");
  });

  test("confirm.resolved retracts the question it names", () => {
    const h = connected();
    const resolved: ConfirmResolvedFrame[] = [];
    h.link.on("confirm.resolved", (r) => resolved.push(r));

    h.socket().deliverEnvelope({
      type: ServerMessage.ConfirmRequest,
      payload: { action_id: "act-1", session: "s-1", prompt: "delete the branch?" },
    });
    h.socket().deliverEnvelope({
      type: ServerMessage.ConfirmRequest,
      payload: { action_id: "act-2", session: "s-2", prompt: "send the email?" },
    });
    assert.deepEqual(h.link.outstandingConfirmations, ["act-1", "act-2"]);

    h.socket().deliverEnvelope({
      type: ServerMessage.ConfirmResolved,
      payload: { action_id: "act-1", reason: "answered in a terminal" },
    });

    assert.deepEqual(
      h.link.outstandingConfirmations,
      ["act-2"],
      "a ping that outlives its question wakes someone to approve what is already approved",
    );
    assert.deepEqual(
      resolved.map((r) => [r.actionId, r.reason, r.wasOutstanding]),
      [["act-1", "answered in a terminal", true]],
    );
  });

  test("a resolution for a question this phone never saw is still surfaced", () => {
    const h = connected();
    const resolved: ConfirmResolvedFrame[] = [];
    h.link.on("confirm.resolved", (r) => resolved.push(r));

    h.socket().deliverEnvelope({
      type: ServerMessage.ConfirmResolved,
      payload: { action_id: "act-99", reason: "cancelled" },
    });

    assert.equal(resolved.length, 1, "the resolution may outrun the request across a reconnect");
    assert.equal(resolved[0]!.wasOutstanding, false);
  });

  test("answering closes the question here, without waiting for the box to say so", () => {
    const h = connected();
    h.socket().deliverEnvelope({
      type: ServerMessage.ConfirmRequest,
      payload: { action_id: "act-1", prompt: "unlock the door?" },
    });
    assert.deepEqual(h.link.outstandingConfirmations, ["act-1"]);

    h.link.send(PhoneMessage.ConsentDecision, { action_id: "act-1", approved: true });
    assert.deepEqual(h.link.outstandingConfirmations, []);

    // The box's retraction still arrives, and reads as a duplicate rather than a
    // retraction of something open.
    const resolved: ConfirmResolvedFrame[] = [];
    h.link.on("confirm.resolved", (r) => resolved.push(r));
    h.socket().deliverEnvelope({
      type: ServerMessage.ConfirmResolved,
      payload: { action_id: "act-1", reason: "answered" },
    });
    assert.equal(resolved[0]!.wasOutstanding, false);
  });

  test("all four survive the relay sealed, like everything else", () => {
    const h = connected({ sealed: true });
    const proof = verifyAuthHeader(h.socket().authHeader!, CREDENTIAL, h.clock.now());
    const box = new SealedChannel(
      linkSealingKey(CREDENTIAL, hexToBytes(proof.nonce)),
      PairingRole.Host,
    );

    const seen: string[] = [];
    h.link.on("ack", () => seen.push("ack"));
    h.link.on("serverError", () => seen.push("error"));
    h.link.on("notify", () => seen.push("notify"));
    h.link.on("confirm.resolved", () => seen.push("confirm.resolved"));

    for (const type of [
      ServerMessage.Ack,
      ServerMessage.Error,
      ServerMessage.Notify,
      ServerMessage.ConfirmResolved,
    ]) {
      h.socket().deliver(
        JSON.stringify(
          box.sealText(
            serialiseEnvelope({
              v: LINK_VERSION,
              id: `srv-${type}`,
              type,
              at: 1,
              payload: { re: "x", code: "failed", title: "t", action_id: "a" },
            }),
          ),
        ),
      );
    }

    assert.deepEqual(seen, ["ack", "error", "notify", "confirm.resolved"]);
  });
});

describe("through the rendezvous relay", () => {
  /**
   * The relay from `docs/SYSTEM.md` §7: it joins two dial-outs and moves strings.
   * It is given every byte and is expected to make nothing of them.
   */
  function relayedPair() {
    const h = connected({ sealed: true });
    const proof = verifyAuthHeader(h.socket().authHeader!, CREDENTIAL, h.clock.now());
    const box = new SealedChannel(
      linkSealingKey(CREDENTIAL, hexToBytes(proof.nonce)),
      PairingRole.Host,
    );
    return { ...h, box };
  }

  test("the relay carries the utterance and cannot read it", () => {
    const { link, socket, box } = relayedPair();
    link.send(PhoneMessage.Utterance, { text: "cancel the payments refactor" });

    const onTheWire = socket().sent[0]!;
    assert.equal(onTheWire.includes("cancel"), false);
    assert.equal(onTheWire.includes("utterance"), false);

    // The box, holding the credential, reads it fine.
    const opened = parseEnvelope(box.openText(JSON.parse(onTheWire)));
    assert.equal(opened.type, "utterance");
    assert.deepEqual(opened.payload, { text: "cancel the payments refactor" });
  });

  test("the box's replies come back sealed too", () => {
    const { link, socket, box } = relayedPair();
    const spoken: unknown[] = [];
    link.on("speak", (e) => spoken.push(e.payload));

    socket().deliver(
      JSON.stringify(
        box.sealText(
          serialiseEnvelope({
            v: LINK_VERSION,
            id: "srv-1",
            type: ServerMessage.Speak,
            at: 1,
            payload: { text: "adding that to the payments refactor" },
          }),
        ),
      ),
    );

    assert.deepEqual(spoken, [{ text: "adding that to the payments refactor" }]);
  });

  test("a relay that tampers with a frame is caught, not obeyed", () => {
    const { link, socket, box, errors } = relayedPair();
    const frame = box.sealText(
      serialiseEnvelope({
        v: LINK_VERSION,
        id: "srv-1",
        type: ServerMessage.ConfirmRequest,
        at: 1,
        payload: { action: "send email" },
      }),
    );
    const bytes = [...atob(frame.ciphertext)].map((c) => c.charCodeAt(0));
    bytes[10] = bytes[10]! ^ 0xff;

    const confirms: unknown[] = [];
    link.on("confirm.request", (e) => confirms.push(e));
    socket().deliver(
      JSON.stringify({ ...frame, ciphertext: btoa(String.fromCharCode(...bytes)) }),
    );

    assert.deepEqual(confirms, []);
    assert.equal(errors.at(-1)!.code, "sealFailed");
  });

  test("a replayed frame does not deliver a second confirmation", () => {
    const { link, socket, box, errors } = relayedPair();
    const frame = JSON.stringify(
      box.sealText(
        serialiseEnvelope({
          v: LINK_VERSION,
          id: "srv-1",
          type: ServerMessage.ConfirmRequest,
          at: 1,
          payload: { action: "unlock the door" },
        }),
      ),
    );

    const confirms: unknown[] = [];
    link.on("confirm.request", (e) => confirms.push(e));
    socket().deliver(frame);
    socket().deliver(frame);

    assert.equal(confirms.length, 1, "a replayed confirm.request must not be asked twice");
    assert.equal(errors.at(-1)!.code, "sealFailed");
  });

  test("a fresh sealed channel per connection, so a reconnect keeps working", async () => {
    const h = connected({ sealed: true });
    h.link.send(PhoneMessage.Utterance, { text: "before" });
    h.socket().dropAbruptly();
    await h.clock.advance(60_000);
    h.socket().acceptOpen();

    const proof = verifyAuthHeader(h.socket().authHeader!, CREDENTIAL, h.clock.now());
    const box = new SealedChannel(
      linkSealingKey(CREDENTIAL, hexToBytes(proof.nonce)),
      PairingRole.Host,
    );
    const opened = parseEnvelope(box.openText(JSON.parse(h.socket().sent[0]!)));
    assert.deepEqual(opened.payload, { text: "before" }, "redelivery must re-seal, not replay");
  });
});
