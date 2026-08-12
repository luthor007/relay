import Foundation

/// The nightly sync, as an explicit state machine.
///
/// A port of `BulkSync` in `glasses/bridge/src/queue.ts`, with one addition iOS
/// forces (``SyncDeferral/needsForeground``). Three facts drive the whole thing:
///
///   1. A day of audio cannot ride BLE — ~173 MB at ~3 KB/s is ~16 hours, longer
///      than the day took to record. It goes over the glasses' own access point.
///   2. The phone cannot hold the glasses' AP and its own uplink at once
///      (`docs/ARCHITECTURE.md` §2.1). So this is two phases with a network
///      change between them, not one transfer.
///   3. The day's audio never crosses the rendezvous relay (`docs/SYSTEM.md`
///      §7). If the box is not on the same LAN, sync **waits and says so**
///      rather than spending a gigabyte of someone's cellular plan.
///
/// Explicit as a state machine because the interesting states are the ones where
/// nothing is transferring: waiting for the LAN, holding the AP with no uplink,
/// and rejoining. A design that models sync as one `await` has nowhere to put
/// those and ends up discovering them as bugs.
///
/// It never deletes anything from the glasses.

public enum BoxReachability: String, Sendable, Equatable {
    /// Same LAN. The only state in which bulk sync moves.
    case lan
    /// Through our rendezvous relay: control traffic only, never the day's audio.
    case relay
    case none
}

/// The phone's radios, injected.
///
/// `joinAccessPoint` costs the uplink. An implementation that could hold both
/// would be a different product; this protocol exists so the state machine
/// cannot accidentally assume one.
///
/// On iOS the real implementation is `NEHotspotConfigurationManager`, which
/// needs the `com.apple.developer.networking.HotspotConfiguration` entitlement —
/// see `apps/ios/README.md`.
public protocol SyncNetwork: AnyObject, Sendable {
    func reachBox() async throws -> BoxReachability
    func joinAccessPoint(_ accessPoint: WifiAccessPoint) async throws
    func leaveAccessPoint() async throws
}

public enum SyncNetworkError: Error, Sendable, Equatable {
    /// Asked to reach the box while the phone is on the glasses' network.
    case uplinkHeldByAccessPoint(String)
}

public enum SyncPhase: String, Sendable, Equatable {
    case idle
    /// Stopped before doing anything. The deferral carries the reason.
    case waiting
    case openingAccessPoint
    /// Uplink lost from here until `rejoiningUplink` completes.
    case joiningAccessPoint
    case pullingFiles
    case leavingAccessPoint
    case rejoiningUplink
    case uploading
    case done
    case failed
}

public enum SyncDeferral: String, Sendable, Equatable {
    case notCharging
    /// Reachable, but only through the relay. Bulk waits for the LAN.
    case boxOnlyViaRelay
    case boxUnreachable
    case notConnected
    case nothingToSync
    /// iOS only, and not in the TypeScript because no other platform has it:
    /// `NEHotspotConfigurationManager.apply` puts up a system dialog ("Relay
    /// wants to join QCGlasses-XXXX") that the user has to accept. There is no
    /// dialog in the background, so the AP phase cannot run from a
    /// `BGProcessingTask`. The upload phase still can, and does.
    case needsForeground
}

public struct SyncResult: Sendable {
    public var phase: SyncPhase
    public var filesPulled: Int
    public var bytesPulled: Int
    public var uploaded: Int
    public var remaining: Int
    /// Set when the run stopped for a reason the user should be told.
    public var deferred: SyncDeferral?
    public var errorDescription: String?
}

/// Handed in at construction, like ``QueueObserver`` and for the same reason.
public struct SyncObserver: Sendable {
    public var onPhaseChanged: (@Sendable (SyncPhase) -> Void)?
    public var onPullProgress: (@Sendable (String, Int, Int) -> Void)?
    public var onFilePulled: (@Sendable (RemoteFile, EnqueueResult) -> Void)?
    /// The "it is waiting, and here is why" callback. Wire it to the UI.
    public var onDeferred: (@Sendable (SyncDeferral, String) -> Void)?
    public var onFinished: (@Sendable (SyncResult) -> Void)?

    public init(
        onPhaseChanged: (@Sendable (SyncPhase) -> Void)? = nil,
        onPullProgress: (@Sendable (String, Int, Int) -> Void)? = nil,
        onFilePulled: (@Sendable (RemoteFile, EnqueueResult) -> Void)? = nil,
        onDeferred: (@Sendable (SyncDeferral, String) -> Void)? = nil,
        onFinished: (@Sendable (SyncResult) -> Void)? = nil
    ) {
        self.onPhaseChanged = onPhaseChanged
        self.onPullProgress = onPullProgress
        self.onFilePulled = onFilePulled
        self.onDeferred = onDeferred
        self.onFinished = onFinished
    }
}

public actor BulkSync {

    private let glasses: GlassesTransport
    private let queue: StoreAndForwardQueue
    private let network: SyncNetwork
    private let upload: QueueSend
    private let clock: RelayClock
    private let requireCharging: Bool
    private let rejoinTimeoutMs: Int
    private let rejoinPollMs: Int
    private let observer: SyncObserver
    /// Whether the AP phase is allowed to run right now. See
    /// ``SyncDeferral/needsForeground``.
    private let canPresentSystemDialogs: @Sendable () -> Bool

    private var currentPhase: SyncPhase = .idle
    private var holdingAp = false

    public init(
        glasses: GlassesTransport,
        queue: StoreAndForwardQueue,
        network: SyncNetwork,
        upload: @escaping QueueSend,
        clock: RelayClock = SystemClock(),
        requireCharging: Bool = true,
        rejoinTimeoutMs: Int = 60_000,
        rejoinPollMs: Int = 2_000,
        canPresentSystemDialogs: @escaping @Sendable () -> Bool = { true },
        observer: SyncObserver = SyncObserver()
    ) {
        self.glasses = glasses
        self.queue = queue
        self.network = network
        self.upload = upload
        self.clock = clock
        self.requireCharging = requireCharging
        self.rejoinTimeoutMs = rejoinTimeoutMs
        self.rejoinPollMs = rejoinPollMs
        self.canPresentSystemDialogs = canPresentSystemDialogs
        self.observer = observer
    }

    public var phase: SyncPhase { currentPhase }

    /// False exactly while the phone is on the glasses' network.
    ///
    /// Anything that wants the box — including the relayd link — has to check
    /// this rather than discover it as a timeout.
    public var uplinkAvailable: Bool { !holdingAp }

    public func run() async -> SyncResult {
        var filesPulled = 0
        var bytesPulled = 0
        setPhase(.idle)

        // Preconditions, before anything costs a radio.
        if let precondition = await checkPreconditions() {
            return await defer_(precondition.0, precondition.1)
        }

        // `listFiles` is a BLE command and costs nothing, so ask before paying
        // for a network change. A run with nothing new to pull and nothing left
        // over should not touch the WiFi radio at all.
        let outstanding: [RemoteFile]
        do {
            let all = try await glasses.listFiles()
            var candidates: [RemoteFile] = []
            for file in all where !file.uploaded {
                let held = await queue.has(id: file.name)
                let done = await queue.deliveredIds.contains(file.name)
                if !held && !done { candidates.append(file) }
            }
            outstanding = candidates
        } catch {
            return await fail(error)
        }

        let queued = await queue.size
        if outstanding.isEmpty && queued == 0 {
            return await defer_(.nothingToSync, "nothing on the glasses that the box lacks")
        }

        // Phase 1 — the glasses' network, with no uplink.
        if !outstanding.isEmpty {
            guard canPresentSystemDialogs() else {
                return await defer_(
                    .needsForeground,
                    "there are \(outstanding.count) recordings to pull, but joining the "
                        + "glasses' network needs you to confirm it. Open Relay to finish."
                )
            }

            do {
                setPhase(.openingAccessPoint)
                let accessPoint = try await glasses.openWifiAccessPoint()
                setPhase(.joiningAccessPoint)
                try await network.joinAccessPoint(accessPoint)
                holdingAp = true
            } catch {
                return await fail(error)
            }

            do {
                setPhase(.pullingFiles)
                for file in outstanding {
                    let name = file.name
                    let observer = self.observer
                    let body = try await glasses.fetchFile(name: name) { progress in
                        observer.onPullProgress?(name, progress.receivedBytes, progress.totalBytes)
                    }

                    let result = await queue.enqueue(QueueRecord(
                        id: file.name,
                        kind: file.name.hasSuffix(".jpg") ? "photo" : "audio",
                        body: body,
                        meta: RecordMeta(
                            sourceName: file.name,
                            sizeBytes: file.sizeBytes,
                            durationS: file.durationS,
                            pulledAtMs: clock.now(),
                            encoding: file.name.hasSuffix(".opus") ? "opus" : nil
                        )
                    ))
                    self.observer.onFilePulled?(file, result)

                    if !result.accepted {
                        // The phone is out of room. Stop pulling — the glasses
                        // still hold the file, which is the correct place for it.
                        break
                    }
                    filesPulled += 1
                    bytesPulled += body.count
                }
            } catch {
                // Always release the AP first: a phone left on the glasses'
                // network with no uplink is worse than a failed sync.
                let failedIn = currentPhase
                await releaseAccessPoint()
                return await fail(error, failedIn: failedIn)
            }

            await releaseAccessPoint()
        }

        // Phase 2 — back on the real network.
        setPhase(.rejoiningUplink)
        let backOnLan = await waitForLan()
        if !backOnLan {
            // The bytes are safe in the queue; the upload is simply not now.
            return await defer_(
                .boxUnreachable,
                "pulled the day, but the box is not on this network yet — the upload stays queued",
                filesPulled: filesPulled,
                bytesPulled: bytesPulled
            )
        }

        setPhase(.uploading)
        let flushed = await queue.flush(upload)
        if let error = flushed.error {
            return await fail(
                error,
                failedIn: .uploading,
                filesPulled: filesPulled,
                bytesPulled: bytesPulled,
                uploaded: flushed.sent,
                remaining: flushed.remaining
            )
        }

        setPhase(.done)
        return finish(SyncResult(
            phase: .done,
            filesPulled: filesPulled,
            bytesPulled: bytesPulled,
            uploaded: flushed.sent,
            remaining: 0
        ))
    }

    // MARK: - internals

    private func checkPreconditions() async -> (SyncDeferral, String)? {
        if requireCharging {
            do {
                let battery = try await glasses.getBattery()
                if !battery.charging {
                    return (
                        .notCharging,
                        "waiting until the glasses are charging — sync is a plugged-in ritual"
                    )
                }
            } catch {
                return (.notConnected, "glasses not reachable: \(String(describing: error))")
            }
        } else if glasses.state != .connected {
            return (.notConnected, "glasses not connected")
        }

        let reach: BoxReachability
        do {
            reach = try await network.reachBox()
        } catch {
            return (.boxUnreachable, "could not test the route to the box: \(String(describing: error))")
        }

        switch reach {
        case .relay:
            return (
                .boxOnlyViaRelay,
                "the box is only reachable through the relay — the day's audio waits for home "
                    + "WiFi rather than riding your data plan"
            )
        case .none:
            return (.boxUnreachable, "the box is not reachable")
        case .lan:
            return nil
        }
    }

    private func waitForLan() async -> Bool {
        let deadline = clock.now() + rejoinTimeoutMs
        while true {
            if let reach = try? await network.reachBox(), reach == .lan { return true }
            if clock.now() >= deadline { return false }
            await clock.sleep(ms: rejoinPollMs)
        }
    }

    private func releaseAccessPoint() async {
        guard holdingAp else { return }
        setPhase(.leavingAccessPoint)
        holdingAp = false
        try? await network.leaveAccessPoint()
        // Closing the device AP is best-effort: the phone has already left it,
        // and a device that keeps its hotspot up costs battery but loses nothing.
        try? await glasses.closeWifiAccessPoint()
    }

    private func setPhase(_ phase: SyncPhase) {
        guard currentPhase != phase else { return }
        currentPhase = phase
        observer.onPhaseChanged?(phase)
    }

    /// `defer` is a keyword, and this is the least ugly way to keep the name.
    private func defer_(
        _ reason: SyncDeferral,
        _ message: String,
        filesPulled: Int = 0,
        bytesPulled: Int = 0
    ) async -> SyncResult {
        setPhase(.waiting)
        observer.onDeferred?(reason, message)
        return finish(SyncResult(
            phase: .waiting,
            filesPulled: filesPulled,
            bytesPulled: bytesPulled,
            uploaded: 0,
            remaining: await queue.size,
            deferred: reason,
            errorDescription: message
        ))
    }

    private func fail(
        _ error: Error,
        failedIn: SyncPhase? = nil,
        filesPulled: Int = 0,
        bytesPulled: Int = 0,
        uploaded: Int = 0,
        remaining: Int? = nil
    ) async -> SyncResult {
        // Read the phase before overwriting it: "failed while pulling files" and
        // "failed while uploading" send the user to different places.
        let phaseAtFailure = failedIn ?? currentPhase
        setPhase(.failed)
        let left = remaining ?? (await queue.size)
        return finish(SyncResult(
            phase: .failed,
            filesPulled: filesPulled,
            bytesPulled: bytesPulled,
            uploaded: uploaded,
            remaining: left,
            errorDescription: "\(phaseAtFailure.rawValue): \(String(describing: error))"
        ))
    }

    private func finish(_ result: SyncResult) -> SyncResult {
        observer.onFinished?(result)
        return result
    }
}

// MARK: - mock

/// A phone's radios, faked — including the part where it only has one.
///
/// `reachBox` throws while the glasses' access point is held, because that is
/// what the hardware does (`docs/ARCHITECTURE.md` §2.1) and a mock that quietly
/// answered would let the state machine grow a dependency the real phone cannot
/// satisfy. This is the one assertion in the file that is structural rather than
/// a test: it cannot be forgotten.
public final class MockSyncNetwork: SyncNetwork, @unchecked Sendable {

    private let lock = NSLock()
    private var reachability: BoxReachability
    private var holdingAp = false
    public private(set) var joined: [WifiAccessPoint] = []
    public private(set) var leaveCount = 0

    public init(_ reachability: BoxReachability = .lan) {
        self.reachability = reachability
    }

    public var holdingAccessPoint: Bool {
        lock.lock(); defer { lock.unlock() }
        return holdingAp
    }

    public func setReachability(_ value: BoxReachability) {
        lock.lock(); defer { lock.unlock() }
        reachability = value
    }

    public func reachBox() async throws -> BoxReachability {
        lock.lock(); defer { lock.unlock() }
        if holdingAp {
            throw SyncNetworkError.uplinkHeldByAccessPoint(
                "the phone cannot reach the box while it holds the glasses' access point"
            )
        }
        return reachability
    }

    public func joinAccessPoint(_ accessPoint: WifiAccessPoint) async throws {
        lock.lock(); defer { lock.unlock() }
        holdingAp = true
        joined.append(accessPoint)
    }

    public func leaveAccessPoint() async throws {
        lock.lock(); defer { lock.unlock() }
        holdingAp = false
        leaveCount += 1
    }
}
