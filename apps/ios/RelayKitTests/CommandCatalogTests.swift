import Foundation
import XCTest
@testable import RelayKit

/// "Every glasses command by hand" (`docs/ORCHESTRATOR.md` §5) is a claim that
/// rots the moment it is only prose. So this re-parses `Transport.swift` — the
/// real file, carried into the test bundle as a resource — and fails if the
/// protocol has grown a method the UI cannot reach.
///
/// Same trick as `glasses/bridge/test/commands.test.ts`, which re-parses
/// `glasses/protocol/commands.py` on every run.
final class CommandCatalogTests: XCTestCase {

    /// Method names declared inside `protocol GlassesTransport`, read off the
    /// source rather than from a list someone has to remember to update.
    private func transportMethods() throws -> Set<String> {
        let url = try XCTUnwrap(
            Bundle(for: CommandCatalogTests.self).url(forResource: "Transport", withExtension: "swift"),
            "Transport.swift is not in the test bundle. It is carried there by the "
                + "`buildPhase: resources` entry for RelayKitTests in project.yml; without it "
                + "this test cannot check anything and must fail rather than pass."
        )
        let source = try String(contentsOf: url, encoding: .utf8)
        let lines = source.components(separatedBy: "\n")

        guard let start = lines.firstIndex(where: {
            $0.hasPrefix("public protocol GlassesTransport")
        }) else {
            XCTFail("could not find `public protocol GlassesTransport` in Transport.swift")
            return []
        }

        var methods: Set<String> = []
        for line in lines[(start + 1)...] {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            // The protocol body has no nested braces, so a bare `}` is its end.
            if trimmed == "}" { break }
            guard let funcRange = trimmed.range(of: "func ") else { continue }
            let rest = trimmed[funcRange.upperBound...]
            guard let paren = rest.firstIndex(of: "(") else { continue }
            methods.insert(String(rest[..<paren]).trimmingCharacters(in: .whitespaces))
        }
        return methods
    }

    func testTheCatalogCoversEveryTransportMethod() throws {
        let declared = try transportMethods()
        XCTAssertGreaterThan(declared.count, 15, "the parse found suspiciously few methods")

        let expected = declared.subtracting(GlassesCatalog.nonActionMethods)
        let missing = expected.subtracting(GlassesCatalog.coveredMethods)
        let extra = GlassesCatalog.coveredMethods.subtracting(expected)

        XCTAssertTrue(
            missing.isEmpty,
            "no tappable way to reach: \(missing.sorted().joined(separator: ", "))"
        )
        XCTAssertTrue(
            extra.isEmpty,
            "the catalog names methods the protocol does not have: "
                + extra.sorted().joined(separator: ", ")
        )
    }

    func testActionIdsAreUnique() {
        let ids = GlassesCatalog.actions.map(\.id)
        XCTAssertEqual(Set(ids).count, ids.count)
    }

    func testEveryActionIsInAGroupThatTheUiRenders() {
        let grouped = CommandGroup.allCases.flatMap { GlassesCatalog.actions(in: $0) }
        XCTAssertEqual(grouped.count, GlassesCatalog.actions.count)
    }

    func testTheMicrophoneActionsAreTheOnesFlaggedForConsent() {
        let flagged = Set(GlassesCatalog.actions.filter(\.opensMicrophone).map(\.id))
        XCTAssertEqual(
            flagged, ["startVoice", "startRecording"],
            "the consent gate is applied from this flag, so a wrong one here is a legal problem"
        )
    }

    func testDestructiveActionsAreFlagged() {
        let flagged = Set(GlassesCatalog.actions.filter(\.destructive).map(\.id))
        XCTAssertTrue(flagged.contains("deleteFile"))
        XCTAssertTrue(
            flagged.contains("openAp"),
            "joining the glasses' network costs the phone its uplink; that deserves a confirmation"
        )
        XCTAssertTrue(flagged.contains("startPreview"))
    }

    func testEveryActionThatNamesAProtocolIdUsesTheFourDigitHexForm() {
        for action in GlassesCatalog.actions {
            for id in action.protocolIds {
                XCTAssertTrue(
                    id.hasPrefix("0x") && id.count == 6,
                    "\(action.id) has a malformed protocol id \(id)"
                )
            }
        }
    }

    func testEveryActionActuallyRunsAgainstATransport() async throws {
        let glasses = StubGlasses()
        glasses.files = [RemoteFile(name: "REC_0001.opus", sizeBytes: 4_096, uploaded: true)]

        for action in GlassesCatalog.actions {
            let argument = action.argument == .fileName ? "REC_0001.opus" : nil
            do {
                let summary = try await action.run(glasses, argument)
                XCTAssertFalse(summary.isEmpty, "\(action.id) produced no evidence that it ran")
            } catch {
                XCTFail("\(action.id) threw against a healthy transport: \(error)")
            }
        }
    }

    func testDeletingAFileTheDeviceHasNotSyncedIsRefused() async {
        let glasses = StubGlasses()
        glasses.files = [RemoteFile(name: "REC_0002.opus", sizeBytes: 4_096, uploaded: false)]
        let action = GlassesCatalog.action(id: "deleteFile")

        do {
            _ = try await action?.run(glasses, "REC_0002.opus")
            XCTFail("deleted the only copy of an afternoon")
        } catch {
            XCTAssertTrue(glasses.deletedNames.isEmpty)
        }
    }
}
