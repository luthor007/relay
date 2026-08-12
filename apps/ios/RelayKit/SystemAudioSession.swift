#if canImport(AVFAudio)
import AVFAudio
import Foundation

/// `AVAudioSession`, and nothing else.
///
/// Every decision about *when* to hold a session lives in
/// ``AudioSessionPolicy``, which is pure and tested. This file only translates
/// ``AudioSessionConfiguration`` into the framework's constants and calls two
/// methods, so the untestable part is small enough to read in one sitting.
///
/// Symbols relied on, so a reviewer can check them against the SDK rather than
/// against my memory:
///
///   `AVAudioSession.sharedInstance()`
///   `setCategory(_:mode:options:)`                    — AVFAudio, iOS 10+
///   `setActive(_:options:)`                            — iOS 6+
///   `AVAudioSession.CategoryOptions.allowBluetooth`    — iOS 6+, renamed
///                                                        `.allowBluetoothHFP` in
///                                                        iOS 26; the old name is
///                                                        still what compiles
///                                                        against a 17.0 target
///   `.allowBluetoothA2DP`, `.duckOthers`               — iOS 10+
///   `.notifyOthersOnDeactivation`                      — iOS 6+
///   `AVAudioSession.interruptionNotification`          — with
///                                                        `AVAudioSessionInterruptionTypeKey`
///                                                        and
///                                                        `AVAudioSessionInterruptionOptionKey`
///   `AVAudioSession.mediaServicesWereResetNotification`
///
/// **Never compiled.** This file has not been through a Swift compiler — see the
/// build note in `apps/ios/README.md`.
public final class SystemAudioSession: AudioSessionControlling, @unchecked Sendable {

    private let session: AVAudioSession
    private let lock = NSLock()
    private var handlers: [Int: @Sendable (AudioInterruption) -> Void] = [:]
    private var nextHandlerID = 0
    private var observers: [NSObjectProtocol] = []
    private var current: AudioSessionConfiguration?
    private var active = false

    public init(session: AVAudioSession = .sharedInstance()) {
        self.session = session
        observeSystemNotifications()
    }

    deinit {
        for observer in observers {
            NotificationCenter.default.removeObserver(observer)
        }
    }

    public var isActive: Bool {
        lock.lock(); defer { lock.unlock() }
        return active
    }

    public var activeConfiguration: AudioSessionConfiguration? {
        lock.lock(); defer { lock.unlock() }
        return current
    }

    public func activate(_ configuration: AudioSessionConfiguration) throws {
        // Setting the category is idempotent but not free — it can tear down and
        // rebuild the route, which is audible as a click. Skip it when nothing
        // changed and we are already active.
        let unchanged: Bool = {
            lock.lock(); defer { lock.unlock() }
            return active && current == configuration
        }()
        if unchanged { return }

        do {
            try session.setCategory(
                Self.category(configuration.category),
                mode: Self.mode(configuration.mode),
                options: Self.options(configuration)
            )
            try session.setActive(true)
        } catch {
            throw AudioSessionError.activationFailed(String(describing: error))
        }

        lock.lock()
        active = true
        current = configuration
        lock.unlock()
    }

    public func deactivate() throws {
        do {
            // `.notifyOthersOnDeactivation` is what un-ducks the podcast we
            // interrupted. Without it the user's music stays quiet after the
            // reply ends, and they have no idea why.
            try session.setActive(false, options: [.notifyOthersOnDeactivation])
        } catch {
            throw AudioSessionError.deactivationFailed(String(describing: error))
        }
        lock.lock()
        active = false
        current = nil
        lock.unlock()
    }

    @discardableResult
    public func onInterruption(
        _ handler: @escaping @Sendable (AudioInterruption) -> Void
    ) -> Subscription {
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

    // MARK: - notifications

    private func observeSystemNotifications() {
        let center = NotificationCenter.default

        let interruption = center.addObserver(
            forName: AVAudioSession.interruptionNotification,
            object: session,
            queue: nil
        ) { [weak self] note in
            guard let self else { return }
            guard
                let raw = note.userInfo?[AVAudioSessionInterruptionTypeKey] as? UInt,
                let type = AVAudioSession.InterruptionType(rawValue: raw)
            else { return }

            switch type {
            case .began:
                self.markInactive()
                self.emit(.began)
            case .ended:
                let rawOptions = note.userInfo?[AVAudioSessionInterruptionOptionKey] as? UInt ?? 0
                let options = AVAudioSession.InterruptionOptions(rawValue: rawOptions)
                self.emit(.ended(shouldResume: options.contains(.shouldResume)))
            @unknown default:
                break
            }
        }

        let reset = center.addObserver(
            forName: AVAudioSession.mediaServicesWereResetNotification,
            object: session,
            queue: nil
        ) { [weak self] _ in
            guard let self else { return }
            self.markInactive()
            self.emit(.mediaServicesReset)
        }

        lock.lock()
        observers = [interruption, reset]
        lock.unlock()
    }

    private func markInactive() {
        lock.lock()
        active = false
        current = nil
        lock.unlock()
    }

    private func emit(_ interruption: AudioInterruption) {
        lock.lock()
        let snapshot = Array(handlers.values)
        lock.unlock()
        for handler in snapshot { handler(interruption) }
    }

    // MARK: - translation

    private static func category(_ value: AudioCategory) -> AVAudioSession.Category {
        switch value {
        case .playback: return .playback
        case .playAndRecord: return .playAndRecord
        }
    }

    private static func mode(_ value: AudioMode) -> AVAudioSession.Mode {
        switch value {
        case .spokenAudio: return .spokenAudio
        case .voiceChat: return .voiceChat
        case .default: return .default
        }
    }

    private static func options(
        _ configuration: AudioSessionConfiguration
    ) -> AVAudioSession.CategoryOptions {
        var options: AVAudioSession.CategoryOptions = []
        // Both Bluetooth options are documented as settable only on
        // `playAndRecord`; on `playback` A2DP is the default route anyway and
        // asking for it throws. That is a runtime error for a compile-time
        // mistake, so gate it here rather than at every call site.
        if configuration.category == .playAndRecord {
            if configuration.allowsBluetoothHandsFree { options.insert(.allowBluetooth) }
            if configuration.allowsBluetoothA2DP { options.insert(.allowBluetoothA2DP) }
        }
        if configuration.duckOthers {
            options.insert(.duckOthers)
        }
        return options
    }
}
#endif
