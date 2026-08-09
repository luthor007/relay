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
 */

import type { MemoryEvent, PermissionScope } from "./manifest.ts";

// --- what woke us -----------------------------------------------------------

export type TriggerContext =
  | { type: "phrase"; transcript: string }
  | { type: "touch"; gesture: string }
  | { type: "memory"; event: MemoryEvent; episodeId?: string }
  | { type: "schedule" }
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

export interface MemoryCapability {
  /** Semantic search across the user's episodes. */
  search(query: string, options?: { limit?: number; since?: Date }): Promise<Episode[]>;
  recentEpisode(filter?: { kind?: Episode["kind"]; within?: number }): Promise<Episode | null>;
  get(episodeId: string): Promise<Episode | null>;
  extractCommitments(episode: Episode): Promise<Commitment[]>;
  /** Requires `memory.write`. */
  write(note: Note): Promise<{ id: string }>;
}

// --- glasses ----------------------------------------------------------------

export interface GlassesCapability {
  /**
   * Speak through the glasses. Resolves when playback finishes, so a sequence
   * of calls does not talk over itself.
   */
  say(text: string): Promise<void>;
  /**
   * Capture a still. Full resolution to device storage by default — it syncs
   * with the day rather than paying a BLE transfer nobody is waiting on.
   * Pass `immediate` only when the pixels are needed to answer right now.
   */
  capture(options?: { immediate?: boolean }): Promise<{ id: string; data?: Uint8Array }>;
  /** Listen for one utterance and return its transcript. */
  listen(options?: { timeoutMs?: number }): Promise<string>;
}

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

export interface AppContext {
  trigger: TriggerContext;
  memory: MemoryCapability;
  glasses: GlassesCapability;
  agent: AgentCapability;
  /** Private to this app, on this user's box. */
  storage: StorageCapability;
  /** Present only with `net.fetch`. */
  fetch?: FetchCapability;
  /** Shorthand for `glasses.say`, the overwhelmingly common case. */
  say(text: string): Promise<void>;
  log(message: string, data?: Record<string, unknown>): void;
  /** Scopes actually granted — may be narrower than requested if the user declined. */
  readonly granted: readonly PermissionScope[];
}

// --- definition -------------------------------------------------------------

export interface AppDefinition {
  /** Called once per invocation, with whatever woke the app. */
  onTrigger(ctx: AppContext): Promise<void> | void;
  /** Optional one-time setup after install. */
  onInstall?(ctx: Pick<AppContext, "storage" | "log">): Promise<void> | void;
  onUninstall?(ctx: Pick<AppContext, "storage" | "log">): Promise<void> | void;
}

/**
 * Declare an app.
 *
 * Deliberately a plain object rather than a class to extend: an app is a
 * function of (trigger, capabilities) and giving it inheritable state would
 * invite exactly the long-lived, cross-invocation behaviour the sandbox is
 * designed to prevent.
 */
export function defineApp(definition: AppDefinition): AppDefinition {
  if (typeof definition.onTrigger !== "function") {
    throw new TypeError("an app must define onTrigger");
  }
  return definition;
}
