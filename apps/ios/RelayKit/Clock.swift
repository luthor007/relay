import Foundation

/// Injectable clock.
///
/// Everything timing-dependent in this framework — photo transfer rate, battery
/// drain, recording duration, trace playback — goes through a clock, so tests
/// assert real behaviour instantly instead of sleeping and hoping. A test that
/// waits on wall-clock time is a test that goes flaky on CI.
///
/// Named `RelayClock` rather than `Clock` because the standard library already
/// owns that name, and the ambiguity is not worth the elegance.
public protocol RelayClock: AnyObject, Sendable {
    /// Milliseconds since an arbitrary epoch; monotonic.
    func now() -> Int
    func sleep(ms: Int) async
}

public final class SystemClock: RelayClock, @unchecked Sendable {
    public init() {}

    public func now() -> Int {
        // Monotonic: a wall-clock source would run backwards when the phone
        // corrects its time, and every duration computed across that jump would
        // come out negative.
        Int(DispatchTime.now().uptimeNanoseconds / 1_000_000)
    }

    public func sleep(ms: Int) async {
        guard ms > 0 else { return }
        try? await Task.sleep(nanoseconds: UInt64(ms) * 1_000_000)
    }
}

/// Deterministic clock for tests. Nothing fires until ``advance(_:)`` is called.
///
/// ## How this differs from the TypeScript original
///
/// `FakeClock` in `glasses/bridge` drains the microtask queue between timers, so
/// a promise chain settles fully before the next timer fires. Swift has no
/// equivalent queue to drain — cooperative tasks resume on an executor when it
/// gets to them. ``drain()`` yields a bounded number of times instead, which is
/// enough for the continuation chains in this framework but is a weaker
/// guarantee than the JS version, and worth knowing before writing a test that
/// depends on deep chaining.
public final class TestClock: RelayClock, @unchecked Sendable {

    private struct Scheduled {
        let at: Int
        let seq: Int
        let resume: () -> Void
    }

    private let lock = NSLock()
    private var nowMs: Int
    private var seq = 0
    private var scheduled: [Scheduled] = []

    public init(startMs: Int = 0) {
        self.nowMs = startMs
    }

    public func now() -> Int {
        lock.lock()
        defer { lock.unlock() }
        return nowMs
    }

    /// Number of timers still outstanding — useful for leak assertions.
    public var pending: Int {
        lock.lock()
        defer { lock.unlock() }
        return scheduled.count
    }

    public func sleep(ms: Int) async {
        guard ms > 0 else {
            await Task.yield()
            return
        }
        await withCheckedContinuation { (cont: CheckedContinuation<Void, Never>) in
            lock.lock()
            seq += 1
            scheduled.append(Scheduled(at: nowMs + ms, seq: seq, resume: { cont.resume() }))
            lock.unlock()
        }
    }

    /// Move time forward, firing everything due along the way.
    ///
    /// Every line that touches the lock lives in a synchronous helper below:
    /// `NSLock.lock()` is unavailable from an async context (it is a hard error
    /// under Swift 6), because holding a lock across a suspension point can
    /// deadlock the cooperative pool.
    public func advance(_ ms: Int) async {
        let target = now() + ms

        while true {
            if let next = takeNextDue(upTo: target) {
                next.resume()
                await drain()
                continue
            }
            // No timer is due *yet*. A task woken a moment ago may still be on
            // its way to registering the next one — a photo transfer registers
            // one sleep per chunk, so exiting here would strand it mid-transfer.
            await settle()
            if !hasDue(upTo: target) { break }
        }

        // Only now is it safe to jump to the end of the window. Committing
        // earlier makes `clock.now()`, read inside a continuation chained off
        // the final timer, report the end of the advance instead of the moment
        // the work actually finished — which silently turns "this took 2 s" into
        // "this took as long as I advanced".
        commit(target)
        await drain()
    }

    /// Advance until nothing is scheduled, or `maxIterations` rounds elapse.
    public func runAll(maxIterations: Int = 10_000) async {
        var i = 0
        while pending > 0, i < maxIterations {
            i += 1
            guard let step = nextStep() else { break }
            await advance(step)
        }
    }

    /// Distance from now to the soonest scheduled timer, or nil if none.
    private func nextStep() -> Int? {
        lock.lock()
        defer { lock.unlock() }
        guard let soonest = scheduled.map(\.at).min() else { return nil }
        return max(0, soonest - nowMs)
    }

    private func commit(_ target: Int) {
        lock.lock()
        defer { lock.unlock() }
        nowMs = max(nowMs, target)
    }

    /// A longer pause than ``drain()``, used when nothing looks due — enough for
    /// a task that was just resumed to finish its work and schedule again.
    private func settle() async {
        for _ in 0 ..< 3 {
            await Task.yield()
            try? await Task.sleep(nanoseconds: 1_000_000)
        }
    }

    private func hasDue(upTo target: Int) -> Bool {
        lock.lock()
        defer { lock.unlock() }
        return scheduled.contains { $0.at <= target }
    }

    private func takeNextDue(upTo target: Int) -> Scheduled? {
        lock.lock()
        defer { lock.unlock() }

        let due = scheduled
            .filter { $0.at <= target }
            .sorted { ($0.at, $0.seq) < ($1.at, $1.seq) }

        guard let first = due.first else { return nil }
        scheduled.removeAll { $0.seq == first.seq }
        nowMs = max(nowMs, first.at)
        return first
    }

    /// Give other tasks a chance to resume and register their next timer.
    ///
    /// `Task.yield()` alone is not enough. Yielding reschedules *this* task on
    /// the cooperative pool; it makes no promise that a specific other task has
    /// run, so a loop that fires a timer and immediately looks for the next one
    /// races the very task it just woke. A short real sleep is a genuine
    /// suspension that lets the pool actually schedule the woken task.
    ///
    /// This is the honest difference from the TypeScript `FakeClock`, which can
    /// drain the microtask queue to a fixpoint because JS is single-threaded.
    /// The cost is ~0.2 ms of wall clock per fired timer, which for the ~90
    /// timers in the slowest test here is imperceptible.
    private func drain() async {
        await Task.yield()
        try? await Task.sleep(nanoseconds: 200_000)
        await Task.yield()
    }

    /// Suspend until `count` timers are registered, or the timeout elapses.
    ///
    /// Tests that spawn work in a detached `Task` need this before advancing:
    /// spawning does not run the task, so advancing immediately moves time past
    /// a `sleep` that has not been reached yet, and the work never completes.
    public func waitForSleepers(_ count: Int, timeoutMs: Int = 2_000) async {
        let deadline = DispatchTime.now().uptimeNanoseconds + UInt64(timeoutMs) * 1_000_000
        while pending < count {
            if DispatchTime.now().uptimeNanoseconds > deadline { return }
            await Task.yield()
            try? await Task.sleep(nanoseconds: 100_000)
        }
    }
}
