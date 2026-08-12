import assert from "node:assert/strict";
import { describe, test } from "node:test";

import { defineApp } from "../src/app.ts";
import type { AppContext } from "../src/app.ts";
import { CapabilityErrorCode, ScopeError, isBoxProblem, isCapabilityError } from "../src/errors.ts";
import { card, view } from "../src/ui.ts";
import type { UICapability, View } from "../src/ui.ts";

// A hand-built `ctx`, the way relayd's runner builds one: only the properties a
// grant minted. Nothing here is a stub that refuses — an ungranted capability
// is simply not assigned, which is the property the runtime enforces and the
// thing these tests are about.
function context(granted: string[], extra: Record<string, unknown> = {}): AppContext {
  const spoken: string[] = [];
  const written: unknown[] = [];
  const drawn: View[] = [];
  const ctx: Record<string, unknown> = {
    trigger: { type: "tool", arguments: {} },
    storage: {
      get: async () => null,
      set: async () => {},
      delete: async () => {},
    },
    log: () => {},
    granted,
    declined: [],
    _spoken: spoken,
    _written: written,
    _drawn: drawn,
    ...extra,
  };

  if (granted.includes("memory.read")) {
    ctx.memory = {
      search: async () => [],
      recentEpisode: async () => ({
        id: "ep-1",
        kind: "meeting",
        startedAt: new Date(0),
        endedAt: new Date(60_000),
        transcript: "we agreed to ship on Friday",
      }),
      get: async () => null,
      extractCommitments: async () => [],
    };
  }
  if (granted.includes("memory.write")) {
    const memory = (ctx.memory ?? {}) as Record<string, unknown>;
    memory.write = async (note: unknown) => {
      written.push(note);
      return { id: "note-1" };
    };
    ctx.memory = memory;
  }
  if (granted.includes("agent.session")) {
    ctx.agent = {
      ask: async () => "Decisions: ship on Friday.",
      stream: async function* () {
        yield "Decisions:";
      },
    };
  }
  if (granted.includes("glasses.speaker")) {
    const say = async (text: string): Promise<void> => {
      spoken.push(text);
    };
    ctx.glasses = { say };
    ctx.say = say;
  }
  const ui: UICapability = {
    render: async (v: View) => {
      drawn.push(v);
    },
    card: async (title, rest) => {
      drawn.push(view(card(title, rest)));
    },
    list: async () => {},
    ask: async () => true,
  };
  ctx.ui = ui;
  return ctx as unknown as AppContext;
}

describe("defineApp keeps APP-PLATFORM.md §2's contract", () => {
  test("the documented app runs against the documented ctx", async () => {
    // The four calls §2's example makes, in the order it makes them. If this
    // stops compiling or stops running, the doc is wrong or the SDK is.
    const app = defineApp({
      async onTrigger(ctx) {
        const meeting = await ctx.memory.recentEpisode({ kind: "meeting" });
        if (!meeting) return ctx.say("I can't find a meeting in the last hour.");
        const summary = await ctx.agent.ask(`Summarise:\n\n${meeting.transcript}`);
        await ctx.memory.write({ kind: "note", title: "Standup", body: summary });
        await ctx.say("Saved. Three commitments — want them read back?");
      },
    });

    const ctx = context(["memory.read", "memory.write", "agent.session", "glasses.speaker"]);
    await app.onTrigger(ctx);

    const seen = ctx as unknown as { _spoken: string[]; _written: unknown[] };
    assert.deepEqual(seen._spoken, ["Saved. Three commitments — want them read back?"]);
    assert.equal(seen._written.length, 1);
  });

  test("an ungranted capability is absent from ctx, not present and throwing", () => {
    const ctx = context(["memory.read"]);
    // Absent. An app cannot feature-detect its way to something the user
    // declined, and it cannot catch a refusal and retry.
    assert.equal("glasses" in ctx, false);
    assert.equal("agent" in ctx, false);
    assert.equal("say" in ctx, false);
    assert.equal("fetch" in ctx, false);
    // And the half it does have has only the half it was granted.
    const memory = (ctx as unknown as { memory: Record<string, unknown> }).memory;
    assert.equal(typeof memory.recentEpisode, "function");
    assert.equal("write" in memory, false);
  });

  test("declined is separate from granted, because they answer different questions", () => {
    const ctx = context(["memory.read"], { declined: ["glasses.camera"] });
    assert.deepEqual([...ctx.granted], ["memory.read"]);
    assert.deepEqual([...ctx.declined], ["glasses.camera"]);
  });
});

describe("defineApp", () => {
  test("returns the definition", () => {
    const app = defineApp({ onTrigger: () => {} });
    assert.equal(typeof app.onTrigger, "function");
  });

  test("an app without onTrigger is a programming error", () => {
    assert.throws(() => defineApp({} as never), TypeError);
  });

  test("without declared scopes it is the same object, untouched", () => {
    // No wrapper, no behaviour change, no surprise for an app that declares its
    // scopes only in relay.json.
    const definition = { onTrigger: () => {} };
    assert.equal(defineApp(definition), definition);
  });

  test("declared scopes that were not granted fail once, clearly", async () => {
    // The alternative is `Cannot read properties of undefined (reading
    // 'write')`, which is a true statement about a manifest bug and a useless
    // one to read at 7am.
    const app = defineApp({
      scopes: ["memory.read", "memory.write"],
      async onTrigger(ctx) {
        await ctx.memory.write({ kind: "note", title: "t", body: "b" });
      },
    });
    await assert.rejects(
      async () => app.onTrigger(context(["memory.read"]) as never),
      (err: unknown) =>
        err instanceof ScopeError &&
        /Missing: memory\.write/.test(err.message) &&
        /declined at the install sheet/.test(err.message),
    );
  });

  test("declared scopes that were granted run normally", async () => {
    const app = defineApp({
      scopes: ["memory.read", "memory.write"],
      async onTrigger(ctx) {
        await ctx.memory.write({ kind: "note", title: "t", body: "b" });
      },
    });
    const ctx = context(["memory.read", "memory.write", "agent.session"]);
    await app.onTrigger(ctx as never);
    assert.equal((ctx as unknown as { _written: unknown[] })._written.length, 1);
  });

  test("a ctx with no granted list is refused rather than assumed to be fine", async () => {
    const app = defineApp({ scopes: ["memory.read"], onTrigger: () => {} });
    await assert.rejects(
      async () => app.onTrigger({} as never),
      (err: unknown) => err instanceof ScopeError && /ctx.granted is missing/.test((err as Error).message),
    );
  });

  test("scopes must be an array", () => {
    assert.throws(() => defineApp({ scopes: "memory.read" as never, onTrigger: () => {} }), TypeError);
  });

  test("the wrapper keeps everything else on the definition", async () => {
    let installed = false;
    const app = defineApp({
      scopes: ["memory.read"],
      onTrigger: () => {},
      onInstall: () => {
        installed = true;
      },
    });
    assert.deepEqual(app.scopes, ["memory.read"]);
    await app.onInstall?.({ storage: {} as never, log: () => {} });
    assert.equal(installed, true);
  });
});

describe("capability failures", () => {
  test("the five codes are the five relayd sends", () => {
    // internal/apps/wire.go's constants. A sixth code here that relayd never
    // sends is an app branch that never runs; a code relayd sends that is not
    // here is an app that cannot tell "no glasses paired" from a bug.
    assert.deepEqual(Object.values(CapabilityErrorCode), [
      "no_such_capability",
      "bad_arguments",
      "denied",
      "unavailable",
      "failed",
    ]);
  });

  test("narrows a rejection the runner produced", () => {
    const err = Object.assign(new Error("no glasses are paired with this box"), {
      code: "unavailable",
    });
    assert.equal(isCapabilityError(err), true);
    assert.equal(isBoxProblem(err), true);
    if (isCapabilityError(err)) assert.equal(err.code, CapabilityErrorCode.Unavailable);
  });

  test("does not claim an ordinary error", () => {
    for (const value of [new Error("boom"), null, undefined, "unavailable", { code: 7 }]) {
      assert.equal(isCapabilityError(value), false);
    }
    assert.equal(isBoxProblem(Object.assign(new Error("x"), { code: "failed" })), false);
  });
});

describe("ctx.ui", () => {
  test("draws a view the phone can render", async () => {
    const ctx = context([]);
    await ctx.ui?.card("Standup", { fields: [{ label: "Length", value: "12 min" }] });
    const drawn = (ctx as unknown as { _drawn: View[] })._drawn;
    assert.equal(drawn.length, 1);
    assert.equal(drawn[0].vocabulary, 1);
    assert.equal(drawn[0].blocks[0].kind, "card");
  });

  test("is optional, because a box with no phone paired has nowhere to draw", () => {
    // Same rule as every other capability: absent when it cannot be served,
    // rather than a render() that resolves having sent a frame into nothing.
    const ctx = context([]) as unknown as Record<string, unknown>;
    delete ctx.ui;
    assert.equal((ctx as { ui?: UICapability }).ui, undefined);
  });
});
