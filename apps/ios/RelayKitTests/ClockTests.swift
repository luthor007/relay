import XCTest
@testable import RelayKit

final class ClockTests: XCTestCase {

    func testNothingFiresUntilTimeMoves() async {
        let clock = TestClock()
        let fired = Locked(false)

        Task { await clock.sleep(ms: 100); fired.set(true) }
        // Wait for the task to actually reach its sleep. Spawning a Task does
        // not run it, so advancing straight away would move time past a timer
        // that had not been registered yet.
        await clock.waitForSleepers(1)

        XCTAssertFalse(fired.get(), "a timer fired without the clock advancing")
        XCTAssertEqual(clock.pending, 1)

        await clock.advance(100)
        XCTAssertTrue(fired.get())
    }

    func testAdvanceStopsShortOfALaterTimer() async {
        let clock = TestClock()
        let fired = Locked(false)

        Task { await clock.sleep(ms: 500); fired.set(true) }
        await clock.waitForSleepers(1)

        await clock.advance(499)
        XCTAssertFalse(fired.get(), "fired one millisecond early")

        await clock.advance(1)
        XCTAssertTrue(fired.get())
    }

    func testTimersFireInScheduledOrderNotRegistrationOrder() async {
        let clock = TestClock()
        let order = Locked([Int]())

        Task { await clock.sleep(ms: 300); order.mutate { $0.append(3) } }
        Task { await clock.sleep(ms: 100); order.mutate { $0.append(1) } }
        Task { await clock.sleep(ms: 200); order.mutate { $0.append(2) } }
        await clock.waitForSleepers(3)

        await clock.advance(500)
        XCTAssertEqual(order.get(), [1, 2, 3])
    }

    func testNowTracksAdvancedTime() async {
        let clock = TestClock(startMs: 1_000)
        XCTAssertEqual(clock.now(), 1_000)
        await clock.advance(250)
        XCTAssertEqual(clock.now(), 1_250)
    }

    func testRunAllDrainsEverySleep() async {
        let clock = TestClock()
        let count = Locked(0)

        for delay in [10, 20, 30, 5_000] {
            Task { await clock.sleep(ms: delay); count.mutate { $0 += 1 } }
        }
        await clock.waitForSleepers(4)

        await clock.runAll()
        XCTAssertEqual(count.get(), 4)
        XCTAssertEqual(clock.pending, 0, "a timer was left outstanding")
    }
}

/// Minimal thread-safe box. XCTest runs these assertions on the main actor while
/// the tasks under test resume elsewhere, so a bare `var` would be a data race
/// that only shows up under load.
final class Locked<T>: @unchecked Sendable {
    private var value: T
    private let lock = NSLock()

    init(_ value: T) { self.value = value }

    func get() -> T {
        lock.lock(); defer { lock.unlock() }
        return value
    }

    func set(_ newValue: T) {
        lock.lock(); defer { lock.unlock() }
        value = newValue
    }

    func mutate(_ body: (inout T) -> Void) {
        lock.lock(); defer { lock.unlock() }
        body(&value)
    }
}
