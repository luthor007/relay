import assert from "node:assert/strict";
import { describe, test } from "node:test";

import {
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
} from "../src/ui.ts";
import type { View } from "../src/ui.ts";

const ok = (v: View): View => v;

describe("the vocabulary is small on purpose", () => {
  test("there are exactly four block kinds", () => {
    // APP-PLATFORM.md §7: "a card, a list, a confirmation, a spoken response".
    // A fifth kind is a product decision — every host on both platforms grows a
    // renderer for it, and a reviewer reading a manifest can no longer picture
    // what the app will draw. This test is here so that decision is taken on
    // purpose rather than in a patch.
    assert.deepEqual([...BLOCK_KINDS], ["card", "list", "confirm", "speak"]);
  });

  test("nothing in the vocabulary carries a URL", () => {
    // A remote image is an exfiltration channel with a pretty name: the URL is
    // chosen by the app and carries whatever the app put in it. "Cannot phone
    // home with your data" is a property of there being nowhere to put a URL.
    const everyField = [
      "title",
      "body",
      "fields",
      "label",
      "value",
      "items",
      "subtitle",
      "detail",
      "question",
      "confirmLabel",
      "cancelLabel",
      "text",
    ];
    for (const name of everyField) {
      assert.ok(
        !/url|href|src|image|icon|style|css|html/i.test(name),
        `${name} would let an app reach off the phone or draw its own pixels`,
      );
    }
  });

  test("only the spoken block costs a permission", () => {
    assert.deepEqual(Object.keys(BLOCK_SCOPES), ["speak"]);
    assert.equal(BLOCK_SCOPES.speak, "glasses.speaker");
  });
});

describe("parseView", () => {
  test("accepts each of the four kinds", () => {
    const v = view(
      card("Standup", { body: "Three decisions.", fields: [{ label: "Length", value: "12 min" }] }),
      list([{ title: "Ship the installer", subtitle: "Alexis", detail: "Friday" }], {
        title: "Commitments",
      }),
      confirm("Read them back?", { confirmLabel: "Go on", cancelLabel: "Later" }),
      speak("Saved. Three commitments."),
    );
    assert.equal(v.vocabulary, VOCABULARY_VERSION);
    assert.equal(v.blocks.length, 4);
    assert.deepEqual(
      v.blocks.map((b) => b.kind),
      ["card", "list", "confirm", "speak"],
    );
  });

  test("stamps the version rather than trusting the caller", () => {
    const v = parseView({ vocabulary: 1, blocks: [{ kind: "speak", text: "hello" }] });
    assert.equal(v.vocabulary, VOCABULARY_VERSION);
  });

  test("refuses a version it does not know, whole", () => {
    // Not "render the parts we recognise": a confirmation whose question drew
    // and whose buttons did not is worse than a screen saying the app needs a
    // newer Relay.
    assert.throws(
      () => parseView({ vocabulary: 2, blocks: [{ kind: "speak", text: "hi" }] }),
      (err: unknown) => err instanceof ViewError && /newer Relay/.test((err as Error).message),
    );
    assert.throws(() => parseView({ blocks: [] }), ViewError);
  });

  test("rejects an unknown block kind", () => {
    assert.throws(
      () => parseView({ vocabulary: 1, blocks: [{ kind: "webview", url: "https://evil" }] }),
      /the vocabulary is card, list, confirm, speak/,
    );
  });

  test("rejects an unknown field on a known block", () => {
    // The closed key set is what keeps a host from having to decide whether an
    // unrecognised field was decoration or content.
    assert.throws(
      () => parseView({ vocabulary: 1, blocks: [{ kind: "card", title: "x", style: "red" }] }),
      /unknown field "style"/,
    );
    assert.throws(
      () =>
        parseView({
          vocabulary: 1,
          blocks: [{ kind: "list", items: [{ title: "a", imageUrl: "https://x" }] }],
        }),
      /unknown field "imageUrl"/,
    );
  });

  test("rejects a view with no blocks and a view with too many", () => {
    assert.throws(() => parseView({ vocabulary: 1, blocks: [] }), /renders nothing/);
    const many = Array.from({ length: LIMITS.blocks + 1 }, (_, i) => ({
      kind: "card" as const,
      title: `card ${i}`,
    }));
    assert.throws(() => parseView({ vocabulary: 1, blocks: many }), /the limit is 8/);
  });

  test("asks at most one question and speaks at most once", () => {
    assert.throws(() => view(confirm("a?"), confirm("b?")), /at most one question/);
    assert.throws(() => view(speak("a"), speak("b")), /talk over each other/);
  });

  test("caps every string", () => {
    assert.throws(() => view(card("x".repeat(LIMITS.cardTitle + 1))), /the limit is 120/);
    assert.throws(() => view(speak("x".repeat(LIMITS.speakText + 1))), /the limit is 1000/);
    assert.throws(
      () => view(card("t", { fields: [{ label: "l", value: "v".repeat(LIMITS.fieldValue + 1) }] })),
      /the limit is 240/,
    );
  });

  test("caps the number of rows and fields", () => {
    const items = Array.from({ length: LIMITS.listItems + 1 }, (_, i) => ({ title: `row ${i}` }));
    assert.throws(() => view(list(items)), /the limit is 50/);
    const fields = Array.from({ length: LIMITS.cardFields + 1 }, (_, i) => ({
      label: `l${i}`,
      value: "v",
    }));
    assert.throws(() => view(card("t", { fields })), /the limit is 12/);
  });

  test("caps the serialised size", () => {
    // Eight cards of two thousand characters each is well inside every
    // per-field cap and well outside anything worth sending to a phone.
    const blocks = Array.from({ length: LIMITS.blocks }, (_, i) => ({
      kind: "card" as const,
      title: `card ${i}`,
      body: "x".repeat(LIMITS.cardBody),
      fields: [{ label: "note", value: "v".repeat(LIMITS.fieldValue) }],
    }));
    assert.throws(() => parseView({ vocabulary: 1, blocks }), /the limit is 16384/);
  });

  test("rejects blank strings", () => {
    assert.throws(() => view(card("   ")), /is empty/);
    assert.throws(() => view(list([{ title: "" }])), /is empty/);
  });

  test("rejects an empty list rather than drawing a heading over nothing", () => {
    assert.throws(() => view(list([])), /empty list/);
  });

  test("refuses control characters instead of stripping them", () => {
    // A card is text a phone draws, not a terminal. Silently removing an escape
    // sequence hides from the app that it sent something it did not mean to.
    assert.throws(() => view(card("done\u001b[31m")), /control character/);
    assert.throws(() => view(list([{ title: "a\u0007" }])), /control character/);
  });

  test("allows newlines only where a paragraph is meant", () => {
    const v = ok(view(card("Notes", { body: "one\ntwo" })));
    assert.equal((v.blocks[0] as { body?: string }).body, "one\ntwo");
    assert.throws(() => view(card("one\ntwo")), /control character/);
  });

  test("rejects a non-object and a top-level extra key", () => {
    for (const bad of [null, 42, "view", []]) {
      assert.throws(() => parseView(bad), ViewError);
    }
    assert.throws(
      () => parseView({ vocabulary: 1, blocks: [{ kind: "speak", text: "hi" }], theme: "dark" }),
      /"theme" is not part of the format/,
    );
  });

  test("drops nothing it accepted", () => {
    const input = {
      vocabulary: 1,
      blocks: [
        { kind: "card", title: "T", body: "B", fields: [{ label: "L", value: "V" }] },
        { kind: "list", title: "L", items: [{ title: "a", subtitle: "b", detail: "c" }] },
        { kind: "confirm", question: "q?", confirmLabel: "y", cancelLabel: "n", detail: "d" },
        { kind: "speak", text: "s" },
      ],
    };
    assert.deepEqual(parseView(input), input);
  });
});

describe("checkScopes", () => {
  test("a speak block needs glasses.speaker", () => {
    const v = view(speak("Saved."));
    assert.throws(() => checkScopes(v, ["memory.read"]), /glasses.speaker/);
    assert.equal(checkScopes(v, ["glasses.speaker"]), v);
  });

  test("a card needs nothing", () => {
    // Drawing on the phone of the person who installed the app reaches nothing
    // of theirs: it cannot read, fetch or capture. It is minted like storage.
    const v = view(card("Standup"));
    assert.equal(checkScopes(v, []), v);
  });
});

describe("viewText", () => {
  test("projects every kind to something an agent can read", () => {
    const v = view(
      card("Standup", { body: "Three decisions.", fields: [{ label: "Length", value: "12 min" }] }),
      list([{ title: "Ship it", subtitle: "Alexis", detail: "Friday" }], { title: "Commitments" }),
      confirm("Read them back?"),
      speak("Saved."),
    );
    assert.equal(
      viewText(v),
      [
        "Standup",
        "Three decisions.",
        "Length: 12 min",
        "Commitments",
        "- Ship it — Alexis — Friday",
        "Read them back?",
        "[Yes / No]",
        "Saved.",
      ].join("\n"),
    );
  });
});

describe("the ui.render frame", () => {
  test("is SYSTEM.md §6.1's envelope", () => {
    const frame = renderFrame(view(card("Standup")), {
      id: "f-1",
      at: 1_700_000_000_000,
      app: "dev.alexis.standup-notes",
    });
    assert.equal(frame.v, ENVELOPE_VERSION);
    assert.equal(frame.type, RENDER_FRAME);
    assert.equal(frame.id, "f-1");
    assert.equal(frame.at, 1_700_000_000_000);
    assert.equal(frame.payload.app, "dev.alexis.standup-notes");
    assert.equal(frame.payload.expects, undefined);
  });

  test("a view with a question expects a decision", () => {
    const v = view(confirm("Send it?"));
    assert.equal(expectsDecision(v), true);
    const frame = renderFrame(v, { id: "f-2", at: 1, app: "a.b", invocation: "inv-9" });
    // The answer comes back on consent.decision, which §6.1 already defines,
    // keyed by this frame's id. No second confirmation channel.
    assert.equal(frame.payload.expects, "decision");
    assert.equal(frame.payload.invocation, "inv-9");
  });

  test("validates the view it wraps", () => {
    assert.throws(
      () => renderFrame({ vocabulary: 1, blocks: [] } as View, { id: "x", at: 1, app: "a.b" }),
      ViewError,
    );
  });

  test("round-trips through JSON", () => {
    const frame = renderFrame(view(card("Standup"), speak("Saved.")), {
      id: "f-3",
      at: 2,
      app: "a.b",
    });
    assert.deepEqual(parseRenderFrame(JSON.parse(JSON.stringify(frame))), frame);
  });

  test("a host refuses a frame it cannot fully validate", () => {
    const good = renderFrame(view(card("Standup")), { id: "f", at: 1, app: "a.b" });
    assert.throws(() => parseRenderFrame({ ...good, v: 2 }), /envelope version/);
    assert.throws(() => parseRenderFrame({ ...good, type: "speak" }), /not ui.render/);
    assert.throws(() => parseRenderFrame({ ...good, id: "" }), /needs an id/);
    assert.throws(() => parseRenderFrame({ ...good, at: "now" }), /unix millisecond/);
    assert.throws(() => parseRenderFrame({ ...good, payload: { view: good.payload.view } }), /payload.app/);
    assert.throws(() => parseRenderFrame(null), ViewError);
  });

  test("refuses expects on a view that asks nothing", () => {
    // Otherwise the phone waits for an answer nobody will ever give it.
    const good = renderFrame(view(card("Standup")), { id: "f", at: 1, app: "a.b" });
    assert.throws(
      () => parseRenderFrame({ ...good, payload: { ...good.payload, expects: "decision" } }),
      /wait for an answer nobody will give/,
    );
  });
});
