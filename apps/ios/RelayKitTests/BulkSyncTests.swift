import Foundation
import XCTest
@testable import RelayKit

/// The two-phase sync is a state machine because the interesting states are the
/// ones where nothing transfers. These tests are mostly about those.
final class BulkSyncTests: XCTestCase {

    private func makeSync(
        glasses: StubGlasses,
        network: MockSyncNetwork,
        queue: StoreAndForwardQueue,
        requireCharging: Bool = true,
        canPresentSystemDialogs: Bool = true,
        rejoinTimeoutMs: Int = 0,
        upload: @escaping QueueSend = { _ in },
        observer: SyncObserver = SyncObserver()
    ) -> BulkSync {
        BulkSync(
            glasses: glasses,
            queue: queue,
            network: network,
            upload: upload,
            clock: TestClock(),
            requireCharging: requireCharging,
            rejoinTimeoutMs: rejoinTimeoutMs,
            canPresentSystemDialogs: { canPresentSystemDialogs },
            observer: observer
        )
    }

    private func day() -> [RemoteFile] {
        [
            RemoteFile(name: "REC_0001.opus", sizeBytes: 90_000, uploaded: false, durationS: 1_800),
            RemoteFile(name: "REC_0002.opus", sizeBytes: 90_000, uploaded: false, durationS: 1_800),
        ]
    }

    // MARK: the happy path, and its shape

    func testAFullRunPullsOverTheAccessPointThenUploadsOffIt() async {
        let glasses = StubGlasses()
        glasses.files = day()
        let network = MockSyncNetwork(.lan)
        let queue = StoreAndForwardQueue()

        let phases = Locked([SyncPhase]())
        let uploaded = Locked([String]())
        let sync = makeSync(
            glasses: glasses,
            network: network,
            queue: queue,
            upload: { record in uploaded.mutate { $0.append(record.id) } },
            observer: SyncObserver(onPhaseChanged: { phase in phases.mutate { $0.append(phase) } })
        )

        let result = await sync.run()

        XCTAssertEqual(result.phase, .done)
        XCTAssertEqual(result.filesPulled, 2)
        XCTAssertEqual(result.uploaded, 2)
        XCTAssertEqual(uploaded.get(), ["REC_0001.opus", "REC_0002.opus"])

        // Phase order is the product: joining the AP costs the uplink, so the
        // upload cannot overlap the pull.
        let order = phases.get()
        let joinIndex = order.firstIndex(of: .joiningAccessPoint)
        let uploadIndex = order.firstIndex(of: .uploading)
        XCTAssertNotNil(joinIndex)
        XCTAssertNotNil(uploadIndex)
        XCTAssertLessThan(joinIndex!, uploadIndex!)
        XCTAssertTrue(order.contains(.leavingAccessPoint))
        let uplinkBack = await sync.uplinkAvailable
        XCTAssertTrue(uplinkBack, "the phone must not be left on the glasses' network")
    }

    func testTheAccessPointIsReleasedEvenWhenTheTransferFails() async {
        let glasses = StubGlasses()
        glasses.files = day()
        glasses.fetchFails = true
        let network = MockSyncNetwork(.lan)
        let sync = makeSync(glasses: glasses, network: network, queue: StoreAndForwardQueue())

        let result = await sync.run()

        XCTAssertEqual(result.phase, .failed)
        XCTAssertFalse(
            network.holdingAccessPoint,
            "a phone left on the glasses' network with no uplink is worse than a failed sync"
        )
        XCTAssertTrue(glasses.callLog.contains("closeWifiAccessPoint"))
        XCTAssertEqual(
            result.errorDescription?.hasPrefix("pullingFiles:"), true,
            "the phase it failed in is what tells the user where to look"
        )
    }

    // MARK: the states where nothing transfers

    func testItWaitsForTheLanRatherThanSpendingADataPlan() async {
        let glasses = StubGlasses()
        glasses.files = day()
        let network = MockSyncNetwork(.relay)
        let deferrals = Locked([SyncDeferral]())
        let sync = makeSync(
            glasses: glasses,
            network: network,
            queue: StoreAndForwardQueue(),
            observer: SyncObserver(onDeferred: { reason, _ in deferrals.mutate { $0.append(reason) } })
        )

        let result = await sync.run()

        XCTAssertEqual(result.deferred, .boxOnlyViaRelay)
        XCTAssertEqual(deferrals.get(), [.boxOnlyViaRelay])
        XCTAssertFalse(
            glasses.callLog.contains("openWifiAccessPoint"),
            "it must decide before touching a radio"
        )
    }

    func testItDefersUntilTheGlassesAreCharging() async {
        let glasses = StubGlasses()
        glasses.files = day()
        glasses.battery = BatteryStatus(percent: 55, charging: false)
        let sync = makeSync(glasses: glasses, network: MockSyncNetwork(.lan), queue: StoreAndForwardQueue())

        let result = await sync.run()

        XCTAssertEqual(result.deferred, .notCharging)
    }

    func testARunWithNothingNewDoesNotTouchTheWifiRadio() async {
        let glasses = StubGlasses()
        glasses.files = [RemoteFile(name: "REC_0001.opus", sizeBytes: 90_000, uploaded: true)]
        let sync = makeSync(glasses: glasses, network: MockSyncNetwork(.lan), queue: StoreAndForwardQueue())

        let result = await sync.run()

        XCTAssertEqual(result.deferred, .nothingToSync)
        XCTAssertFalse(glasses.callLog.contains("openWifiAccessPoint"))
    }

    func testTheAccessPointPhaseDefersWhenNoSystemDialogCanBeShown() async {
        // A `BGProcessingTask` at 3am cannot put up "Relay wants to join
        // QCGlasses-XXXX". Pretending otherwise produces a sync that hangs until
        // iOS kills the task.
        let glasses = StubGlasses()
        glasses.files = day()
        let sync = makeSync(
            glasses: glasses,
            network: MockSyncNetwork(.lan),
            queue: StoreAndForwardQueue(),
            canPresentSystemDialogs: false
        )

        let result = await sync.run()

        XCTAssertEqual(result.deferred, .needsForeground)
        XCTAssertFalse(glasses.callLog.contains("openWifiAccessPoint"))
    }

    func testAQueuedBacklogStillUploadsWhenThereIsNothingNewToPull() async {
        let glasses = StubGlasses()
        glasses.files = []
        let queue = StoreAndForwardQueue()
        await queue.enqueue(QueueRecord(id: "yesterday", kind: "audio", body: Data(count: 512)))

        let uploaded = Locked([String]())
        let sync = makeSync(
            glasses: glasses,
            network: MockSyncNetwork(.lan),
            queue: queue,
            upload: { record in uploaded.mutate { $0.append(record.id) } }
        )

        let result = await sync.run()

        XCTAssertEqual(result.phase, .done)
        XCTAssertEqual(uploaded.get(), ["yesterday"])
        XCTAssertFalse(
            glasses.callLog.contains("openWifiAccessPoint"),
            "an empty pull must not cost a network change"
        )
    }

    // MARK: the rules that are not negotiable

    func testItNeverDeletesAnythingFromTheGlasses() async {
        let glasses = StubGlasses()
        glasses.files = day()
        let sync = makeSync(glasses: glasses, network: MockSyncNetwork(.lan), queue: StoreAndForwardQueue())

        _ = await sync.run()

        XCTAssertTrue(
            glasses.deletedNames.isEmpty,
            "the firmware tracks un-uploaded files itself (0x0911); sync is not allowed an opinion"
        )
    }

    func testAlreadyDeliveredFilesAreNotPulledAgain() async {
        let glasses = StubGlasses()
        glasses.files = day()
        let queue = StoreAndForwardQueue()
        await queue.enqueue(QueueRecord(id: "REC_0001.opus", kind: "audio", body: Data(count: 8)))
        _ = await queue.flush { _ in }

        let sync = makeSync(glasses: glasses, network: MockSyncNetwork(.lan), queue: queue)
        _ = await sync.run()

        XCTAssertFalse(glasses.callLog.contains("fetchFile:REC_0001.opus"))
        XCTAssertTrue(glasses.callLog.contains("fetchFile:REC_0002.opus"))
    }

    func testPullingStopsWhenThePhoneRunsOutOfRoom() async {
        let glasses = StubGlasses()
        glasses.files = day()
        // Room for one file and not two.
        let queue = StoreAndForwardQueue(capacityBytes: 100_000)
        let sync = makeSync(glasses: glasses, network: MockSyncNetwork(.lan), queue: queue)

        let result = await sync.run()

        XCTAssertEqual(result.filesPulled, 1, "the second file stays on the glasses, where it is safe")
        XCTAssertTrue(glasses.deletedNames.isEmpty)
    }

    func testTheMockNetworkRefusesToReachTheBoxWhileHoldingTheAccessPoint() async {
        // Structural, not a test of our code: `docs/ARCHITECTURE.md` §2.1 says
        // the phone has one WiFi radio, and a double that quietly answered would
        // let the state machine grow a dependency no phone can satisfy.
        let network = MockSyncNetwork(.lan)
        try? await network.joinAccessPoint(
            WifiAccessPoint(ssid: "QCGlasses", password: "x", host: "192.168.31.1")
        )

        do {
            _ = try await network.reachBox()
            XCTFail("reachBox answered while the access point was held")
        } catch {
            XCTAssertTrue(error is SyncNetworkError)
        }
    }
}
