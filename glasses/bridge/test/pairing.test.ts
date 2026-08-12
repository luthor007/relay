import assert from "node:assert/strict";
import nodeCrypto from "node:crypto";
import { describe, test } from "node:test";

import { FakeClock } from "../src/clock.ts";
import {
  PAIRING_VERSION,
  TEST_ONLY_PAKE_NAME,
  PairingClient,
  PairingError,
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
  hexToBytes,
  hkdf,
  hmacSha256,
  linkSealingKey,
  newPairingCode,
  openSealed,
  parsePairingCode,
  seal,
  sha256,
  unsafeTestOnlyPake,
  utf8,
} from "../src/pairing.ts";
import { base64ToBytes } from "../src/trace.ts";
import type {
  PairAccept,
  PairHello,
  PakeEngine,
  RandomSource,
} from "../src/pairing.ts";

/**
 * The fake engine, asked for the way `pairing.ts` demands it be asked for.
 *
 * Every test in this file goes through here, so the opt-in argument is written
 * once rather than twenty times — and the one test that checks the opt-in is
 * enforced calls `unsafeTestOnlyPake` directly.
 */
function testPake(random: RandomSource): PakeEngine {
  return unsafeTestOnlyPake({ iUnderstandThisIsNotCryptography: true, random });
}

/** A host and a client that already agree on everything but the code. */
function pairingPair(options: { code?: string; clock?: FakeClock; ttlMs?: number } = {}) {
  const clock = options.clock ?? new FakeClock(1_700_000_000_000);
  const random = countingRandom(7);
  const pake = testPake(random);
  const host = new PairingHost({
    boxId: "box-mini-01",
    boxName: "Alexis's Mac mini",
    clock,
    random,
    pake,
    ...(options.ttlMs === undefined ? {} : { ttlMs: options.ttlMs }),
  });
  const client = new PairingClient({
    code: options.code ?? host.code.text,
    deviceId: "phone-abc",
    platform: "ios",
    deviceName: "iPhone 15",
    clock,
    pake,
  });
  return { host, client, clock, random, pake };
}

// ---------------------------------------------------------------------------

describe("hashing primitives", () => {
  // RFC 6234 / FIPS 180-4. Wrong constants produce a hash that is
  // self-consistent and useless, which no round-trip test would catch.
  test("sha256 matches the published vectors", () => {
    assert.equal(
      bytesToHex(sha256(utf8(""))),
      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    );
    assert.equal(
      bytesToHex(sha256(utf8("abc"))),
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
    );
    assert.equal(
      bytesToHex(sha256(utf8("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq"))),
      "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1",
    );
  });

  test("sha256 handles both sides of a block boundary", () => {
    // 55 bytes fits the length field in the same block; 56 forces a second one.
    assert.equal(
      bytesToHex(sha256(utf8("a".repeat(55)))),
      "9f4390f8d30c2dd92ec9f095b65e2b9ae9b0a925a5258e241c9f1e910f734318",
    );
    assert.equal(
      bytesToHex(sha256(utf8("a".repeat(56)))),
      "b35439a4ac6f0948b6d6f9e3c6af0f5f590ce20f1bde7090ef7970686ec6738a",
    );
  });

  test("hmac matches RFC 4231", () => {
    const key = new Uint8Array(20).fill(0x0b);
    assert.equal(
      bytesToHex(hmacSha256(key, utf8("Hi There"))),
      "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7",
    );
    assert.equal(
      bytesToHex(hmacSha256(utf8("Jefe"), utf8("what do ya want for nothing?"))),
      "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843",
    );
  });

  test("hmac folds an over-long key, per the RFC", () => {
    const key = new Uint8Array(131).fill(0xaa);
    assert.equal(
      bytesToHex(
        hmacSha256(key, utf8("Test Using Larger Than Block-Size Key - Hash Key First")),
      ),
      "60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54",
    );
  });

  test("hkdf matches RFC 5869 test case 1", () => {
    const ikm = new Uint8Array(22).fill(0x0b);
    const salt = hexToBytes("000102030405060708090a0b0c");
    const info = hexToBytes("f0f1f2f3f4f5f6f7f8f9");
    assert.equal(
      bytesToHex(hkdf(ikm, salt, info, 42)),
      "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865",
    );
  });

  test("field prefixes lengths, so concatenation cannot be ambiguous", () => {
    assert.notDeepEqual([...field("ab", "c")], [...field("a", "bc")]);
    assert.deepEqual([...field("ab", "c")], [...field("ab", "c")]);
  });

  test("hexToBytes refuses non-hex rather than coercing it to zeros", () => {
    // `parseInt("zz", 16)` is NaN, which a Uint8Array stores as 0 — so an
    // unvalidated signing key would silently become an all-zero key.
    assert.throws(() => hexToBytes("zz"), (e: PairingError) => e.code === "malformed");
    assert.throws(() => hexToBytes("abc"), (e: PairingError) => e.code === "malformed");
    assert.deepEqual([...hexToBytes("00ff10")], [0, 255, 16]);
  });

  test("equalBytes rejects a length mismatch and the empty case", () => {
    assert.equal(equalBytes(utf8("abc"), utf8("abc")), true);
    assert.equal(equalBytes(utf8("abc"), utf8("abd")), false);
    assert.equal(equalBytes(utf8("abc"), utf8("ab")), false);
    assert.equal(equalBytes(new Uint8Array(0), new Uint8Array(0)), false);
  });
});

describe("sealing", () => {
  const key = new Uint8Array(32).fill(9);

  test("a sealed payload round trips and hides the plaintext", () => {
    const plaintext = utf8("the box is at 192.168.1.44");
    const frame = seal(key, "test", new Uint8Array([1, 2, 3, 4]), plaintext);

    assert.equal(new TextDecoder().decode(openSealed(key, "test", frame)), "the box is at 192.168.1.44");
    assert.ok(!frame.ciphertext.includes("192"), "ciphertext must not carry the plaintext");
  });

  test("a flipped byte fails authentication rather than decrypting to garbage", () => {
    const frame = seal(key, "test", new Uint8Array([1]), utf8("approve payment"));
    const bytes = [...atob(frame.ciphertext)].map((c) => c.charCodeAt(0));
    bytes[0] = (bytes[0]! ^ 0xff) & 0xff;
    const tampered = { ...frame, ciphertext: btoa(String.fromCharCode(...bytes)) };

    assert.throws(
      () => openSealed(key, "test", tampered),
      (e: PairingError) => e.code === "tamperDetected",
    );
  });

  test("the wrong key does not open it", () => {
    const frame = seal(key, "test", new Uint8Array([1]), utf8("hello"));
    assert.throws(
      () => openSealed(new Uint8Array(32).fill(8), "test", frame),
      (e: PairingError) => e.code === "tamperDetected",
    );
  });

  test("a payload longer than HKDF-Expand's 8160-byte ceiling still works", () => {
    // An `audio.chunk` envelope can exceed it, and a keystream that quietly
    // stops at a limit is a bug that only shows up on long utterances.
    const big = new Uint8Array(50_000);
    for (let i = 0; i < big.length; i++) big[i] = i & 0xff;
    const frame = seal(key, "bulk", new Uint8Array([7]), big);
    assert.deepEqual([...openSealed(key, "bulk", frame)], [...big]);
  });

  test("two directions never share a keystream", () => {
    const phone = new SealedChannel(key, PairingRole.Client);
    const box = new SealedChannel(key, PairingRole.Host);

    const up = phone.sealText("from the phone");
    const down = box.sealText("from the box");

    assert.equal(box.openText(up), "from the phone");
    assert.equal(phone.openText(down), "from the box");
    // Same counter, same key, different label — so the bytes must differ.
    assert.notEqual(up.ciphertext, down.ciphertext);
  });

  test("a replayed frame is refused", () => {
    const phone = new SealedChannel(key, PairingRole.Client);
    const box = new SealedChannel(key, PairingRole.Host);

    const first = phone.sealText("consent granted");
    assert.equal(box.openText(first), "consent granted");
    assert.throws(() => box.openText(first), (e: PairingError) => e.code === "outOfOrder");
  });

  test("the nonce advances, so identical plaintexts do not repeat on the wire", () => {
    const phone = new SealedChannel(key, PairingRole.Client);
    const a = phone.sealText("yes");
    const b = phone.sealText("yes");
    assert.notEqual(a.nonce, b.nonce);
    assert.notEqual(a.ciphertext, b.ciphertext);
  });
});

describe("the pairing code", () => {
  test("is nine characters in three groups — readable down a phone line", () => {
    const code = newPairingCode(countingRandom(3));
    assert.match(code.text, /^[0-9A-Z]{3}-[0-9A-Z]{3}-[0-9A-Z]{3}$/);
    assert.equal(code.slot.length, 2);
    assert.equal(code.secret.length, 6);
  });

  test("never contains the characters people misread", () => {
    for (let seed = 1; seed < 200; seed++) {
      const code = newPairingCode(countingRandom(seed));
      assert.doesNotMatch(code.text, /[ILOU]/, `${code.text} contains a confusable character`);
    }
  });

  test("accepts what a person actually types", () => {
    const code = newPairingCode(countingRandom(11));
    const canonical = parsePairingCode(code.text);
    for (const variant of [
      code.text.toLowerCase(),
      code.text.replace(/-/g, ""),
      code.text.replace(/-/g, " "),
    ]) {
      assert.deepEqual(parsePairingCode(variant), canonical, `failed on "${variant}"`);
    }
  });

  test("maps O to 0 and I or L to 1 rather than rejecting them", () => {
    const canonical = parsePairingCode(formatPairingCode("01", "011000"));
    assert.deepEqual(parsePairingCode(formatPairingCode("01", "011000").replace(/0/g, "O")), canonical);
    assert.deepEqual(parsePairingCode(formatPairingCode("01", "011000").replace(/1/g, "I")), canonical);
  });

  test("a single mistyped character fails the check digit, not a round trip", () => {
    const code = newPairingCode(countingRandom(23));
    const chars = [...code.text.replace(/-/g, "")];
    chars[4] = chars[4] === "Z" ? "Y" : "Z";
    const typo = chars.join("");

    assert.throws(
      () => parsePairingCode(typo),
      (e: PairingError) => e.code === "checksum",
      "a typo must be caught locally, before it burns one of the box's three attempts",
    );
  });

  test("wrong length and non-alphabet characters are malformed", () => {
    assert.throws(() => parsePairingCode("ABC-DEF"), (e: PairingError) => e.code === "malformed");
    assert.throws(
      () => parsePairingCode("ABC-DEF-GHU"),
      (e: PairingError) => e.code === "malformed",
      "U is deliberately not in the alphabet",
    );
  });
});

describe("pairing, end to end", () => {
  test("a correct code yields a long-lived credential on both sides", async () => {
    const { host, client, clock } = pairingPair();
    const { credential } = await completePairing(client, host);

    assert.equal(credential.deviceId, "phone-abc");
    assert.equal(credential.boxId, "box-mini-01");
    assert.equal(credential.boxName, "Alexis's Mac mini");
    assert.equal(credential.protocolVersion, PAIRING_VERSION);
    assert.equal(credential.pairedAtMs, clock.now());
    assert.equal(credential.deviceToken.length, 64);
    assert.equal(credential.signingKey.length, 64);

    // The box derives exactly the same thing, without either being transmitted.
    const boxSide = host.issuedCredential();
    assert.equal(boxSide.deviceToken, credential.deviceToken);
    assert.equal(boxSide.signingKey, credential.signingKey);
  });

  test("the code stops being the credential the moment it is used", async () => {
    const { host, client } = pairingPair();
    await completePairing(client, host);

    assert.equal(host.state, PairingHostState.Consumed);

    const second = new PairingClient({
      code: host.code.text,
      deviceId: "phone-thief",
      platform: "android",
      deviceName: "someone else's phone",
      pake: testPake(countingRandom(99)),
    });
    await assert.rejects(
      completePairing(second, host),
      (e: PairingError) => e.code === "alreadyUsed",
    );
  });

  test("a code on a screen expires", async () => {
    const clock = new FakeClock(1_000);
    const { host, client } = pairingPair({ clock, ttlMs: 10 * 60 * 1000 });

    await clock.advance(10 * 60 * 1000);
    assert.equal(host.state, PairingHostState.Expired);
    await assert.rejects(completePairing(client, host), (e: PairingError) => e.code === "expired");
  });

  test("a wrong code fails confirmation and spends an attempt", async () => {
    const { host } = pairingPair();
    const wrong = newPairingCode(countingRandom(555));
    const attacker = new PairingClient({
      code: { slot: host.code.slot, secret: wrong.secret, text: "" },
      deviceId: "phone-thief",
      platform: "android",
      deviceName: "guesser",
      pake: testPake(countingRandom(31)),
    });

    // The box's own proof is what fails first, so the phone never sends its half.
    const hello = await attacker.hello();
    const accept = await host.handleHello(hello);
    await assert.rejects(
      attacker.accept(accept),
      (e: PairingError) => e.code === "confirmationFailed",
    );
  });

  test("thirty bits of secret survive only because guesses are counted", async () => {
    const { host } = pairingPair();
    const codes: PairingError[] = [];

    for (let guess = 0; guess < 3; guess++) {
      const wrong = newPairingCode(countingRandom(1_000 + guess));
      const attacker = new PairingClient({
        code: { slot: host.code.slot, secret: wrong.secret, text: "" },
        deviceId: `guess-${guess}`,
        platform: "android",
        deviceName: "guesser",
        pake: testPake(countingRandom(2_000 + guess)),
      });
      const hello = await attacker.hello();
      const accept = await host.handleHello(hello);
      // Skip the client's own check — a real attacker would.
      try {
        await host.handleConfirm({ v: PAIRING_VERSION, kind: "pair.confirm", confirm: accept.confirm });
      } catch (error) {
        codes.push(error as PairingError);
      }
    }

    assert.deepEqual(
      codes.map((e) => e.code),
      ["confirmationFailed", "confirmationFailed", "tooManyAttempts"],
    );
    assert.equal(host.state, PairingHostState.Locked);
    assert.equal(host.attemptsRemaining, 0);
  });

  test("a hello alone does not spend an attempt", async () => {
    const { host } = pairingPair();
    for (let i = 0; i < 20; i++) {
      const noise = new PairingClient({
        code: host.code,
        deviceId: `noise-${i}`,
        platform: "android",
        deviceName: "noise",
        pake: testPake(countingRandom(i + 1)),
      });
      await host.handleHello(await noise.hello());
    }
    assert.equal(host.attemptsRemaining, 3, "hellos prove nothing, so they cost nothing");
  });

  test("a hello for another box's slot is rejected without spending an attempt", async () => {
    const { host, client } = pairingPair();
    const hello = await client.hello();
    await assert.rejects(
      host.handleHello({ ...hello, slot: host.code.slot === "00" ? "11" : "00" }),
      (e: PairingError) => e.code === "malformed",
    );
    assert.equal(host.attemptsRemaining, 3);
  });

  test("out-of-order messages are refused rather than half-applied", async () => {
    const { host, client } = pairingPair();
    await assert.rejects(
      host.handleConfirm({ v: PAIRING_VERSION, kind: "pair.confirm", confirm: "AAAA" }),
      (e: PairingError) => e.code === "outOfOrder",
    );
    await assert.rejects(
      client.accept({ v: PAIRING_VERSION, kind: "pair.accept", pake: "", confirm: "" }),
      (e: PairingError) => e.code === "outOfOrder",
    );
  });

  test("a version mismatch is named, not guessed at", async () => {
    const { host, client } = pairingPair();
    const hello = await client.hello();
    await assert.rejects(
      host.handleHello({ ...hello, v: 99 }),
      (e: PairingError) => e.code === "versionMismatch",
    );
  });
});

describe("what the rendezvous relay can see", () => {
  test("the secret never appears in anything that crosses the wire", async () => {
    const { host, client } = pairingPair();
    const { transcript } = await completePairing(client, host);
    const wire = transcript.join("\n");

    assert.equal(wire.includes(host.code.secret), false, "the PAKE secret must never be sent");
    assert.equal(wire.includes(host.code.text), false);
    assert.equal(wire.includes(host.code.text.replace(/-/g, "")), false);
    // The slot is the public half — the relay needs it to join two dial-outs.
    assert.equal(transcript[0]!.includes(host.code.slot), true);
  });

  test("neither the token nor the signing key crosses the wire", async () => {
    const { host, client } = pairingPair();
    const { credential, transcript } = await completePairing(client, host);
    const wire = transcript.join("\n");

    assert.equal(wire.includes(credential.deviceToken), false);
    assert.equal(wire.includes(credential.signingKey), false);
  });

  test("box identity is sealed, so the relay cannot redirect the pairing", async () => {
    const { host, client } = pairingPair();
    const transcript: string[] = [];

    const hello = await client.hello();
    transcript.push(JSON.stringify(hello));
    const accept = await host.handleHello(hello);
    const confirm = await client.accept(accept);
    const grant = await host.handleConfirm(confirm);

    assert.equal(JSON.stringify(grant).includes("box-mini-01"), false);
    assert.equal(JSON.stringify(grant).includes("Mac mini"), false);
    assert.equal(client.grant(grant).boxId, "box-mini-01");
  });

  test("a relay that replays the whole transcript still cannot pair", async () => {
    const { host, client } = pairingPair();
    const hello = await client.hello();
    const accept = await host.handleHello(hello);
    const confirm = await client.accept(accept);

    // Everything the relay saw, replayed against a fresh box with the same code.
    const freshHost = new PairingHost({
      boxId: "box-mini-01",
      code: host.code,
      clock: new FakeClock(1_700_000_000_000),
      random: countingRandom(41),
      pake: testPake(countingRandom(41)),
    });
    await freshHost.handleHello(hello);
    await assert.rejects(
      freshHost.handleConfirm(confirm),
      (e: PairingError) => e.code === "confirmationFailed",
      "the confirmation binds the box's own ephemeral, so a captured one is useless",
    );
  });

  test("someone in the middle who rewrites the hello is caught", async () => {
    const { host, client } = pairingPair();
    const hello = await client.hello();

    // The attacker leaves the PAKE bytes alone but claims a different device —
    // the kind of tampering a relay is in a perfect position to do.
    const tampered: PairHello = { ...hello, deviceName: "attacker's phone" };
    const accept: PairAccept = await host.handleHello(tampered);

    await assert.rejects(
      client.accept(accept),
      (e: PairingError) => e.code === "confirmationFailed",
      "the transcript binds the hello, so a rewritten field breaks confirmation",
    );
  });
});

describe("link key derivation", () => {
  test("the same credential and nonce give both sides the same channel", async () => {
    const { host, client } = pairingPair();
    const { credential } = await completePairing(client, host);
    const nonce = countingRandom(5)(16);

    const phone = new SealedChannel(linkSealingKey(credential, nonce), PairingRole.Client);
    const box = new SealedChannel(linkSealingKey(host.issuedCredential(), nonce), PairingRole.Host);

    assert.equal(box.openText(phone.sealText("utterance")), "utterance");
  });

  test("a different nonce gives a different key", async () => {
    const { host, client } = pairingPair();
    const { credential } = await completePairing(client, host);

    const a = linkSealingKey(credential, new Uint8Array([1, 2, 3]));
    const b = linkSealingKey(credential, new Uint8Array([1, 2, 4]));
    assert.equal(equalBytes(a, b), false);
  });
});

describe("the fake PAKE cannot be shipped by accident", () => {
  // `docs/APPS-SCOPE.md` §4.3: the only implementation of `PakeEngine` in this
  // repo is not a PAKE, and an observer holding its transcript can brute-force
  // the 30-bit secret offline. A comment saying so is not a control; these are.

  test("it refuses to be built without the opt-in", () => {
    assert.throws(
      // The cast is the point: this is what a JavaScript caller, or a caller who
      // silenced the compiler, actually does.
      () => (unsafeTestOnlyPake as (options: unknown) => PakeEngine)({}),
      (e: PairingError) => e.code === "notCryptography",
    );
    assert.throws(
      () => (unsafeTestOnlyPake as (options: unknown) => PakeEngine)(undefined),
      (e: PairingError) => e.code === "notCryptography",
    );
    assert.throws(
      () =>
        (unsafeTestOnlyPake as (options: unknown) => PakeEngine)({
          iUnderstandThisIsNotCryptography: "yes",
        }),
      (e: PairingError) => e.code === "notCryptography",
      "the opt-in is the literal true, not anything truthy",
    );
  });

  test("the opt-in argument is the only way through", () => {
    const engine = unsafeTestOnlyPake({ iUnderstandThisIsNotCryptography: true });
    assert.equal(engine.name, TEST_ONLY_PAKE_NAME);
    assert.match(engine.name, /not-a-pake/, "the transcript has to say what it carried");
  });

  test("it refuses to build in anything that says it is production", () => {
    const previous = process.env.NODE_ENV;
    try {
      process.env.NODE_ENV = "production";
      assert.throws(
        () => unsafeTestOnlyPake({ iUnderstandThisIsNotCryptography: true }),
        (e: PairingError) =>
          e.code === "notCryptography" && /NODE_ENV=production/.test(e.message),
      );
    } finally {
      if (previous === undefined) delete process.env.NODE_ENV;
      else process.env.NODE_ENV = previous;
    }
  });

  test("a React Native release build is production too", () => {
    const globals = globalThis as { __DEV__?: unknown };
    const had = "__DEV__" in globals;
    const previous = globals.__DEV__;
    try {
      globals.__DEV__ = false;
      assert.throws(
        () => unsafeTestOnlyPake({ iUnderstandThisIsNotCryptography: true }),
        (e: PairingError) => e.code === "notCryptography" && /__DEV__/.test(e.message),
      );
      globals.__DEV__ = true;
      assert.ok(
        unsafeTestOnlyPake({ iUnderstandThisIsNotCryptography: true }),
        "a debug build is where this belongs",
      );
    } finally {
      if (had) globals.__DEV__ = previous;
      else delete globals.__DEV__;
    }
  });

  test("pairing has no default engine on either side", () => {
    const withoutPake = { boxId: "box-mini-01" } as unknown as ConstructorParameters<
      typeof PairingHost
    >[0];
    assert.throws(
      () => new PairingHost(withoutPake),
      (e: PairingError) => e.code === "notCryptography",
      "a `pake ?? mock` default is exactly how the mock ships",
    );

    const clientWithout = {
      code: newPairingCode(countingRandom(3)),
      deviceId: "phone-abc",
      platform: "ios",
      deviceName: "iPhone",
    } as unknown as ConstructorParameters<typeof PairingClient>[0];
    assert.throws(
      () => new PairingClient(clientWithout),
      (e: PairingError) => e.code === "notCryptography",
    );
  });

  test("an object that is not an engine is refused rather than duck-typed", () => {
    assert.throws(
      () =>
        new PairingHost({
          boxId: "box-mini-01",
          pake: { name: "looks real" } as unknown as PakeEngine,
        }),
      (e: PairingError) => e.code === "notCryptography",
    );
  });

  test("the transcript names the engine, so two platforms cannot disagree silently", async () => {
    const { client } = pairingPair();
    const hello = await client.hello();
    assert.equal(hello.pakeName, TEST_ONLY_PAKE_NAME);
  });

  test("it is honest about what it is not: its transcript falls to an offline search", async () => {
    // Not a defect being tested — the defining property being *recorded*. Two
    // captured frames are enough to try every candidate secret at home, with no
    // further contact with the box, so the box's three-attempt counter never
    // sees the search. That is exactly what a real PAKE prevents, and it is why
    // this engine may not ship.
    const { host, client } = pairingPair();
    const hello = await client.hello();
    const accept = await host.handleHello(hello);

    // Everything below uses only what crossed the relay.
    const capturedMessage = base64ToBytes(hello.pake);
    const candidates = [
      newPairingCode(countingRandom(101)).secret,
      newPairingCode(countingRandom(202)).secret,
      host.code.secret,
      newPairingCode(countingRandom(303)).secret,
    ];

    const recovered: string[] = [];
    for (const candidate of candidates) {
      const attacker = new PairingClient({
        code: { slot: hello.slot, secret: candidate, text: "" },
        deviceId: hello.deviceId,
        platform: hello.platform,
        deviceName: hello.deviceName,
        // Replaying the captured ephemeral is what makes the search offline: the
        // attacker reproduces the exact hello and checks the box's own proof.
        pake: testPake(() => capturedMessage),
      });
      await attacker.hello();
      try {
        await attacker.accept(accept);
        recovered.push(candidate);
      } catch {
        // wrong guess, and nothing anywhere heard about it
      }
    }

    assert.deepEqual(recovered, [host.code.secret], "the secret came back out of the transcript");
    assert.equal(
      host.attemptsRemaining,
      3,
      "the box counted none of those guesses, because none of them reached it",
    );
  });
});

describe("the hand-written primitives agree with a reference implementation", () => {
  // `pairing.ts` implements SHA-256, HMAC and HKDF by hand because `src/` may not
  // import `node:crypto` and React Native has no WebCrypto. The *test* has no
  // such constraint, so the hand-written code is checked differentially against
  // Node's OpenSSL-backed implementation as well as against the published
  // vectors above. Nobody should have to take the hand-written version on trust.

  const lengths = [0, 1, 31, 32, 54, 55, 56, 57, 63, 64, 65, 119, 120, 127, 128, 1_000];

  function pattern(length: number): Uint8Array {
    const out = new Uint8Array(length);
    for (let i = 0; i < length; i++) out[i] = (i * 31 + 7) & 0xff;
    return out;
  }

  test("sha256 matches node:crypto across the padding boundaries", () => {
    for (const length of lengths) {
      const input = pattern(length);
      assert.equal(
        bytesToHex(sha256(input)),
        nodeCrypto.createHash("sha256").update(input).digest("hex"),
        `sha256 diverges at ${length} bytes`,
      );
    }
  });

  test("hmacSha256 matches node:crypto, including keys longer than a block", () => {
    for (const keyLength of [0, 1, 32, 63, 64, 65, 131]) {
      const key = pattern(keyLength);
      for (const length of [0, 55, 64, 200]) {
        const data = pattern(length);
        assert.equal(
          bytesToHex(hmacSha256(key, data)),
          nodeCrypto.createHmac("sha256", key).update(data).digest("hex"),
          `hmac diverges at key ${keyLength} / data ${length}`,
        );
      }
    }
  });

  test("hkdf matches node:crypto over multiple output blocks", () => {
    for (const length of [1, 32, 33, 64, 128, 255]) {
      const ikm = pattern(40);
      const salt = pattern(13);
      const info = pattern(10);
      const expected = Buffer.from(
        nodeCrypto.hkdfSync("sha256", ikm, salt, info, length),
      ).toString("hex");
      assert.equal(bytesToHex(hkdf(ikm, salt, info, length)), expected, `hkdf diverges at ${length}`);
    }
  });

  test("hkdf refuses more than the RFC's 255 blocks rather than wrapping the counter", () => {
    assert.throws(
      () => hkdf(new Uint8Array(32), new Uint8Array(0), new Uint8Array(0), 255 * 32 + 1),
      (e: PairingError) => e.code === "malformed",
    );
  });
});
