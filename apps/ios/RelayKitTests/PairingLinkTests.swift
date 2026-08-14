import XCTest
@testable import RelayKit

/// The link that makes the app work away from home.
///
/// Every rule here is one the box also implements — `relayd/internal/pairing`
/// writes the link and `api.Server.ServeRelayedSocket` reads the frame — so
/// these tests are one half of a pair, and the strings they pin are the
/// interface.
final class PairingLinkTests: XCTestCase {

    func testItCarriesAllThreeFacts() {
        let link = PairingLink("relay://box-abc:tok-123@rz.relay.glass")
        XCTAssertEqual(link?.boxID, "box-abc")
        XCTAssertEqual(link?.token, "tok-123")
        XCTAssertEqual(link?.relayHost, "rz.relay.glass")
    }

    /// A relay on a port is a self-hoster's relay, and dropping the port would
    /// send them to the wrong one — or to nothing.
    func testAPortIsKept() {
        let link = PairingLink("relay://box-abc:tok@relay.example:8443")
        XCTAssertEqual(link?.relayHost, "relay.example:8443")
        XCTAssertEqual(link?.socketURL?.absoluteString,
                       "wss://relay.example:8443/rz/v1/connect/box-abc?p=phone.v1")
    }

    /// Anything short of all three is refused, because the alternative is a
    /// phone configured to connect to something it cannot authenticate to, and
    /// finding that out later on cellular.
    func testIncompleteLinksAreRefused() {
        for text in [
            "relay://box-abc@rz.relay.glass",       // no token
            "relay://:tok@rz.relay.glass",          // no box
            "relay://box:tok@",                     // no relay
            "https://box:tok@rz.relay.glass",       // not a pairing link
            "box-abc:tok@rz.relay.glass",           // no scheme
            "",
        ] {
            XCTAssertNil(PairingLink(text), "accepted \(text)")
        }
    }

    /// The path is the relay's route and the query is the protocol label the
    /// box registered under. Both are pinned on the Go side; a disagreement
    /// here joins the phone to nothing and leaves both ends waiting.
    func testTheSocketURLIsTheRelaysRoute() {
        let link = PairingLink("relay://box-abc:tok@rz.relay.glass")
        XCTAssertEqual(link?.socketURL?.absoluteString,
                       "wss://rz.relay.glass/rz/v1/connect/box-abc?p=phone.v1")
        XCTAssertEqual(PairingLink.phoneProtocol, "phone.v1")
    }

    /// The credential goes in the first frame, because the relay terminates the
    /// handshake a bearer header would have ridden on.
    func testTheAuthFrameIsWhatTheBoxDemands() throws {
        let link = try XCTUnwrap(PairingLink("relay://box-abc:tok-123@rz.relay.glass"))
        let text = try XCTUnwrap(link.authFrame(id: "a1", at: 1_700_000_000_000))
        let json = try XCTUnwrap(
            try JSONSerialization.jsonObject(with: Data(text.utf8)) as? [String: Any])

        XCTAssertEqual(json["type"] as? String, "auth")
        XCTAssertEqual(json["v"] as? Int, linkVersion)
        XCTAssertEqual(json["id"] as? String, "a1")
        XCTAssertEqual((json["payload"] as? [String: Any])?["token"] as? String, "tok-123")
    }

    /// A percent-encoded token survives the round trip. `url.UserPassword` on
    /// the Go side escapes, and a phone that unescaped nothing would present a
    /// credential the box has never issued.
    func testAnEscapedTokenIsDecoded() {
        let link = PairingLink("relay://box-abc:a%2Fb%3Ac@rz.relay.glass")
        XCTAssertEqual(link?.token, "a/b:c")
    }
}
