import Foundation
import XCTest
@testable import RelayKit

/// Cross-platform signing parity.
///
/// The vectors below were produced by `connector/src/protocol.ts`, which is the
/// source of truth, and the identical vectors are pinned in
/// `apps/android/relay-bridge/src/test/.../ConnectorSigningTest.kt`. Three
/// implementations sign these requests; one character of disagreement in the
/// payload construction makes every request from that platform fail
/// authentication with an error that says nothing about why.
final class ConnectorSigningTests: XCTestCase {

    private let signingKey = "relay-test-signing-key"
    private let body = "chunk-zero-payload"
    private let path = "/v1/sessions/abc/chunks"
    private let timestamp = 1_700_000_000_000
    private let bodyHash = "21a542316a0d20d4264d3846a4d94a96d7e477f51407b30338f9d1e5f02b0cd9"
    private let signature = "ccc92ba48607d71dd0ee2d2d0f1a542318e43a338126b3e3a6cde7e7085f674b"

    func testBodyHashMatchesTheReferenceImplementation() {
        XCTAssertEqual(ConnectorClient.sha256Hex(Data(body.utf8)), bodyHash)
    }

    func testSignatureMatchesTheReferenceImplementationByteForByte() {
        let produced = ConnectorClient.sign(
            signingKey: signingKey,
            method: "POST",
            path: path,
            bodyHash: bodyHash,
            timestampMs: timestamp
        )
        XCTAssertEqual(produced, signature)
    }

    func testMethodIsBoundIntoTheSignature() {
        let asGet = ConnectorClient.sign(
            signingKey: signingKey, method: "GET", path: path,
            bodyHash: bodyHash, timestampMs: timestamp
        )
        XCTAssertNotEqual(
            asGet, signature,
            "signing must bind the method, or an upload can be replayed as a different verb"
        )
    }

    func testPathIsBoundIntoTheSignature() {
        let elsewhere = ConnectorClient.sign(
            signingKey: signingKey, method: "POST", path: "/v1/sessions/other/chunks",
            bodyHash: bodyHash, timestampMs: timestamp
        )
        XCTAssertNotEqual(
            elsewhere, signature,
            "signing must bind the path, or a chunk can be replayed into another session"
        )
    }

    func testMethodIsUppercasedBeforeSigning() {
        let lowercased = ConnectorClient.sign(
            signingKey: signingKey, method: "post", path: path,
            bodyHash: bodyHash, timestampMs: timestamp
        )
        XCTAssertEqual(lowercased, signature)
    }

    func testHexEncodingIsLowercaseAndZeroPadded() {
        // A byte below 0x10 rendered as one nibble is the classic way two HMAC
        // implementations agree on the digest and disagree on the string.
        let hash = ConnectorClient.sha256Hex(Data())
        XCTAssertEqual(hash.count, 64)
        XCTAssertEqual(hash, hash.lowercased())
        XCTAssertTrue(hash.allSatisfy { $0.isHexDigit })
    }
}
