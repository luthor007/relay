import Foundation
import XCTest
@testable import RelayKit

/// `docs/APPS-SCOPE.md` §3.2. The asymmetry these tests keep honest: video is
/// about four hundred times more expensive per second than audio, so "a few
/// minutes of video" and "a day of conversation" are the same amount of storage.
final class StoragePolicyTests: XCTestCase {

    private let total = 4 * 1024 * 1024 * 1024

    private func disk(freeGB: Double) -> DiskInfo {
        DiskInfo(totalBytes: total, freeBytes: Int(freeGB * 1_073_741_824))
    }

    func testADayOfOpusIsAboutOneHundredAndSeventyMegabytes() {
        // The number the whole sync design rests on (§3.1). If this changes, the
        // nightly ritual's duration changes with it.
        let day = captureDaySeconds * CaptureFormat.opus.bytesPerSecond
        XCTAssertEqual(Double(day) / 1_000_000, 172.8, accuracy: 0.1)
    }

    func testPcmWouldBeTenTimesTheProblem() {
        let opus = captureDaySeconds * CaptureFormat.opus.bytesPerSecond
        let pcm = captureDaySeconds * CaptureFormat.pcm16.bytesPerSecond
        XCTAssertGreaterThan(pcm, opus * 10)
    }

    func testPlentyOfRoomReadsAsOk() {
        let assessment = StoragePolicy.assess(disk: disk(freeGB: 3))
        XCTAssertEqual(assessment.level, .ok)
        XCTAssertTrue(assessment.videoAllowed)
    }

    func testBelowTheAudioReserveVideoIsRefused() {
        // Reserve is one 16 h day of Opus ≈ 173 MB.
        let assessment = StoragePolicy.assess(disk: DiskInfo(totalBytes: total, freeBytes: 100_000_000))
        XCTAssertEqual(assessment.level, .warn)
        XCTAssertFalse(assessment.videoAllowed)
        XCTAssertTrue(assessment.message.contains("Video is off"))
    }

    func testNearlyFullIsCritical() {
        let assessment = StoragePolicy.assess(disk: DiskInfo(totalBytes: total, freeBytes: 20_000_000))
        XCTAssertEqual(assessment.level, .critical)
        XCTAssertFalse(assessment.videoAllowed)
    }

    func testFortyMinutesOfVideoCostsADayOfAudio() {
        // The sentence from §3.2, as arithmetic.
        let fortyMinutes = 40 * 60 * videoBytesPerSecond
        let aDay = captureDaySeconds * CaptureFormat.opus.bytesPerSecond
        XCTAssertGreaterThan(fortyMinutes, aDay * 18)
    }

    func testVideoIsRefusedBeforeItBreachesTheReserveNotAfter() {
        let nearlyFull = DiskInfo(totalBytes: total, freeBytes: 300_000_000)
        XCTAssertFalse(
            StoragePolicy.videoWouldBreachReserve(seconds: 30, disk: nearlyFull),
            "half a minute fits inside the spare 127 MB"
        )
        XCTAssertTrue(
            StoragePolicy.videoWouldBreachReserve(seconds: 5 * 60, disk: nearlyFull),
            "five minutes does not, and the UI has to know before the shutter"
        )
    }

    func testOnlyUploadedFilesAreEverDeletable() {
        let files = [
            RemoteFile(name: "REC_0001.opus", sizeBytes: 100, uploaded: true),
            RemoteFile(name: "REC_0002.opus", sizeBytes: 100, uploaded: false),
            RemoteFile(name: "IMG_0001.jpg", sizeBytes: 100, uploaded: true),
        ]
        XCTAssertEqual(
            StoragePolicy.safeToDelete(files).map(\.name),
            ["REC_0001.opus", "IMG_0001.jpg"]
        )
    }

    func testReclaimNeverReachesPastTheUploadedSet() {
        let files = [
            RemoteFile(name: "old.opus", sizeBytes: 100, uploaded: true),
            RemoteFile(name: "today.opus", sizeBytes: 10_000, uploaded: false),
        ]
        let chosen = StoragePolicy.reclaim(targetBytes: 5_000, from: files)

        XCTAssertEqual(chosen.map(\.name), ["old.opus"])
        XCTAssertLessThan(
            chosen.reduce(0) { $0 + $1.sizeBytes }, 5_000,
            "freeing less than asked is correct; the only copy of today is not ours to spend"
        )
    }

    func testReclaimTakesOldestFirstAndStopsWhenItHasEnough() {
        let files = (1 ... 5).map {
            RemoteFile(name: "REC_000\($0).opus", sizeBytes: 100, uploaded: true)
        }
        let chosen = StoragePolicy.reclaim(targetBytes: 250, from: files)
        XCTAssertEqual(chosen.map(\.name), ["REC_0001.opus", "REC_0002.opus", "REC_0003.opus"])
    }

    func testAssessmentSeparatesReclaimableFromUnsynced() {
        let files = [
            RemoteFile(name: "a.opus", sizeBytes: 1_000, uploaded: true),
            RemoteFile(name: "b.opus", sizeBytes: 2_000, uploaded: false),
        ]
        let assessment = StoragePolicy.assess(disk: disk(freeGB: 3), files: files)
        XCTAssertEqual(assessment.reclaimableBytes, 1_000)
        XCTAssertEqual(assessment.unsyncedBytes, 2_000)
    }
}

final class StorageMonitorTests: XCTestCase {

    func testItPollsOnTheClockRatherThanOnceAtLaunch() async {
        let glasses = StubGlasses()
        let clock = TestClock()
        let samples = Locked(0)
        let monitor = StorageMonitor(
            glasses: glasses,
            clock: clock,
            intervalMs: 60_000,
            onAssessment: { _ in samples.mutate { $0 += 1 } }
        )

        await monitor.start()
        await clock.waitForSleepers(1)
        XCTAssertEqual(samples.get(), 1)

        await clock.advance(60_000)
        await clock.waitForSleepers(1)
        XCTAssertGreaterThanOrEqual(samples.get(), 2)

        await monitor.stop()
        // Drain, so the loop's outstanding `sleep` continuation is resumed
        // rather than deallocated — an abandoned CheckedContinuation logs a
        // runtime warning that looks like a real bug in the next test's output.
        await clock.runAll()
    }

    func testALevelChangeIsFlaggedSoTheUiNagsOnceNotEveryFiveMinutes() async {
        let glasses = StubGlasses()
        let monitor = StorageMonitor(glasses: glasses, clock: TestClock())

        _ = await monitor.sample()
        let firstChanged = await monitor.levelChanged
        XCTAssertTrue(firstChanged, "the first sample is always a change")

        _ = await monitor.sample()
        let secondChanged = await monitor.levelChanged
        XCTAssertFalse(secondChanged, "a warning repeated every poll is a warning nobody reads")

        glasses.disk = DiskInfo(totalBytes: 4 * 1024 * 1024 * 1024, freeBytes: 20_000_000)
        _ = await monitor.sample()
        let thirdChanged = await monitor.levelChanged
        XCTAssertTrue(thirdChanged)
        let latest = await monitor.latest
        XCTAssertEqual(latest?.level, .critical)
    }
}
