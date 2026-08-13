import SwiftUI
import RelayKit

/// Where you tell the phone about your box.
///
/// Two fields, because there are two facts and no way to discover either: the
/// address, and the token relayd prints when it starts. Nothing here is
/// negotiated — see `BoxSettings` for why this is not the pairing flow the
/// architecture describes, and `RelaydWebSocket` for why the token is a bearer
/// token rather than the signed proof `RelayKit` knows how to build.
///
/// Changing either field takes effect on the next launch. That is a real
/// limitation and it is stated on screen rather than hidden: the link is built
/// in the composition root and handed to a coordinator that holds it for its
/// lifetime, and threading a replacement through would be a larger change than
/// the one that made the link exist at all.
struct BoxView: View {
    @State private var address: String = BoxSettings.address ?? ""
    @State private var token: String = BoxSettings.token ?? ""
    @State private var saved = false

    private var resolved: URL? { BoxSettings.socketURL(from: address) }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("192.168.1.42:8080", text: $address)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .keyboardType(.URL)
                } header: {
                    Text("Your box")
                } footer: {
                    if address.isEmpty {
                        Text("The machine running relayd. A LAN address is fine; "
                             + "http:// is assumed if you leave the scheme off.")
                    } else if let resolved {
                        Text("Connects to \(resolved.absoluteString)")
                    } else {
                        Text("That does not look like an address.")
                            .foregroundStyle(.red)
                    }
                }

                Section {
                    SecureField("Token", text: $token)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                } header: {
                    Text("Token")
                } footer: {
                    Text("relayd prints this when it starts. It is kept in the "
                         + "keychain on this phone, never in the address.")
                }

                Section {
                    Button("Save") {
                        BoxSettings.address = address
                        BoxSettings.token = token
                        saved = true
                    }
                    .disabled(address.isEmpty || token.isEmpty || resolved == nil)

                    Button("Forget this box", role: .destructive) {
                        BoxSettings.clear()
                        address = ""
                        token = ""
                        saved = true
                    }
                    .disabled(!BoxSettings.isConfigured)
                } footer: {
                    if saved {
                        Text("Saved. Quit and reopen Relay for it to take effect.")
                    }
                }
            }
            .navigationTitle("Box")
        }
    }
}
