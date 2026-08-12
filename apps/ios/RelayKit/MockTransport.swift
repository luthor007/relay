import Foundation

/// MockTransport — the glasses, without the glasses.
///
/// A port of `glasses/bridge/src/mock.ts`, and deliberately inconvenient in the
/// same ways. The point is not to make calls resolve; it is to make them resolve
/// *the way the hardware does*, so the app is built against real constraints:
///
///   - photos arrive over BLE at a few KB/s, so `takePhoto` takes seconds and
///     reports progress; asking for a smaller image is genuinely faster
///   - `fetchFile` over BLE is unusably slow and fast over the WiFi AP, which is
///     why the sync design is a nightly ritual rather than a trickle
///   - local recording consumes the 4 GB budget, and storage can fill
///   - the link drops, and commands in flight fail when it does
///
/// A mock that returns instantly and never fails produces a UI that lies about
/// latency and has no error states.
///
/// Drive it with a ``TestClock``: nothing happens until time is advanced.
public final class MockTransport: GlassesTransport, @unchecked Sendable {

    // Every default here is an **estimate** until the hardware is measured.
    // Replace with real figures from `tools/capture_trace.py measure`.
    public enum Defaults {
        public static let connectDelayMs = 800
        /// Practical BLE application throughput.
        public static let bleBytesPerSecond = 3_000
        /// Over the glasses' own access point.
        public static let wifiBytesPerSecond = 2_000_000
        /// 4 GB device, per supplier.
        public static let totalStorageBytes = 4 * 1024 * 1024 * 1024
        /// Opus ~24 kbps mono.
        public static let recordingBytesPerSecond = 3_000
        public static let batteryPercent = 92
        public static let batteryDrainPerHour = 12.0
        public static let batteryChargePerHour = 45.0
        /// Rough JPEG bytes per pixel at the device's default quality.
        public static let jpegBytesPerPixel = 0.08
        public static let defaultPhotoWidth = 2_048
        public static let defaultPhotoHeight = 1_536
        public static let photoProgressIntervalMs = 250
    }

    public struct Faults: Sendable, Equatable {
        /// Reject every connect attempt.
        public var connectFails = false
        /// Fail photo capture partway through the transfer.
        public var photoFails = false
        /// Drop the link this long after connecting.
        public var dropAfterMs: Int?
        /// Reject writes once storage is exhausted rather than silently wrapping.
        public var storageFull = false

        public init(
            connectFails: Bool = false,
            photoFails: Bool = false,
            dropAfterMs: Int? = nil,
            storageFull: Bool = false
        ) {
            self.connectFails = connectFails
            self.photoFails = photoFails
            self.dropAfterMs = dropAfterMs
            self.storageFull = storageFull
        }
    }

    public struct Options: Sendable {
        public var connectDelayMs = Defaults.connectDelayMs
        public var bleBytesPerSecond = Defaults.bleBytesPerSecond
        public var wifiBytesPerSecond = Defaults.wifiBytesPerSecond
        public var totalStorageBytes = Defaults.totalStorageBytes
        public var usedStorageBytes = 0
        public var recordingBytesPerSecond = Defaults.recordingBytesPerSecond
        public var batteryPercent = Defaults.batteryPercent
        public var charging = false
        public var features = Features()
        public var faults = Faults()

        public init() {}
    }

    private let clock: RelayClock
    private var options: Options
    private let lock = NSLock()

    private var handlers: [Int: @Sendable (GlassesEvent) -> Void] = [:]
    private var nextHandlerID = 0

    private var currentState: ConnectionState = .disconnected
    private var batteryPercent: Double
    private var charging: Bool
    private var batteryAtMs: Int
    private var usedStorageBytes: Int
    private var apOpen = false
    private var previewing = false
    private var voiceOpen = false
    private var recordingSinceMs: Int?
    private var files: [RemoteFile] = []
    private var audioSeq = 0
    private var fileCounter = 0
    private var dropTask: Task<Void, Never>?

    public init(clock: RelayClock = SystemClock(), options: Options = Options()) {
        self.clock = clock
        self.options = options
        self.batteryPercent = Double(options.batteryPercent)
        self.charging = options.charging
        self.batteryAtMs = clock.now()
        self.usedStorageBytes = options.usedStorageBytes
    }

    // MARK: - lifecycle

    public var state: ConnectionState {
        lock.lock()
        defer { lock.unlock() }
        return currentState
    }

    @discardableResult
    public func on(_ handler: @escaping @Sendable (GlassesEvent) -> Void) -> Subscription {
        lock.lock()
        nextHandlerID += 1
        let id = nextHandlerID
        handlers[id] = handler
        lock.unlock()

        return Subscription { [weak self] in
            guard let self else { return }
            self.lock.lock()
            self.handlers.removeValue(forKey: id)
            self.lock.unlock()
        }
    }

    public func connect(options connectOptions: ConnectOptions) async throws {
        if state == .connected { return }

        setState(.connecting)
        await clock.sleep(ms: readOptions().connectDelayMs)

        let opts = readOptions()
        if opts.faults.connectFails {
            setState(.disconnected)
            throw GlassesError(.connectFailed, "mock: connect failed")
        }
        if let timeout = connectOptions.timeoutMs, timeout < opts.connectDelayMs {
            setState(.disconnected)
            throw GlassesError(.timeout, "mock: connect timed out")
        }

        setState(.connected)

        if let dropAfter = opts.faults.dropAfterMs {
            let task = Task { [weak self] in
                guard let self else { return }
                await self.clock.sleep(ms: dropAfter)
                guard !Task.isCancelled else { return }
                self.simulateDisconnect(reason: "mock: link dropped")
            }
            locked { dropTask = task }
        }
    }

    public func disconnect() async {
        locked {
            dropTask?.cancel()
            dropTask = nil
            voiceOpen = false
            previewing = false
            apOpen = false
        }
        setState(.disconnected)
    }

    // MARK: - device

    public func getFeatures() async throws -> Features {
        try requireConnected()
        return readOptions().features
    }

    public func getBattery() async throws -> BatteryStatus {
        try requireConnected()
        let status = settleBattery()
        emit(.battery(status))
        return status
    }

    public func getDiskInfo() async throws -> DiskInfo {
        try requireConnected()
        let info = disk()
        emit(.diskInfo(info))
        return info
    }

    public func setTime(_ date: Date) async throws {
        try requireConnected()
    }

    // MARK: - voice

    public func startVoiceSession() async throws {
        try requireConnected()
        let changed = locked { () -> Bool in
            guard !voiceOpen else { return false }
            voiceOpen = true
            audioSeq = 0
            return true
        }
        if changed { emit(.voiceSessionChanged(true)) }
    }

    public func stopVoiceSession() async throws {
        try requireConnected()
        let changed = locked { () -> Bool in
            guard voiceOpen else { return false }
            voiceOpen = false
            return true
        }
        if changed { emit(.voiceSessionChanged(false)) }
    }

    // MARK: - camera

    public func capturePhoto() async throws -> RemoteFile {
        try requireConnected()

        let sizeBytes = Int(
            (Double(Defaults.defaultPhotoWidth) * Double(Defaults.defaultPhotoHeight)
                * Defaults.jpegBytesPerPixel).rounded()
        )
        guard disk().freeBytes >= sizeBytes else {
            throw GlassesError(.storageFull, "mock: storage full")
        }

        let file = locked { () -> RemoteFile in
            usedStorageBytes += sizeBytes
            fileCounter += 1
            let file = RemoteFile(
                name: String(format: "IMG_%04d.jpg", fileCounter),
                sizeBytes: sizeBytes,
                uploaded: false
            )
            files.append(file)
            return file
        }

        emit(.diskInfo(disk()))
        return file
    }

    public func fetchThumbnail(name: String, options thumbOptions: ThumbnailOptions) async throws -> Photo {
        try requireConnected()
        guard let _ = file(named: name) else {
            throw GlassesError(.transferFailed, "mock: no such file \(name)")
        }

        let clarity = max(0, min(6, thumbOptions.clarity ?? 2))
        // 2 KB at clarity 0 up to ~50 KB at clarity 6.
        let totalBytes = Int((2_048.0 * pow(1.75, Double(clarity))).rounded())
        let totalMs = Int((Double(totalBytes) / Double(readOptions().bleBytesPerSecond) * 1000).rounded())

        await clock.sleep(ms: totalMs)
        try requireConnected()

        return Photo(
            data: Self.syntheticJPEG(totalBytes),
            widthPx: 160 * (clarity + 1),
            heightPx: 120 * (clarity + 1)
        )
    }

    public func takePhoto(options photoOptions: PhotoOptions) async throws -> Photo {
        try requireConnected()

        let width = photoOptions.maxWidth ?? Defaults.defaultPhotoWidth
        let height = photoOptions.maxHeight ?? Defaults.defaultPhotoHeight
        let totalBytes = max(
            1024,
            Int((Double(width) * Double(height) * Defaults.jpegBytesPerPixel).rounded())
        )

        let rate = readOptions().bleBytesPerSecond
        let totalMs = Double(totalBytes) / Double(rate) * 1000
        let step = Double(Defaults.photoProgressIntervalMs)
        let chunkCount = max(1, Int((totalMs / step).rounded(.up)))

        for chunk in 1 ... chunkCount {
            let slice = min(step, totalMs - step * Double(chunk - 1))
            await clock.sleep(ms: Int(slice.rounded()))
            try requireConnected()

            if readOptions().faults.photoFails, chunk == Int((Double(chunkCount) / 2).rounded(.up)) {
                throw GlassesError(.transferFailed, "mock: photo transfer failed")
            }

            let received = min(totalBytes, Int((Double(totalBytes * chunk) / Double(chunkCount)).rounded()))
            emit(.photoProgress(PhotoProgress(
                receivedBytes: received,
                totalBytes: totalBytes,
                chunkIndex: chunk,
                chunkCount: chunkCount
            )))
        }

        let photo = Photo(data: Self.syntheticJPEG(totalBytes), widthPx: width, heightPx: height)
        emit(.photo(photo))
        return photo
    }

    // MARK: - local recording

    public func startLocalRecording() async throws {
        try requireConnected()
        guard readOptions().features.localRecording else {
            throw GlassesError(.unsupported, "mock: local recording unsupported")
        }
        let alreadyRecording = locked { recordingSinceMs != nil }
        if alreadyRecording { return }

        guard disk().freeBytes > 0 else {
            throw GlassesError(.storageFull, "mock: storage full")
        }

        let startedAt = clock.now()
        locked { recordingSinceMs = startedAt }
        emit(.recordingState(RecordingState(recording: true, durationS: 0)))
    }

    public func stopLocalRecording() async throws {
        try requireConnected()

        let started = locked { () -> Int? in
            defer { recordingSinceMs = nil }
            return recordingSinceMs
        }
        guard let since = started else { return }

        let durationS = Double(clock.now() - since) / 1000
        let wanted = Int((durationS * Double(readOptions().recordingBytesPerSecond)).rounded())
        let free = disk().freeBytes

        if wanted > free, readOptions().faults.storageFull {
            throw GlassesError(.storageFull, "mock: storage full during write")
        }

        let sizeBytes = min(wanted, free)
        locked {
            usedStorageBytes += sizeBytes
            fileCounter += 1
            files.append(RemoteFile(
                name: String(format: "REC_%04d.opus", fileCounter),
                sizeBytes: sizeBytes,
                uploaded: false,
                durationS: Int(durationS)
            ))
        }

        emit(.recordingState(RecordingState(recording: false, durationS: 0)))
        emit(.diskInfo(disk()))
    }

    public func listFiles() async throws -> [RemoteFile] {
        try requireConnected()
        return locked { files }
    }

    public func deleteFile(name: String) async throws {
        try requireConnected()
        let removed = locked { () -> Bool in
            guard let index = files.firstIndex(where: { $0.name == name }) else { return false }
            usedStorageBytes = max(0, usedStorageBytes - files[index].sizeBytes)
            files.remove(at: index)
            return true
        }
        guard removed else {
            throw GlassesError(.transferFailed, "mock: no such file \(name)")
        }
        emit(.diskInfo(disk()))
    }

    // MARK: - bulk transfer

    public func openWifiAccessPoint() async throws -> WifiAccessPoint {
        try requireConnected()
        guard readOptions().features.wifiAp else {
            throw GlassesError(.unsupported, "mock: no WiFi AP")
        }
        locked { apOpen = true }
        return WifiAccessPoint(ssid: "QCGlasses-MOCK", password: "12345678", host: "192.168.31.1")
    }

    public func closeWifiAccessPoint() async throws {
        try requireConnected()
        locked { apOpen = false }
    }

    /// Rate depends on whether the access point is open — the whole reason the
    /// sync design is a nightly WiFi ritual rather than a background BLE trickle.
    public func fetchFile(
        name: String,
        onProgress: (@Sendable (FetchProgress) -> Void)?
    ) async throws -> Data {
        try requireConnected()
        guard let file = file(named: name) else {
            throw GlassesError(.transferFailed, "mock: no such file \(name)")
        }

        let opts = readOptions()
        let rate = isAccessPointOpen ? opts.wifiBytesPerSecond : opts.bleBytesPerSecond
        let totalMs = Double(file.sizeBytes) / Double(rate) * 1000
        let steps = max(1, min(50, Int((totalMs / 200).rounded(.up))))

        for step in 1 ... steps {
            await clock.sleep(ms: Int((totalMs / Double(steps)).rounded()))
            try requireConnected()
            onProgress?(FetchProgress(
                receivedBytes: file.sizeBytes * step / steps,
                totalBytes: file.sizeBytes
            ))
        }

        markUploaded(name: name)
        return Data(count: file.sizeBytes)
    }

    // MARK: - video

    public func startPreview() async throws -> String {
        try requireConnected()
        guard readOptions().features.livePreview else {
            throw GlassesError(.unsupported, "mock: no live preview")
        }
        locked {
            apOpen = true
            previewing = true
        }

        let url = "rtsp://192.168.31.1:8554/live"
        emit(.rtspUrl(url))
        return url
    }

    public func stopPreview() async throws {
        try requireConnected()
        locked { previewing = false }
    }

    // MARK: - test controls
    // Not part of GlassesTransport; the app never sees these.

    /// Push a touch event. `.wear` and `.remove` additionally emit `.wear`.
    public func emitTouch(_ action: TouchAction) {
        emit(.touch(action))
        if action == .wear { emit(.wear(true)) }
        if action == .remove { emit(.wear(false)) }
    }

    /// Push one microphone chunk. Ignored unless a voice session is open.
    @discardableResult
    public func emitAudioChunk(bytes: Int = 320, format: AudioFormat = .opus) -> Bool {
        lock.lock()
        guard voiceOpen else { lock.unlock(); return false }
        let sequence = audioSeq
        audioSeq += 1
        lock.unlock()

        emit(.audioChunk(AudioChunk(
            data: Data(count: bytes),
            format: format,
            sequence: sequence,
            deviceTimeMs: clock.now()
        )))
        return true
    }

    public func emitTranscript(_ text: String) {
        emit(.transcriptText(text))
    }

    public func setCharging(_ value: Bool) {
        _ = settleBattery() // settle accumulated drain at the old rate first
        lock.lock()
        charging = value
        lock.unlock()
    }

    public func setFaults(_ faults: Faults) {
        lock.lock()
        options.faults = faults
        lock.unlock()
    }

    public func simulateDisconnect(reason: String = "mock: disconnected") {
        if state == .disconnected { return }
        lock.lock()
        voiceOpen = false
        previewing = false
        apOpen = false
        recordingSinceMs = nil
        lock.unlock()
        setState(.disconnected)
        emit(.error(GlassesError(.notConnected, reason)))
    }

    public var isAccessPointOpen: Bool {
        lock.lock(); defer { lock.unlock() }
        return apOpen
    }

    public var isPreviewing: Bool {
        lock.lock(); defer { lock.unlock() }
        return previewing
    }

    public var isVoiceSessionOpen: Bool {
        lock.lock(); defer { lock.unlock() }
        return voiceOpen
    }

    // MARK: - internals

    /// Synchronous critical section.
    ///
    /// Every mutation goes through this rather than calling `lock.lock()`
    /// inline, because `NSLock.lock()` is unavailable from an async context —
    /// a warning under Swift 5 and a hard error under Swift 6. The reason is
    /// real: holding a non-reentrant lock across a suspension point can park a
    /// cooperative-pool thread and deadlock. Keeping the lock inside a
    /// non-async function makes it impossible to hold one across an `await`.
    @inline(__always)
    private func locked<T>(_ body: () -> T) -> T {
        lock.lock()
        defer { lock.unlock() }
        return body()
    }

    private func readOptions() -> Options {
        lock.lock(); defer { lock.unlock() }
        return options
    }

    private func file(named name: String) -> RemoteFile? {
        lock.lock(); defer { lock.unlock() }
        return files.first { $0.name == name }
    }

    private func markUploaded(name: String) {
        lock.lock(); defer { lock.unlock() }
        guard let index = files.firstIndex(where: { $0.name == name }) else { return }
        let old = files[index]
        files[index] = RemoteFile(
            name: old.name,
            sizeBytes: old.sizeBytes,
            uploaded: true,
            durationS: old.durationS
        )
    }

    private func setState(_ newState: ConnectionState) {
        let changed = locked { () -> Bool in
            guard currentState != newState else { return false }
            currentState = newState
            return true
        }
        guard changed else { return }
        emit(.connectionChanged(newState))
    }

    private func requireConnected() throws {
        guard state == .connected else {
            throw GlassesError(.notConnected, "mock: not connected")
        }
    }

    private func emit(_ event: GlassesEvent) {
        lock.lock()
        let snapshot = Array(handlers.values)
        lock.unlock()
        // Handlers are called outside the lock: one of them re-entering the
        // transport (a `wear` handler that starts recording, which is the
        // realistic case) would otherwise deadlock.
        for handler in snapshot { handler(event) }
    }

    private func settleBattery() -> BatteryStatus {
        lock.lock()
        defer { lock.unlock() }
        let now = clock.now()
        let hours = Double(now - batteryAtMs) / 3_600_000
        let delta = charging
            ? hours * Defaults.batteryChargePerHour
            : -hours * Defaults.batteryDrainPerHour
        batteryPercent = max(0, min(100, batteryPercent + delta))
        batteryAtMs = now
        return BatteryStatus(percent: Int(batteryPercent.rounded()), charging: charging)
    }

    private func disk() -> DiskInfo {
        lock.lock()
        defer { lock.unlock() }
        var used = Double(usedStorageBytes)
        if let since = recordingSinceMs {
            let seconds = Double(clock.now() - since) / 1000
            used += seconds * Double(options.recordingBytesPerSecond)
        }
        let total = options.totalStorageBytes
        return DiskInfo(totalBytes: total, freeBytes: max(0, total - Int(used.rounded())))
    }

    /// Bytes shaped like a JPEG — correct SOI/EOI markers, filler between.
    public static func syntheticJPEG(_ totalBytes: Int) -> Data {
        let size = max(4, totalBytes)
        var bytes = [UInt8](repeating: 0, count: size)
        bytes[0] = 0xFF
        bytes[1] = 0xD8 // SOI
        if size > 4 {
            for i in 2 ..< (size - 2) { bytes[i] = UInt8(i & 0xFF) }
        }
        bytes[size - 2] = 0xFF
        bytes[size - 1] = 0xD9 // EOI
        return Data(bytes)
    }
}
