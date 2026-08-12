import Foundation

/// The transport contract.
///
/// One protocol, three implementations:
///
///   MockTransport    — this framework; no hardware, deterministic, fault-injecting
///   VendorTransport  — wraps QCSDK.framework (arm64, device only)
///   TraceTransport   — replays a recorded session
///
/// The whole product surface is written against this and nothing else, so it
/// builds and UI-tests with no glasses present. That matters concretely here:
/// the vendor frameworks are **arm64 device-only** (`lipo -info` on all five),
/// so linking them removes the Simulator as an option entirely. Keeping them
/// behind this protocol is what keeps `xcodebuild test` runnable on a laptop.
///
/// Method names are product-level, not protocol-level; the command IDs each maps
/// to are noted so the native adapter has an unambiguous target.
public protocol GlassesTransport: AnyObject, Sendable {

    var state: ConnectionState { get }

    func connect(options: ConnectOptions) async throws
    func disconnect() async

    /// Subscribe to every event. Returns a token; releasing it unsubscribes.
    @discardableResult
    func on(_ handler: @escaping @Sendable (GlassesEvent) -> Void) -> Subscription

    // MARK: device

    /// Protocol 0x0005. Call before anything else; firmware revisions differ.
    func getFeatures() async throws -> Features
    /// Protocol 0x0101.
    func getBattery() async throws -> BatteryStatus
    /// Protocol 0x0909 / 0x091C.
    func getDiskInfo() async throws -> DiskInfo
    /// Protocol 0x0903. Align the device clock before any capture.
    func setTime(_ date: Date) async throws

    // MARK: interactive voice (capture Path A)

    /// Open the live microphone stream; `.audioChunk` events follow until
    /// stopped. Expensive on both batteries — open on user intent, close
    /// immediately after. Protocol 0x0805.
    func startVoiceSession() async throws
    func stopVoiceSession() async throws

    // MARK: camera
    //
    // Three calls, because photos split the same way audio does: most are for
    // the memory and nobody is waiting on them, a few are answering a question
    // right now. Reaching for `takePhoto` by default is the mistake — it pays
    // tens of seconds of BLE transfer for an image that could have ridden the
    // nightly sync at full resolution for free.

    /// Capture at full resolution to the glasses' own storage. Returns as soon
    /// as the shutter fires; nothing transfers.
    func capturePhoto() async throws -> RemoteFile

    /// Pull a low-resolution preview of a stored photo over BLE.
    func fetchThumbnail(name: String, options: ThumbnailOptions) async throws -> Photo

    /// Capture and deliver in one call, chunked over BLE. Emits `.photoProgress`
    /// throughout — at a few KB/s this is seconds, not milliseconds, so callers
    /// must show progress rather than block. Protocol 0x0906 / 0x0907.
    func takePhoto(options: PhotoOptions) async throws -> Photo

    // MARK: passive capture (Path B)

    /// Record to the glasses' own storage. This is the all-day path: no
    /// sustained radio, and it survives the phone being out of range. 0x0E04.
    func startLocalRecording() async throws
    func stopLocalRecording() async throws
    /// Protocol 0x0E01.
    func listFiles() async throws -> [RemoteFile]
    /// Protocol 0x0E02. Check `uploaded` first.
    func deleteFile(name: String) async throws

    // MARK: bulk transfer

    /// Open the device's WiFi access point. The host must then join that
    /// network, which costs it its own WiFi uplink — so this is a deliberate,
    /// foregrounded operation. Protocol 0x090B.
    func openWifiAccessPoint() async throws -> WifiAccessPoint
    func closeWifiAccessPoint() async throws

    /// Pull a recording. Only viable over the WiFi AP: a day of audio is 173 MB
    /// to 1.8 GB, which over BLE would take longer than the day took to record.
    func fetchFile(name: String, onProgress: (@Sendable (FetchProgress) -> Void)?) async throws -> Data

    // MARK: video

    /// Start RTSP preview and resolve the stream URL. Protocol 0x090A → 0x0908.
    func startPreview() async throws -> String
    func stopPreview() async throws
}

public struct ConnectOptions: Sendable, Equatable {
    /// Opaque per-platform handle: a CoreBluetooth UUID on iOS, a MAC on Android.
    public var deviceId: String?
    /// Give up after this long.
    public var timeoutMs: Int?

    public init(deviceId: String? = nil, timeoutMs: Int? = nil) {
        self.deviceId = deviceId
        self.timeoutMs = timeoutMs
    }
}

public struct FetchProgress: Sendable, Equatable {
    public let receivedBytes: Int
    public let totalBytes: Int
}

/// Unsubscribes on deinit, so a forgotten token cannot leak a handler that
/// outlives the screen that registered it.
public final class Subscription: @unchecked Sendable {
    private let cancel: @Sendable () -> Void
    private var cancelled = false
    private let lock = NSLock()

    init(cancel: @escaping @Sendable () -> Void) {
        self.cancel = cancel
    }

    public func unsubscribe() {
        lock.lock()
        let alreadyCancelled = cancelled
        cancelled = true
        lock.unlock()
        guard !alreadyCancelled else { return }
        cancel()
    }

    deinit { unsubscribe() }
}

// Defaults, so call sites read the way the TypeScript ones do.
public extension GlassesTransport {
    func connect() async throws { try await connect(options: ConnectOptions()) }
    func takePhoto() async throws -> Photo { try await takePhoto(options: PhotoOptions()) }
    func fetchThumbnail(name: String) async throws -> Photo {
        try await fetchThumbnail(name: name, options: ThumbnailOptions())
    }
    func fetchFile(name: String) async throws -> Data {
        try await fetchFile(name: name, onProgress: nil)
    }
}
