/**
 * The declarative UI vocabulary — the wire format an app's server-side code
 * yields and the phone app draws natively.
 *
 * This is the file that makes `ORCHESTRATOR.md` §5's two sentences hold at the
 * same time, and they only look like a contradiction:
 *
 *   - **App code runs on the server**, sandboxed, never on the phone. That is
 *     what keeps the author from ever seeing your transcript.
 *   - **App UI renders in the phone app**, through a small declarative
 *     vocabulary the host draws natively — a card, a list, a confirmation, a
 *     spoken reply.
 *
 * So a mini-app is a manifest plus server-side code that yields phone-side UI.
 * One iOS app and one Android app are enough, an app author needs no Swift or
 * Kotlin, and nothing third-party executes on the handset.
 *
 * # Why it is this small, and why it must stay this small
 *
 * `APP-PLATFORM.md` §7 states the tradeoff plainly: *an app cannot draw
 * arbitrary pixels on your phone. In exchange, it works identically on both
 * platforms, cannot phone home with your data, and gets reviewed as a manifest
 * instead of a binary.*
 *
 * All three of those are properties of the vocabulary's **size**, not of its
 * design:
 *
 *   - *Identical on both platforms* holds only while every block has one
 *     obvious native rendering. `card` is a title and some labelled values;
 *     there is no arrangement to get wrong. A block with a layout parameter has
 *     two renderings and one of them is somebody's bug report.
 *   - *Cannot phone home* holds only while nothing here fetches. There are no
 *     URLs in this vocabulary — no images, no links, no stylesheets, no web
 *     views. A remote image is an exfiltration channel with a pretty name: the
 *     URL it is fetched from is chosen by the app and carries whatever the app
 *     put in it.
 *   - *Reviewed as a manifest* holds only while a reviewer can read the whole
 *     vocabulary. Four block kinds fit on a page.
 *
 * A vocabulary that grows until it is a rendering engine loses all three at
 * once, so [BLOCK_KINDS] is closed, every field is plain text with a length
 * cap, and adding a kind is a decision about the product rather than a patch.
 *
 * # Versioned, and refused rather than guessed
 *
 * [VOCABULARY_VERSION] travels inside every view. A host that does not know a
 * version **refuses to draw it** — it never renders the parts it recognises and
 * drops the rest, because a confirmation whose question rendered and whose
 * buttons did not is worse than a screen that says "this app needs a newer
 * Relay".
 *
 * # Transport
 *
 * A view travels to the phone inside `SYSTEM.md` §6.1's `ui.render` frame:
 *
 *     { v: 1, id: "<uuid>", type: "ui.render", at: <unix_ms>, payload: { … } }
 *
 * [renderFrame] builds one. A view carrying a `confirm` sets
 * `payload.expects = "decision"`, and the answer comes back on the
 * `consent.decision` frame §6.1 already defines, keyed by this frame's `id`.
 * No new frame in either direction: a second confirmation channel would have to
 * re-earn everything the first one already enforces.
 */

import type { PermissionScope } from "./manifest.ts";

// --- version ----------------------------------------------------------------

/**
 * The vocabulary's version, carried inside every view.
 *
 * Bumped only when a host that understands the old version could draw a new
 * view *wrongly*. Adding an optional field a host can ignore is not a bump;
 * adding a block kind is, because ignoring a block loses content the app
 * intended the user to see.
 */
export const VOCABULARY_VERSION = 1;

/** The envelope version from `SYSTEM.md` §6.1. Not the same number as above. */
export const ENVELOPE_VERSION = 1;

/** The `type` of the server→phone frame a view travels in. */
export const RENDER_FRAME = "ui.render";

// --- the vocabulary ---------------------------------------------------------

/**
 * Every block kind there is.
 *
 * Four, matching `APP-PLATFORM.md` §7 word for word: "a card, a list, a
 * confirmation, a spoken response". Adding a fifth means every host on every
 * platform has to grow a renderer for it, and means a reviewer reading a
 * manifest can no longer picture what the app will put on the screen.
 */
export const BLOCK_KINDS = ["card", "list", "confirm", "speak"] as const;

export type BlockKind = (typeof BLOCK_KINDS)[number];

/** One labelled value on a card. Plain text, both halves. */
export interface Field {
  label: string;
  value: string;
}

/**
 * A card: a title, an optional paragraph, and up to [LIMITS.cardFields]
 * labelled values. The native rendering is a titled panel; there is nothing to
 * arrange and therefore nothing to arrange differently on the two platforms.
 */
export interface CardBlock {
  kind: "card";
  title: string;
  body?: string;
  fields?: Field[];
}

/** One row of a list. */
export interface ListItem {
  title: string;
  subtitle?: string;
  /** Trailing text — a time, a count, a status. Rendered at the end of the row. */
  detail?: string;
}

/** A list: an optional heading and up to [LIMITS.listItems] rows. */
export interface ListBlock {
  kind: "list";
  title?: string;
  items: ListItem[];
}

/**
 * A confirmation: one question and two buttons.
 *
 * The answer returns on `consent.decision` (`SYSTEM.md` §6.1), keyed by the
 * `ui.render` frame's id. Two buttons and not N: a chooser is a different
 * control with different affordances on the two platforms, and an app that
 * needs one can render a `list` and ask about the choice in a second turn.
 */
export interface ConfirmBlock {
  kind: "confirm";
  question: string;
  /** Defaults to "Yes" when the host is drawing it. */
  confirmLabel?: string;
  /** Defaults to "No". */
  cancelLabel?: string;
  /** One line of context under the question. */
  detail?: string;
}

/**
 * A spoken reply.
 *
 * The only block that costs a permission: speech is `glasses.speaker`, and an
 * app that could put a `speak` block in a view would have the speaker without
 * having asked for it. [checkScopes] is the enforcement on this side and
 * relayd's mirror of this file is the enforcement that counts.
 */
export interface SpeakBlock {
  kind: "speak";
  text: string;
}

export type Block = CardBlock | ListBlock | ConfirmBlock | SpeakBlock;

/**
 * What an app yields: a version and an ordered handful of blocks.
 *
 * Ordered because "a card, then read it out" is the common shape and two calls
 * would let the speech arrive before the card. Small because a view is a reply,
 * not a screen.
 */
export interface View {
  vocabulary: number;
  blocks: Block[];
}

/**
 * Every cap, in one frozen object, so a reviewer can read the whole envelope of
 * what an app can put on a phone without reading the validator.
 *
 * Caps exist for the same reason the vocabulary is closed: a rendering engine
 * has no caps, and something with no caps is not reviewable. They are also the
 * difference between a card and a wall of text an app used to hide a URL in.
 */
export const LIMITS = Object.freeze({
  /** Blocks in one view. */
  blocks: 8,
  cardTitle: 120,
  cardBody: 2000,
  cardFields: 12,
  fieldLabel: 60,
  fieldValue: 240,
  listTitle: 120,
  listItems: 50,
  itemTitle: 120,
  itemSubtitle: 240,
  itemDetail: 60,
  question: 240,
  buttonLabel: 32,
  confirmDetail: 600,
  speakText: 1000,
  /** The serialised view, in bytes. A frame larger than this is refused. */
  bytes: 16 * 1024,
});

/**
 * The scope each block kind costs.
 *
 * Only `speak` costs one. Drawing on the phone of the person who installed the
 * app is not a capability that reaches anything of theirs — it cannot read,
 * cannot fetch and cannot capture — so it is minted like `storage` and `log`
 * are, without a scope. Speaking is different: it comes out of the glasses in
 * someone's ear, and `APP-PLATFORM.md` §3 already sells that as
 * `glasses.speaker`.
 */
export const BLOCK_SCOPES: Readonly<Partial<Record<BlockKind, PermissionScope>>> = Object.freeze({
  speak: "glasses.speaker",
});

// --- errors -----------------------------------------------------------------

/** A view that will not render. Thrown by [parseView] and [checkScopes]. */
export class ViewError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ViewError";
  }
}

// --- builders ---------------------------------------------------------------

/**
 * Build a card.
 *
 * The builders exist so an app never hand-writes `{ kind: "card" }` and never
 * hand-writes the version. They validate nothing on their own — [view] does,
 * once, over the whole thing, so an app gets one error naming one field rather
 * than an exception from whichever builder happened to run first.
 */
export function card(title: string, rest: Omit<CardBlock, "kind" | "title"> = {}): CardBlock {
  return { kind: "card", title, ...rest };
}

/** Build a list. */
export function list(items: ListItem[], rest: Omit<ListBlock, "kind" | "items"> = {}): ListBlock {
  return { kind: "list", items, ...rest };
}

/** Build a confirmation. */
export function confirm(
  question: string,
  rest: Omit<ConfirmBlock, "kind" | "question"> = {},
): ConfirmBlock {
  return { kind: "confirm", question, ...rest };
}

/** Build a spoken reply. Requires `glasses.speaker`. */
export function speak(text: string): SpeakBlock {
  return { kind: "speak", text };
}

/**
 * Assemble and validate a view.
 *
 * Validation happens here rather than at render time on purpose: an app should
 * find out that its card is too long while it is still running and can say
 * something shorter, not after the frame has been dropped by a host it cannot
 * hear from.
 */
export function view(...blocks: Block[]): View {
  return parseView({ vocabulary: VOCABULARY_VERSION, blocks });
}

// --- validation -------------------------------------------------------------

function fail(message: string): never {
  throw new ViewError(message);
}

const ALLOWED_KEYS: Readonly<Record<BlockKind, readonly string[]>> = Object.freeze({
  card: ["kind", "title", "body", "fields"],
  list: ["kind", "title", "items"],
  confirm: ["kind", "question", "confirmLabel", "cancelLabel", "detail"],
  speak: ["kind", "text"],
});

/**
 * Control characters are refused rather than stripped.
 *
 * A card is text on a phone, not a terminal: an escape sequence in a title has
 * no meaning the host should try to interpret, and silently removing it would
 * hide from the app that it sent something it did not mean to. Tab and newline
 * are allowed in the two fields that are paragraphs.
 */
const CONTROL = /[\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F]/;
const LINE_BREAK = /[\n\t]/;

function text(
  value: unknown,
  where: string,
  max: number,
  options: { multiline?: boolean; optional?: boolean } = {},
): string | undefined {
  if (value === undefined) {
    if (options.optional) return undefined;
    fail(`${where} is required`);
  }
  if (typeof value !== "string") fail(`${where} must be a string, got ${typeOf(value)}`);
  const s = value;
  if (s.trim() === "") fail(`${where} is empty — a blank ${where} draws a blank space`);
  if (s.length > max) fail(`${where} is ${s.length} characters; the limit is ${max}`);
  if (CONTROL.test(s) || (!options.multiline && LINE_BREAK.test(s))) {
    fail(`${where} contains a control character; a view is text a phone draws, not a terminal`);
  }
  return s;
}

function typeOf(value: unknown): string {
  if (value === null) return "null";
  if (Array.isArray(value)) return "an array";
  return typeof value;
}

function keys(block: Record<string, unknown>, kind: BlockKind, index: number): void {
  const allowed = ALLOWED_KEYS[kind];
  for (const key of Object.keys(block)) {
    if (!allowed.includes(key)) {
      fail(
        `blocks[${index}] is a ${kind} with an unknown field "${key}". ` +
          `A ${kind} draws ${allowed.filter((k) => k !== "kind").join(", ")} and nothing else — ` +
          `a field the host does not know is a field it would not draw`,
      );
    }
  }
}

function parseBlock(input: unknown, i: number): Block {
  if (input === null || typeof input !== "object" || Array.isArray(input)) {
    fail(`blocks[${i}] must be an object, got ${typeOf(input)}`);
  }
  const raw = input as Record<string, unknown>;
  const kind = raw.kind;
  if (typeof kind !== "string" || !(BLOCK_KINDS as readonly string[]).includes(kind)) {
    fail(
      `blocks[${i}].kind is ${JSON.stringify(kind)}; the vocabulary is ${BLOCK_KINDS.join(", ")}`,
    );
  }
  const k = kind as BlockKind;
  keys(raw, k, i);

  switch (k) {
    case "card": {
      const block: CardBlock = { kind: "card", title: text(raw.title, `blocks[${i}].title`, LIMITS.cardTitle)! };
      const body = text(raw.body, `blocks[${i}].body`, LIMITS.cardBody, {
        multiline: true,
        optional: true,
      });
      if (body !== undefined) block.body = body;
      if (raw.fields !== undefined) {
        if (!Array.isArray(raw.fields)) fail(`blocks[${i}].fields must be an array`);
        if (raw.fields.length > LIMITS.cardFields) {
          fail(`blocks[${i}].fields has ${raw.fields.length} entries; the limit is ${LIMITS.cardFields}`);
        }
        block.fields = raw.fields.map((f, j): Field => {
          if (f === null || typeof f !== "object" || Array.isArray(f)) {
            fail(`blocks[${i}].fields[${j}] must be an object`);
          }
          const field = f as Record<string, unknown>;
          for (const key of Object.keys(field)) {
            if (key !== "label" && key !== "value") {
              fail(`blocks[${i}].fields[${j}] has an unknown field "${key}"`);
            }
          }
          return {
            label: text(field.label, `blocks[${i}].fields[${j}].label`, LIMITS.fieldLabel)!,
            value: text(field.value, `blocks[${i}].fields[${j}].value`, LIMITS.fieldValue)!,
          };
        });
      }
      return block;
    }
    case "list": {
      if (!Array.isArray(raw.items)) fail(`blocks[${i}].items must be an array`);
      if (raw.items.length === 0) {
        fail(`blocks[${i}].items is empty — an empty list draws as a heading with nothing under it`);
      }
      if (raw.items.length > LIMITS.listItems) {
        fail(`blocks[${i}].items has ${raw.items.length} entries; the limit is ${LIMITS.listItems}`);
      }
      const block: ListBlock = {
        kind: "list",
        items: raw.items.map((it, j): ListItem => {
          if (it === null || typeof it !== "object" || Array.isArray(it)) {
            fail(`blocks[${i}].items[${j}] must be an object`);
          }
          const item = it as Record<string, unknown>;
          for (const key of Object.keys(item)) {
            if (key !== "title" && key !== "subtitle" && key !== "detail") {
              fail(`blocks[${i}].items[${j}] has an unknown field "${key}"`);
            }
          }
          const row: ListItem = {
            title: text(item.title, `blocks[${i}].items[${j}].title`, LIMITS.itemTitle)!,
          };
          const subtitle = text(item.subtitle, `blocks[${i}].items[${j}].subtitle`, LIMITS.itemSubtitle, {
            optional: true,
          });
          if (subtitle !== undefined) row.subtitle = subtitle;
          const detail = text(item.detail, `blocks[${i}].items[${j}].detail`, LIMITS.itemDetail, {
            optional: true,
          });
          if (detail !== undefined) row.detail = detail;
          return row;
        }),
      };
      const title = text(raw.title, `blocks[${i}].title`, LIMITS.listTitle, { optional: true });
      if (title !== undefined) block.title = title;
      return block;
    }
    case "confirm": {
      const block: ConfirmBlock = {
        kind: "confirm",
        question: text(raw.question, `blocks[${i}].question`, LIMITS.question)!,
      };
      const confirmLabel = text(raw.confirmLabel, `blocks[${i}].confirmLabel`, LIMITS.buttonLabel, {
        optional: true,
      });
      if (confirmLabel !== undefined) block.confirmLabel = confirmLabel;
      const cancelLabel = text(raw.cancelLabel, `blocks[${i}].cancelLabel`, LIMITS.buttonLabel, {
        optional: true,
      });
      if (cancelLabel !== undefined) block.cancelLabel = cancelLabel;
      const detail = text(raw.detail, `blocks[${i}].detail`, LIMITS.confirmDetail, {
        multiline: true,
        optional: true,
      });
      if (detail !== undefined) block.detail = detail;
      return block;
    }
    default: {
      return { kind: "speak", text: text(raw.text, `blocks[${i}].text`, LIMITS.speakText, { multiline: true })! };
    }
  }
}

/**
 * Validate a view, from an app or off the wire.
 *
 * Strict, and strict about the version first: a view whose vocabulary this host
 * does not know is refused whole. Partially drawing a view from the future is
 * how a confirmation ends up on screen with the wrong buttons.
 */
export function parseView(input: unknown): View {
  if (input === null || typeof input !== "object" || Array.isArray(input)) {
    fail(`a view must be an object, got ${typeOf(input)}`);
  }
  const raw = input as Record<string, unknown>;
  for (const key of Object.keys(raw)) {
    if (key !== "vocabulary" && key !== "blocks") {
      fail(`a view has "vocabulary" and "blocks"; "${key}" is not part of the format`);
    }
  }
  if (raw.vocabulary !== VOCABULARY_VERSION) {
    fail(
      `this view says vocabulary ${JSON.stringify(raw.vocabulary)} and this host draws ` +
        `${VOCABULARY_VERSION}. Refusing the whole view rather than drawing the parts it ` +
        `recognises: a confirmation with a question and no buttons is worse than a screen ` +
        `that says the app needs a newer Relay`,
    );
  }
  if (!Array.isArray(raw.blocks)) fail("blocks must be an array");
  if (raw.blocks.length === 0) fail("a view with no blocks renders nothing; say something or say nothing");
  if (raw.blocks.length > LIMITS.blocks) {
    fail(`a view has ${raw.blocks.length} blocks; the limit is ${LIMITS.blocks}`);
  }

  const blocks = raw.blocks.map(parseBlock);
  const count = (kind: BlockKind): number => blocks.filter((b) => b.kind === kind).length;
  if (count("confirm") > 1) {
    fail("a view asks at most one question — two confirmations in one view have no defined answer");
  }
  if (count("speak") > 1) {
    fail("a view speaks at most once — two spoken blocks would talk over each other");
  }

  const out: View = { vocabulary: VOCABULARY_VERSION, blocks };
  const size = byteLength(JSON.stringify(out));
  if (size > LIMITS.bytes) {
    fail(`this view serialises to ${size} bytes; the limit is ${LIMITS.bytes}`);
  }
  return out;
}

function byteLength(s: string): number {
  return new TextEncoder().encode(s).length;
}

/**
 * Refuse a view containing a block the app was not granted.
 *
 * Called with `ctx.granted`, which is what the user actually consented to and
 * may be narrower than the manifest asked for. An app that lost
 * `glasses.speaker` at the install sheet does not get to speak through a view —
 * "never emit an event you cannot observe" applies to the output side too, and
 * the alternative is a `speak` block the host silently drops.
 */
export function checkScopes(v: View, granted: readonly PermissionScope[]): View {
  for (const block of v.blocks) {
    const needed = BLOCK_SCOPES[block.kind];
    if (needed && !granted.includes(needed)) {
      throw new ViewError(
        `a ${block.kind} block needs the ${needed} permission and this app was not granted it. ` +
          `Ask for it in relay.json with a reason the user will read`,
      );
    }
  }
  return v;
}

/**
 * The plain-text projection of a view.
 *
 * Two callers, one format. The agent that called an app as an MCP tool reads
 * this — it has no screen — and so does anything that has to log or narrate
 * what an app put in front of someone. It is deliberately not a rendering:
 * there is no attempt at columns or box drawing, because the thing that draws
 * this properly is the phone.
 */
export function viewText(v: View): string {
  const lines: string[] = [];
  for (const block of v.blocks) {
    switch (block.kind) {
      case "card":
        lines.push(block.title);
        if (block.body) lines.push(block.body);
        for (const f of block.fields ?? []) lines.push(`${f.label}: ${f.value}`);
        break;
      case "list":
        if (block.title) lines.push(block.title);
        for (const item of block.items) {
          const bits = [item.title, item.subtitle, item.detail].filter((x): x is string => !!x);
          lines.push(`- ${bits.join(" — ")}`);
        }
        break;
      case "confirm":
        lines.push(block.question);
        if (block.detail) lines.push(block.detail);
        lines.push(`[${block.confirmLabel ?? "Yes"} / ${block.cancelLabel ?? "No"}]`);
        break;
      case "speak":
        lines.push(block.text);
        break;
    }
  }
  return lines.join("\n");
}

/** Whether this view asks a question the host must collect an answer to. */
export function expectsDecision(v: View): boolean {
  return v.blocks.some((b) => b.kind === "confirm");
}

// --- transport --------------------------------------------------------------

/** What a `ui.render` frame carries. */
export interface RenderPayload {
  /** Which app drew it. The host labels the card with this; apps cannot spoof it. */
  app: string;
  /** The invocation, so a log line and a rendered card can be tied together. */
  invocation?: string;
  view: View;
  /**
   * Set when the view contains a `confirm`. The host answers on
   * `consent.decision` (`SYSTEM.md` §6.1) carrying this frame's `id`.
   */
  expects?: "decision";
}

/** `SYSTEM.md` §6.1's envelope, carrying a view. */
export interface RenderFrame {
  v: number;
  id: string;
  type: typeof RENDER_FRAME;
  at: number;
  payload: RenderPayload;
}

/**
 * Wrap a view in the frame that carries it to the phone.
 *
 * `app` is stamped by whoever builds the frame — relayd — and never by the app,
 * because a card that could claim to be from another app is a phishing surface
 * on a device whose whole pitch is that you can trust what it shows you.
 */
export function renderFrame(
  v: View,
  meta: { id: string; at: number; app: string; invocation?: string },
): RenderFrame {
  const payload: RenderPayload = { app: meta.app, view: parseView(v) };
  if (meta.invocation) payload.invocation = meta.invocation;
  if (expectsDecision(v)) payload.expects = "decision";
  return { v: ENVELOPE_VERSION, id: meta.id, type: RENDER_FRAME, at: meta.at, payload };
}

/**
 * Validate a `ui.render` frame off the wire — the host app's side.
 *
 * A phone that received a frame it cannot fully validate draws nothing and says
 * why. It does not draw the half it understood.
 */
export function parseRenderFrame(input: unknown): RenderFrame {
  if (input === null || typeof input !== "object" || Array.isArray(input)) {
    fail(`a frame must be an object, got ${typeOf(input)}`);
  }
  const raw = input as Record<string, unknown>;
  if (raw.v !== ENVELOPE_VERSION) {
    fail(`envelope version ${JSON.stringify(raw.v)} is not ${ENVELOPE_VERSION}`);
  }
  if (raw.type !== RENDER_FRAME) fail(`this is a ${JSON.stringify(raw.type)} frame, not ${RENDER_FRAME}`);
  if (typeof raw.id !== "string" || raw.id === "") fail("a frame needs an id, so an answer can name it");
  if (typeof raw.at !== "number" || !Number.isFinite(raw.at)) fail("at must be a unix millisecond timestamp");
  const payload = raw.payload;
  if (payload === null || typeof payload !== "object" || Array.isArray(payload)) {
    fail("payload must be an object");
  }
  const p = payload as Record<string, unknown>;
  if (typeof p.app !== "string" || p.app === "") {
    fail("payload.app must name the app, so the host can label the card with something the app cannot forge");
  }
  const v = parseView(p.view);
  const out: RenderFrame = {
    v: ENVELOPE_VERSION,
    id: raw.id,
    type: RENDER_FRAME,
    at: raw.at,
    payload: { app: p.app, view: v },
  };
  if (typeof p.invocation === "string" && p.invocation !== "") out.payload.invocation = p.invocation;
  if (expectsDecision(v)) out.payload.expects = "decision";
  else if (p.expects !== undefined) {
    fail(`payload.expects is set and no block asks a question; the host would wait for an answer nobody will give`);
  }
  return out;
}

// --- the capability ---------------------------------------------------------

/**
 * `ctx.ui` — how an app puts something in front of the person wearing the
 * glasses.
 *
 * Every method resolves when the frame has been handed to the transport, not
 * when a human has looked at it — except [UICapability.ask], which resolves
 * with the answer, because a question whose answer nobody waits for is not a
 * question.
 */
export interface UICapability {
  /** Draw a view. Rejects with a [ViewError] if it does not validate. */
  render(v: View): Promise<void>;
  /** Draw one card. Shorthand for the overwhelmingly common case. */
  card(title: string, rest?: Omit<CardBlock, "kind" | "title">): Promise<void>;
  /** Draw one list. */
  list(items: ListItem[], rest?: Omit<ListBlock, "kind" | "items">): Promise<void>;
  /**
   * Ask a yes-or-no question and wait for the answer.
   *
   * Resolves false when the user says no *and* when nobody answered in time:
   * an app must treat silence as a no, and giving it a third outcome to ignore
   * is how "confirm before you send" becomes "send".
   */
  ask(question: string, rest?: Omit<ConfirmBlock, "kind" | "question">): Promise<boolean>;
}
