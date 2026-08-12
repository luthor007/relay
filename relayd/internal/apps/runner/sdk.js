// `@relay/sdk`, as it exists inside the sandbox.
//
// The SDK is TypeScript, and almost all of it is types: `AppContext`,
// `MemoryCapability`, `Episode` and the rest are erased before anything runs.
// Two things survive to runtime — `defineApp` and the `PermissionScope` values —
// and this is them, in plain JavaScript, because Node will not strip types from
// a file inside `node_modules`.
//
// It is generated into the app's read-only root at install time rather than
// resolved from the developer's machine, so the module an app imports is one
// relayd wrote. An app that shipped its own `@relay/sdk` with a `defineApp` that
// did something else would still be handed the same `ctx` by the runner, because
// `ctx` comes from the host and not from this file — but a stub the app cannot
// replace is one less thing to reason about.

export function defineApp(definition) {
  if (!definition || typeof definition.onTrigger !== "function") {
    throw new TypeError("an app must define onTrigger");
  }
  return definition;
}

export const PermissionScope = Object.freeze({
  GlassesAudio: "glasses.audio",
  GlassesCamera: "glasses.camera",
  GlassesSpeaker: "glasses.speaker",
  GlassesTouch: "glasses.touch",
  MemoryRead: "memory.read",
  MemoryWrite: "memory.write",
  AgentSession: "agent.session",
  NetFetch: "net.fetch",
  Schedule: "schedule",
});

export class ManifestError extends Error {
  constructor(message) {
    super(message);
    this.name = "ManifestError";
  }
}

// parseManifest is the SDK's validator, and it is deliberately absent here: the
// manifest is read by relayd before this process exists, and an app parsing its
// own manifest at runtime would be reading a file it is not allowed to act on
// anyway. Importing it throws rather than returning something plausible.
export function parseManifest() {
  throw new ManifestError(
    "an app cannot parse its own manifest: relayd read it before this process started, and the " +
      "capabilities on ctx are the result",
  );
}
