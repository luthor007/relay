import Foundation

/// Domain types for the glasses bridge.
///
/// A direct port of `glasses/bridge/src/types.ts`. The two are kept in step by
/// hand, which is a real cost — but the alternative is a code generator between
/// TypeScript and Swift for six enums, and that is a worse trade at this size.
/// When a case is added there, it is added here.

// MARK: - connection

public enum ConnectionState: String, Sendable {
    case disconnected
    case connecting
    case connected
    case reconnecting
}

// MARK: - input

/// Mirrors `QCDeviceTouchAction`. Wear and remove arrive on the same channel as
/// taps, which is a vendor quirk — the transport re-emits them as `.wear` too,
/// because capture gating cares about them and nothing else does.
public enum TouchAction: String, Sendable {
    case wear
    case remove
    case forward
    case backward
    case longPress
    case singleTap
    case doubleTap
    case tripleTap
}

// MARK: - audio

public enum AudioFormat: String, Sendable {
    /// What the device streams natively; do not transcode without a reason.
    case opus
    /// 16-bit signed little-endian.
    case pcm16
}

public struct AudioChunk: Sendable, Equatable {
    public let data: Data
    public let format: AudioFormat
    /// 16000 on this hardware.
    public let sampleRate: Int
    public let channels: Int
    /// Monotonic per voice session; gaps mean dropped chunks.
    public let sequence: Int
    /// Device clock, milliseconds. Aligned by `setTime` (protocol 0x0903).
    public let deviceTimeMs: Int

    public init(
        data: Data,
        format: AudioFormat = .opus,
        sampleRate: Int = 16_000,
        channels: Int = 1,
        sequence: Int,
        deviceTimeMs: Int
    ) {
        self.data = data
        self.format = format
        self.sampleRate = sampleRate
        self.channels = channels
        self.sequence = sequence
        self.deviceTimeMs = deviceTimeMs
    }
}

// MARK: - camera

public struct Photo: Sendable, Equatable {
    public let data: Data
    public let mimeType: String
    public let widthPx: Int?
    public let heightPx: Int?

    public init(data: Data, mimeType: String = "image/jpeg", widthPx: Int? = nil, heightPx: Int? = nil) {
        self.data = data
        self.mimeType = mimeType
        self.widthPx = widthPx
        self.heightPx = heightPx
    }
}

public struct PhotoProgress: Sendable, Equatable {
    public let receivedBytes: Int
    /// Nil until the device reports a total.
    public let totalBytes: Int?
    public let chunkIndex: Int
    public let chunkCount: Int?
}

public struct PhotoOptions: Sendable, Equatable {
    /// Requested pixel dimensions. This is a latency dial, not a quality
    /// setting: photos arrive over BLE at a few KB/s, so a full-size JPEG takes
    /// tens of seconds. See docs/ARCHITECTURE.md §5.2.
    public var maxWidth: Int?
    public var maxHeight: Int?

    public init(maxWidth: Int? = nil, maxHeight: Int? = nil) {
        self.maxWidth = maxWidth
        self.maxHeight = maxHeight
    }
}

public struct ThumbnailOptions: Sendable, Equatable {
    /// Vendor thumbnail quality, 0-6. Their note: "the higher the number, the
    /// clearer the image, but the slower the transmission speed."
    public var clarity: Int?

    public init(clarity: Int? = nil) {
        self.clarity = clarity
    }
}

// MARK: - device state

public struct BatteryStatus: Sendable, Equatable {
    /// 0-100.
    public let percent: Int
    public let charging: Bool

    public init(percent: Int, charging: Bool) {
        self.percent = percent
        self.charging = charging
    }
}

public struct DiskInfo: Sendable, Equatable {
    public let totalBytes: Int
    public let freeBytes: Int
}

public struct RecordingState: Sendable, Equatable {
    public let recording: Bool
    /// Seconds elapsed in the current recording, 0 when stopped.
    public let durationS: Int
}

public struct RemoteFile: Sendable, Equatable {
    public let name: String
    public let sizeBytes: Int
    /// Whether the device considers this file already pulled. The firmware
    /// tracks it — protocol 0x0911 is "clear un-uploaded files" — so never
    /// delete on the basis of our own bookkeeping alone.
    public let uploaded: Bool
    public let durationS: Int?

    public init(name: String, sizeBytes: Int, uploaded: Bool, durationS: Int? = nil) {
        self.name = name
        self.sizeBytes = sizeBytes
        self.uploaded = uploaded
        self.durationS = durationS
    }
}

public struct WifiAccessPoint: Sendable, Equatable {
    public let ssid: String
    public let password: String
    /// 192.168.31.1 on this hardware.
    public let host: String
}

/// Capability bitmap from protocol 0x0005 获取支持功能. Query before issuing
/// anything else — firmware revisions differ in what they honour.
public struct Features: Sendable, Equatable {
    public var localRecording: Bool
    public var wifiAp: Bool
    public var wifiP2p: Bool
    public var livePreview: Bool
    public var voiceWakeup: Bool
    public var wearDetection: Bool
    public var stabilization: Bool
    /// Anything the adapter did not recognise, by raw bit index.
    public var unknownBits: [Int]

    public init(
        localRecording: Bool = true,
        wifiAp: Bool = true,
        wifiP2p: Bool = true,
        livePreview: Bool = true,
        voiceWakeup: Bool = true,
        wearDetection: Bool = true,
        stabilization: Bool = true,
        unknownBits: [Int] = []
    ) {
        self.localRecording = localRecording
        self.wifiAp = wifiAp
        self.wifiP2p = wifiP2p
        self.livePreview = livePreview
        self.voiceWakeup = voiceWakeup
        self.wearDetection = wearDetection
        self.stabilization = stabilization
        self.unknownBits = unknownBits
    }
}

// MARK: - errors

public enum GlassesErrorCode: String, Sendable {
    case notConnected
    case timeout
    case deviceBusy
    case unsupported
    case transferFailed
    case lowBattery
    case storageFull
    case connectFailed
}

public struct GlassesError: Error, Sendable, Equatable {
    public let code: GlassesErrorCode
    public let message: String

    public init(_ code: GlassesErrorCode, _ message: String) {
        self.code = code
        self.message = message
    }
}

// MARK: - events

/// The event vocabulary, as one enum rather than TypeScript's name→payload map.
///
/// Swift has no structural equivalent of an indexed interface, and the
/// alternative — a separate closure registry per event name — trades one
/// exhaustive `switch` for eight places to forget to unsubscribe.
public enum GlassesEvent: Sendable, Equatable {
    case connectionChanged(ConnectionState)
    case battery(BatteryStatus)
    case touch(TouchAction)
    /// Derived from `.wear`/`.remove` touch actions; true when they go on.
    case wear(Bool)
    /// Live microphone, only between `startVoiceSession` and `stopVoiceSession`.
    case audioChunk(AudioChunk)
    case voiceSessionChanged(Bool)
    /// A transcript for the turn that just happened.
    ///
    /// **Not** device-side recognition: `0x0803`/`0x0805` are the device saying
    /// *something was said*, and the glasses have no ASR of their own
    /// (`glasses/bridge/src/commands.ts`, and `docs/SYSTEM.md` §7b). The text
    /// comes from the app's recogniser — see ``SpeechRecognizing`` — and is
    /// re-emitted here so screens have one event stream rather than two.
    case transcriptText(String)
    case photoProgress(PhotoProgress)
    case photo(Photo)
    case recordingState(RecordingState)
    case diskInfo(DiskInfo)
    case rtspUrl(String)
    case error(GlassesError)
}
