import Foundation
import Security

/// Where the box is, and the token that opens it.
///
/// relayd prints a token when it starts. That token is the whole of the
/// phone's authority over the box — it carries the write scope, which is what
/// `POST /v1/sessions/{id}/turns` needs — so it goes in the keychain and not in
/// `UserDefaults`, which is a plist in the app container that any backup or
/// file-sharing route reads in the clear.
///
/// The address is not a secret and lives in `UserDefaults`, where it is easy to
/// see and change while this is being set up by hand.
///
/// This is deliberately not the pairing flow described in `SYSTEM.md`. There is
/// no PAKE here, no `DeviceCredential`, no box identity to verify — you read a
/// token off a terminal and type it into a phone on your own network. It is the
/// smallest thing that makes the link real, and it is honest about being that.
enum BoxSettings {

    private static let urlKey = "box.url"
    private static let service = "glass.relay.box"
    private static let account = "token"

    /// The base address, e.g. `http://192.168.1.42:8080` or a Tailscale name.
    /// Stored as typed; `socketURL` does the scheme and path work.
    static var address: String? {
        get { UserDefaults.standard.string(forKey: urlKey) }
        set {
            let trimmed = newValue?.trimmingCharacters(in: .whitespacesAndNewlines)
            if let trimmed, !trimmed.isEmpty {
                UserDefaults.standard.set(trimmed, forKey: urlKey)
            } else {
                UserDefaults.standard.removeObject(forKey: urlKey)
            }
        }
    }

    static var token: String? {
        get {
            let query: [String: Any] = [
                kSecClass as String: kSecClassGenericPassword,
                kSecAttrService as String: service,
                kSecAttrAccount as String: account,
                kSecReturnData as String: true,
                kSecMatchLimit as String: kSecMatchLimitOne,
            ]
            var item: CFTypeRef?
            guard SecItemCopyMatching(query as CFDictionary, &item) == errSecSuccess,
                  let data = item as? Data else { return nil }
            return String(data: data, encoding: .utf8)
        }
        set {
            let base: [String: Any] = [
                kSecClass as String: kSecClassGenericPassword,
                kSecAttrService as String: service,
                kSecAttrAccount as String: account,
            ]
            SecItemDelete(base as CFDictionary)

            guard let value = newValue?.trimmingCharacters(in: .whitespacesAndNewlines),
                  !value.isEmpty,
                  let data = value.data(using: .utf8) else { return }

            var add = base
            add[kSecValueData as String] = data
            // The link reconnects in the background when the glasses come back,
            // so the token has to be readable while the phone is locked. After
            // first unlock, not always: there is no reason for it to survive a
            // cold boot that nobody has unlocked.
            add[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlock
            SecItemAdd(add as CFDictionary, nil)
        }
    }

    /// The WebSocket URL for `GET /v1/ws`, or nil if the address is unusable.
    ///
    /// `http` becomes `ws` and `https` becomes `wss`. A bare host with no scheme
    /// is treated as `http`, because the thing being typed in is almost always a
    /// LAN address and asking someone to type a scheme is how the first attempt
    /// fails.
    static func socketURL(from address: String) -> URL? {
        var text = address.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return nil }
        if !text.contains("://") { text = "http://" + text }

        guard var parts = URLComponents(string: text), let scheme = parts.scheme else { return nil }
        switch scheme.lowercased() {
        case "http", "ws": parts.scheme = "ws"
        case "https", "wss": parts.scheme = "wss"
        default: return nil
        }
        guard parts.host?.isEmpty == false else { return nil }
        parts.path = "/v1/ws"
        parts.query = nil
        parts.fragment = nil
        return parts.url
    }

    static var isConfigured: Bool {
        guard let address, socketURL(from: address) != nil else { return false }
        return token?.isEmpty == false
    }

    static func clear() {
        address = nil
        token = nil
    }
}
