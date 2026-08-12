import Foundation
import Network
import NetworkExtension
import RelayKit

/// The phone's radios, for real.
///
/// This is the whole of ``SyncNetwork`` on iOS, and it is the file that cannot
/// be unit-tested: `NEHotspotConfigurationManager` needs an entitlement, a
/// device, and a hotspot to join. Everything that *decides* anything lives in
/// `RelayKit/BulkSync.swift`, which is why this is forty lines of translation
/// with `MockSyncNetwork` standing in for it in every test.
///
/// Symbols relied on, so a reviewer can check them against the SDK:
///
///   `NEHotspotConfiguration(ssid:passphrase:isWEP:)`      NetworkExtension, iOS 11+
///   `NEHotspotConfigurationManager.shared.apply(_:completionHandler:)`
///   `NEHotspotConfigurationManager.shared.removeConfiguration(forSSID:)`
///   `NEHotspotConfigurationError.alreadyAssociated` (raw value 13)
///   `NWConnection`, `NWEndpoint.Host/.Port`, `NWConnection.State`  Network, iOS 12+
///
/// **Two things worth knowing before this is trusted on hardware.**
///
/// 1. `apply` puts up a system dialog — "Relay wants to join QCGlasses-XXXX" —
///    that the user has to accept. There is no dialog in the background, which
///    is why `BulkSync` has a `needsForeground` deferral and the nightly
///    `BGProcessingTask` only runs the upload half.
/// 2. Joining costs the phone its WiFi uplink (`docs/ARCHITECTURE.md` §2.1).
///    iOS may also route traffic over cellular while associated with a network
///    that has no internet, which would silently defeat the reason bulk sync
///    waits for the LAN — so ``reachBox`` refuses to answer while the access
///    point is held, exactly as `MockSyncNetwork` does.
///
/// **Never compiled.** See the build note in `apps/ios/README.md`.
final class HotspotSyncNetwork: SyncNetwork, @unchecked Sendable {

    private let lock = NSLock()
    private var joinedSSID: String?
    private var boxHost: String?
    private var boxPort: UInt16

    init(boxHost: String? = nil, boxPort: UInt16 = 8787) {
        self.boxHost = boxHost
        self.boxPort = boxPort
    }

    func setBox(host: String?, port: UInt16) {
        lock.lock(); boxHost = host; boxPort = port; lock.unlock()
    }

    private var holdingAccessPoint: Bool {
        lock.lock(); defer { lock.unlock() }
        return joinedSSID != nil
    }

    func reachBox() async throws -> BoxReachability {
        if holdingAccessPoint {
            throw SyncNetworkError.uplinkHeldByAccessPoint(
                "the phone cannot reach the box while it holds the glasses' access point"
            )
        }
        lock.lock()
        let host = boxHost
        let port = boxPort
        lock.unlock()

        guard let host, let nwPort = Network.NWEndpoint.Port(rawValue: port) else { return .none }
        // A direct TCP connect, not a ping: what matters is whether *this* box
        // answers on the LAN, and "the internet is up" is a different question
        // with a different answer.
        return await Self.canConnect(host: host, port: nwPort) ? .lan : .relay
    }

    func joinAccessPoint(_ accessPoint: WifiAccessPoint) async throws {
        let configuration = NEHotspotConfiguration(
            ssid: accessPoint.ssid,
            passphrase: accessPoint.password,
            isWEP: false
        )
        // Otherwise iOS keeps the network in Settings forever and rejoins it in
        // the user's kitchen next week.
        configuration.joinOnce = true

        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            NEHotspotConfigurationManager.shared.apply(configuration) { error in
                if let error = error as NSError?,
                   error.code != NEHotspotConfigurationError.alreadyAssociated.rawValue {
                    continuation.resume(throwing: error)
                } else {
                    continuation.resume()
                }
            }
        }

        lock.lock(); joinedSSID = accessPoint.ssid; lock.unlock()
    }

    func leaveAccessPoint() async throws {
        lock.lock()
        let ssid = joinedSSID
        joinedSSID = nil
        lock.unlock()
        guard let ssid else { return }
        NEHotspotConfigurationManager.shared.removeConfiguration(forSSID: ssid)
    }

    private static func canConnect(
        host: String,
        port: Network.NWEndpoint.Port,
        timeoutMs: Int = 2_000
    ) async -> Bool {
        await withCheckedContinuation { (continuation: CheckedContinuation<Bool, Never>) in
            let connection = NWConnection(host: Network.NWEndpoint.Host(host), port: port, using: .tcp)
            let settled = NSLock()
            var finished = false

            func finish(_ value: Bool) {
                settled.lock()
                let alreadyFinished = finished
                finished = true
                settled.unlock()
                guard !alreadyFinished else { return }
                connection.cancel()
                continuation.resume(returning: value)
            }

            connection.stateUpdateHandler = { state in
                switch state {
                case .ready: finish(true)
                case .failed, .cancelled: finish(false)
                default: break
                }
            }
            connection.start(queue: .global())
            DispatchQueue.global().asyncAfter(deadline: .now() + .milliseconds(timeoutMs)) {
                finish(false)
            }
        }
    }
}
