/**
 * Injectable clock.
 *
 * Everything timing-dependent in this package — photo transfer rate, battery
 * drain, recording duration, trace playback — goes through a Clock, so tests
 * assert real behaviour instantly instead of sleeping and hoping. A test that
 * waits on wall-clock time is a test that goes flaky on CI.
 */

export type CancelTimer = () => void;

export interface Clock {
  /** Milliseconds since an arbitrary epoch; monotonic. */
  now(): number;
  sleep(ms: number): Promise<void>;
  setTimeout(fn: () => void, ms: number): CancelTimer;
}

export const systemClock: Clock = {
  now: () => Date.now(),
  sleep: (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
  setTimeout: (fn, ms) => {
    const handle = setTimeout(fn, ms);
    return () => clearTimeout(handle);
  },
};

interface ScheduledTask {
  at: number;
  seq: number;
  fn: () => void;
  cancelled: boolean;
}

/**
 * Yield long enough for every pending microtask to run.
 *
 * A single `await Promise.resolve()` only advances one tick, which is not enough
 * for a multi-`await` async function to reach its next timer. Hopping to a
 * macrotask drains the whole microtask queue first, so timer-driven async code
 * settles fully before the fake clock moves again.
 */
const scheduleMacrotask: (fn: () => void) => void =
  typeof setImmediate === "function"
    ? (fn) => void setImmediate(fn)
    : (fn) => void setTimeout(fn, 0);

function drainMicrotasks(): Promise<void> {
  return new Promise((resolve) => scheduleMacrotask(() => resolve()));
}

/**
 * Deterministic clock for tests. Nothing fires until `advance` is called.
 *
 * `advance` runs due timers in scheduled order and yields to the microtask queue
 * between them, so promise chains resolve the way they would in real time.
 */
export class FakeClock implements Clock {
  #now: number;
  #seq = 0;
  #tasks: ScheduledTask[] = [];

  constructor(startMs = 0) {
    this.#now = startMs;
  }

  now(): number {
    return this.#now;
  }

  sleep(ms: number): Promise<void> {
    return new Promise((resolve) => {
      this.setTimeout(resolve, ms);
    });
  }

  setTimeout(fn: () => void, ms: number): CancelTimer {
    const task: ScheduledTask = {
      at: this.#now + Math.max(0, ms),
      seq: this.#seq++,
      fn,
      cancelled: false,
    };
    this.#tasks.push(task);
    return () => {
      task.cancelled = true;
    };
  }

  /** Number of timers still outstanding — useful for leak assertions. */
  get pending(): number {
    return this.#tasks.filter((t) => !t.cancelled).length;
  }

  /**
   * Move time forward, firing everything due along the way.
   *
   * Await this: it drains microtasks between timers so that code awaiting a
   * `sleep` has actually resumed by the time it returns.
   */
  async advance(ms: number): Promise<void> {
    const target = this.#now + ms;

    for (;;) {
      const due = this.#tasks
        .filter((t) => !t.cancelled && t.at <= target)
        .sort((a, b) => a.at - b.at || a.seq - b.seq);

      const next = due[0];
      if (!next) break;

      this.#tasks = this.#tasks.filter((t) => t !== next);
      this.#now = Math.max(this.#now, next.at);
      next.fn();
      // Let every continuation chained off this timer run before the next one
      // fires. This is what makes `clock.now()` observed inside a `.then()` the
      // time the work actually finished, rather than the end of the advance.
      await drainMicrotasks();
    }

    this.#now = target;
    await drainMicrotasks();
  }

  /** Advance until nothing is scheduled, or `maxIterations` rounds elapse. */
  async runAll(maxIterations = 10_000): Promise<void> {
    let i = 0;
    while (this.pending > 0 && i++ < maxIterations) {
      const soonest = this.#tasks
        .filter((t) => !t.cancelled)
        .reduce((min, t) => Math.min(min, t.at), Number.POSITIVE_INFINITY);
      if (!Number.isFinite(soonest)) break;
      await this.advance(Math.max(0, soonest - this.#now));
    }
  }
}
