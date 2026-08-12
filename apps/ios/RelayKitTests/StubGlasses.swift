import Foundation
@testable import RelayKit

/// A transport with no clock in it.
///
/// ``MockTransport`` models rates — a photo takes as long over BLE as a photo
/// takes over BLE — which is exactly right when the thing under test is the UI's
/// relationship with latency, and exactly wrong when the thing under test is a
/// state machine like ``BulkSync``. There, every `await` would need a matching
/// `clock.advance`, and the test would be about the clock.
///
/// So this one answers immediately and records what it was asked. It is a
/// deliberately *different* kind of double from `MockTransport`, not a
/// replacement for it.
final class StubGlasses: GlassesTransport, @unchecked Sendable {

    private let lock = NSLock()
    private var handlers: [Int: @Sendable (GlassesEvent) -> Void] = [:]
    private var nextHandlerID = 0

    var currentState: ConnectionState = .connected
    var files: [RemoteFile] = []
    var battery = BatteryStatus(percent: 80, charging: true)
    var disk = DiskInfo(totalBytes: 4 * 1024 * 1024 * 1024, freeBytes: 3 * 1024 * 1024 * 1024)
    var features = Features()
    var accessPoint = WifiAccessPoint(ssid: "QCGlasses-STUB", password: "12345678", host: "192.168.31.1")

    /// Faults, one flag each so a test names what it is proving.
    var fetchFails = false
    var openApFails = false
    var startRecordingFails = false

    private(set) var calls: [String] = []
    private(set) var deletedNames: [String] = []
    private(set) var apOpen = false
    private(set) var recording = false
    private(set) var voiceOpen = false

    private func note(_ name: String) {
        lock.lock(); calls.append(name); lock.unlock()
    }

    var callLog: [String] {
        lock.lock(); defer { lock.unlock() }
        return calls
    }

    var state: ConnectionState { currentState }

    func connect(options: ConnectOptions) async throws {
        note("connect")
        currentState = .connected
        emit(.connectionChanged(.connected))
    }

    func disconnect() async {
        note("disconnect")
        currentState = .disconnected
        emit(.connectionChanged(.disconnected))
    }

    @discardableResult
    func on(_ handler: @escaping @Sendable (GlassesEvent) -> Void) -> Subscription {
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

    func emit(_ event: GlassesEvent) {
        lock.lock()
        let snapshot = Array(handlers.values)
        lock.unlock()
        for handler in snapshot { handler(event) }
    }

    func getFeatures() async throws -> Features { note("getFeatures"); return features }
    func getBattery() async throws -> BatteryStatus { note("getBattery"); return battery }
    func getDiskInfo() async throws -> DiskInfo { note("getDiskInfo"); return disk }
    func setTime(_ date: Date) async throws { note("setTime") }

    func startVoiceSession() async throws {
        note("startVoiceSession")
        voiceOpen = true
        emit(.voiceSessionChanged(true))
    }

    func stopVoiceSession() async throws {
        note("stopVoiceSession")
        voiceOpen = false
        emit(.voiceSessionChanged(false))
    }

    func capturePhoto() async throws -> RemoteFile {
        note("capturePhoto")
        return RemoteFile(name: "IMG_0001.jpg", sizeBytes: 250_000, uploaded: false)
    }

    func fetchThumbnail(name: String, options: ThumbnailOptions) async throws -> Photo {
        note("fetchThumbnail")
        return Photo(data: Data(count: 2_048))
    }

    func takePhoto(options: PhotoOptions) async throws -> Photo {
        note("takePhoto")
        return Photo(data: Data(count: 250_000))
    }

    func startLocalRecording() async throws {
        note("startLocalRecording")
        if startRecordingFails { throw GlassesError(.storageFull, "stub: storage full") }
        recording = true
        emit(.recordingState(RecordingState(recording: true, durationS: 0)))
    }

    func stopLocalRecording() async throws {
        note("stopLocalRecording")
        recording = false
        emit(.recordingState(RecordingState(recording: false, durationS: 0)))
    }

    func listFiles() async throws -> [RemoteFile] { note("listFiles"); return files }

    func deleteFile(name: String) async throws {
        note("deleteFile")
        lock.lock(); deletedNames.append(name); lock.unlock()
        files.removeAll { $0.name == name }
    }

    func openWifiAccessPoint() async throws -> WifiAccessPoint {
        note("openWifiAccessPoint")
        if openApFails { throw GlassesError(.unsupported, "stub: no access point") }
        apOpen = true
        return accessPoint
    }

    func closeWifiAccessPoint() async throws {
        note("closeWifiAccessPoint")
        apOpen = false
    }

    func fetchFile(name: String, onProgress: (@Sendable (FetchProgress) -> Void)?) async throws -> Data {
        note("fetchFile:\(name)")
        if fetchFails { throw GlassesError(.transferFailed, "stub: transfer failed") }
        let size = files.first { $0.name == name }?.sizeBytes ?? 0
        onProgress?(FetchProgress(receivedBytes: size, totalBytes: size))
        return Data(count: size)
    }

    func startPreview() async throws -> String {
        note("startPreview")
        return "rtsp://192.168.31.1:8554/live"
    }

    func stopPreview() async throws { note("stopPreview") }
}
