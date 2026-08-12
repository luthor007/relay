/**
 * What a capability call rejects with.
 *
 * The runtime distinguishes five failures on the capability channel and an app
 * has to be able to tell them apart, because the right response to each is
 * different and two of them are not the app's fault:
 *
 *   - `unavailable` — you were granted this and this box has nothing behind it.
 *     No glasses are paired; there is no note store; no agent is configured.
 *     Say so and stop. Retrying will not help.
 *   - `denied` — the egress allowlist refused a host. The manifest is what
 *     fixes this, not a retry.
 *   - `bad_arguments` — the app called it wrong. A bug, and it should look
 *     like one.
 *   - `failed` — it ran and did not work. This is the one worth retrying.
 *   - `no_such_capability` — the method is not on this app's grant.
 *
 * That last one should be unreachable from a well-formed app, and that is the
 * point rather than an oversight: an ungranted capability is **absent from
 * `ctx`**, not present and refusing, so the ordinary way to hit it is a
 * `TypeError` about reading a property of undefined. It exists on the wire
 * because the in-sandbox runner is not the security boundary — relayd keeps its
 * own table of granted methods, so an app that speaks the protocol by hand gets
 * this code and learns nothing about what the user declined.
 */

/** The five codes, matching relayd's capability channel exactly. */
export const CapabilityErrorCode = {
  /** The grant did not mint this method. Not "denied" — from outside the grant there is nothing to deny. */
  NoSuchCapability: "no_such_capability",
  /** A malformed call. */
  BadArguments: "bad_arguments",
  /** The egress allowlist refused a host. */
  Denied: "denied",
  /** Granted, but nothing behind it on this box. */
  Unavailable: "unavailable",
  /** It ran and failed. */
  Failed: "failed",
} as const;

export type CapabilityErrorCode = (typeof CapabilityErrorCode)[keyof typeof CapabilityErrorCode];

/**
 * A rejected capability call.
 *
 * An interface rather than a class, because the object an app catches is
 * constructed by the in-sandbox runner and crossing a class identity over that
 * boundary buys nothing. Use [isCapabilityError] rather than `instanceof`.
 */
export interface CapabilityError extends Error {
  readonly code: CapabilityErrorCode;
}

const CODES: ReadonlySet<string> = new Set<string>(Object.values(CapabilityErrorCode));

/** Narrow an unknown catch to a capability failure. */
export function isCapabilityError(err: unknown): err is CapabilityError {
  if (err === null || typeof err !== "object") return false;
  const code = (err as { code?: unknown }).code;
  return typeof code === "string" && CODES.has(code);
}

/**
 * True when the box, not the app, is what went wrong.
 *
 * The distinction is worth a helper because it decides what an app says out
 * loud. "I could not reach your printer" is a sentence about the box that the
 * user can act on; "something went wrong" is the app blaming itself for a
 * missing speaker.
 */
export function isBoxProblem(err: unknown): boolean {
  return isCapabilityError(err) && err.code === CapabilityErrorCode.Unavailable;
}

/**
 * Thrown by `defineApp` when the scopes an app declared are not the scopes it
 * was granted.
 *
 * This is drift between `relay.json` and the app's own `scopes` list, and it is
 * caught on the first invocation rather than at the first missing property:
 * `Cannot read properties of undefined (reading 'recentEpisode')` is a true
 * statement about a bug in the manifest and a useless one to read at 7am.
 */
export class ScopeError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ScopeError";
  }
}
