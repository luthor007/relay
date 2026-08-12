import Foundation

/// Storage policy for the glasses' 4 GB.
///
/// `docs/APPS-SCOPE.md` §3.2, which is short enough to quote: poll disk state
/// (`0x0909` / `0x091C`), reserve headroom for audio and warn before video eats
/// it, sync-and-free proactively rather than waiting to hit full, and **never
/// delete un-uploaded audio** — `0x0911` 清除未上传文件 exists precisely because
/// the firmware tracks that distinction.
///
/// The asymmetry that drives all of it: 1080p video runs ~4.5 GB/h and Opus
/// audio runs ~10.8 MB/h, so **video is about four hundred times more
/// expensive**. Forty minutes of video is a whole day of conversation. That is
/// not a warning to bury in a settings screen.

public enum CaptureFormat: String, Sendable, Equatable {
    /// ~24 kbps mono. `docs/APPS-SCOPE.md` §3.1.
    case opus
    /// 16 kHz / 16-bit mono.
    case pcm16

    /// Bytes per second on the glasses' own storage.
    public var bytesPerSecond: Int {
        switch self {
        case .opus: return 3_000
        case .pcm16: return 32_000
        }
    }
}

/// ~4.5 GB/h at 1080p.
public let videoBytesPerSecond = 1_342_177

/// The capture-worthy part of a day, per `docs/APPS-SCOPE.md` §3.1.
public let captureDaySeconds = 16 * 3_600

public struct StorageBudget: Sendable, Equatable {
    /// What the glasses record audio as. Still an open question against real
    /// hardware (§8), and it changes every number here by ~10×, so it is a
    /// parameter rather than a constant.
    public var format: CaptureFormat
    /// Days of audio that video is not allowed to take. One by default: the
    /// product promise is "records your day", so the day is the floor.
    public var reservedDays: Double
    /// Below this fraction of the reserve, stop being polite about it.
    public var criticalFraction: Double

    public init(format: CaptureFormat = .opus, reservedDays: Double = 1, criticalFraction: Double = 0.25) {
        self.format = format
        self.reservedDays = reservedDays
        self.criticalFraction = criticalFraction
    }

    /// Bytes that must stay available for audio.
    public var reservedBytes: Int {
        Int(Double(captureDaySeconds * format.bytesPerSecond) * reservedDays)
    }
}

public enum StorageLevel: String, Sendable, Equatable, Comparable {
    case ok
    case watch
    /// Below the audio reserve. Video is refused from here down.
    case warn
    /// Deep below it. Sync now, and say so out loud.
    case critical

    private var rank: Int {
        switch self {
        case .ok: return 0
        case .watch: return 1
        case .warn: return 2
        case .critical: return 3
        }
    }

    public static func < (lhs: StorageLevel, rhs: StorageLevel) -> Bool {
        lhs.rank < rhs.rank
    }
}

public struct StorageAssessment: Sendable, Equatable {
    public let level: StorageLevel
    public let freeBytes: Int
    public let totalBytes: Int
    /// How much more audio fits, in seconds.
    public let audioSecondsRemaining: Int
    /// How much video fits before the audio reserve is breached. Zero once it
    /// already is.
    public let videoSecondsBeforeReserve: Int
    /// False once video would come out of the audio reserve.
    public let videoAllowed: Bool
    /// Bytes held by files the device says are already uploaded. This is the
    /// only space the app may reclaim on its own.
    public let reclaimableBytes: Int
    /// Bytes that exist only on the glasses. Never touched.
    public let unsyncedBytes: Int
    public let message: String
}

public enum StoragePolicy {

    public static func assess(
        disk: DiskInfo,
        files: [RemoteFile] = [],
        budget: StorageBudget = StorageBudget()
    ) -> StorageAssessment {
        let reserve = budget.reservedBytes
        let audioRate = budget.format.bytesPerSecond
        let audioSeconds = disk.freeBytes / max(1, audioRate)
        let spare = max(0, disk.freeBytes - reserve)
        let videoSeconds = spare / videoBytesPerSecond

        let reclaimable = files.filter(\.uploaded).reduce(0) { $0 + $1.sizeBytes }
        let unsynced = files.filter { !$0.uploaded }.reduce(0) { $0 + $1.sizeBytes }

        let level: StorageLevel
        if disk.freeBytes < Int(Double(reserve) * budget.criticalFraction) {
            level = .critical
        } else if disk.freeBytes < reserve {
            level = .warn
        } else if disk.freeBytes < reserve * 2 {
            level = .watch
        } else {
            level = .ok
        }

        return StorageAssessment(
            level: level,
            freeBytes: disk.freeBytes,
            totalBytes: disk.totalBytes,
            audioSecondsRemaining: audioSeconds,
            videoSecondsBeforeReserve: videoSeconds,
            videoAllowed: level < .warn && videoSeconds > 0,
            reclaimableBytes: reclaimable,
            unsyncedBytes: unsynced,
            message: message(level: level, audioSeconds: audioSeconds, videoSeconds: videoSeconds)
        )
    }

    private static func message(level: StorageLevel, audioSeconds: Int, videoSeconds: Int) -> String {
        let audioHours = Double(audioSeconds) / 3_600
        switch level {
        case .ok:
            return String(format: "About %.0f hours of audio left on the glasses.", audioHours)
        case .watch:
            return String(
                format: "About %.0f hours of audio left. Video would use it up in %d minutes.",
                audioHours,
                max(0, videoSeconds / 60)
            )
        case .warn:
            return String(
                format: "Under a day of audio left (%.0f hours). Video is off until the "
                    + "glasses sync — a few minutes of it would cost you the day.",
                audioHours
            )
        case .critical:
            return String(
                format: "The glasses are nearly full — about %.0f hours left. Plug them in "
                    + "so tonight's sync can free the space.",
                audioHours
            )
        }
    }

    /// The only files the app may delete, ever.
    ///
    /// `RemoteFile.uploaded` is the *device's* opinion, not ours. The firmware
    /// tracks it and `0x0911` acts on it, so deleting on the strength of our own
    /// bookkeeping is how the only copy of an afternoon disappears — our
    /// bookkeeping is exactly what a crash loses.
    public static func safeToDelete(_ files: [RemoteFile]) -> [RemoteFile] {
        files.filter(\.uploaded)
    }

    /// Oldest-uploaded-first, until `targetBytes` is met. Returns fewer than
    /// asked for — possibly none — rather than reaching past the uploaded set.
    ///
    /// `listFiles` returns device order, which is creation order on this
    /// firmware. That is the only ordering available: `RemoteFile` carries no
    /// timestamp, because the reply layout for the file-list command is one of
    /// the payloads `docs/APPS-SCOPE.md` §5.1 marks as unattested.
    public static func reclaim(targetBytes: Int, from files: [RemoteFile]) -> [RemoteFile] {
        var chosen: [RemoteFile] = []
        var freed = 0
        for file in safeToDelete(files) {
            if freed >= targetBytes { break }
            chosen.append(file)
            freed += file.sizeBytes
        }
        return chosen
    }

    /// Would recording video for this long push audio below its reserve?
    ///
    /// The question the UI has to answer *before* the shutter, not after.
    public static func videoWouldBreachReserve(
        seconds: Int,
        disk: DiskInfo,
        budget: StorageBudget = StorageBudget()
    ) -> Bool {
        let cost = seconds * videoBytesPerSecond
        return disk.freeBytes - cost < budget.reservedBytes
    }
}

// MARK: - polling

/// Polls `getDiskInfo` and reports assessments.
///
/// A poll rather than a subscription because the device only reports disk state
/// when asked (`0x0909` / `0x091C` are commands, not notifications), and a
/// product that discovers a full device at the end of the day has already lost
/// the afternoon.
///
/// Five minutes by default: at 4.5 GB/h a video session can consume the whole
/// reserve inside one interval, so the recording paths ask directly rather than
/// waiting for the next tick.
public actor StorageMonitor {

    private let glasses: GlassesTransport
    private let clock: RelayClock
    private let intervalMs: Int
    private let budget: StorageBudget
    private let onAssessment: @Sendable (StorageAssessment) -> Void

    private var running = false
    private var lastLevel: StorageLevel?
    public private(set) var latest: StorageAssessment?

    public init(
        glasses: GlassesTransport,
        clock: RelayClock = SystemClock(),
        intervalMs: Int = 5 * 60 * 1_000,
        budget: StorageBudget = StorageBudget(),
        onAssessment: @escaping @Sendable (StorageAssessment) -> Void = { _ in }
    ) {
        self.glasses = glasses
        self.clock = clock
        self.intervalMs = intervalMs
        self.budget = budget
        self.onAssessment = onAssessment
    }

    public var isRunning: Bool { running }

    /// Whether the level changed on the last sample. The UI nags on a change,
    /// not on every poll — a warning repeated every five minutes is a warning
    /// nobody reads.
    public private(set) var levelChanged = false

    public func start() {
        guard !running else { return }
        running = true
        Task { [weak self] in
            await self?.loop()
        }
    }

    public func stop() {
        running = false
    }

    /// One sample, on demand. Called before starting video, and after a sync.
    @discardableResult
    public func sample() async -> StorageAssessment? {
        guard let disk = try? await glasses.getDiskInfo() else { return nil }
        let files = (try? await glasses.listFiles()) ?? []
        let assessment = StoragePolicy.assess(disk: disk, files: files, budget: budget)
        levelChanged = lastLevel != assessment.level
        lastLevel = assessment.level
        latest = assessment
        onAssessment(assessment)
        return assessment
    }

    private func loop() async {
        while running {
            await sample()
            await clock.sleep(ms: intervalMs)
        }
    }
}
