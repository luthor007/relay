/**
 * Minimal typed event emitter.
 *
 * Node's EventEmitter is untyped and React Native's is a different
 * implementation again; this package has to run in both plus plain Node tests,
 * so it carries its own 40-line version rather than a dependency.
 *
 * A throwing handler must never break the transport or starve the handlers
 * registered after it — a UI bug should not stop audio capture. Handler errors
 * are routed to `onHandlerError` instead of propagating.
 */

import type { Unsubscribe } from "./types.ts";

export type Handler<T> = (payload: T) => void;

/**
 * `Events extends object` rather than `Record<string, unknown>`: an interface
 * does not gain an implicit index signature the way a type alias does, so
 * constraining to Record would reject `GlassesEvents`. Everything here indexes
 * via `keyof Events`, so `object` is sufficient and keeps the payload types exact.
 */
export class TypedEmitter<Events extends object> {
  #handlers = new Map<keyof Events, Set<Handler<never>>>();

  /** Called when a listener throws. Defaults to reporting on the console. */
  onHandlerError: (event: keyof Events, error: unknown) => void = (event, error) => {
    console.error(`[glasses] listener for "${String(event)}" threw:`, error);
  };

  on<K extends keyof Events>(event: K, handler: Handler<Events[K]>): Unsubscribe {
    let set = this.#handlers.get(event);
    if (!set) {
      set = new Set();
      this.#handlers.set(event, set);
    }
    set.add(handler as Handler<never>);
    return () => {
      set!.delete(handler as Handler<never>);
    };
  }

  /** Subscribe until the first emission, then unsubscribe. */
  once<K extends keyof Events>(event: K, handler: Handler<Events[K]>): Unsubscribe {
    const off = this.on(event, (payload) => {
      off();
      handler(payload);
    });
    return off;
  }

  /** Resolve on the next emission of `event`. */
  next<K extends keyof Events>(event: K): Promise<Events[K]> {
    return new Promise((resolve) => this.once(event, resolve));
  }

  emit<K extends keyof Events>(event: K, payload: Events[K]): void {
    const set = this.#handlers.get(event);
    if (!set || set.size === 0) return;
    // Copy first: a handler may unsubscribe itself or others mid-emit.
    for (const handler of [...set]) {
      try {
        (handler as Handler<Events[K]>)(payload);
      } catch (error) {
        this.onHandlerError(event, error);
      }
    }
  }

  listenerCount<K extends keyof Events>(event: K): number {
    return this.#handlers.get(event)?.size ?? 0;
  }

  removeAll(): void {
    this.#handlers.clear();
  }
}
