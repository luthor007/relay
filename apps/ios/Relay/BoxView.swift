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
    @State private var pairing: String = ""
    @State private var saved = false
    @State private var badLink = false

    private var resolved: URL? { BoxSettings.socketURL(from: address) }

    var body: some View {
        NavigationStack {
            Form {
                // The way this is meant to happen. `relay pair` prints one
                // link; a tapped link does this without the screen being
                // opened at all, and this is for a link that arrived somewhere
                // it cannot be tapped.
                Section {
                    TextField("relay://box-…:…@rz.relay.glass", text: $pairing)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                    Button("Use this link") {
                        if BoxSettings.apply(pairing: pairing) {
                            address = BoxSettings.address ?? ""
                            token = BoxSettings.token ?? ""
                            pairing = ""
                            saved = true
                            badLink = false
                        } else {
                            badLink = true
                        }
                    }
                    .disabled(pairing.isEmpty)
                } header: {
                    Text("Pairing link")
                } footer: {
                    if badLink {
                        Text("That is not a pairing link. `relay pair` on your box prints one.")
                            .foregroundStyle(.red)
                    } else if BoxSettings.isRelayed {
                        Text("Paired through the relay, so this works away from home.")
                    } else {
                        Text("Run `relay pair` on your box. The link carries the token — treat it "
                             + "like one.")
                    }
                }

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
                        // Typing an address by hand is the LAN route, and a box
                        // id left over from a relay pairing would send it to
                        // the relay instead.
                        BoxSettings.boxID = nil
                        saved = true
                    }
                    .disabled(address.isEmpty || token.isEmpty || resolved == nil)

                    Button("Forget this box", role: .destructive) {
                        BoxSettings.clear()
                        address = ""
                        token = ""
                        pairing = ""
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
