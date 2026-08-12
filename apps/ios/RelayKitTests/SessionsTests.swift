import Foundation
import XCTest
@testable import RelayKit

private func envelope(_ type: String, _ payload: JSONValue) -> RelaydEnvelope {
    RelaydEnvelope(id: UUID().uuidString, type: type, at: 1_700_000_000_000, payload: payload)
}

final class SessionDirectoryTests: XCTestCase {

    private func listEnvelope() -> RelaydEnvelope {
        envelope(ServerMessage.sessionList.rawValue, .object([
            "sessions": .array([
                .object([
                    "id": .string("s-1"),
                    "title": .string("fix the fixture drift"),
                    "runtime": .string("claude-code"),
                    "state": .string("running"),
                    "startedAtMs": .number(1_700_000_000_000),
                    "lastLine": .string("running tests…"),
                ]),
                .object([
                    "id": .string("s-2"),
                    "title": .string("morning digest"),
                    "runtime": .string("codex"),
                    "state": .string("waiting"),
                    "startedAtMs": .number(1_700_000_001_000),
                ]),
            ]),
        ]))
    }

    func testASessionListDecodes() {
        let directory = SessionDirectory()
        directory.apply(listEnvelope())

        XCTAssertEqual(directory.all.map(\.id), ["s-1", "s-2"])
        XCTAssertEqual(directory.all.first?.runtime, "claude-code")
        XCTAssertEqual(directory.all.first?.state, .running)
        XCTAssertEqual(directory.all.last?.state, .waiting)
        XCTAssertNil(directory.all.last?.lastLine)
    }

    func testAnUnknownStateDoesNotDropTheSession() {
        let directory = SessionDirectory()
        directory.apply(envelope("session.list", .object([
            "sessions": .array([.object([
                "id": .string("s-9"),
                "state": .string("hibernating"),
            ])]),
        ])))

        XCTAssertEqual(directory.all.count, 1, "a session we cannot render is still one to stop")
        XCTAssertEqual(directory.all.first?.state, .idle)
    }

    func testASessionWithNoIdIsSkipped() {
        let directory = SessionDirectory()
        directory.apply(envelope("session.list", .object([
            "sessions": .array([.object(["title": .string("nameless")])]),
        ])))
        XCTAssertTrue(directory.all.isEmpty)
    }

    func testAttachIsLocalIntentAndProducesASessionCommand() {
        let directory = SessionDirectory()
        directory.apply(listEnvelope())

        let payload = directory.attach("s-2")

        XCTAssertEqual(directory.attachedId, "s-2")
        XCTAssertEqual(directory.attachedSession?.title, "morning digest")
        XCTAssertEqual(payload["verb"]?.stringValue, "attach")
        XCTAssertEqual(payload["sessionId"]?.stringValue, "s-2")
    }

    func testASessionThatDisappearsCannotStayAttached() {
        let directory = SessionDirectory()
        directory.apply(listEnvelope())
        _ = directory.attach("s-2")

        directory.apply(envelope("session.list", .object([
            "sessions": .array([.object(["id": .string("s-1"), "state": .string("running")])]),
        ])))

        XCTAssertNil(
            directory.attachedId,
            "otherwise the next utterance is spoken into a session that is gone"
        )
    }

    func testDetachOnNothingIsANoOp() {
        let directory = SessionDirectory()
        XCTAssertNil(directory.detach())
    }

    func testTheCommandVocabularyStaysInsideSystemMd() {
        // §6.1 gives the phone eight message types and `confirm.response` is not
        // one of them. Every verb rides `session.command`.
        for verb in SessionVerb.allCases {
            let payload = SessionDirectory.command(verb, sessionId: "s-1")
            XCTAssertEqual(payload["verb"]?.stringValue, verb.rawValue)
        }
    }
}

final class ApprovalInboxTests: XCTestCase {

    private func request(_ id: String, risk: String? = "high") -> RelaydEnvelope {
        var fields: [String: JSONValue] = [
            "requestId": .string(id),
            "sessionId": .string("s-1"),
            "summary": .string("Delete 12 files in ~/Downloads"),
            "detail": .string("rm -rf ~/Downloads/*.dmg"),
        ]
        if let risk { fields["risk"] = .string(risk) }
        return envelope(ServerMessage.confirmRequest.rawValue, .object(fields))
    }

    func testAConfirmRequestDecodesWithItsExactCommand() {
        let inbox = ApprovalInbox()
        let decoded = inbox.apply(request("r-1"))

        XCTAssertEqual(decoded?.id, "r-1")
        XCTAssertEqual(
            decoded?.detail, "rm -rf ~/Downloads/*.dmg",
            "an approval screen that paraphrases a command is an approval for something else"
        )
        XCTAssertEqual(inbox.pending.count, 1)
    }

    func testAnUnknownRiskIsTreatedAsHighNotAsTheDefault() {
        let inbox = ApprovalInbox()
        let decoded = inbox.apply(request("r-2", risk: "spicy"))
        XCTAssertEqual(
            decoded?.risk, .high,
            "guessing low on something we do not understand is the wrong way round"
        )
    }

    func testTheSameRequestArrivingTwiceIsOneRow() {
        let inbox = ApprovalInbox()
        inbox.apply(request("r-1"))
        inbox.apply(request("r-1"))
        XCTAssertEqual(inbox.pending.count, 1, "relayd redelivers on reconnect")
    }

    func testAnsweringRemovesItAndProducesASessionCommand() {
        let inbox = ApprovalInbox()
        inbox.apply(request("r-1"))

        let payload = inbox.answer("r-1", .deny)

        XCTAssertEqual(payload?["verb"]?.stringValue, "confirm")
        XCTAssertEqual(payload?["requestId"]?.stringValue, "r-1")
        XCTAssertEqual(payload?["decision"]?.stringValue, "deny")
        XCTAssertEqual(payload?["sessionId"]?.stringValue, "s-1")
        XCTAssertTrue(inbox.pending.isEmpty)
    }

    func testAnsweringTwiceIsHarmless() {
        let inbox = ApprovalInbox()
        inbox.apply(request("r-1"))
        XCTAssertNotNil(inbox.answer("r-1", .approve))
        XCTAssertNil(inbox.answer("r-1", .approve), "a double tap is not an error")
    }

    func testARetractionTakesTheQuestionDown() {
        // `confirm.resolved`: the approval was answered in a terminal, or the
        // turn was cancelled. A ping that outlives its question wakes someone to
        // approve what is already approved.
        let inbox = ApprovalInbox()
        inbox.apply(request("r-1"))
        inbox.apply(request("r-2"))

        XCTAssertEqual(inbox.retract("r-1")?.id, "r-1")
        XCTAssertEqual(inbox.pending.map(\.id), ["r-2"])
    }

    func testRetractingSomethingUnknownIsSilent() {
        let inbox = ApprovalInbox()
        inbox.apply(request("r-1"))
        XCTAssertNil(
            inbox.retract("r-9"),
            "a resolution can outrun its request across a reconnect; that is not an error"
        )
        XCTAssertEqual(inbox.pending.count, 1)
    }

    func testARetractionIsNotAnAnswer() {
        // Retracting must not send a decision: the box already has one, and a
        // second `deny` arriving after an `allow` is how something gets refused
        // twice.
        let inbox = ApprovalInbox()
        inbox.apply(request("r-1"))
        inbox.retract("r-1")
        XCTAssertNil(inbox.answer("r-1", .approve))
    }

    func testExpiryIsReportedRatherThanActedOn() {
        let inbox = ApprovalInbox()
        inbox.apply(envelope("confirm.request", .object([
            "requestId": .string("r-3"),
            "sessionId": .string("s-1"),
            "summary": .string("push to main"),
            "expiresAtMs": .number(1_000),
        ])))

        XCTAssertEqual(inbox.expired(now: 2_000).map(\.id), ["r-3"])
        XCTAssertEqual(
            inbox.pending.count, 1,
            "an approval that vanishes while it is being read teaches people to tap fast"
        )
    }
}
