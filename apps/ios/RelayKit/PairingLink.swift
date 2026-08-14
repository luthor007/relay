import Foundation

/// The one string that says where a box is and how to open it.
///
/// `relay://<box id>:<token>@<relay host>` — what `relay pair` prints on the
/// box, and what the app registers `relay://` for, so a link tapped in a
/// message or scanned from a QR code pairs the phone without anybody typing
/// forty random characters on a phone keyboard.
///
/// It lives in `RelayKit` rather than next to `BoxSettings` because it is the
/// half with rules: what parses, what does not, and what URL the rules produce.
/// `BoxSettings` is storage — a `UserDefaults` key and a keychain item — and
/// storage is not where a wire format should be decided.
public struct PairingLink: Equatable, Sendable {

    /// The relay's host, e.g. `rz.relay.glass`, with a port when there is one.
    public let relayHost: String
    /// The box's durable name at that relay.
    public let boxID: String
    /// The API token. It is a credential; the link is one too, by carrying it.
    public let token: String

    /// The subprotocol the box registered with the relay.
    ///
    /// One string with `relayd/cmd/relayd/relay.go`'s `ProtoPhone`. If these two
    /// ever disagree the relay joins nothing and both sides wait, which is the
    /// most expensive kind of disagreement to debug — hence the name here, in
    /// the file a reader of either end will find.
    public static let phoneProtocol = "phone.v1"

    public init(relayHost: String, boxID: String, token: String) {
        self.relayHost = relayHost
        self.boxID = boxID
        self.token = token
    }

    /// Parses a pairing link, or returns nil.
    ///
    /// Nil for anything that is not all three facts: a link missing the token
    /// would configure a phone that cannot authenticate, and it would do it
    /// silently, which is worse than refusing the paste.
    public init?(_ text: String) {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let parts = URLComponents(string: trimmed),
              parts.scheme?.lowercased() == "relay",
              let host = parts.host, !host.isEmpty,
              let user = parts.user, !user.isEmpty,
              let password = parts.password, !password.isEmpty
        else { return nil }

        var relay = host
        if let port = parts.port { relay += ":\(port)" }
        self.relayHost = relay
        self.boxID = user.removingPercentEncoding ?? user
        self.token = password.removingPercentEncoding ?? password
    }

    /// The socket to dial: `wss://<relay>/rz/v1/connect/<box>?p=phone.v1`.
    ///
    /// The path belongs to the relay, not to the daemon. What travels inside it
    /// is the daemon's protocol, frame for frame — the only difference from the
    /// LAN route is that the credential goes in the first frame, because the
    /// relay terminates the handshake a bearer header would have ridden on.
    public var socketURL: URL? {
        Self.socketURL(relayHost: relayHost, boxID: boxID)
    }

    public static func socketURL(relayHost: String, boxID: String) -> URL? {
        var text = relayHost.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty, !boxID.isEmpty else { return nil }
        if !text.contains("://") { text = "wss://" + text }
        guard var parts = URLComponents(string: text), let scheme = parts.scheme else { return nil }
        switch scheme.lowercased() {
        case "http", "ws": parts.scheme = "ws"
        case "https", "wss": parts.scheme = "wss"
        default: return nil
        }
        guard parts.host?.isEmpty == false else { return nil }
        let id = boxID.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? boxID
        parts.path = "/rz/v1/connect/" + id
        parts.queryItems = [URLQueryItem(name: "p", value: phoneProtocol)]
        parts.fragment = nil
        return parts.url
    }

    /// The opening frame a relayed socket must send before anything else.
    ///
    /// `api.Server.ServeRelayedSocket` reads exactly one frame, requires it to
    /// be this, and closes the socket otherwise — so an utterance that beat it
    /// to the wire would not be delivered late, it would end the connection.
    public func authFrame(id: String, at: Int) -> String? {
        Self.authFrame(token: token, id: id, at: at)
    }

    /// The same frame, for the socket, which holds a token and not a link.
    public static func authFrame(token: String, id: String, at: Int) -> String? {
        let envelope: [String: Any] = [
            "v": linkVersion,
            "id": id,
            "type": "auth",
            "at": at,
            "payload": ["token": token],
        ]
        guard let data = try? JSONSerialization.data(withJSONObject: envelope) else { return nil }
        return String(data: data, encoding: .utf8)
    }
}
