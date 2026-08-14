import Foundation
import RelayKit

/// The real socket to the box, over `URLSessionWebSocketTask`.
///
/// Until this file existed, `RelaydLink` had exactly one implementation of
/// `RelaydSocket` — `MockSocket`, in the test bundle. Every one of its
/// twenty-odd tests passed against it, and the app passed `link: nil` to the
/// coordinator, so every `link?.send(…)` in `CaptureCoordinator` — consent
/// decisions, utterances, touch, wear — compiled, ran, and did nothing. The
/// link layer was complete and unreachable.
///
/// # Authentication, and why it is a bearer token
///
/// `LinkAuth.header` builds `relay.auth.<deviceId>.<token>.<ts>.<nonce>.<hmac>`
/// and presents it as a WebSocket subprotocol. **relayd does not check it.**
/// `internal/api/server.go` guards `GET /v1/ws` with the same `guard(ScopeWrite,
/// …)` as every other route, and `internal/api/auth.go` reads one thing:
/// `Authorization: Bearer <token>`. There is no subprotocol parsing anywhere in
/// the daemon.
///
/// The Android client reached this conclusion first and wrote it down
/// (`relay-bridge/.../RelaydLink.kt`): *"a credential the server ignores reads
/// as security to whoever inspects the tree, which is the same failure as an
/// unwired consent policy."* So this sends what the server actually verifies,
/// and the two clients now authenticate the same way.
///
/// The HMAC path is left in `RelayKit` rather than deleted, because the design
/// it encodes — proof that expires, so a captured header cannot be replayed —
/// is better than a constant bearer token, and is worth having when relayd
/// grows a device registry. It is dead code today and should be read as a
/// proposal, not as protection.
///
/// The token still never goes in the URL: a query string ends up in proxy logs
/// and in `URLSession`'s own diagnostics.
final class RelaydWebSocket: NSObject, RelaydSocket, @unchecked Sendable {

    private let task: URLSessionWebSocketTask
    private let lock = NSLock()
    private var handlers: RelaydSocketHandlers?
    /// Set once, so a failure during the handshake and a later close do not
    /// both report a close to `RelaydLink` — it treats the second as a socket
    /// that closed while already closed and logs a state it never entered.
    private var finished = false

    /// The opening frame for a relayed socket, or nil on the LAN.
    ///
    /// The bearer header below reaches the box on a local network and reaches
    /// the *relay* through the rendezvous, which is a pipe that forwards bytes
    /// and not headers. So the credential becomes the first frame instead, and
    /// `api.Server.ServeRelayedSocket` refuses the socket without it.
    private let authFrame: String?

    init(url: URL, token: String, subprotocols: [String], authInBand: Bool = false) {
        // Built by RelayKit, where the shape is pinned by a test against the
        // frame `api.Server.ServeRelayedSocket` demands.
        authFrame = authInBand
            ? PairingLink.authFrame(token: token, id: UUID().uuidString,
                                    at: Int(Date().timeIntervalSince1970 * 1000))
            : nil

        var request = URLRequest(url: url)
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        if !subprotocols.isEmpty {
            request.setValue(subprotocols.joined(separator: ", "),
                             forHTTPHeaderField: "Sec-WebSocket-Protocol")
        }

        let config = URLSessionConfiguration.default
        // The phone sleeps, the box is on the same LAN, and a stalled socket
        // should surface as a close that triggers RelaydLink's backoff rather
        // than as a task that hangs until the system decides.
        config.timeoutIntervalForRequest = 30
        config.waitsForConnectivity = false

        let session = URLSession(configuration: config)
        self.task = session.webSocketTask(with: request)
        super.init()
    }

    func attach(_ handlers: RelaydSocketHandlers) {
        lock.lock()
        self.handlers = handlers
        lock.unlock()

        receive()
        task.resume()

        // URLSessionWebSocketTask has no "did open" callback without becoming
        // the session delegate, and the delegate route needs a retain cycle or
        // a box to break it. Sending a ping is the documented way to learn the
        // handshake completed: it cannot succeed before the socket is open.
        task.sendPing { [weak self] error in
            guard let self else { return }
            if let error {
                self.finish { $0.onClose?(1006, "handshake failed: \(error.localizedDescription)") }
            } else {
                // Before anything else, and before the link is told it is
                // open: the box refuses a relayed socket whose first frame is
                // not the credential, so an utterance that beat it to the wire
                // would close the socket rather than be delivered.
                if let frame = self.authFrame {
                    self.send(frame)
                }
                self.currentHandlers?.onOpen?()
            }
        }
    }

    func send(_ text: String) {
        task.send(.string(text)) { [weak self] error in
            guard let self, let error else { return }
            // A send that fails is a dead socket. Reporting it as a close is
            // what puts the envelope back at the head of the outbox instead of
            // dropping it: RelaydLink requeues in-flight work on close.
            self.finish { $0.onClose?(1006, "send failed: \(error.localizedDescription)") }
        }
    }

    func close(code: Int, reason: String) {
        let closeCode = URLSessionWebSocketTask.CloseCode(rawValue: code) ?? .normalClosure
        task.cancel(with: closeCode, reason: reason.data(using: .utf8))
        finish { $0.onClose?(code, reason) }
    }

    private func receive() {
        task.receive { [weak self] result in
            guard let self else { return }
            switch result {
            case .success(let message):
                switch message {
                case .string(let text):
                    self.currentHandlers?.onMessage?(text)
                case .data(let data):
                    // The protocol is JSON text. A binary frame is either a
                    // different protocol or a bug, and decoding it as UTF-8
                    // quietly would hide both.
                    if let text = String(data: data, encoding: .utf8) {
                        self.currentHandlers?.onMessage?(text)
                    }
                @unknown default:
                    break
                }
                self.receive()
            case .failure(let error):
                self.finish { $0.onClose?(1006, error.localizedDescription) }
            }
        }
    }

    private var currentHandlers: RelaydSocketHandlers? {
        lock.lock(); defer { lock.unlock() }
        return handlers
    }

    private func finish(_ body: (RelaydSocketHandlers) -> Void) {
        lock.lock()
        if finished {
            lock.unlock()
            return
        }
        finished = true
        let h = handlers
        lock.unlock()
        if let h { body(h) }
    }
}
