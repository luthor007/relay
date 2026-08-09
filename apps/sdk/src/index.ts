/**
 * @engram/sdk — build apps for Engram One.
 *
 *     import { defineApp } from "@engram/sdk";
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
 */

export { defineApp } from "./app.ts";
export type {
  AgentCapability,
  AppContext,
  AppDefinition,
  Commitment,
  Episode,
  FetchCapability,
  GlassesCapability,
  MemoryCapability,
  Note,
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
