import XCTest
@testable import RelayKit

/// These assert that the mock is *inconvenient in the right ways*. Each one
/// corresponds to a constraint the real hardware imposes, so if a change here
/// makes a test easier to pass, that is the signal — not the fix.
final class MockTransportTests: XCTestCase {

    private func makeTransport(
        _ configure: (inout MockTransport.Options) -> Void = { _ in }
    ) -> (MockTransport, TestClock) {
        let clock = TestClock()
        var options = MockTransport.Options()
        configure(&options)
        return (MockTransport(clock: clock, options: options), clock)
    }

    // MARK: connection

    func testConnectTakesTheConfiguredTime() async throws {
        let (glasses, clock) = makeTransport { $0.connectDelayMs = 800 }

        let connected = Locked(false)
        Task { try? await glasses.connect(); connected.set(true) }
        await clock.waitForSleepers(1)

        await clock.advance(799)
        XCTAssertFalse(connected.get(), "connect resolved before the link was up")

        await clock.advance(1)
        XCTAssertTrue(connected.get())
        XCTAssertEqual(glasses.state, .connected)
    }

    func testConnectFailureIsSurfacedNotSwallowed() async throws {
        let (glasses, clock) = makeTransport { $0.faults = .init(connectFails: true) }

        let caught = Locked<GlassesErrorCode?>(nil)
        Task {
            do { try await glasses.connect() }
            catch let error as GlassesError { caught.set(error.code) }
            catch {}
        }
        await clock.waitForSleepers(1)
        await clock.advance(1_000)

        XCTAssertEqual(caught.get(), .connectFailed)
        XCTAssertEqual(glasses.state, .disconnected)
    }

    func testCommandsBeforeConnectingFail() async {
        let (glasses, _) = makeTransport()
        do {
            _ = try await glasses.getBattery()
            XCTFail("a command succeeded on a disconnected transport")
        } catch let error as GlassesError {
            XCTAssertEqual(error.code, .notConnected)
        } catch {
            XCTFail("wrong error type: \(error)")
        }
    }

    func testTimeoutShorterThanConnectDelayFails() async {
        let (glasses, clock) = makeTransport { $0.connectDelayMs = 800 }

        let caught = Locked<GlassesErrorCode?>(nil)
        Task {
            do { try await glasses.connect(options: ConnectOptions(timeoutMs: 100)) }
            catch let error as GlassesError { caught.set(error.code) }
            catch {}
        }
        await clock.waitForSleepers(1)
        await clock.advance(1_000)

        XCTAssertEqual(caught.get(), .timeout)
    }

    // MARK: camera — resolution is a latency dial

    func testSmallerPhotosAreGenuinelyFaster() async throws {
        let (glasses, clock) = try await connected()

        let smallFinishedAt = Locked(-1)
        Task {
            _ = try? await glasses.takePhoto(options: PhotoOptions(maxWidth: 320, maxHeight: 240))
            smallFinishedAt.set(clock.now())
        }
        await clock.waitForSleepers(1)
        await clock.advance(120_000)

        let small = smallFinishedAt.get()
        XCTAssertGreaterThan(small, 0, "the small photo never completed")

        // 320x240 at 0.08 B/px is ~6 KB, which over 3 KB/s is about two seconds.
        // A full-size 2048x1536 is ~252 KB — roughly 84 seconds. If these ever
        // come out equal, the mock has stopped modelling BLE throughput and the
        // UI built on it will have no reason to offer a resolution choice.
        XCTAssertLessThan(small, 10_000, "a 320x240 photo should take seconds, not a minute")
    }

    func testTakePhotoReportsProgressThroughout() async throws {
        let (glasses, clock) = try await connected()

        let progress = Locked([PhotoProgress]())
        let token = glasses.on { event in
            if case let .photoProgress(p) = event { progress.mutate { $0.append(p) } }
        }
        defer { token.unsubscribe() }

        Task { _ = try? await glasses.takePhoto(options: PhotoOptions(maxWidth: 640, maxHeight: 480)) }
        await clock.waitForSleepers(1)
        await clock.advance(120_000)

        let samples = progress.get()
        XCTAssertGreaterThan(samples.count, 1, "a multi-second transfer reported no intermediate progress")
        XCTAssertEqual(samples.last?.receivedBytes, samples.last?.totalBytes)
    }

    func testPhotoFailureMidTransferPropagates() async throws {
        let (glasses, clock) = try await connected { $0.faults = .init(photoFails: true) }

        let caught = Locked<GlassesErrorCode?>(nil)
        Task {
            do { _ = try await glasses.takePhoto(options: PhotoOptions(maxWidth: 640, maxHeight: 480)) }
            catch let error as GlassesError { caught.set(error.code) }
            catch {}
        }
        await clock.waitForSleepers(1)
        await clock.advance(120_000)

        XCTAssertEqual(caught.get(), .transferFailed)
    }

    // MARK: transfer — why sync rides WiFi

    func testFetchIsDramaticallyFasterOverTheAccessPoint() async throws {
        let (glasses, clock) = try await connected()

        try await glasses.startLocalRecording()
        await clock.advance(60_000)          // a minute of audio
        try await glasses.stopLocalRecording()

        let files = try await glasses.listFiles()
        let name = try XCTUnwrap(files.first?.name)

        // Over BLE first.
        let bleFinished = Locked(-1)
        let bleStart = clock.now()
        Task { _ = try? await glasses.fetchFile(name: name); bleFinished.set(clock.now()) }
        await clock.waitForSleepers(1)
        await clock.advance(10 * 60 * 1_000)
        let bleElapsed = bleFinished.get() - bleStart
        XCTAssertGreaterThan(bleElapsed, 0, "the BLE fetch never completed")

        // Then over the access point.
        _ = try await glasses.openWifiAccessPoint()
        let wifiFinished = Locked(-1)
        let wifiStart = clock.now()
        Task { _ = try? await glasses.fetchFile(name: name); wifiFinished.set(clock.now()) }
        await clock.waitForSleepers(1)
        await clock.advance(10 * 60 * 1_000)
        let wifiElapsed = wifiFinished.get() - wifiStart

        XCTAssertGreaterThan(wifiElapsed, -1)
        XCTAssertLessThan(
            wifiElapsed * 10, bleElapsed,
            "the AP must be more than 10x faster than BLE — that gap is the entire "
                + "justification for the nightly WiFi sync in ARCHITECTURE.md §5.3"
        )
    }

    // MARK: storage

    func testRecordingConsumesTheStorageBudget() async throws {
        let (glasses, clock) = try await connected()

        let before = try await glasses.getDiskInfo().freeBytes
        try await glasses.startLocalRecording()
        await clock.advance(60_000)
        try await glasses.stopLocalRecording()
        let after = try await glasses.getDiskInfo().freeBytes

        XCTAssertLessThan(after, before, "a minute of recording consumed no storage")
        // Opus at ~24 kbps: a minute is about 180 KB.
        XCTAssertEqual(Double(before - after), 180_000, accuracy: 20_000)
    }

    func testDeletingAFileReturnsItsSpace() async throws {
        let (glasses, clock) = try await connected()

        try await glasses.startLocalRecording()
        await clock.advance(60_000)
        try await glasses.stopLocalRecording()

        let occupied = try await glasses.getDiskInfo().freeBytes
        let files = try await glasses.listFiles()
        let name = try XCTUnwrap(files.first?.name)
        try await glasses.deleteFile(name: name)

        // Hoisted into a local: XCTAssert* take autoclosures, which cannot
        // contain an `await`.
        let reclaimed = try await glasses.getDiskInfo().freeBytes
        XCTAssertGreaterThan(reclaimed, occupied)
    }

    // MARK: link loss

    func testDroppedLinkFailsInFlightCommands() async throws {
        let (glasses, clock) = try await connected()

        let caught = Locked<GlassesErrorCode?>(nil)
        Task {
            do { _ = try await glasses.takePhoto() }
            catch let error as GlassesError { caught.set(error.code) }
            catch {}
        }
        await clock.waitForSleepers(1)

        await clock.advance(1_000)
        glasses.simulateDisconnect()
        await clock.advance(120_000)

        XCTAssertEqual(caught.get(), .notConnected, "an in-flight transfer survived losing the link")
    }

    // MARK: wear

    func testWearTouchAlsoEmitsWear() async throws {
        let (glasses, _) = try await connected()

        let worn = Locked<Bool?>(nil)
        let token = glasses.on { event in
            if case let .wear(value) = event { worn.set(value) }
        }
        defer { token.unsubscribe() }

        glasses.emitTouch(.wear)
        XCTAssertEqual(worn.get(), true)

        glasses.emitTouch(.remove)
        XCTAssertEqual(worn.get(), false)
    }

    func testUnsubscribingStopsDelivery() async throws {
        let (glasses, _) = try await connected()

        let count = Locked(0)
        let token = glasses.on { _ in count.mutate { $0 += 1 } }

        glasses.emitTouch(.singleTap)
        let afterFirst = count.get()
        XCTAssertGreaterThan(afterFirst, 0)

        token.unsubscribe()
        glasses.emitTouch(.singleTap)
        XCTAssertEqual(count.get(), afterFirst, "handler fired after unsubscribing")
    }

    // MARK: battery

    func testBatteryDrainsOverTimeAndChargesWhenPluggedIn() async throws {
        let (glasses, clock) = try await connected { $0.batteryPercent = 90 }

        await clock.advance(2 * 3_600_000)   // two hours
        let drained = try await glasses.getBattery()
        XCTAssertLessThan(drained.percent, 90, "battery did not drain over two hours")

        glasses.setCharging(true)
        await clock.advance(3_600_000)       // one hour on the charger
        let charged = try await glasses.getBattery()
        XCTAssertGreaterThan(charged.percent, drained.percent)
        XCTAssertTrue(charged.charging)
    }

    // MARK: helpers

    private func connected(
        _ configure: (inout MockTransport.Options) -> Void = { _ in }
    ) async throws -> (MockTransport, TestClock) {
        let clock = TestClock()
        var options = MockTransport.Options()
        options.connectDelayMs = 0
        configure(&options)
        let glasses = MockTransport(clock: clock, options: options)
        try await glasses.connect()
        return (glasses, clock)
    }
}
