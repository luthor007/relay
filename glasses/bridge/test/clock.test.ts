import assert from "node:assert/strict";
import { describe, test } from "node:test";

import { FakeClock } from "../src/clock.ts";

describe("FakeClock", () => {
  test("does not move on its own", async () => {
    const clock = new FakeClock(1_000);
    let fired = false;
    clock.setTimeout(() => {
      fired = true;
    }, 10);

    await Promise.resolve();
    assert.equal(fired, false);
    assert.equal(clock.now(), 1_000);
  });

  test("fires timers due within the advance, in scheduled order", async () => {
    const clock = new FakeClock();
    const order: string[] = [];

    clock.setTimeout(() => order.push("c"), 300);
    clock.setTimeout(() => order.push("a"), 100);
    clock.setTimeout(() => order.push("b"), 200);
    clock.setTimeout(() => order.push("late"), 5_000);

    await clock.advance(1_000);

    assert.deepEqual(order, ["a", "b", "c"]);
    assert.equal(clock.now(), 1_000);
  });

  test("breaks ties by scheduling order", async () => {
    const clock = new FakeClock();
    const order: string[] = [];
    clock.setTimeout(() => order.push("first"), 50);
    clock.setTimeout(() => order.push("second"), 50);
    await clock.advance(50);
    assert.deepEqual(order, ["first", "second"]);
  });

  test("cancelled timers never fire", async () => {
    const clock = new FakeClock();
    let fired = false;
    const cancel = clock.setTimeout(() => {
      fired = true;
    }, 100);

    cancel();
    await clock.advance(1_000);
    assert.equal(fired, false);
  });

  test("sleep resumes at exactly the right time", async () => {
    const clock = new FakeClock();
    let resumedAt = -1;
    void clock.sleep(250).then(() => {
      resumedAt = clock.now();
    });

    await clock.advance(1_000);
    assert.equal(resumedAt, 250);
  });

  test("chained sleeps settle within a single advance", async () => {
    const clock = new FakeClock();
    const marks: number[] = [];

    void (async () => {
      await clock.sleep(100);
      marks.push(clock.now());
      await clock.sleep(100);
      marks.push(clock.now());
      await clock.sleep(100);
      marks.push(clock.now());
    })();

    await clock.advance(500);
    assert.deepEqual(marks, [100, 200, 300]);
  });

  test("tracks outstanding timers", async () => {
    const clock = new FakeClock();
    clock.setTimeout(() => {}, 100);
    clock.setTimeout(() => {}, 200);
    assert.equal(clock.pending, 2);

    await clock.advance(150);
    assert.equal(clock.pending, 1);
  });

  test("runAll drains everything, including timers scheduled by timers", async () => {
    const clock = new FakeClock();
    const order: number[] = [];

    clock.setTimeout(() => {
      order.push(1);
      clock.setTimeout(() => {
        order.push(2);
        clock.setTimeout(() => order.push(3), 100);
      }, 100);
    }, 100);

    await clock.runAll();
    assert.deepEqual(order, [1, 2, 3]);
    assert.equal(clock.pending, 0);
  });

  test("a negative delay is treated as immediate, not as time travel", async () => {
    const clock = new FakeClock(500);
    let firedAt = -1;
    clock.setTimeout(() => {
      firedAt = clock.now();
    }, -100);

    await clock.advance(1);
    assert.equal(firedAt, 500);
  });
});
