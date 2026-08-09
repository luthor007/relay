import assert from "node:assert/strict";
import { describe, test } from "node:test";

import { TypedEmitter } from "../src/emitter.ts";

type Events = { ping: number; pong: string };

describe("TypedEmitter", () => {
  test("delivers to every listener in registration order", () => {
    const emitter = new TypedEmitter<Events>();
    const seen: string[] = [];
    emitter.on("ping", (n) => seen.push(`a${n}`));
    emitter.on("ping", (n) => seen.push(`b${n}`));

    emitter.emit("ping", 1);
    assert.deepEqual(seen, ["a1", "b1"]);
  });

  test("unsubscribe stops delivery", () => {
    const emitter = new TypedEmitter<Events>();
    const seen: number[] = [];
    const off = emitter.on("ping", (n) => seen.push(n));

    emitter.emit("ping", 1);
    off();
    emitter.emit("ping", 2);

    assert.deepEqual(seen, [1]);
    assert.equal(emitter.listenerCount("ping"), 0);
  });

  test("once fires exactly once", () => {
    const emitter = new TypedEmitter<Events>();
    const seen: number[] = [];
    emitter.once("ping", (n) => seen.push(n));

    emitter.emit("ping", 1);
    emitter.emit("ping", 2);

    assert.deepEqual(seen, [1]);
  });

  test("next resolves on the following emission", async () => {
    const emitter = new TypedEmitter<Events>();
    const pending = emitter.next("pong");
    emitter.emit("pong", "hello");
    assert.equal(await pending, "hello");
  });

  test("a throwing listener does not starve the others", () => {
    const emitter = new TypedEmitter<Events>();
    const errors: unknown[] = [];
    emitter.onHandlerError = (_event, error) => errors.push(error);

    const seen: string[] = [];
    emitter.on("ping", () => seen.push("before"));
    emitter.on("ping", () => {
      throw new Error("listener blew up");
    });
    emitter.on("ping", () => seen.push("after"));

    emitter.emit("ping", 1);

    assert.deepEqual(seen, ["before", "after"], "a UI bug must not stop capture");
    assert.equal(errors.length, 1);
  });

  test("a listener may unsubscribe another mid-emit without skipping anyone", () => {
    const emitter = new TypedEmitter<Events>();
    const seen: string[] = [];

    emitter.on("ping", () => {
      seen.push("first");
      offSecond();
    });
    const offSecond = emitter.on("ping", () => seen.push("second"));
    emitter.on("ping", () => seen.push("third"));

    emitter.emit("ping", 1);
    assert.deepEqual(seen, ["first", "second", "third"]);

    seen.length = 0;
    emitter.emit("ping", 2);
    assert.deepEqual(seen, ["first", "third"]);
  });

  test("emitting an event with no listeners is a no-op", () => {
    const emitter = new TypedEmitter<Events>();
    assert.doesNotThrow(() => emitter.emit("ping", 1));
  });

  test("removeAll clears every subscription", () => {
    const emitter = new TypedEmitter<Events>();
    emitter.on("ping", () => {});
    emitter.on("pong", () => {});
    emitter.removeAll();

    assert.equal(emitter.listenerCount("ping"), 0);
    assert.equal(emitter.listenerCount("pong"), 0);
  });
});
