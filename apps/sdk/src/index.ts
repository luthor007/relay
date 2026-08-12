/**
 * @relay/sdk — build apps for Relay One.
 *
 *     import { defineApp } from "@relay/sdk";
 *
 *     export default defineApp({
 *       async onTrigger(ctx) {
 *         const meeting = await ctx.memory.recentEpisode({ kind: "meeting" });
 *         if (!meeting) return ctx.say("No meeting found.");
 *         await ctx.say(await ctx.agent.ask(`Summarise:\n${meeting.transcript}`));
 *       },
 *     });
 *
 * Apps run on the user's own box, not on yours. You never receive their data,
 * and you never pay to host their app. See docs/APP-PLATFORM.md.
 *
 * Two halves, and the split is the product:
 *
 *   - **`defineApp` and the capabilities on `ctx`** run server-side, sandboxed,
 *     on the user's machine.
 *   - **`ui`** is the declarative vocabulary the phone app draws natively. It
 *     is data, not code: nothing third-party executes on the handset.
 */

export { defineApp } from "./app.ts";
export type {
  AgentCapability,
  AppContext,
  AppDefinition,
  CameraCapability,
  Commitment,
  CoreContext,
  Episode,
  FetchCapability,
  Gesture,
  GlassesCapability,
  GlassesFor,
  Has,
  MemoryCapability,
  MemoryFor,
  MemoryReadCapability,
  MemoryWriteCapability,
  MicrophoneCapability,
  Note,
  SetupContext,
  SpeakerCapability,
  StorageCapability,
  TriggerContext,
} from "./app.ts";

export { ManifestError, PermissionScope, parseManifest } from "./manifest.ts";
export type {
  AppManifest,
  MemoryEvent,
  PermissionRequest,
  Trigger,
} from "./manifest.ts";

export { CapabilityErrorCode, isBoxProblem, isCapabilityError, ScopeError } from "./errors.ts";
export type { CapabilityError } from "./errors.ts";

export {
  BLOCK_KINDS,
  BLOCK_SCOPES,
  ENVELOPE_VERSION,
  LIMITS,
  RENDER_FRAME,
  VOCABULARY_VERSION,
  ViewError,
  card,
  checkScopes,
  confirm,
  expectsDecision,
  list,
  parseRenderFrame,
  parseView,
  renderFrame,
  speak,
  view,
  viewText,
} from "./ui.ts";
export type {
  Block,
  BlockKind,
  CardBlock,
  ConfirmBlock,
  Field,
  ListBlock,
  ListItem,
  RenderFrame,
  RenderPayload,
  SpeakBlock,
  UICapability,
  View,
} from "./ui.ts";
