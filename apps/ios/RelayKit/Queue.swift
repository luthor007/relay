import CryptoKit
import Foundation

/// Store-and-forward, with the durability half that `docs/APPS-SCOPE.md` §4.5
/// says the platform owes.
///
/// A port of `glasses/bridge/src/queue.ts`. The four semantics that every
/// implementation shares — idempotent enqueue, refuse-newest, FIFO,
/// stop-at-first-failure — are identical here on purpose, and `QueueTests`
/// ports the same cases the TypeScript and Kotlin suites use. Two
/// implementations of one queue that disagree is a bug that only appears on a
/// bad network, which is the worst place to find one.
///
/// ## The eviction policy, stated
///
/// | Situation | What happens | Why |
/// |---|---|---|
/// | Queue full | the **newest** record is refused, with a reason | dropping the oldest silently loses last Tuesday; a refusal is a thing the UI can honestly report |
/// | Record larger than the whole capacity | refused as `tooLarge` immediately | it will never fit, and pretending otherwise blocks the queue forever |
/// | Same id enqueued twice | accepted, not duplicated | a retry after a failed flush must not double-upload |
/// | Id already delivered | accepted as a no-op | replaying a sync must not re-upload the day |
/// | Flush hits an error | stops at that record, keeps order | skipping past a stuck record quietly reorders someone's day, and the box segments episodes by time |
///
/// Nothing is ever dropped silently.
///
/// ## Why this is an `actor` and the TypeScript one is not
///
/// `enqueue` awaits the store between checking capacity and committing. In
/// JavaScript that window cannot interleave; in Swift two tasks could both pass
/// the capacity check and then both commit. An actor closes it without a lock
/// held across a suspension point, which is the thing Swift 6 makes an error.

/// 2 GB. Half the glasses' storage: enough for a day, bounded on the phone.
public let defaultQueueCapacityBytes = 2 * 1024 * 1024 * 1024

/// Ids remembered after delivery. ~40 files a day, so this is weeks of cover.
public let defaultDeliveredMemory = 1024

// MARK: - records

/// Manifest fields the uploader needs, typed rather than free-form.
///
/// The TypeScript carries `Record<string, unknown>`; Swift has no cheap
/// equivalent that also round-trips through `Codable`, and a
/// `[String: AnyCodable]` shim would be more code than the six fields anyone
/// actually sets.
public struct RecordMeta: Codable, Sendable, Equatable {
    public var sourceName: String?
    public var sizeBytes: Int?
    public var durationS: Int?
    public var startedAtMs: Int?
    public var pulledAtMs: Int?
    public var encoding: String?

    public init(
        sourceName: String? = nil,
        sizeBytes: Int? = nil,
        durationS: Int? = nil,
        startedAtMs: Int? = nil,
        pulledAtMs: Int? = nil,
        encoding: String? = nil
    ) {
        self.sourceName = sourceName
        self.sizeBytes = sizeBytes
        self.durationS = durationS
        self.startedAtMs = startedAtMs
        self.pulledAtMs = pulledAtMs
        self.encoding = encoding
    }
}

public struct QueueRecord: Sendable, Equatable {
    /// Stable and derived from the source, not random — the device filename for
    /// a pulled recording, the envelope id for a control message. Dedupe is only
    /// as good as this being the same across a restart.
    public let id: String
    /// "audio" | "photo" | "envelope". Free-form; the box reads the meta.
    public let kind: String
    public let body: Data
    public var meta: RecordMeta

    public init(id: String, kind: String, body: Data, meta: RecordMeta = RecordMeta()) {
        self.id = id
        self.kind = kind
        self.body = body
        self.meta = meta
    }
}

public struct StoredRecord: Sendable, Equatable {
    public let record: QueueRecord
    public let enqueuedAtMs: Int
    /// Monotonic. This, not insertion into an array, is what preserves order.
    public let sequence: Int

    public var id: String { record.id }
    public var kind: String { record.kind }
    public var body: Data { record.body }
    public var meta: RecordMeta { record.meta }
}

// MARK: - durable backing

/// `append` must not resolve until the bytes are durable. Resolving early turns
/// a crash into silent data loss, which is exactly the case this exists to
/// prevent — and it is what lets `BulkSync` stop holding a pulled recording in
/// memory the moment `enqueue` returns.
public protocol QueueStore: AnyObject, Sendable {
    func load() async throws -> (pending: [StoredRecord], delivered: [String])
    func append(_ record: StoredRecord) async throws
    func remove(id: String) async throws
    func markDelivered(id: String) async throws
}

/// In-memory store for tests and the simulator. Loses everything on restart,
/// which is the point: a test that wants to prove durability must reach for the
/// file store.
public final class MemoryQueueStore: QueueStore, @unchecked Sendable {

    public struct Faults: Sendable, Equatable {
        /// Simulate a full or failing disk. Enqueue must surface it, not swallow it.
        public var appendFails: Bool

        public init(appendFails: Bool = false) {
            self.appendFails = appendFails
        }
    }

    private let lock = NSLock()
    private var pending: [String: StoredRecord] = [:]
    private var delivered: [String] = []
    private var faults: Faults

    public init(faults: Faults = Faults()) {
        self.faults = faults
    }

    public func setFaults(_ faults: Faults) {
        lock.lock(); defer { lock.unlock() }
        self.faults = faults
    }

    public var persistedIds: [String] {
        lock.lock(); defer { lock.unlock() }
        return pending.values.sorted { $0.sequence < $1.sequence }.map(\.id)
    }

    public func load() async throws -> (pending: [StoredRecord], delivered: [String]) {
        lock.lock(); defer { lock.unlock() }
        return (pending.values.sorted { $0.sequence < $1.sequence }, delivered)
    }

    public func append(_ record: StoredRecord) async throws {
        lock.lock(); defer { lock.unlock() }
        if faults.appendFails { throw QueueStoreError.appendFailed("store: append failed") }
        pending[record.id] = record
    }

    public func remove(id: String) async throws {
        lock.lock(); defer { lock.unlock() }
        pending.removeValue(forKey: id)
    }

    public func markDelivered(id: String) async throws {
        lock.lock(); defer { lock.unlock() }
        delivered.append(id)
    }
}

public enum QueueStoreError: Error, Sendable, Equatable {
    case appendFailed(String)
    case corrupt(String)
}

/// Files under `NSFileManager`, one per record plus an index.
///
/// The body is *not* in the index. A day of audio is 173 MB, and base64 inside a
/// JSON document that is rewritten on every append would cost more than the
/// recording did. Bodies are written to their own files named by the SHA-256 of
/// the record id, which is both collision-free enough and always a legal
/// filename — a device filename with a `/` in it would otherwise escape the
/// directory.
///
/// Durability here is `Data.write(to:options:.atomic)`, which writes a temp file
/// and renames. That survives a crash of this process. It does **not** fsync the
/// containing directory, so it does not survive sudden power loss — an honest
/// limitation on a phone, where the OS is the thing that gets killed and the
/// hardware rarely loses power without warning.
public final class FileQueueStore: QueueStore, @unchecked Sendable {

    private struct Index: Codable {
        var version: Int
        var records: [Header]
        var delivered: [String]
    }

    private struct Header: Codable {
        var id: String
        var kind: String
        var meta: RecordMeta
        var enqueuedAtMs: Int
        var sequence: Int
        var byteCount: Int
    }

    private static let schemaVersion = 1

    private let root: URL
    private let bodies: URL
    private let indexURL: URL
    private let lock = NSLock()

    public init(directory: URL) throws {
        self.root = directory
        self.bodies = directory.appendingPathComponent("records", isDirectory: true)
        self.indexURL = directory.appendingPathComponent("index.json")
        try FileManager.default.createDirectory(at: bodies, withIntermediateDirectories: true)
    }

    /// `Application Support/relay-queue`, which iOS backs up and does not purge.
    /// Caches would be wrong: the system evicts them under pressure, and what it
    /// would evict is the only copy of a conversation.
    public static func inApplicationSupport(name: String = "relay-queue") throws -> FileQueueStore {
        let base = try FileManager.default.url(
            for: .applicationSupportDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: true
        )
        return try FileQueueStore(directory: base.appendingPathComponent(name, isDirectory: true))
    }

    public func load() async throws -> (pending: [StoredRecord], delivered: [String]) {
        lock.lock(); defer { lock.unlock() }
        let index = readIndex()
        var restored: [StoredRecord] = []
        for header in index.records.sorted(by: { $0.sequence < $1.sequence }) {
            guard let body = try? Data(contentsOf: bodyURL(for: header.id)) else {
                // The index knows about a body that is not there. Skipping it is
                // right: the alternative is handing the uploader an empty record
                // and calling the day delivered.
                continue
            }
            restored.append(StoredRecord(
                record: QueueRecord(id: header.id, kind: header.kind, body: body, meta: header.meta),
                enqueuedAtMs: header.enqueuedAtMs,
                sequence: header.sequence
            ))
        }
        return (restored, index.delivered)
    }

    public func append(_ record: StoredRecord) async throws {
        lock.lock(); defer { lock.unlock() }
        do {
            try record.body.write(to: bodyURL(for: record.id), options: [.atomic])
        } catch {
            throw QueueStoreError.appendFailed(String(describing: error))
        }
        var index = readIndex()
        index.records.removeAll { $0.id == record.id }
        index.records.append(Header(
            id: record.id,
            kind: record.kind,
            meta: record.meta,
            enqueuedAtMs: record.enqueuedAtMs,
            sequence: record.sequence,
            byteCount: record.body.count
        ))
        try writeIndex(index)
    }

    public func remove(id: String) async throws {
        lock.lock(); defer { lock.unlock() }
        var index = readIndex()
        index.records.removeAll { $0.id == id }
        try writeIndex(index)
        try? FileManager.default.removeItem(at: bodyURL(for: id))
    }

    public func markDelivered(id: String) async throws {
        lock.lock(); defer { lock.unlock() }
        var index = readIndex()
        index.delivered.append(id)
        if index.delivered.count > defaultDeliveredMemory * 2 {
            index.delivered.removeFirst(index.delivered.count - defaultDeliveredMemory)
        }
        try writeIndex(index)
    }

    // MARK: internals

    private func readIndex() -> Index {
        guard
            let data = try? Data(contentsOf: indexURL),
            let decoded = try? JSONDecoder().decode(Index.self, from: data)
        else {
            return Index(version: Self.schemaVersion, records: [], delivered: [])
        }
        // A future build's index is not something this build can interpret.
        // Starting empty loses nothing that is not still on the glasses.
        guard decoded.version == Self.schemaVersion else {
            return Index(version: Self.schemaVersion, records: [], delivered: [])
        }
        return decoded
    }

    private func writeIndex(_ index: Index) throws {
        do {
            try JSONEncoder().encode(index).write(to: indexURL, options: [.atomic])
        } catch {
            throw QueueStoreError.appendFailed(String(describing: error))
        }
    }

    private func bodyURL(for id: String) -> URL {
        let digest = SHA256.hash(data: Data(id.utf8)).map { String(format: "%02x", $0) }.joined()
        return bodies.appendingPathComponent("\(digest).bin")
    }
}

// MARK: - results

public enum QueueRefusal: String, Sendable, Equatable {
    /// Capacity reached. The newest record is the one refused.
    case full
    /// More records than `capacityItems`.
    case itemLimit
    /// Bigger than the entire capacity — it can never fit, so say so now.
    case tooLarge
    /// The disk said no. Never swallowed: the caller must not delete its source.
    case storeFailed
}

public struct EnqueueResult: Sendable, Equatable {
    public let accepted: Bool
    /// Already pending under this id — accepted, not duplicated.
    public var duplicate: Bool = false
    /// Already delivered in a previous run — accepted as a no-op.
    public var alreadyDelivered: Bool = false
    public var reason: QueueRefusal?
    public var message: String?
}

/// Not `Sendable`: it carries an `Error` existential, which is not. Hoisting the
/// error out to make the struct conform would lose the thing the caller needs.
public struct FlushResult {
    public let sent: Int
    public let remaining: Int
    /// Present when the flush stopped early. The queue is intact and ordered.
    public let error: Error?
}

/// Handed in at construction rather than subscribed to.
///
/// The two callers that matter are a UI and a log, and both exist for the whole
/// life of the queue — so a subscription list would be bookkeeping with no
/// subscriber ever removed from it.
public struct QueueObserver: Sendable {
    public var onEnqueued: (@Sendable (StoredRecord) -> Void)?
    public var onRefused: (@Sendable (QueueRecord, QueueRefusal, String) -> Void)?
    public var onSent: (@Sendable (StoredRecord) -> Void)?
    public var onFlushFailed: (@Sendable (StoredRecord, Error) -> Void)?
    public var onRestored: (@Sendable (Int, Int) -> Void)?

    public init(
        onEnqueued: (@Sendable (StoredRecord) -> Void)? = nil,
        onRefused: (@Sendable (QueueRecord, QueueRefusal, String) -> Void)? = nil,
        onSent: (@Sendable (StoredRecord) -> Void)? = nil,
        onFlushFailed: (@Sendable (StoredRecord, Error) -> Void)? = nil,
        onRestored: (@Sendable (Int, Int) -> Void)? = nil
    ) {
        self.onEnqueued = onEnqueued
        self.onRefused = onRefused
        self.onSent = onSent
        self.onFlushFailed = onFlushFailed
        self.onRestored = onRestored
    }
}

public typealias QueueSend = @Sendable (StoredRecord) async throws -> Void

// MARK: - the queue

public actor StoreAndForwardQueue {

    private let store: QueueStore
    private let clock: RelayClock
    private let capacity: Int
    private let capacityItems: Int
    private let deliveredMemory: Int
    private let observer: QueueObserver

    private var pending: [StoredRecord] = []
    private var delivered: [String] = []
    private var deliveredSet: Set<String> = []
    private var sequence = 0

    public init(
        store: QueueStore = MemoryQueueStore(),
        clock: RelayClock = SystemClock(),
        capacityBytes: Int = defaultQueueCapacityBytes,
        capacityItems: Int = Int.max,
        deliveredMemory: Int = defaultDeliveredMemory,
        observer: QueueObserver = QueueObserver()
    ) {
        self.store = store
        self.clock = clock
        self.capacity = capacityBytes
        self.capacityItems = capacityItems
        self.deliveredMemory = deliveredMemory
        self.observer = observer
    }

    /// Build and restore in one step. Reloading what a previous run left on disk
    /// is not optional, and an initialiser cannot await.
    public static func open(
        store: QueueStore = MemoryQueueStore(),
        clock: RelayClock = SystemClock(),
        capacityBytes: Int = defaultQueueCapacityBytes,
        capacityItems: Int = Int.max,
        deliveredMemory: Int = defaultDeliveredMemory,
        observer: QueueObserver = QueueObserver()
    ) async throws -> StoreAndForwardQueue {
        let queue = StoreAndForwardQueue(
            store: store,
            clock: clock,
            capacityBytes: capacityBytes,
            capacityItems: capacityItems,
            deliveredMemory: deliveredMemory,
            observer: observer
        )
        try await queue.restore()
        return queue
    }

    /// Reload whatever the last run left behind. This is the whole of
    /// `docs/APPS-SCOPE.md` §4.5 from the queue's side: the app being killed
    /// mid-day costs nothing but the in-flight record, which is re-pulled
    /// because the glasses still have it.
    public func restore() async throws {
        let loaded = try await store.load()
        delivered = Array(loaded.delivered.suffix(deliveredMemory))
        deliveredSet = Set(delivered)
        // A record marked delivered but not yet removed is the crash window in
        // `flush`. Trusting the pending list alone would re-upload it every
        // restart.
        pending = loaded.pending
            .filter { !deliveredSet.contains($0.id) }
            .sorted { $0.sequence < $1.sequence }
        sequence = loaded.pending.reduce(0) { max($0, $1.sequence + 1) }
        observer.onRestored?(pending.count, usedBytes)
    }

    public var size: Int { pending.count }

    public var usedBytes: Int { pending.reduce(0) { $0 + $1.body.count } }

    public var capacityBytes: Int { capacity }

    public var freeBytes: Int { max(0, capacity - usedBytes) }

    /// In order.
    public var ids: [String] { pending.map(\.id) }

    public var deliveredIds: [String] { delivered }

    public func has(id: String) -> Bool { pending.contains { $0.id == id } }

    /// Accept a record, or refuse it with a reason.
    ///
    /// Async only because durability is: the record is on disk before this
    /// returns, so a caller may release its source the moment it does.
    @discardableResult
    public func enqueue(_ record: QueueRecord) async -> EnqueueResult {
        if deliveredSet.contains(record.id) {
            return EnqueueResult(accepted: true, alreadyDelivered: true)
        }
        if has(id: record.id) {
            return EnqueueResult(accepted: true, duplicate: true)
        }

        let byteCount = record.body.count
        if byteCount > capacity {
            return refuse(record, .tooLarge, "record is \(byteCount) bytes and the queue holds \(capacity)")
        }
        if usedBytes + byteCount > capacity {
            return refuse(record, .full, "queue full: \(usedBytes) + \(byteCount) exceeds \(capacity)")
        }
        if pending.count + 1 > capacityItems {
            return refuse(record, .itemLimit, "queue full: \(pending.count) records is the limit")
        }

        let stored = StoredRecord(record: record, enqueuedAtMs: clock.now(), sequence: sequence)
        sequence += 1

        do {
            try await store.append(stored)
        } catch {
            // The caller still owns its bytes. Saying "accepted" here is how a
            // day of audio gets deleted off the glasses and never arrives.
            return refuse(record, .storeFailed, String(describing: error))
        }

        // Re-check after the suspension. `store.append` is the only await in
        // this method, and an actor is re-entrant across it: a second task can
        // run the whole of `enqueue` while this one is writing. Without this
        // block `capacityBytes` would be advisory rather than a bound, and a
        // concurrent duplicate would be held twice.
        if has(id: stored.id) {
            return EnqueueResult(accepted: true, duplicate: true)
        }
        if usedBytes + byteCount > capacity {
            try? await store.remove(id: stored.id)
            return refuse(record, .full, "queue full: \(usedBytes) + \(byteCount) exceeds \(capacity)")
        }
        pending.append(stored)
        observer.onEnqueued?(stored)
        return EnqueueResult(accepted: true)
    }

    /// Send everything, oldest first, stopping at the first failure.
    ///
    /// Stopping rather than skipping is deliberate: the box segments episodes by
    /// time, so a queue that steps over a stuck record reorders someone's day.
    public func flush(_ send: QueueSend) async -> FlushResult {
        var sent = 0
        while let next = pending.first {
            do {
                try await send(next)
            } catch {
                observer.onFlushFailed?(next, error)
                return FlushResult(sent: sent, remaining: pending.count, error: error)
            }
            // Mark delivered *before* removing. A crash between the two leaves a
            // record that is both pending and delivered, which `restore`
            // resolves in favour of delivered — the other order re-uploads it.
            await remember(next.id)
            try? await store.remove(id: next.id)
            if let index = pending.firstIndex(where: { $0.id == next.id }) {
                pending.remove(at: index)
            }
            observer.onSent?(next)
            sent += 1
        }
        return FlushResult(sent: sent, remaining: 0, error: nil)
    }

    private func refuse(_ record: QueueRecord, _ reason: QueueRefusal, _ message: String) -> EnqueueResult {
        observer.onRefused?(record, reason, message)
        return EnqueueResult(accepted: false, reason: reason, message: message)
    }

    private func remember(_ id: String) async {
        guard deliveredMemory > 0 else { return }
        delivered.append(id)
        deliveredSet.insert(id)
        while delivered.count > deliveredMemory {
            let dropped = delivered.removeFirst()
            deliveredSet.remove(dropped)
        }
        try? await store.markDelivered(id: id)
    }
}
