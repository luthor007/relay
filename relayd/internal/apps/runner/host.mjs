// The in-sandbox half of the SDK.
//
// This file is relayd's, not the app's, and it is the only code inside the
// sandbox that relayd wrote. Its whole job is to turn the capability list the
// host minted into the `ctx` object APP-PLATFORM.md §2 shows, hand it to the
// app's `onTrigger`, and say what happened.
//
// Three things about it are load-bearing:
//
//   1. **It blocks until the host sends `start`.** Nothing of the app's is
//      imported before that frame arrives, and the host does not send it until
//      the resource limits have been applied to this process. That handshake is
//      how the caps come to be in force before app code loads — Go gives no hook
//      between fork and exec, so the ordering has to be bought with a message.
//
//   2. **`ctx` is built from the capability list and nothing else.** There is no
//      table of methods in this file that the host did not send. A capability the
//      manifest did not ask for is not a property that refuses; it is a property
//      that does not exist, and an object whose members are all ungranted is
//      never created at all. `"memory" in ctx` is false, and that is the point.
//
//   3. **It is not the security boundary.** The app runs in this process and can
//      reach anything this file can reach — including these two pipes. That is
//      fine and expected: the host keeps its own table of granted methods, so
//      speaking the protocol by hand buys an app exactly nothing. The boundaries
//      are the sandbox around this process and the table on the other end of the
//      pipe, never the discipline of this file.
//
// The channel is newline-delimited JSON on fd 3 (host → app) and fd 4
// (app → host). Pipes rather than a socket, because the strongest sandbox puts
// this process in an empty network namespace where a socket to relayd would not
// exist.

import net from "node:net";
import { pathToFileURL } from "node:url";

const IN_FD = 3;
const OUT_FD = 4;

const inbound = new net.Socket({ fd: IN_FD, readable: true, writable: false });
const outbound = new net.Socket({ fd: OUT_FD, readable: false, writable: true });

let nextId = 1;
const pending = new Map();

function send(frame) {
  outbound.write(JSON.stringify(frame) + "\n");
}

/** One capability call. Resolves with the host's result, rejects with its error. */
function call(method, args) {
  const id = nextId++;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject, chunks: null });
    send({ t: "call", id, method, args: args ?? {} });
  });
}

/**
 * A streaming capability call. The host emits `chunk` frames and closes with
 * `ok`; this turns them into an async iterable so the app can `for await` it.
 */
function callStream(method, args) {
  const id = nextId++;
  const queue = [];
  let push = null;
  let ended = false;
  let failure = null;

  pending.set(id, {
    resolve: () => {
      ended = true;
      if (push) push();
    },
    reject: (err) => {
      failure = err;
      ended = true;
      if (push) push();
    },
    chunks: (value) => {
      queue.push(value);
      if (push) push();
    },
  });
  send({ t: "call", id, method, args: args ?? {} });

  return {
    async *[Symbol.asyncIterator]() {
      for (;;) {
        if (queue.length > 0) {
          yield queue.shift();
          continue;
        }
        if (failure) throw failure;
        if (ended) return;
        await new Promise((r) => {
          push = () => {
            push = null;
            r();
          };
        });
      }
    },
  };
}

class CapabilityError extends Error {
  constructor(code, message) {
    super(message);
    this.name = "CapabilityError";
    this.code = code;
  }
}

function onFrame(frame) {
  if (frame.t === "start") {
    run(frame).catch((err) => fail(err));
    return;
  }
  const waiter = pending.get(frame.id);
  if (!waiter) return;
  switch (frame.t) {
    case "chunk":
      if (waiter.chunks) waiter.chunks(frame.value);
      return;
    case "ok":
      pending.delete(frame.id);
      waiter.resolve(frame.result ?? null);
      return;
    case "err":
      pending.delete(frame.id);
      waiter.reject(new CapabilityError(frame.error?.code ?? "failed", frame.error?.message ?? "call failed"));
      return;
  }
}

let buffer = "";
inbound.on("data", (chunk) => {
  buffer += chunk.toString("utf8");
  for (;;) {
    const i = buffer.indexOf("\n");
    if (i < 0) break;
    const line = buffer.slice(0, i);
    buffer = buffer.slice(i + 1);
    if (line.trim() === "") continue;
    try {
      onFrame(JSON.parse(line));
    } catch (err) {
      fail(err);
      return;
    }
  }
});

inbound.on("error", () => finish(0));

// --- shaping the host's answers into the SDK's types -------------------------

function reviveEpisode(e) {
  if (!e) return null;
  return {
    ...e,
    startedAt: new Date(e.startedAt),
    endedAt: new Date(e.endedAt),
  };
}

function reviveCommitment(c) {
  return { ...c, dueAt: c.dueAt ? new Date(c.dueAt) : undefined };
}

/**
 * The shaping table: for each host method, the function `ctx` gets.
 *
 * Every entry here corresponds to a row of the host's capability table. An entry
 * whose method the host did not mint is never reached, because the loop below
 * walks the host's list rather than this one.
 */
const shapers = {
  "memory.search": (m) => async (query, options = {}) =>
    (await call(m, { query, limit: options.limit, since: options.since ? +options.since : undefined }) ?? [])
      .map(reviveEpisode),
  "memory.recentEpisode": (m) => async (filter = {}) =>
    reviveEpisode(await call(m, { kind: filter.kind, within: filter.within })),
  "memory.get": (m) => async (episodeId) => reviveEpisode(await call(m, { episodeId })),
  "memory.extractCommitments": (m) => async (episode) =>
    (await call(m, { episode }) ?? []).map(reviveCommitment),
  "memory.write": (m) => async (note) => call(m, { note }),

  "glasses.say": (m) => async (text) => {
    await call(m, { text });
  },
  "glasses.capture": (m) => async (options = {}) => {
    const res = await call(m, { immediate: options.immediate === true });
    if (res && typeof res.data === "string") {
      return { id: res.id, data: Uint8Array.from(Buffer.from(res.data, "base64")) };
    }
    return { id: res?.id };
  },
  "glasses.listen": (m) => async (options = {}) => call(m, { timeoutMs: options.timeoutMs }),

  "agent.ask": (m) => async (prompt, options = {}) => call(m, { prompt, model: options.model }),
  "agent.stream": (m) => (prompt) => callStream(m, { prompt }),

  // The view is sent as-is. The SDK's `view()` builder has already validated it
  // in the app's own process, and the host validates it again on the other side
  // of this pipe — that second pass is the one that counts, because everything
  // in here is inside the sandbox with the app and is not a boundary. Shaping it
  // further would be a third opinion about the vocabulary in the one place that
  // has the least authority to hold one.
  "ui.render": (m) => async (view) => {
    await call(m, view);
  },
  "ui.ask": (m) => async (view) => call(m, view),

  "storage.get": (m) => async (key) => call(m, { key }),
  "storage.set": (m) => async (key, value) => {
    await call(m, { key, value });
  },
  "storage.delete": (m) => async (key) => {
    await call(m, { key });
  },

  fetch: (m) => async (input, init = {}) => {
    const url = typeof input === "string" ? input : String(input);
    const headers = {};
    if (init.headers) {
      const entries =
        typeof init.headers.entries === "function"
          ? [...init.headers.entries()]
          : Object.entries(init.headers);
      for (const [k, v] of entries) headers[k] = String(v);
    }
    const res = await call(m, {
      url,
      method: init.method ?? "GET",
      headers,
      body: typeof init.body === "string" ? init.body : undefined,
    });
    // 204, 205 and 304 are null-body statuses: the Response constructor throws
    // if it is handed a body with one, even an empty string. A 204 from a real
    // API would otherwise reach the app as "Invalid response status code",
    // which is a sentence about us wearing the server's name.
    const nullBody = res.status === 101 || res.status === 204 || res.status === 205 || res.status === 304;
    return new Response(nullBody || res.body === "" ? null : res.body, {
      status: res.status,
      statusText: res.statusText,
      headers: flatten(res.headers),
    });
  },

  log: (m) => (message, data) => {
    // Deliberately not awaited: `ctx.log` is synchronous in the SDK, and a log
    // line that makes an app's control flow wait on relayd is a log line that
    // changes what the app does.
    call(m, { level: "info", message: String(message), data }).catch(() => {});
  },
};

function flatten(headers) {
  const out = {};
  for (const [k, vs] of Object.entries(headers ?? {})) {
    out[k] = Array.isArray(vs) ? vs.join(", ") : String(vs);
  }
  return out;
}

// --- the run -----------------------------------------------------------------

async function run(start) {
  const ctx = {
    trigger: start.trigger,
    granted: Object.freeze([...(start.granted ?? [])]),
    declined: Object.freeze([...(start.declined ?? [])]),
  };

  for (const cap of start.capabilities ?? []) {
    const shape = shapers[cap.method];
    if (!shape) continue; // A capability this runner does not know how to shape.
    const fn = shape(cap.method);
    if (cap.object) {
      if (!ctx[cap.object]) ctx[cap.object] = {};
      ctx[cap.object][cap.member] = fn;
    } else {
      ctx[cap.member] = fn;
    }
  }

  // `ctx.say` is the SDK's shorthand for the overwhelmingly common case, and it
  // exists only when the speaker does. An app without `glasses.speaker` has no
  // `say`, for the same reason it has no `glasses`: the shorthand cannot be more
  // generous than the thing it is short for.
  if (ctx.glasses && typeof ctx.glasses.say === "function") {
    ctx.say = (text) => ctx.glasses.say(text);
  }

  // `ui.card`, `ui.list` and the argument shape of `ui.ask` are the SDK's
  // shorthands for the two overwhelmingly common cases, built here for the same
  // reason `ctx.say` is: they are sugar over a capability that already exists,
  // so they appear exactly when it does and cannot be more generous than it.
  //
  // They assemble a one-block view rather than calling a second host method.
  // Every path an app can take to the phone therefore goes through the same
  // validator on the far side, which is what stops a shorthand from becoming a
  // way to send something `render` would have refused.
  if (ctx.ui && typeof ctx.ui.render === "function") {
    const one = (block) => ({ vocabulary: 1, blocks: [block] });
    ctx.ui.card = (title, rest = {}) => ctx.ui.render(one({ kind: "card", title, ...rest }));
    ctx.ui.list = (items, rest = {}) => ctx.ui.render(one({ kind: "list", items, ...rest }));
    const ask = ctx.ui.ask;
    if (typeof ask === "function") {
      ctx.ui.ask = (question, rest = {}) =>
        typeof question === "string"
          ? ask(one({ kind: "confirm", question, ...rest }))
          : ask(question);
    }
  }

  const mod = await import(pathToFileURL(start.entry).href);
  const app = mod.default ?? mod.app;
  if (!app || typeof app.onTrigger !== "function") {
    throw new Error("this app has no default export with an onTrigger — see defineApp in @relay/sdk");
  }
  await app.onTrigger(ctx);
  finish(0, { t: "done" });
}

function fail(err) {
  finish(1, {
    t: "failed",
    error: {
      message: err && err.message ? String(err.message) : String(err),
      name: err && err.name ? String(err.name) : "Error",
      stack: err && err.stack ? String(err.stack) : "",
    },
  });
}

let finished = false;
function finish(code, frame) {
  if (finished) return;
  finished = true;
  const done = () => {
    process.exitCode = code;
    inbound.destroy();
    outbound.end();
  };
  if (frame) {
    outbound.write(JSON.stringify(frame) + "\n", done);
  } else {
    done();
  }
}

process.on("uncaughtException", (err) => fail(err));
process.on("unhandledRejection", (err) => fail(err));
