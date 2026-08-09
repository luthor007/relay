/**
 * The app manifest — `relay.json`.
 *
 * Read by the runtime before any app code runs, so it is the only place a
 * capability can be requested. Anything not declared here is not mintable at
 * runtime, which is what makes the permission sheet shown at install time an
 * accurate description of what the app can do rather than a promise.
 */

export const PermissionScope = {
  /** Live microphone during an open voice session. */
  GlassesAudio: "glasses.audio",
  /** Capture a still. Never silent — the indicator LEDs are wired to capture. */
  GlassesCamera: "glasses.camera",
  /** Speak through the glasses. */
  GlassesSpeaker: "glasses.speaker",
  /** Tap and gesture events. */
  GlassesTouch: "glasses.touch",
  /** Search and read the user's episodes and transcripts. */
  MemoryRead: "memory.read",
  /** Add notes, commitments and tags. */
  MemoryWrite: "memory.write",
  /** Send prompts to the user's agent and read replies. */
  AgentSession: "agent.session",
  /** Outbound HTTP, restricted to `allowedHosts`. */
  NetFetch: "net.fetch",
  /** Wake on a schedule. */
  Schedule: "schedule",
} as const;

export type PermissionScope = (typeof PermissionScope)[keyof typeof PermissionScope];

export interface PermissionRequest {
  scope: PermissionScope;
  /**
   * Shown verbatim on the install sheet. Required, and reviewed.
   *
   * "To transcribe your voice commands" is a reason. "Microphone access" is a
   * restatement of the scope and tells the user nothing they did not already
   * see.
   */
  reason: string;
}

export type Trigger =
  | { type: "phrase"; match: string }
  | { type: "touch"; gesture: "doubleTap" | "tripleTap" | "longPress" }
  | { type: "memory"; event: MemoryEvent }
  | { type: "schedule"; cron: string }
  /** Exposed to the user's agent as a callable tool. */
  | { type: "tool"; description: string };

export type MemoryEvent =
  | "meeting.ended"
  | "commitment.detected"
  | "day.synced"
  | "episode.created";

export interface AppManifest {
  /** Reverse-DNS, globally unique, immutable once published. */
  id: string;
  name: string;
  version: string;
  description: string;
  author: { name: string; url?: string; email?: string };
  permissions: PermissionRequest[];
  triggers: Trigger[];
  /**
   * Hosts this app may reach with `net.fetch`. Default-deny: an app holding
   * `memory.read` plus unrestricted egress is an exfiltration tool, so the
   * allowlist is declared up front and enforced by a proxy, not by the app.
   */
  allowedHosts?: string[];
  /** Wall-clock ceiling per invocation. Runtime kills overruns. */
  timeoutMs?: number;
}

export class ManifestError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ManifestError";
  }
}

const ID_PATTERN = /^[a-z0-9]+(\.[a-z0-9][a-z0-9-]*)+$/;
const SEMVER_PATTERN = /^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/;
const VALID_SCOPES = new Set<string>(Object.values(PermissionScope));

/**
 * Validate a parsed `relay.json`.
 *
 * Strict, and strict early: a manifest that half-parses installs an app with
 * permissions nobody reviewed.
 */
export function parseManifest(input: unknown): AppManifest {
  if (input === null || typeof input !== "object") {
    throw new ManifestError("manifest must be an object");
  }
  const raw = input as Record<string, unknown>;

  const id = raw.id;
  if (typeof id !== "string" || !ID_PATTERN.test(id)) {
    throw new ManifestError(
      `id must be reverse-DNS like "dev.you.app-name", got ${JSON.stringify(id)}`,
    );
  }
  if (typeof raw.version !== "string" || !SEMVER_PATTERN.test(raw.version)) {
    throw new ManifestError(`version must be semver, got ${JSON.stringify(raw.version)}`);
  }
  for (const field of ["name", "description"] as const) {
    if (typeof raw[field] !== "string" || (raw[field] as string).trim() === "") {
      throw new ManifestError(`${field} is required`);
    }
  }
  const author = raw.author as AppManifest["author"] | undefined;
  if (!author || typeof author.name !== "string") {
    throw new ManifestError("author.name is required");
  }

  if (!Array.isArray(raw.permissions)) {
    throw new ManifestError("permissions must be an array (use [] for none)");
  }
  const permissions = raw.permissions.map((entry, i): PermissionRequest => {
    const p = entry as Record<string, unknown>;
    if (typeof p?.scope !== "string" || !VALID_SCOPES.has(p.scope)) {
      throw new ManifestError(`permissions[${i}].scope is not a known scope: ${String(p?.scope)}`);
    }
    if (typeof p.reason !== "string" || p.reason.trim().length < 10) {
      throw new ManifestError(
        `permissions[${i}].reason must explain why, in a sentence a user can read ` +
          `(scope: ${p.scope})`,
      );
    }
    return { scope: p.scope as PermissionScope, reason: p.reason };
  });

  const scopes = permissions.map((p) => p.scope);
  if (new Set(scopes).size !== scopes.length) {
    throw new ManifestError("duplicate permission scopes");
  }

  if (!Array.isArray(raw.triggers) || raw.triggers.length === 0) {
    throw new ManifestError("an app needs at least one trigger, or nothing can start it");
  }

  const allowedHosts = raw.allowedHosts as string[] | undefined;
  if (scopes.includes(PermissionScope.NetFetch) && (!allowedHosts || allowedHosts.length === 0)) {
    throw new ManifestError(
      "net.fetch requires allowedHosts — unrestricted egress plus memory.read is an " +
        "exfiltration tool, so the hosts are declared up front",
    );
  }
  if (allowedHosts && !scopes.includes(PermissionScope.NetFetch)) {
    throw new ManifestError("allowedHosts declared without the net.fetch permission");
  }

  return {
    id,
    name: raw.name as string,
    version: raw.version,
    description: raw.description as string,
    author,
    permissions,
    triggers: raw.triggers as Trigger[],
    allowedHosts,
    timeoutMs: typeof raw.timeoutMs === "number" ? raw.timeoutMs : 30_000,
  };
}
