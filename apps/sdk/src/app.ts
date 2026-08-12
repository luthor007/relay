/**
 * The app-facing API.
 *
 * Everything an app can do arrives on `ctx`. There is no filesystem, no
 * `child_process`, no raw socket, and no ambient global that reaches the host —
 * the capability objects here are the entire surface, and the runtime mints each
 * one only if the manifest asked for the matching scope.
 *
 * That is what makes the install-time permission sheet true rather than
 * aspirational: an app cannot reach for something it did not declare, because
 * the object simply is not on `ctx`.
 *
 * # Absent, not refusing — in the types too
 *
 * The runtime has enforced that since `internal/apps`: a capability whose scope
 * was not granted is not a property on the object, so `"memory" in ctx` is
 * false and an app cannot feature-detect its way to something the user
 * declined. These types used to say otherwise — `memory`, `glasses` and `agent`
 * were declared unconditionally — which meant the SDK described a `ctx` no app
 * with a narrow manifest would ever receive.
 *
 * So [AppContext] is parameterised by the scopes the app declares, and the
 * capabilities appear exactly when their scope does:
 *
 *     export default defineApp({
 *       scopes: ["memory.read", "agent.session"],
 *       async onTrigger(ctx) {
 *         ctx.memory.recentEpisode({ kind: "meeting" });  // fine
 *         ctx.memory.write({ … });                        // does not compile
 *         ctx.say("…");                                   // does not compile
 *       },
 *     });
 *
 * Declaring `scopes` is optional and omitting it keeps the wide `ctx` that
 * `APP-PLATFORM.md` §2's example uses. It is worth declaring: without it the
 * first thing an over-reaching app learns is `Cannot read properties of
 * undefined (reading 'write')`, at 7am, on somebody else's machine.
 */

import type { MemoryEvent, PermissionScope } from "./manifest.ts";
import type { UICapability } from "./ui.ts";
import { ScopeError } from "./errors.ts";

// --- what woke us -----------------------------------------------------------

/** The gestures the glasses report. */
export type Gesture = "doubleTap" | "tripleTap" | "longPress";

export type TriggerContext =
  | { type: "phrase"; transcript: string }
  | { type: "touch"; gesture: Gesture }
  | { type: "memory"; event: MemoryEvent; episodeId?: string }
  | { type: "schedule" }
  /** The user's agent called this app as an MCP tool — `APP-PLATFORM.md` §4. */
  | { type: "tool"; arguments: Record<string, unknown> };

// --- memory -----------------------------------------------------------------

export interface Episode {
  id: string;
  kind: "meeting" | "focus" | "conversation" | "ambient";
  startedAt: Date;
  endedAt: Date;
  transcript: string;
  participants?: string[];
  location?: string;
}

export interface Commitment {
  text: string;
  /** Who it was made to, when it can be identified from the transcript. */
  to?: string;
  dueAt?: Date;
  sourceEpisodeId: string;
}

export interface Note {
  kind: "note";
  title: string;
  body: string;
  commitments?: Commitment[];
  tags?: string[];
}

/** Granted by `memory.read`. Every call is recorded against this app. */
export interface MemoryReadCapability {
  /** Semantic search across the user's episodes. */
  search(query: string, options?: { limit?: number; since?: Date }): Promise<Episode[]>;
  recentEpisode(filter?: { kind?: Episode["kind"]; within?: number }): Promise<Episode | null>;
  get(episodeId: string): Promise<Episode | null>;
  extractCommitments(episode: Episode): Promise<Commitment[]>;
}

/** Granted by `memory.write`. */
export interface MemoryWriteCapability {
  write(note: Note): Promise<{ id: string }>;
}

/** Both halves. What an app holding `memory.read` and `memory.write` receives. */
export interface MemoryCapability extends MemoryReadCapability, MemoryWriteCapability {}

// --- glasses ----------------------------------------------------------------

/** Granted by `glasses.speaker`. */
export interface SpeakerCapability {
  /**
   * Speak through the glasses. Resolves when playback finishes, so a sequence
   * of calls does not talk over itself.
   */
  say(text: string): Promise<void>;
}

/** Granted by `glasses.camera`. */
export interface CameraCapability {
  /**
   * Capture a still. Full resolution to device storage by default — it syncs
   * with the day rather than paying a BLE transfer nobody is waiting on.
   * Pass `immediate` only when the pixels are needed to answer right now.
   *
   * Never silent: the indicator LEDs are wired to capture and this call goes
   * through them, so a capture the user could not see is not reachable from
   * here.
   */
  capture(options?: { immediate?: boolean }): Promise<{ id: string; data?: Uint8Array }>;
}

/** Granted by `glasses.audio`. */
export interface MicrophoneCapability {
  /** Listen for one utterance and return its transcript. */
  listen(options?: { timeoutMs?: number }): Promise<string>;
}

/** Everything the glasses can do, for an app that asked for all three. */
export interface GlassesCapability
  extends SpeakerCapability,
    CameraCapability,
    MicrophoneCapability {}

// --- agent ------------------------------------------------------------------

export interface AgentCapability {
  /**
   * Ask the user's own agent, running on their box with their model
   * configuration. Apps do not bring their own API keys, and the user is never
   * billed twice.
   */
  ask(prompt: string, options?: { model?: string }): Promise<string>;
  /** Stream the same thing, for anything long enough to be worth reading aloud. */
  stream(prompt: string): AsyncIterable<string>;
}

// --- misc -------------------------------------------------------------------

export interface StorageCapability {
  get<T = unknown>(key: string): Promise<T | null>;
  set(key: string, value: unknown): Promise<void>;
  delete(key: string): Promise<void>;
}

export interface FetchCapability {
  /** Restricted to the manifest's `allowedHosts`; enforced by the egress proxy. */
  (input: string, init?: RequestInit): Promise<Response>;
}

// --- scope-conditional shape ------------------------------------------------

/**
 * Whether a set of declared scopes contains any of `K`.
 *
 * The `[…] extends [never]` form is deliberate: a bare conditional over a
 * generic distributes across the union and answers per-member, which is not the
 * question being asked.
 */
export type Has<S extends readonly PermissionScope[], K extends PermissionScope> =
  [Extract<S[number], K>] extends [never] ? false : true;

/** `T` when the condition holds, and nothing at all when it does not. */
type When<C extends boolean, T> = C extends true ? T : unknown;

/** The memory object an app with these scopes receives — one half, or both. */
export type MemoryFor<S extends readonly PermissionScope[]> = When<
  Has<S, "memory.read">,
  MemoryReadCapability
> &
  When<Has<S, "memory.write">, MemoryWriteCapability>;

/** The glasses object an app with these scopes receives. */
export type GlassesFor<S extends readonly PermissionScope[]> = When<
  Has<S, "glasses.speaker">,
  SpeakerCapability
> &
  When<Has<S, "glasses.camera">, CameraCapability> &
  When<Has<S, "glasses.audio">, MicrophoneCapability>;

/** What is on `ctx` no matter what the manifest asked for. */
export interface CoreContext {
  trigger: TriggerContext;
  /** Private to this app, on this user's box. Served by the host, not a mounted directory. */
  storage: StorageCapability;
  /**
   * Put something in front of the user — `ORCHESTRATOR.md` §5's declarative
   * vocabulary, drawn natively by the phone app.
   *
   * Optional for the same reason every other capability is: it is present when
   * there is somewhere to draw. A box with no phone paired has no render
   * surface, and an absent capability is the honest answer — better than a
   * `render()` that resolves having sent a frame into nothing.
   */
  ui?: UICapability;
  log(message: string, data?: Record<string, unknown>): void;
  /** Scopes actually granted — may be narrower than requested if the user declined. */
  readonly granted: readonly PermissionScope[];
  /**
   * Scopes the manifest asked for and the user refused.
   *
   * Worth having separately from `granted`: "you have not given me the camera"
   * and "this box has no camera" are different sentences, and an app that can
   * only see what it has cannot tell them apart.
   */
  readonly declined: readonly PermissionScope[];
}

/**
 * What an app receives, shaped by the scopes it declared.
 *
 * With no type argument every capability is present, which is the right default
 * for `APP-PLATFORM.md` §2's example and for an app that declares its scopes
 * only in `relay.json`.
 */
export type AppContext<S extends readonly PermissionScope[] = readonly PermissionScope[]> =
  CoreContext &
    When<Has<S, "memory.read" | "memory.write">, { memory: MemoryFor<S> }> &
    When<
      Has<S, "glasses.speaker" | "glasses.camera" | "glasses.audio">,
      { glasses: GlassesFor<S> }
    > &
    When<Has<S, "agent.session">, { agent: AgentCapability }> &
    When<Has<S, "net.fetch">, { fetch: FetchCapability }> &
    /**
     * `ctx.say` is the shorthand for the overwhelmingly common case, and it
     * exists exactly when `glasses.say` does. A shorthand cannot be more
     * generous than the thing it is short for.
     */
    When<Has<S, "glasses.speaker">, { say(text: string): Promise<void> }>;

/** What `onInstall` and `onUninstall` get: no trigger, and nothing of the user's. */
export interface SetupContext {
  storage: StorageCapability;
  log(message: string, data?: Record<string, unknown>): void;
}

// --- definition -------------------------------------------------------------

export interface AppDefinition<S extends readonly PermissionScope[] = readonly PermissionScope[]> {
  /**
   * The scopes this app's code expects, matching `relay.json`.
   *
   * Optional. Declaring it narrows `ctx` to exactly those capabilities at
   * compile time, and turns a manifest that no longer matches the code into one
   * clear failure on the first invocation instead of a `TypeError` deep inside
   * `onTrigger`.
   */
  scopes?: S;
  /** Called once per invocation, with whatever woke the app. */
  onTrigger(ctx: AppContext<S>): Promise<void> | void;
  /** Optional one-time setup after install. */
  onInstall?(ctx: SetupContext): Promise<void> | void;
  onUninstall?(ctx: SetupContext): Promise<void> | void;
}

/**
 * Declare an app.
 *
 * Deliberately a plain object rather than a class to extend: an app is a
 * function of (trigger, capabilities) and giving it inheritable state would
 * invite exactly the long-lived, cross-invocation behaviour the sandbox is
 * designed to prevent.
 *
 * When `scopes` is declared, `onTrigger` is wrapped in one check: every
 * declared scope must actually be in `ctx.granted`. That check is not a
 * security boundary — the boundary is the host's dispatch table, and nothing
 * here could add a capability — it is a diagnostic. An app whose `relay.json`
 * lost `memory.write` in review should fail saying so, not fail reading a
 * property of `undefined`.
 */
export function defineApp<S extends readonly PermissionScope[] = readonly PermissionScope[]>(
  definition: AppDefinition<S>,
): AppDefinition<S> {
  if (typeof definition.onTrigger !== "function") {
    throw new TypeError("an app must define onTrigger");
  }
  const declared = definition.scopes;
  if (declared === undefined) return definition;
  if (!Array.isArray(declared)) {
    throw new TypeError("scopes must be an array of permission scopes, or absent");
  }

  const onTrigger = definition.onTrigger.bind(definition);
  return {
    ...definition,
    onTrigger(ctx: AppContext<S>): Promise<void> | void {
      const granted = (ctx as CoreContext).granted;
      if (!Array.isArray(granted)) {
        throw new ScopeError(
          "ctx.granted is missing, so the scopes this app declared cannot be checked against " +
            "what the user actually consented to",
        );
      }
      const missing = declared.filter((s) => !granted.includes(s));
      if (missing.length > 0) {
        throw new ScopeError(
          `this app declared ${declared.join(", ")} and was granted ${
            granted.length > 0 ? granted.join(", ") : "nothing"
          }. Missing: ${missing.join(", ")}. Either relay.json does not ask for ` +
            `${missing.length === 1 ? "it" : "them"}, or the user declined at the install sheet — ` +
            `an app must cope with being declined`,
        );
      }
      return onTrigger(ctx);
    },
  };
}
