/**
 * Pairing — glasses ↔ phone ↔ box, across a relay that must never learn the secret.
 *
 * ---------------------------------------------------------------------------
 * ## Read this before you read the code: this file contains hand-written crypto
 *
 * SHA-256, HMAC-SHA-256 and HKDF are implemented here, by hand, in TypeScript.
 * That is a thing you are normally right to refuse. It is done here for one
 * reason and under one set of conditions, both of which are load-bearing:
 *
 * **Why.** `src/` may not import `node:crypto` — it runs unchanged in React
 * Native, in a browser and under `node --test` (`docs/APPS-SCOPE.md` §5.1) — and
 * React Native ships no WebCrypto. WebCrypto would also be `async` and unusable
 * from the deterministic, synchronous derivations below. There was no primitive
 * to call.
 *
 * **What backs it.** Every primitive is pinned in `test/pairing.test.ts` against
 * two independent references:
 *
 *   - the published vectors — FIPS 180-4 / RFC 6234 for SHA-256, RFC 4231 for
 *     HMAC-SHA-256, RFC 5869 for HKDF, including the block-boundary and
 *     over-long-key cases that a round-trip test cannot catch, and
 *   - `node:crypto` itself, differentially, over a range of input lengths that
 *     crosses the 55/56- and 64-byte padding boundaries. The *test* may import
 *     `node:crypto`; only `src/` may not.
 *
 * A hash that is wrong is self-consistent and useless, so "it round-trips" is
 * not evidence of anything. The vectors are the evidence.
 *
 * **What this is not.** It has not been reviewed as a crypto library, it is not
 * constant-time beyond [equalBytes], and it is not the shipping cipher path on
 * either phone. The native apps use platform crypto for the same constructions:
 * `apps/ios/RelayKit` uses CryptoKit (`SHA256`, `HMAC<SHA256>`) and
 * `apps/android/.../connector/ConnectorClient.kt` uses `MessageDigest` and
 * `javax.crypto.Mac`. Treat the TypeScript below as the executable reference the
 * two platforms have to agree with, and as the thing that makes pairing testable
 * with no device — not as the cipher that ships.
 *
 * **And the group operations are not here at all.** See "the PAKE seam" below:
 * [PakeEngine] is injected, and the only implementation in this repo is
 * [unsafeTestOnlyPake], which is not a PAKE and refuses to run outside a test.
 * ---------------------------------------------------------------------------
 *
 * The installer prints a code (`docs/ORCHESTRATOR.md` §2, step 5), the user reads
 * it into the phone, and from then on the link is authenticated. Four properties
 * are load-bearing and each one is a test below:
 *
 *   short       nine characters, Crockford base32, read aloud from a terminal
 *   single-use  a code that pairs a second phone is a code that pairs an attacker's
 *   expiring    a code on a screen at a conference is a credential on a screen
 *   exchangeable  possession yields a long-lived credential; the code itself dies
 *
 * The fourth is the one that is easy to get wrong. A short code that stays the
 * credential is a 30-bit password on every request for the life of the device.
 *
 * ## Why the relay forces a PAKE
 *
 * `docs/SYSTEM.md` §7: the phone usually reaches the box through *our* rendezvous
 * relay, which pipes bytes it cannot read. "Cannot read" has to be true against us,
 * not merely intended by us. If the pairing transcript let an observer recover the
 * code offline, a 30-bit secret falls in seconds and the relay operator — us —
 * could mint a credential.
 *
 * So the code authenticates a key exchange rather than travelling over it. That is
 * a PAKE, and a PAKE needs group operations this package cannot carry: `src/` has
 * zero dependencies and no Node built-ins, because it runs in React Native, in a
 * browser and under `node --test` unchanged. The primitive is therefore injected
 * as [PakeEngine] and supplied per platform — see "the PAKE seam" for the exact
 * library each platform is expected to bind and what the interface demands of it.
 *
 * Everything *around* the primitive — code format, expiry, attempt limits,
 * transcript binding, key confirmation, credential derivation — is here, is the
 * part that is normally got wrong, and is fully tested with
 * [unsafeTestOnlyPake].
 *
 * There is no default engine. [PairingHost] and [PairingClient] both require one
 * to be passed, because a default is how a mock ships.
 *
 * ## Nothing secret crosses the wire
 *
 * The device token and signing key are **derived** from the PAKE output on both
 * sides rather than transmitted. The sealed grant carries only box identity, and
 * is sealed so a relay cannot tamper with which box you think you paired to.
 */

import type { Clock } from "./clock.ts";
import { systemClock } from "./clock.ts";
import { base64ToBytes, bytesToBase64 } from "./trace.ts";

export const PAIRING_VERSION = 1;

// --- bytes ------------------------------------------------------------------

const TEXT_ENCODER = new TextEncoder();
const TEXT_DECODER = new TextDecoder();

export function utf8(text: string): Uint8Array {
  return TEXT_ENCODER.encode(text);
}

export function fromUtf8(bytes: Uint8Array): string {
  return TEXT_DECODER.decode(bytes);
}

export function bytesToHex(bytes: Uint8Array): string {
  let out = "";
  for (const byte of bytes) out += byte.toString(16).padStart(2, "0");
  return out;
}

export function hexToBytes(hex: string): Uint8Array {
  // Validated rather than coerced: `parseInt("zz", 16)` is NaN, which a typed
  // array stores as 0 — so an unchecked signing key would silently become zeros.
  if (hex.length % 2 !== 0 || !/^[0-9a-fA-F]*$/.test(hex)) {
    throw new PairingError(PairingErrorCode.Malformed, "not an even-length hex string");
  }
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = Number.parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  return out;
}

/**
 * Length-prefixed concatenation.
 *
 * Every byte string that goes into a hash goes through this. Plain concatenation
 * makes `("ab", "c")` and `("a", "bc")` the same input, which is how transcript
 * binding quietly stops binding anything.
 */
export function field(...parts: Array<Uint8Array | string>): Uint8Array {
  const encoded = parts.map((part) => (typeof part === "string" ? utf8(part) : part));
  const total = encoded.reduce((sum, part) => sum + 4 + part.length, 0);
  const out = new Uint8Array(total);
  const view = new DataView(out.buffer);
  let offset = 0;
  for (const part of encoded) {
    view.setUint32(offset, part.length, false);
    out.set(part, offset + 4);
    offset += 4 + part.length;
  }
  return out;
}

/** Constant time. A fast-exit compare on a MAC leaks the MAC one byte at a time. */
export function equalBytes(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length || a.length === 0) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a[i]! ^ b[i]!;
  return diff === 0;
}

// --- SHA-256 ----------------------------------------------------------------

const K256 = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

const SHA256_BLOCK_BYTES = 64;

function rotr(x: number, n: number): number {
  return ((x >>> n) | (x << (32 - n))) >>> 0;
}

export function sha256(data: Uint8Array): Uint8Array {
  const state = new Uint32Array([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ]);

  const paddedLength = Math.ceil((data.length + 9) / SHA256_BLOCK_BYTES) * SHA256_BLOCK_BYTES;
  const padded = new Uint8Array(paddedLength);
  padded.set(data);
  padded[data.length] = 0x80;

  const view = new DataView(padded.buffer);
  // Length in bits as a 64-bit big-endian count. Split rather than shifted: a
  // 512 MB input overflows a 32-bit shift and silently hashes as a shorter one.
  view.setUint32(paddedLength - 8, Math.floor(data.length / 0x20000000), false);
  view.setUint32(paddedLength - 4, (data.length * 8) >>> 0, false);

  const w = new Uint32Array(64);

  for (let offset = 0; offset < paddedLength; offset += SHA256_BLOCK_BYTES) {
    for (let t = 0; t < 16; t++) w[t] = view.getUint32(offset + t * 4, false);
    for (let t = 16; t < 64; t++) {
      const x = w[t - 15]!;
      const y = w[t - 2]!;
      const s0 = (rotr(x, 7) ^ rotr(x, 18) ^ (x >>> 3)) >>> 0;
      const s1 = (rotr(y, 17) ^ rotr(y, 19) ^ (y >>> 10)) >>> 0;
      w[t] = (w[t - 16]! + s0 + w[t - 7]! + s1) >>> 0;
    }

    let a = state[0]!;
    let b = state[1]!;
    let c = state[2]!;
    let d = state[3]!;
    let e = state[4]!;
    let f = state[5]!;
    let g = state[6]!;
    let h = state[7]!;

    for (let t = 0; t < 64; t++) {
      const S1 = (rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25)) >>> 0;
      const ch = ((e & f) ^ (~e & g)) >>> 0;
      const temp1 = (h + S1 + ch + K256[t]! + w[t]!) >>> 0;
      const S0 = (rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22)) >>> 0;
      const maj = ((a & b) ^ (a & c) ^ (b & c)) >>> 0;
      const temp2 = (S0 + maj) >>> 0;

      h = g;
      g = f;
      f = e;
      e = (d + temp1) >>> 0;
      d = c;
      c = b;
      b = a;
      a = (temp1 + temp2) >>> 0;
    }

    state[0] = (state[0]! + a) >>> 0;
    state[1] = (state[1]! + b) >>> 0;
    state[2] = (state[2]! + c) >>> 0;
    state[3] = (state[3]! + d) >>> 0;
    state[4] = (state[4]! + e) >>> 0;
    state[5] = (state[5]! + f) >>> 0;
    state[6] = (state[6]! + g) >>> 0;
    state[7] = (state[7]! + h) >>> 0;
  }

  const digest = new Uint8Array(32);
  const digestView = new DataView(digest.buffer);
  for (let i = 0; i < 8; i++) digestView.setUint32(i * 4, state[i]!, false);
  return digest;
}

export function hmacSha256(key: Uint8Array, data: Uint8Array): Uint8Array {
  const block = new Uint8Array(SHA256_BLOCK_BYTES);
  block.set(key.length > SHA256_BLOCK_BYTES ? sha256(key) : key);

  const inner = new Uint8Array(SHA256_BLOCK_BYTES + data.length);
  const outer = new Uint8Array(SHA256_BLOCK_BYTES + 32);
  for (let i = 0; i < SHA256_BLOCK_BYTES; i++) {
    inner[i] = block[i]! ^ 0x36;
    outer[i] = block[i]! ^ 0x5c;
  }
  inner.set(data, SHA256_BLOCK_BYTES);
  outer.set(sha256(inner), SHA256_BLOCK_BYTES);
  return sha256(outer);
}

/** RFC 5869 extract-then-expand. `length` is capped at 255 × 32 by the RFC. */
export function hkdf(
  ikm: Uint8Array,
  salt: Uint8Array,
  info: Uint8Array,
  length: number,
): Uint8Array {
  if (length > 255 * 32) {
    throw new PairingError(PairingErrorCode.Malformed, "hkdf: length exceeds 255 blocks");
  }
  const prk = hmacSha256(salt, ikm);
  const out = new Uint8Array(length);
  let previous: Uint8Array = new Uint8Array(0);
  for (let block = 1; (block - 1) * 32 < length; block++) {
    const input = new Uint8Array(previous.length + info.length + 1);
    input.set(previous);
    input.set(info, previous.length);
    input[input.length - 1] = block;
    previous = hmacSha256(prk, input);
    out.set(previous.subarray(0, Math.min(32, length - (block - 1) * 32)), (block - 1) * 32);
  }
  return out;
}

// --- randomness -------------------------------------------------------------

export type RandomSource = (byteCount: number) => Uint8Array;

/**
 * `globalThis.crypto.getRandomValues` where it exists, and a loud failure where it
 * does not.
 *
 * React Native ships no WebCrypto; the app installs a polyfill or passes its own
 * source. Falling back to `Math.random` would produce pairing codes that look fine
 * and are guessable, which is the worst available outcome — so it throws instead.
 */
export const webCryptoRandom: RandomSource = (byteCount) => {
  const webcrypto = (globalThis as { crypto?: Crypto }).crypto;
  if (!webcrypto?.getRandomValues) {
    throw new PairingError(
      PairingErrorCode.NoRandomSource,
      "no crypto.getRandomValues; pass an explicit RandomSource (React Native needs a polyfill)",
    );
  }
  const out = new Uint8Array(byteCount);
  webcrypto.getRandomValues(out);
  return out;
};

/**
 * Deterministic bytes for tests. **Never ship this.**
 *
 * Exported because the alternative is every test file rewriting it slightly
 * differently, and a subtly different one is how a test stops testing.
 */
export function countingRandom(seed = 1): RandomSource {
  let counter = seed;
  return (byteCount) => {
    const out = new Uint8Array(byteCount);
    for (let i = 0; i < byteCount; i++) {
      counter = (counter * 1103515245 + 12345) >>> 0;
      out[i] = (counter >>> 16) & 0xff;
    }
    return out;
  };
}

// --- errors -----------------------------------------------------------------

export const PairingErrorCode = {
  /** The text is not a pairing code at all. */
  Malformed: "malformed",
  /** Right shape, failed the check character — almost always a typo. */
  Checksum: "checksum",
  Expired: "expired",
  /** Already exchanged for a credential. Codes pair exactly one device. */
  AlreadyUsed: "alreadyUsed",
  /** Too many wrong codes. The code is dead; the installer prints a new one. */
  TooManyAttempts: "tooManyAttempts",
  /** Key confirmation failed: wrong code, or someone in the middle. */
  ConfirmationFailed: "confirmationFailed",
  /** A sealed payload did not authenticate. */
  TamperDetected: "tamperDetected",
  /** Messages arrived in an order the protocol does not allow. */
  OutOfOrder: "outOfOrder",
  VersionMismatch: "versionMismatch",
  NoRandomSource: "noRandomSource",
  /** Something asked for the fake PAKE without saying so, or asked for it in production. */
  NotCryptography: "notCryptography",
} as const;

export type PairingErrorCode = (typeof PairingErrorCode)[keyof typeof PairingErrorCode];

export class PairingError extends Error {
  readonly code: PairingErrorCode;

  constructor(code: PairingErrorCode, message: string) {
    super(message);
    this.name = "PairingError";
    this.code = code;
  }
}

// --- sealing ----------------------------------------------------------------

export interface SealedFrame {
  /** base64; unique per frame per key. */
  nonce: string;
  ciphertext: string;
  tag: string;
}

/**
 * Keystream in 32-byte HMAC blocks rather than HKDF-Expand.
 *
 * HKDF-Expand caps at 255 × 32 = 8,160 bytes, and an `audio.chunk` envelope can
 * exceed that. A cap that is *usually* enough is a bug that only shows up on long
 * utterances, so this counts blocks directly and has no ceiling.
 */
function keystream(key: Uint8Array, nonce: Uint8Array, label: string, length: number): Uint8Array {
  const prk = hmacSha256(nonce, key);
  const out = new Uint8Array(length);
  const counter = new Uint8Array(4);
  const counterView = new DataView(counter.buffer);
  for (let block = 0; block * 32 < length; block++) {
    counterView.setUint32(0, block, false);
    const chunk = hmacSha256(prk, field(label, nonce, counter));
    out.set(chunk.subarray(0, Math.min(32, length - block * 32)), block * 32);
  }
  return out;
}

function macKeyFor(key: Uint8Array, nonce: Uint8Array, label: string): Uint8Array {
  return hkdf(key, nonce, utf8(`${label} mac`), 32);
}

/**
 * Encrypt-then-MAC with a one-time keystream.
 *
 * Sound only because the nonce never repeats under a key: reuse XORs two
 * plaintexts together. [SealedChannel] owns nonce discipline; call this directly
 * only for a one-shot payload under a freshly derived key.
 */
export function seal(
  key: Uint8Array,
  label: string,
  nonce: Uint8Array,
  plaintext: Uint8Array,
): SealedFrame {
  const stream = keystream(key, nonce, label, plaintext.length);
  const ciphertext = new Uint8Array(plaintext.length);
  for (let i = 0; i < plaintext.length; i++) ciphertext[i] = plaintext[i]! ^ stream[i]!;
  const tag = hmacSha256(macKeyFor(key, nonce, label), field(label, nonce, ciphertext));
  return {
    nonce: bytesToBase64(nonce),
    ciphertext: bytesToBase64(ciphertext),
    tag: bytesToBase64(tag),
  };
}

export function openSealed(key: Uint8Array, label: string, frame: SealedFrame): Uint8Array {
  const nonce = base64ToBytes(frame.nonce);
  const ciphertext = base64ToBytes(frame.ciphertext);
  const expected = hmacSha256(macKeyFor(key, nonce, label), field(label, nonce, ciphertext));
  if (!equalBytes(expected, base64ToBytes(frame.tag))) {
    throw new PairingError(PairingErrorCode.TamperDetected, "sealed frame failed authentication");
  }
  const stream = keystream(key, nonce, label, ciphertext.length);
  const plaintext = new Uint8Array(ciphertext.length);
  for (let i = 0; i < ciphertext.length; i++) plaintext[i] = ciphertext[i]! ^ stream[i]!;
  return plaintext;
}

export const PairingRole = {
  /** The phone. Enters the code. */
  Client: "client",
  /** The box. Printed the code. */
  Host: "host",
} as const;

export type PairingRole = (typeof PairingRole)[keyof typeof PairingRole];

/**
 * A one-key, two-direction sealed channel with replay rejection.
 *
 * This is what makes `docs/SYSTEM.md` §7's promise structural rather than a
 * policy: the relay pipes these frames and holds none of the keys. Each direction
 * gets its own label so the two never share a keystream, and the nonce is a
 * counter so a replayed frame is refused rather than delivered twice.
 *
 * Not forward-secret: the key comes from the long-lived credential, so a stolen
 * credential decrypts recorded traffic. That is an accepted layer under the
 * box's own mTLS/WireGuard (`connector/src/protocol.ts`), not a substitute for it.
 */
export class SealedChannel {
  #key: Uint8Array;
  #txLabel: string;
  #rxLabel: string;
  #txCounter = 0;
  #rxHighWater = -1;

  constructor(key: Uint8Array, role: PairingRole) {
    if (key.length < 16) {
      throw new PairingError(PairingErrorCode.Malformed, "sealed channel key too short");
    }
    this.#key = key;
    this.#txLabel = `relay/seal/v1 ${role}`;
    this.#rxLabel = `relay/seal/v1 ${role === PairingRole.Client ? PairingRole.Host : PairingRole.Client}`;
  }

  seal(plaintext: Uint8Array): SealedFrame {
    const nonce = new Uint8Array(8);
    new DataView(nonce.buffer).setUint32(4, this.#txCounter++, false);
    return seal(this.#key, this.#txLabel, nonce, plaintext);
  }

  open(frame: SealedFrame): Uint8Array {
    const nonce = base64ToBytes(frame.nonce);
    if (nonce.length !== 8) {
      throw new PairingError(PairingErrorCode.Malformed, "sealed frame nonce must be 8 bytes");
    }
    const counter = new DataView(nonce.buffer, nonce.byteOffset, nonce.byteLength).getUint32(4);
    if (counter <= this.#rxHighWater) {
      throw new PairingError(
        PairingErrorCode.OutOfOrder,
        `replayed or reordered frame: nonce ${counter} <= ${this.#rxHighWater}`,
      );
    }
    const plaintext = openSealed(this.#key, this.#rxLabel, frame);
    this.#rxHighWater = counter;
    return plaintext;
  }

  sealText(text: string): SealedFrame {
    return this.seal(utf8(text));
  }

  openText(frame: SealedFrame): string {
    return fromUtf8(this.open(frame));
  }
}

// --- the pairing code -------------------------------------------------------

/**
 * Crockford base32: no I, L, O or U.
 *
 * I/L/1 and O/0 are the pairs people get wrong reading a terminal aloud, and U is
 * excluded so a random code cannot spell something the user has to say to a
 * colleague. Input decoding maps the confusable letters back rather than rejecting
 * them, because "I typed an O" should pair, not fail.
 */
const ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

const SLOT_CHARS = 2;
const SECRET_CHARS = 6;
const CODE_CHARS = SLOT_CHARS + SECRET_CHARS + 1;

export interface PairingCode {
  /**
   * Public rendezvous slot. The relay needs *something* to join two dial-outs, so
   * this part is deliberately not secret — the same split Magic Wormhole uses.
   */
  slot: string;
  /** The PAKE secret. 30 bits, which is only safe because guesses are counted. */
  secret: string;
  /** Canonical rendering: three groups of three. */
  text: string;
}

function checksumChar(body: string): string {
  let sum = 0;
  for (let i = 0; i < body.length; i++) sum += (ALPHABET.indexOf(body[i]!) + 1) * (i + 1);
  return ALPHABET[sum % 32]!;
}

export function formatPairingCode(slot: string, secret: string): string {
  const body = `${slot}${secret}`;
  const full = `${body}${checksumChar(body)}`;
  return `${full.slice(0, 3)}-${full.slice(3, 6)}-${full.slice(6, 9)}`;
}

/** What the installer prints. */
export function newPairingCode(random: RandomSource = webCryptoRandom): PairingCode {
  const bytes = random(SLOT_CHARS + SECRET_CHARS);
  let slot = "";
  let secret = "";
  for (let i = 0; i < SLOT_CHARS; i++) slot += ALPHABET[bytes[i]! % 32];
  for (let i = 0; i < SECRET_CHARS; i++) secret += ALPHABET[bytes[SLOT_CHARS + i]! % 32];
  return { slot, secret, text: formatPairingCode(slot, secret) };
}

/**
 * Accept what a person actually types: any case, any separators, O for 0, I or L
 * for 1. The check character catches the typo before a round trip and before it
 * spends one of the box's three attempts.
 */
export function parsePairingCode(input: string): PairingCode {
  const cleaned = input
    .toUpperCase()
    .replace(/[^0-9A-Z]/g, "")
    .replace(/O/g, "0")
    .replace(/[IL]/g, "1");

  if (cleaned.length !== CODE_CHARS) {
    throw new PairingError(
      PairingErrorCode.Malformed,
      `pairing code must be ${CODE_CHARS} characters, got ${cleaned.length}`,
    );
  }
  for (const char of cleaned) {
    if (!ALPHABET.includes(char)) {
      throw new PairingError(PairingErrorCode.Malformed, `"${char}" is not a pairing-code character`);
    }
  }

  const body = cleaned.slice(0, SLOT_CHARS + SECRET_CHARS);
  if (cleaned[CODE_CHARS - 1] !== checksumChar(body)) {
    throw new PairingError(PairingErrorCode.Checksum, "pairing code check character does not match");
  }

  const slot = body.slice(0, SLOT_CHARS);
  const secret = body.slice(SLOT_CHARS);
  return { slot, secret, text: formatPairingCode(slot, secret) };
}

// --- the PAKE seam ----------------------------------------------------------

/**
 * One run of the balanced PAKE. Begin, hand the peer [message], finish with
 * theirs.
 *
 * One run is **one online guess**, and the whole security argument depends on
 * that: 30 bits of secret is safe only because [PairingHost] counts wrong
 * confirmations and locks the code at three. An engine that let a caller test
 * several secrets against one transcript would silently remove the counting.
 */
export interface PakeSession {
  /** The bytes to hand the peer. Safe for the relay to see. */
  readonly message: Uint8Array;
  /** Shared 32-byte key. Both sides agree only if both used the same secret. */
  finish(peerMessage: Uint8Array): Promise<Uint8Array>;
}

/**
 * The primitive this file refuses to implement.
 *
 * ## What each platform is expected to bind
 *
 * The primitive is **CPace** (CFRG draft-irtf-cfrg-cpace, ristretto255 cipher
 * suite) or **SPAKE2** (RFC 9382), from a reviewed implementation, pinned by
 * version. Not hand-rolled here, not hand-rolled there. Candidates, to be
 * confirmed against each library's current API and recorded in
 * `docs/APPS-SCOPE.md` §4.3 once chosen:
 *
 *   iOS       a C CPace/SPAKE2 implementation over ristretto255 (jedisct1's
 *             `libcpace`, built on libsodium, or BoringSSL's `SPAKE2_CTX`),
 *             linked as a SwiftPM system-library target and wrapped in a Swift
 *             type conforming to this interface. **CryptoKit cannot do this**:
 *             it exposes no ristretto255 group and no hash-to-group, so there is
 *             nothing to build a PAKE out of.
 *   Android   the same C library through JNI, or the RustCrypto `spake2` crate
 *             (the one magic-wormhole uses) through UniFFI. Neither the JCA nor
 *             Tink ships a PAKE.
 *   box       `relayd` owns its own binding; it must be the *same* primitive and
 *             the same suite, or the two halves derive different keys and the
 *             failure surfaces as "confirmation failed" with no cause.
 *
 * ## What this interface demands of whichever one is chosen
 *
 *   - `secret` is the six-character Crockford base32 code as UTF-8. It is
 *     low-entropy **by design** — that is what a PAKE is for. It must be fed as
 *     the password input, never as a key.
 *   - `associatedData` must be bound into the protocol run. CPace takes it as
 *     `AD`; SPAKE2 takes identities plus context. If a library exposes no such
 *     parameter, bind it by hashing — `password' = H(secret ‖ associatedData)` —
 *     and say so at the binding site. Unbound, the transcript binding above
 *     stops binding anything and a relay can splice two runs together.
 *   - `message` must be safe to publish: it is handed to our own relay.
 *   - `finish` must reject a malformed, identity or low-order peer element
 *     rather than deriving something from it, and must return the library's
 *     32-byte ISK — a raw group element is not a key.
 *   - `finish` must be callable once per session. Reuse breaks the one-guess
 *     rule above.
 *   - No fallback path. An engine that degrades to something weaker when a
 *     library is missing is the bug this seam exists to prevent.
 *   - Production randomness comes from the platform CSPRNG. Injected
 *     deterministic bytes are a test facility and nothing else.
 *
 * [name] rides in `pair.hello` so a phone and a box that bound different
 * primitives fail with a sentence rather than an unexplained MAC mismatch.
 */
export interface PakeEngine {
  /** Recorded in the transcript so a mismatched pair fails loudly, not subtly. */
  readonly name: string;
  begin(role: PairingRole, secret: Uint8Array, associatedData: Uint8Array): Promise<PakeSession>;
}

/** The name [unsafeTestOnlyPake] puts in the transcript. */
export const TEST_ONLY_PAKE_NAME = "unsafe-test-only-not-a-pake";

export interface TestOnlyPakeOptions {
  /**
   * Required, and spelled out. There is no way to construct the fake engine that
   * does not read, at the call site, as a statement that it is not cryptography.
   */
  iUnderstandThisIsNotCryptography: true;
  random?: RandomSource;
  /**
   * Override the production check, for the one test that has to prove the check
   * fires. Nothing else may pass it, and it is not a way to ship.
   */
  ignoreProductionEnvironment?: boolean;
}

/**
 * Signals that this is a real build rather than a test run.
 *
 * Deliberately signal-based rather than "am I under a test runner": the question
 * that matters is whether this could be a user's phone, and the honest answer
 * when in doubt is *maybe*. Each signal below is an explicit statement by the
 * environment that it is production.
 */
function productionSignals(): string[] {
  const found: string[] = [];
  const env = (globalThis as { process?: { env?: Record<string, string | undefined> } }).process
    ?.env;
  if (env?.NODE_ENV === "production") found.push("process.env.NODE_ENV=production");
  if (env?.RELAY_ENV === "production") found.push("process.env.RELAY_ENV=production");
  // React Native's global: false in a release build, true in a debug one.
  const dev = (globalThis as { __DEV__?: unknown }).__DEV__;
  if (dev === false) found.push("__DEV__=false (React Native release build)");
  return found;
}

/**
 * **NOT A PAKE. NOT CRYPTOGRAPHY. TESTS AND SIMULATOR ONLY.**
 *
 * It models the one property the protocol above depends on — same secret gives the
 * same key, different secret gives a different one — so every state transition,
 * attempt limit and confirmation check is exercised for real.
 *
 * What it does not model is offline resistance: an observer holding this
 * transcript can brute-force the 30-bit secret, which is *exactly* the property a
 * real PAKE exists to provide. Shipping it would hand the relay operator — us —
 * every credential the relay ever carried, and `docs/SYSTEM.md` §7 says the relay
 * learns nothing.
 *
 * A comment saying so is not a control, so there are three real ones:
 *
 *   1. the name — nothing reads `unsafeTestOnlyPake(...)` as production code;
 *   2. [TestOnlyPakeOptions.iUnderstandThisIsNotCryptography], which must be
 *      passed explicitly and cannot arrive by defaulting;
 *   3. a runtime throw when the environment says it is production, so a build
 *      that somehow reaches a phone fails at pairing instead of pairing badly.
 *
 * And a fourth, structural one: [PairingHost] and [PairingClient] have no default
 * engine, so nothing falls back to this by omission.
 */
export function unsafeTestOnlyPake(options: TestOnlyPakeOptions): PakeEngine {
  if (options?.iUnderstandThisIsNotCryptography !== true) {
    throw new PairingError(
      PairingErrorCode.NotCryptography,
      "unsafeTestOnlyPake is not cryptography and must be asked for explicitly: " +
        "pass { iUnderstandThisIsNotCryptography: true }",
    );
  }
  if (options.ignoreProductionEnvironment !== true) {
    const signals = productionSignals();
    if (signals.length > 0) {
      throw new PairingError(
        PairingErrorCode.NotCryptography,
        `refusing to build the fake PAKE in what looks like production (${signals.join(", ")}); ` +
          "bind a reviewed CPace or SPAKE2 implementation — see PakeEngine",
      );
    }
  }

  const random = options.random ?? webCryptoRandom;
  return {
    name: TEST_ONLY_PAKE_NAME,
    async begin(_role, secret, associatedData) {
      const ephemeral = random(32);
      return {
        message: ephemeral,
        async finish(peerMessage: Uint8Array): Promise<Uint8Array> {
          if (peerMessage.length !== 32) {
            throw new PairingError(PairingErrorCode.Malformed, "test-only pake: bad peer message");
          }
          // Order-independent so both sides mix the same transcript.
          const mine = bytesToBase64(ephemeral);
          const theirs = bytesToBase64(peerMessage);
          const [lo, hi] = mine <= theirs ? [ephemeral, peerMessage] : [peerMessage, ephemeral];
          return hkdf(secret, field(lo, hi, associatedData), utf8("mock-pake"), 32);
        },
      };
    },
  };
}

/**
 * The type system already requires an engine; this is what catches the caller
 * the type system does not see — plain JavaScript from the React Native side, or
 * an options object assembled from config where the field came out `undefined`.
 * Failing here is a crash at pairing. Not failing here is a silent downgrade.
 */
function requirePake(pake: PakeEngine | undefined): PakeEngine {
  if (!pake || typeof pake.begin !== "function" || typeof pake.name !== "string") {
    throw new PairingError(
      PairingErrorCode.NotCryptography,
      "pairing needs a PakeEngine and has no default; bind a reviewed CPace or SPAKE2 " +
        "implementation (see PakeEngine), or unsafeTestOnlyPake in a test",
    );
  }
  return pake;
}

// --- wire messages ----------------------------------------------------------

export interface PairHello {
  v: number;
  kind: "pair.hello";
  slot: string;
  deviceId: string;
  platform: string;
  deviceName: string;
  pake: string;
  pakeName: string;
}

export interface PairAccept {
  v: number;
  kind: "pair.accept";
  pake: string;
  /** The box proving it knows the code, before the phone commits to anything. */
  confirm: string;
}

export interface PairConfirm {
  v: number;
  kind: "pair.confirm";
  confirm: string;
}

export interface PairGrant {
  v: number;
  kind: "pair.grant";
  sealed: SealedFrame;
}

export type PairingMessage = PairHello | PairAccept | PairConfirm | PairGrant;

/**
 * What pairing produces. `deviceToken` and `signingKey` are **derived** on both
 * sides from the PAKE output and never appear on the wire — so even a relay that
 * broke the sealing would have nothing to replay.
 */
export interface DeviceCredential {
  deviceId: string;
  boxId: string;
  boxName?: string;
  /** Bearer identity, hex. */
  deviceToken: string;
  /** HMAC key for link auth and body signing, hex. */
  signingKey: string;
  protocolVersion: number;
  pairedAtMs: number;
}

/** The non-secret half of the grant, sealed so the relay cannot rewrite it. */
interface GrantBody {
  boxId: string;
  boxName?: string;
  protocolVersion: number;
  issuedAtMs: number;
}

// --- derivation -------------------------------------------------------------

const CONFIRM_INFO = "relay/pair/v1 confirm";
const TOKEN_INFO = "relay/pair/v1 device-token";
const SIGNING_INFO = "relay/pair/v1 signing-key";
const GRANT_INFO = "relay/pair/v1 grant";
const GRANT_LABEL = "relay/pair/v1 grant-seal";

function helloBytes(hello: PairHello): Uint8Array {
  return field(
    "pair.hello",
    String(hello.v),
    hello.slot,
    hello.deviceId,
    hello.platform,
    hello.deviceName,
    hello.pakeName,
    base64ToBytes(hello.pake),
  );
}

/**
 * The hello as it stood *before* its own PAKE message was filled in.
 *
 * This is what the PAKE binds to, and both sides have to reconstruct it byte for
 * byte: the client cannot include a message it has not generated yet, and the host
 * must therefore blank the same field rather than hashing what it received.
 */
function helloContext(hello: PairHello): Uint8Array {
  return helloBytes({ ...hello, pake: "" });
}

function transcriptOf(hello: PairHello, hostPake: Uint8Array): Uint8Array {
  return sha256(field("relay/pair/v1", helloBytes(hello), hostPake));
}

function confirmMac(sharedKey: Uint8Array, role: PairingRole, transcript: Uint8Array): string {
  const key = hkdf(sharedKey, transcript, utf8(CONFIRM_INFO), 32);
  return bytesToBase64(hmacSha256(key, field(CONFIRM_INFO, role, transcript)));
}

function credentialFrom(
  sharedKey: Uint8Array,
  transcript: Uint8Array,
  deviceId: string,
  body: GrantBody,
  pairedAtMs: number,
): DeviceCredential {
  return {
    deviceId,
    boxId: body.boxId,
    boxName: body.boxName,
    deviceToken: bytesToHex(hkdf(sharedKey, transcript, utf8(TOKEN_INFO), 32)),
    signingKey: bytesToHex(hkdf(sharedKey, transcript, utf8(SIGNING_INFO), 32)),
    protocolVersion: body.protocolVersion,
    pairedAtMs,
  };
}

/** Derive the per-link sealing key from a credential. See `relayd.ts`. */
export function linkSealingKey(credential: DeviceCredential, linkNonce: Uint8Array): Uint8Array {
  return hkdf(hexToBytes(credential.signingKey), linkNonce, utf8("relay/link/v1 seal"), 32);
}

// --- the box side -----------------------------------------------------------

export const PairingHostState = {
  /** Printed, nobody has claimed it. */
  Open: "open",
  /** A phone has said hello and owes a confirmation. */
  AwaitingConfirm: "awaitingConfirm",
  /** Exchanged for a credential. Done. */
  Consumed: "consumed",
  Expired: "expired",
  /** Burned by wrong guesses. The installer prints a new one. */
  Locked: "locked",
} as const;

export type PairingHostState = (typeof PairingHostState)[keyof typeof PairingHostState];

export interface PairingHostOptions {
  boxId: string;
  boxName?: string;
  code?: PairingCode;
  clock?: Clock;
  random?: RandomSource;
  /**
   * Required. There is no default, because a default is how a mock ships: the
   * one implementation in this repo is [unsafeTestOnlyPake], and a
   * `pake ?? unsafeTestOnlyPake(...)` here would make every caller that forgot
   * the argument silently insecure. See [PakeEngine] for what to bind.
   */
  pake: PakeEngine;
  /** Default 10 minutes: long enough to walk to the phone, short enough to matter. */
  ttlMs?: number;
  /** Default 3. 30 bits of secret is only safe because this number is small. */
  maxAttempts?: number;
}

/**
 * The box half, in TypeScript.
 *
 * `relayd` is Go and owns the production implementation; this is the executable
 * reference it has to agree with, and it is what makes the phone side testable
 * end to end with no server, no sockets and no network — the same reason
 * `connector/src/server.ts` exists next to `connector/src/client.ts`.
 */
export class PairingHost {
  #code: PairingCode;
  #clock: Clock;
  #random: RandomSource;
  #pake: PakeEngine;
  #boxId: string;
  #boxName: string | undefined;
  #expiresAtMs: number;
  #maxAttempts: number;

  #attempts = 0;
  #state: PairingHostState = PairingHostState.Open;
  #pending: { hello: PairHello; session: PakeSession; transcript: Uint8Array } | null = null;
  #sharedKey: Uint8Array | null = null;
  #issued: DeviceCredential | null = null;

  constructor(options: PairingHostOptions) {
    this.#clock = options.clock ?? systemClock;
    this.#random = options.random ?? webCryptoRandom;
    this.#pake = requirePake(options.pake);
    this.#code = options.code ?? newPairingCode(this.#random);
    this.#boxId = options.boxId;
    this.#boxName = options.boxName;
    this.#expiresAtMs = this.#clock.now() + (options.ttlMs ?? 10 * 60 * 1000);
    this.#maxAttempts = options.maxAttempts ?? 3;
  }

  /** What the installer prints. Safe to log — it dies in ten minutes either way. */
  get code(): PairingCode {
    return { ...this.#code };
  }

  get state(): PairingHostState {
    if (
      this.#state !== PairingHostState.Consumed &&
      this.#state !== PairingHostState.Locked &&
      this.#clock.now() >= this.#expiresAtMs
    ) {
      return PairingHostState.Expired;
    }
    return this.#state;
  }

  get attemptsRemaining(): number {
    return Math.max(0, this.#maxAttempts - this.#attempts);
  }

  /**
   * A hello costs nothing and proves nothing, so it does not spend an attempt.
   * Only a failed *confirmation* is a wrong guess.
   */
  async handleHello(hello: PairHello): Promise<PairAccept> {
    this.#requireUsable();
    if (hello.v !== PAIRING_VERSION) {
      throw new PairingError(
        PairingErrorCode.VersionMismatch,
        `pairing version ${hello.v}, expected ${PAIRING_VERSION}`,
      );
    }
    if (hello.slot !== this.#code.slot) {
      // Wrong mailbox: not this box's code at all. Not an attempt either.
      throw new PairingError(PairingErrorCode.Malformed, "hello is for a different rendezvous slot");
    }

    const session = await this.#pake.begin(
      PairingRole.Host,
      utf8(this.#code.secret),
      helloContext(hello),
    );
    const transcript = transcriptOf(hello, session.message);
    const sharedKey = await session.finish(base64ToBytes(hello.pake));

    this.#pending = { hello, session, transcript };
    this.#sharedKey = sharedKey;
    this.#state = PairingHostState.AwaitingConfirm;

    return {
      v: PAIRING_VERSION,
      kind: "pair.accept",
      pake: bytesToBase64(session.message),
      confirm: confirmMac(sharedKey, PairingRole.Host, transcript),
    };
  }

  /**
   * Verify the phone's proof, then — and only then — release the grant.
   *
   * Order matters: releasing box identity before the phone has proved knowledge of
   * the code would let anyone who can reach the relay enumerate boxes.
   */
  async handleConfirm(confirm: PairConfirm): Promise<PairGrant> {
    this.#requireUsable();
    const pending = this.#pending;
    const sharedKey = this.#sharedKey;
    if (!pending || !sharedKey) {
      throw new PairingError(PairingErrorCode.OutOfOrder, "confirm before hello");
    }

    const expected = confirmMac(sharedKey, PairingRole.Client, pending.transcript);
    if (!equalBytes(base64ToBytes(expected), base64ToBytes(confirm.confirm))) {
      this.#attempts += 1;
      this.#pending = null;
      this.#sharedKey = null;
      this.#state =
        this.#attempts >= this.#maxAttempts ? PairingHostState.Locked : PairingHostState.Open;
      throw new PairingError(
        this.#state === PairingHostState.Locked
          ? PairingErrorCode.TooManyAttempts
          : PairingErrorCode.ConfirmationFailed,
        this.#state === PairingHostState.Locked
          ? "pairing code locked after too many wrong codes"
          : "pairing confirmation failed — wrong code, or someone in the middle",
      );
    }

    const body: GrantBody = {
      boxId: this.#boxId,
      boxName: this.#boxName,
      protocolVersion: PAIRING_VERSION,
      issuedAtMs: this.#clock.now(),
    };
    const grantKey = hkdf(sharedKey, pending.transcript, utf8(GRANT_INFO), 32);
    const sealed = seal(grantKey, GRANT_LABEL, this.#random(16), utf8(JSON.stringify(body)));

    this.#issued = credentialFrom(
      sharedKey,
      pending.transcript,
      pending.hello.deviceId,
      body,
      body.issuedAtMs,
    );
    this.#state = PairingHostState.Consumed;
    return { v: PAIRING_VERSION, kind: "pair.grant", sealed };
  }

  /** What the box stores for this device once pairing succeeds. */
  issuedCredential(): DeviceCredential {
    if (!this.#issued) {
      throw new PairingError(PairingErrorCode.OutOfOrder, "no credential has been issued");
    }
    return { ...this.#issued };
  }

  #requireUsable(): void {
    const state = this.state;
    if (state === PairingHostState.Expired) {
      throw new PairingError(PairingErrorCode.Expired, "pairing code has expired");
    }
    if (state === PairingHostState.Consumed) {
      throw new PairingError(PairingErrorCode.AlreadyUsed, "pairing code has already been used");
    }
    if (state === PairingHostState.Locked) {
      throw new PairingError(PairingErrorCode.TooManyAttempts, "pairing code is locked");
    }
  }
}

// --- the phone side ---------------------------------------------------------

export interface PairingClientOptions {
  /** As typed, or already parsed. Typos fail here, before the network. */
  code: string | PairingCode;
  /** Stable per install, not per connection. */
  deviceId: string;
  /** "ios" | "android" — the box shows it in its device list. */
  platform: string;
  deviceName: string;
  clock?: Clock;
  random?: RandomSource;
  /** Required, for the reason in [PairingHostOptions.pake]. */
  pake: PakeEngine;
}

export class PairingClient {
  #code: PairingCode;
  #clock: Clock;
  #pake: PakeEngine;
  #deviceId: string;
  #platform: string;
  #deviceName: string;

  #session: PakeSession | null = null;
  #hello: PairHello | null = null;
  #transcript: Uint8Array | null = null;
  #sharedKey: Uint8Array | null = null;
  #credential: DeviceCredential | null = null;

  constructor(options: PairingClientOptions) {
    this.#code = typeof options.code === "string" ? parsePairingCode(options.code) : options.code;
    this.#clock = options.clock ?? systemClock;
    this.#pake = requirePake(options.pake);
    this.#deviceId = options.deviceId;
    this.#platform = options.platform;
    this.#deviceName = options.deviceName;
  }

  get credential(): DeviceCredential | null {
    return this.#credential;
  }

  async hello(): Promise<PairHello> {
    const partial: PairHello = {
      v: PAIRING_VERSION,
      kind: "pair.hello",
      slot: this.#code.slot,
      deviceId: this.#deviceId,
      platform: this.#platform,
      deviceName: this.#deviceName,
      pake: "",
      pakeName: this.#pake.name,
    };
    // The PAKE binds to the hello it is carried in, so the hello has to exist
    // first — with the pake field empty, which both sides reproduce identically.
    const session = await this.#pake.begin(
      PairingRole.Client,
      utf8(this.#code.secret),
      helloContext(partial),
    );
    const hello: PairHello = { ...partial, pake: bytesToBase64(session.message) };

    this.#session = session;
    this.#hello = hello;
    return hello;
  }

  /**
   * Check the box's proof before sending ours.
   *
   * If this throws, the user typed the wrong code or something is in the middle —
   * either way the phone has revealed nothing, because the confirmation MAC it
   * would have sent is the only thing that proves knowledge.
   */
  async accept(accept: PairAccept): Promise<PairConfirm> {
    const session = this.#session;
    const hello = this.#hello;
    if (!session || !hello) {
      throw new PairingError(PairingErrorCode.OutOfOrder, "accept before hello");
    }
    if (accept.v !== PAIRING_VERSION) {
      throw new PairingError(
        PairingErrorCode.VersionMismatch,
        `pairing version ${accept.v}, expected ${PAIRING_VERSION}`,
      );
    }

    const hostPake = base64ToBytes(accept.pake);
    const transcript = transcriptOf(hello, hostPake);
    const sharedKey = await session.finish(hostPake);

    const expected = confirmMac(sharedKey, PairingRole.Host, transcript);
    if (!equalBytes(base64ToBytes(expected), base64ToBytes(accept.confirm))) {
      throw new PairingError(
        PairingErrorCode.ConfirmationFailed,
        "the box did not prove it knows this code — wrong code, or someone in the middle",
      );
    }

    this.#transcript = transcript;
    this.#sharedKey = sharedKey;
    return {
      v: PAIRING_VERSION,
      kind: "pair.confirm",
      confirm: confirmMac(sharedKey, PairingRole.Client, transcript),
    };
  }

  grant(grant: PairGrant): DeviceCredential {
    const transcript = this.#transcript;
    const sharedKey = this.#sharedKey;
    const hello = this.#hello;
    if (!transcript || !sharedKey || !hello) {
      throw new PairingError(PairingErrorCode.OutOfOrder, "grant before confirm");
    }
    const grantKey = hkdf(sharedKey, transcript, utf8(GRANT_INFO), 32);
    const body = JSON.parse(fromUtf8(openSealed(grantKey, GRANT_LABEL, grant.sealed))) as GrantBody;

    const credential = credentialFrom(
      sharedKey,
      transcript,
      hello.deviceId,
      body,
      this.#clock.now(),
    );
    this.#credential = credential;
    return credential;
  }
}

export interface PairingRun {
  credential: DeviceCredential;
  /** Exactly the JSON that crossed the relay, in order. */
  transcript: string[];
}

/**
 * Drive the four messages between a client and a host.
 *
 * Nothing here touches a socket: the two halves are plain objects passing JSON, so
 * a test can pair a phone to a box in microseconds and inspect every byte the
 * relay would have seen.
 */
export async function completePairing(
  client: PairingClient,
  host: PairingHost,
): Promise<PairingRun> {
  const transcript: string[] = [];
  const record = <T>(message: T): T => {
    transcript.push(JSON.stringify(message));
    return message;
  };

  const hello = record(await client.hello());
  const accept = record(await host.handleHello(hello));
  const confirm = record(await client.accept(accept));
  const grant = record(await host.handleConfirm(confirm));

  return { credential: client.grant(grant), transcript };
}
