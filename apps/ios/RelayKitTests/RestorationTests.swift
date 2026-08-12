import Foundation
import XCTest
@testable import RelayKit

/// A CoreBluetooth relaunch is not reproducible on a laptop and barely
/// reproducible on a phone — you have to get the app killed and then have the
/// glasses reconnect. So the decision is a pure function and this is where it is
/// checked.
final class RestorationTests: XCTestCase {

    private func liveSnapshot() -> CaptureSnapshot {
        var snapshot = CaptureSnapshot()
        snapshot.setConsent(.granted(version: currentConsentVersion, atMs: 1_000))
        snapshot.captureEnabled = true
        snapshot.localRecordingBelievedRunning = true
        snapshot.wornAtLastSample = true
        snapshot.deviceId = "5B2F9C4E-0000-4000-8000-000000000001"
        return snapshot
    }

    func testNoSnapshotIsAColdStart() {
        let plan = Restoration.plan(for: nil, launch: .user)
        XCTAssertTrue(plan.coldStart)
        XCTAssertTrue(plan.presentUI)
        XCTAssertNil(plan.reconnectDeviceId)
    }

    func testABluetoothRelaunchResumesWithoutShowingUI() {
        let plan = Restoration.plan(for: liveSnapshot(), launch: .bluetoothRestoration)

        XCTAssertFalse(plan.presentUI, "there is no window on a background relaunch")
        XCTAssertEqual(plan.reconnectDeviceId, "5B2F9C4E-0000-4000-8000-000000000001")
        XCTAssertTrue(plan.resumeLocalRecording)
        XCTAssertTrue(plan.restoreQueue)
        XCTAssertTrue(plan.confirmRecordingWithDevice)
    }

    func testItReconnectsToTheKnownDeviceRatherThanScanning() {
        let plan = Restoration.plan(for: liveSnapshot(), launch: .bluetoothRestoration)
        XCTAssertNotNil(
            plan.reconnectDeviceId,
            "scanning on a background relaunch is slow and costs battery for no reason"
        )
    }

    func testWithdrawnConsentIsNotResumedByAReconnect() {
        var snapshot = liveSnapshot()
        snapshot.setConsent(.withdrawn(atMs: 2_000))

        let plan = Restoration.plan(for: snapshot, launch: .bluetoothRestoration)

        XCTAssertTrue(plan.coldStart)
        XCTAssertFalse(plan.resumeLocalRecording)
        XCTAssertNil(plan.reconnectDeviceId)
        XCTAssertTrue(
            plan.restoreQueue,
            "audio captured while consent was in force still belongs to the user"
        )
    }

    func testConsentAgainstAnOlderVersionOfTheTextIsAskedAgain() {
        var snapshot = liveSnapshot()
        snapshot.consentVersion = currentConsentVersion - 1

        let plan = Restoration.plan(for: snapshot, launch: .user)

        XCTAssertTrue(plan.coldStart)
        XCTAssertFalse(plan.resumeLocalRecording)
    }

    func testAnUnknownSchemaIsDiscardedRatherThanGuessedAt() {
        var snapshot = liveSnapshot()
        snapshot.schemaVersion = 99

        let plan = Restoration.plan(for: snapshot, launch: .user)

        XCTAssertTrue(plan.coldStart)
        XCTAssertTrue(plan.reason.contains("schema"))
        XCTAssertTrue(plan.restoreQueue, "the queue on disk has its own schema and its own version")
    }

    func testCaptureOffMeansReconnectOnly() {
        var snapshot = liveSnapshot()
        snapshot.captureEnabled = false

        let plan = Restoration.plan(for: snapshot, launch: .user)

        XCTAssertFalse(plan.coldStart)
        XCTAssertNotNil(plan.reconnectDeviceId)
        XCTAssertFalse(plan.resumeLocalRecording)
        XCTAssertFalse(plan.confirmRecordingWithDevice)
    }

    func testGlassesNotWornDoesNotResumeRecording() {
        var snapshot = liveSnapshot()
        snapshot.wornAtLastSample = false

        let plan = Restoration.plan(for: snapshot, launch: .bluetoothRestoration)

        XCTAssertFalse(plan.resumeLocalRecording, "wear gating survives a relaunch")
        XCTAssertTrue(plan.confirmRecordingWithDevice, "but we still ask what the device is doing")
    }

    func testDyingMidSyncLeavesTheAccessPointOnTheNextLaunch() {
        var snapshot = liveSnapshot()
        snapshot.holdingAccessPoint = true

        let plan = Restoration.plan(for: snapshot, launch: .user)

        XCTAssertTrue(
            plan.leaveAccessPoint,
            "otherwise the phone has no uplink and every request times out mysteriously"
        )
    }

    func testConsentRoundTripsThroughTheSnapshot() {
        var snapshot = CaptureSnapshot()
        XCTAssertEqual(snapshot.consent, .notAsked)

        snapshot.setConsent(.granted(version: 1, atMs: 5))
        XCTAssertEqual(snapshot.consent, .granted(version: 1, atMs: 5))
        XCTAssertTrue(snapshot.consent.allowsCapture)

        snapshot.setConsent(.withdrawn(atMs: 6))
        XCTAssertEqual(snapshot.consent, .withdrawn(atMs: 6))
        XCTAssertFalse(snapshot.consent.allowsCapture)
    }
}

final class SnapshotStoreTests: XCTestCase {

    func testFileStoreRoundTripsAndSurvivesANewInstance() {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("relay-snapshot-\(UUID().uuidString).json")
        defer { try? FileManager.default.removeItem(at: url) }

        var snapshot = CaptureSnapshot()
        snapshot.setConsent(.granted(version: 1, atMs: 42))
        snapshot.captureEnabled = true
        snapshot.deviceId = "device-1"
        FileSnapshotStore(url: url).save(snapshot)

        let reloaded = FileSnapshotStore(url: url).load()
        XCTAssertEqual(reloaded, snapshot)
    }

    func testAMissingFileLoadsAsNilRatherThanCrashing() {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("relay-snapshot-missing-\(UUID().uuidString).json")
        XCTAssertNil(FileSnapshotStore(url: url).load())
    }

    func testCorruptContentLoadsAsNil() {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("relay-snapshot-corrupt-\(UUID().uuidString).json")
        defer { try? FileManager.default.removeItem(at: url) }
        try? Data("half a fi".utf8).write(to: url)

        XCTAssertNil(
            FileSnapshotStore(url: url).load(),
            "a truncated write is what a kill during save looks like"
        )
    }
}
