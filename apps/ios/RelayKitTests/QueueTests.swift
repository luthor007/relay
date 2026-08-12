import Foundation
import XCTest
@testable import RelayKit

/// The four semantics every implementation of this queue shares are asserted
/// first, in the same order and with the same names as
/// `glasses/bridge/test/queue.test.ts` and
/// `apps/android/.../StoreAndForwardQueueTest.kt`. If one of those four fails
/// here, the platforms have drifted — and a queue that disagrees with itself is
/// a bug that only shows up on a bad network.
final class QueueTests: XCTestCase {

    private func record(_ id: String, bytes: Int) -> QueueRecord {
        QueueRecord(
            id: id,
            kind: "audio",
            body: Data(count: bytes),
            meta: RecordMeta(sourceName: "\(id).opus", sizeBytes: bytes, durationS: 60)
        )
    }

    // MARK: the four shared semantics

    func testQueueingTheSameIdTwiceDoesNotDuplicateIt() async {
        let queue = StoreAndForwardQueue()

        let first = await queue.enqueue(record("a", bytes: 128))
        let second = await queue.enqueue(record("a", bytes: 128))

        XCTAssertTrue(first.accepted)
        XCTAssertTrue(second.accepted)
        XCTAssertTrue(second.duplicate)
        let size = await queue.size
        XCTAssertEqual(size, 1)
    }

    func testAFullQueueRefusesNewRecordsRatherThanEvictingOldOnes() async {
        let queue = StoreAndForwardQueue(capacityBytes: 1_000)

        let kept = await queue.enqueue(record("keep", bytes: 800))
        let refused = await queue.enqueue(record("drop", bytes: 800))

        XCTAssertTrue(kept.accepted)
        XCTAssertFalse(refused.accepted)
        XCTAssertEqual(refused.reason, .full)
        let ids = await queue.ids
        XCTAssertEqual(ids, ["keep"], "the older record was evicted")
    }

    func testOrderIsPreserved() async {
        let queue = StoreAndForwardQueue()
        for name in ["first", "second", "third"] {
            await queue.enqueue(record(name, bytes: 10))
        }
        let ids = await queue.ids
        XCTAssertEqual(ids, ["first", "second", "third"])
    }

    func testUsedBytesTracksWhatIsHeld() async {
        let queue = StoreAndForwardQueue()
        await queue.enqueue(record("a", bytes: 100))
        await queue.enqueue(record("b", bytes: 250))
        let used = await queue.usedBytes
        XCTAssertEqual(used, 350)
    }

    func testFlushStopsAtTheFirstFailureAndKeepsOrder() async {
        let queue = StoreAndForwardQueue()
        for name in ["a", "b", "c"] {
            await queue.enqueue(record(name, bytes: 10))
        }

        let sent = Locked([String]())
        let result = await queue.flush { stored in
            if stored.id == "b" { throw GlassesError(.transferFailed, "no") }
            sent.mutate { $0.append(stored.id) }
        }

        XCTAssertEqual(result.sent, 1)
        XCTAssertNotNil(result.error)
        XCTAssertEqual(sent.get(), ["a"])
        let ids = await queue.ids
        XCTAssertEqual(ids, ["b", "c"], "a stuck record must not be skipped past")
    }

    // MARK: refusals are never silent

    func testARecordLargerThanTheWholeQueueIsRefusedAsTooLarge() async {
        let refusals = Locked([QueueRefusal]())
        let queue = StoreAndForwardQueue(
            capacityBytes: 1_000,
            observer: QueueObserver(onRefused: { _, reason, _ in
                refusals.mutate { $0.append(reason) }
            })
        )

        let result = await queue.enqueue(record("huge", bytes: 2_000))

        XCTAssertFalse(result.accepted)
        XCTAssertEqual(result.reason, .tooLarge, "a record that can never fit must not be 'full'")
        XCTAssertEqual(refusals.get(), [.tooLarge], "a refusal must reach the UI as well as the caller")
    }

    func testAFailingStoreRefusesRatherThanAcceptingBytesItDidNotWrite() async {
        let store = MemoryQueueStore(faults: .init(appendFails: true))
        let queue = StoreAndForwardQueue(store: store)

        let result = await queue.enqueue(record("a", bytes: 10))

        XCTAssertFalse(result.accepted)
        XCTAssertEqual(result.reason, .storeFailed)
        let size = await queue.size
        XCTAssertEqual(size, 0, "accepting here is how the source gets deleted and never arrives")
    }

    func testItemLimitIsEnforcedSeparatelyFromByteCapacity() async {
        let queue = StoreAndForwardQueue(capacityBytes: 1_000_000, capacityItems: 2)
        await queue.enqueue(record("a", bytes: 1))
        await queue.enqueue(record("b", bytes: 1))
        let third = await queue.enqueue(record("c", bytes: 1))

        XCTAssertFalse(third.accepted)
        XCTAssertEqual(third.reason, .itemLimit)
    }

    // MARK: durability — what the TypeScript has and the others owed

    func testPendingRecordsSurviveARestart() async throws {
        let store = MemoryQueueStore()
        let first = StoreAndForwardQueue(store: store)
        await first.enqueue(record("monday", bytes: 64))
        await first.enqueue(record("tuesday", bytes: 64))

        // A new queue over the same store is what a relaunch looks like.
        let second = try await StoreAndForwardQueue.open(store: store)

        let ids = await second.ids
        XCTAssertEqual(ids, ["monday", "tuesday"], "order must survive too, not just the records")
    }

    func testADeliveredRecordIsNotReUploadedAfterARestart() async throws {
        let store = MemoryQueueStore()
        let first = StoreAndForwardQueue(store: store)
        await first.enqueue(record("monday", bytes: 64))
        _ = await first.flush { _ in }

        let second = try await StoreAndForwardQueue.open(store: store)
        let replayed = await second.enqueue(record("monday", bytes: 64))

        XCTAssertTrue(replayed.accepted)
        XCTAssertTrue(replayed.alreadyDelivered, "a replayed sync must not re-upload the day")
        let size = await second.size
        XCTAssertEqual(size, 0)
    }

    func testFileStoreRoundTripsBodiesAndSurvivesANewProcess() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("relay-queue-test-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: directory) }

        let store = try FileQueueStore(directory: directory)
        let queue = StoreAndForwardQueue(store: store)
        var body = Data(count: 4_096)
        body[0] = 0x4F
        body[4_095] = 0x53
        await queue.enqueue(QueueRecord(id: "REC_0001.opus", kind: "audio", body: body))

        // A brand new store object over the same directory: no shared memory,
        // only what actually reached the disk.
        let reopened = try FileQueueStore(directory: directory)
        let restored = try await StoreAndForwardQueue.open(store: reopened)

        let ids = await restored.ids
        XCTAssertEqual(ids, ["REC_0001.opus"])

        let recovered = Locked(Data())
        _ = await restored.flush { stored in recovered.set(stored.body) }
        XCTAssertEqual(recovered.get().count, 4_096)
        XCTAssertEqual(recovered.get().first, 0x4F)
        XCTAssertEqual(recovered.get().last, 0x53)
    }

    func testAFilenameWithASlashInItCannotEscapeTheQueueDirectory() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("relay-queue-test-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: directory) }

        let store = try FileQueueStore(directory: directory)
        let queue = StoreAndForwardQueue(store: store)
        // The id is a device filename, and this app does not get to assume what
        // the firmware puts in one.
        let result = await queue.enqueue(
            QueueRecord(id: "../../etc/passwd", kind: "audio", body: Data(count: 8))
        )

        XCTAssertTrue(result.accepted)
        let escaped = directory.deletingLastPathComponent()
            .appendingPathComponent("etc/passwd")
        XCTAssertFalse(
            FileManager.default.fileExists(atPath: escaped.path),
            "a record id must never become a path"
        )
    }

    func testDeliveredMemoryIsBoundedAndDropsOldestFirst() async {
        let queue = StoreAndForwardQueue(deliveredMemory: 2)
        for name in ["a", "b", "c"] {
            await queue.enqueue(record(name, bytes: 4))
        }
        _ = await queue.flush { _ in }

        let delivered = await queue.deliveredIds
        XCTAssertEqual(delivered, ["b", "c"], "the memory is a window, not a leak")
    }

    func testFlushEmitsSentForEveryRecordItDelivered() async {
        let sent = Locked([String]())
        let queue = StoreAndForwardQueue(
            observer: QueueObserver(onSent: { stored in sent.mutate { $0.append(stored.id) } })
        )
        await queue.enqueue(record("a", bytes: 4))
        await queue.enqueue(record("b", bytes: 4))
        _ = await queue.flush { _ in }

        XCTAssertEqual(sent.get(), ["a", "b"])
    }
}
