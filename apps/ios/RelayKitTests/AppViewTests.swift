import XCTest
@testable import RelayKit

/// The vocabulary is defined three times and this is the third.
///
/// `apps/sdk/src/ui.ts`, `relayd/internal/apps/ui.go` and `AppView.swift`. The
/// Go and TypeScript copies are pinned to each other by a test that reads one
/// from the other; this one cannot be, because nothing here can read the
/// daemon's tree at build time. What it has instead is the same fixtures —
/// frames written the way relayd writes them — so drift shows up as a refusal
/// rather than as a card that silently loses a field.
///
/// **Never compiled.** `APPS-SCOPE.md`'s open row: there is no Xcode on the
/// machine this was written on, so this file is unverified prose in the same way
/// `SystemSpeechRecognizer` is. It is written to run, not known to.
final class AppViewTests: XCTestCase {

    private let card = #"{"vocabulary":1,"blocks":[{"kind":"card","title":"Standup"}]}"#

    private func decode(_ json: String) throws -> JSONValue {
        try JSONDecoder().decode(JSONValue.self, from: Data(json.utf8))
    }

    private func frame(_ view: String, extra: [String: JSONValue] = [:]) throws -> JSONValue {
        var fields: [String: JSONValue] = [
            "app": .string("dev.test.standup"),
            "appName": .string("Standup"),
            "view": try decode(view),
        ]
        for (key, value) in extra { fields[key] = value }
        return .object(fields)
    }

    func testACardArrivesWithTheAppThatDrewIt() throws {
        let view = try AppView.parse(try frame(card))
        XCTAssertEqual(view.app, "dev.test.standup")
        XCTAssertEqual(view.appName, "Standup")
        XCTAssertNil(view.actionId)
        guard case .card(let title, _, _) = view.blocks[0] else {
            return XCTFail("expected a card, got \(view.blocks[0])")
        }
        XCTAssertEqual(title, "Standup")
    }

    func testAnUnattributedViewIsRefused() throws {
        // "Which of my apps is asking me this" is the first question a card
        // raises and the blocks cannot answer it.
        let payload = JSONValue.object(["view": try decode(card)])
        XCTAssertThrowsError(try AppView.parse(payload))
    }

    func testAViewFromANewerDaemonIsRefusedWhole() throws {
        // Not partially drawn: a confirmation with a question and no buttons is
        // worse than a screen that says the app needs a newer Relay.
        let payload = try frame(#"{"vocabulary":2,"blocks":[{"kind":"card","title":"Standup"}]}"#)
        XCTAssertThrowsError(try AppView.parse(payload)) { error in
            XCTAssertTrue("\(error)".contains("vocabulary 2"), "\(error)")
        }
    }

    func testAnUnknownBlockRefusesTheViewRatherThanBeingSkipped() throws {
        let payload = try frame("""
        {"vocabulary":1,"blocks":[
          {"kind":"card","title":"Standup"},
          {"kind":"chart","title":"Burndown"}]}
        """)
        XCTAssertThrowsError(try AppView.parse(payload)) { error in
            XCTAssertTrue("\(error)".contains("chart"), "\(error)")
        }
    }

    func testAQuestionCarriesTheIdThatAnswersIt() throws {
        let payload = try frame("""
        {"vocabulary":1,"blocks":[
          {"kind":"card","title":"About to send"},
          {"kind":"confirm","question":"Send it?","detail":"Four lines."}]}
        """, extra: ["action_id": .string("act-1"), "deadline": .number(123_456)])
        let view = try AppView.parse(payload)
        XCTAssertEqual(view.actionId, "act-1")
        XCTAssertEqual(view.deadlineMs, 123_456)
        guard case .confirm(let question, let yes, let no, _)? = view.question else {
            return XCTFail("no question found")
        }
        XCTAssertEqual(question, "Send it?")
        // The platform's words when the app sends none, so a button is never blank.
        XCTAssertEqual(yes, "Yes")
        XCTAssertEqual(no, "No")
    }

    func testAQuestionWithNoIdIsRefusedBecauseNothingCouldAnswerIt() throws {
        let payload = try frame(#"{"vocabulary":1,"blocks":[{"kind":"confirm","question":"Send it?"}]}"#)
        XCTAssertThrowsError(try AppView.parse(payload)) { error in
            XCTAssertTrue("\(error)".contains("action id"), "\(error)")
        }
    }

    func testTwoQuestionsInOneViewAreRefused() throws {
        let payload = try frame("""
        {"vocabulary":1,"blocks":[
          {"kind":"confirm","question":"A?"},{"kind":"confirm","question":"B?"}]}
        """, extra: ["action_id": .string("act-1")])
        XCTAssertThrowsError(try AppView.parse(payload))
    }

    func testAControlCharacterIsRefusedRatherThanDrawn() throws {
        // Refused, not stripped: a card is text a phone draws, not a terminal,
        // and quietly removing an escape hides from everyone that one arrived.
        //
        // The title is assembled from the six characters that spell a JSON
        // unicode escape rather than written as a raw byte, because a real
        // control character in the source would not be valid JSON at all — the
        // decoder would throw before the check under test ever ran. The decoder
        // is what turns those six into the one ESC this is about.
        let title = "Stand" + #"\u001b"# + "[31mup"
        let payload = try frame(#"{"vocabulary":1,"blocks":[{"kind":"card","title":"\#(title)"}]}"#)
        XCTAssertThrowsError(try AppView.parse(payload)) { error in
            XCTAssertTrue("\(error)".contains("control character"), "\(error)")
        }
    }

    func testANewlineIsAllowedInABodyAndRefusedInATitle() throws {
        _ = try AppView.parse(
            try frame(#"{"vocabulary":1,"blocks":[{"kind":"card","title":"S","body":"one\ntwo"}]}"#))
        let bad = try frame(#"{"vocabulary":1,"blocks":[{"kind":"card","title":"one\ntwo"}]}"#)
        XCTAssertThrowsError(try AppView.parse(bad))
    }

    func testAStringPastItsCapIsRefused() throws {
        let long = String(repeating: "x", count: AppView.Caps.cardTitle + 1)
        let payload = try frame(#"{"vocabulary":1,"blocks":[{"kind":"card","title":"\#(long)"}]}"#)
        XCTAssertThrowsError(try AppView.parse(payload)) { error in
            XCTAssertTrue("\(error)".contains("the limit is \(AppView.Caps.cardTitle)"), "\(error)")
        }
    }

    func testLengthIsCountedInUTF16TheWayTheHostCountsIt() throws {
        // The SDK measures with String.length and relayd mirrors it. Counting
        // Characters here would accept a view the host refused, and the first
        // emoji in a title would move the boundary.
        let atCap = String(repeating: "🙂", count: AppView.Caps.cardTitle / 2)
        _ = try AppView.parse(
            try frame(#"{"vocabulary":1,"blocks":[{"kind":"card","title":"\#(atCap)"}]}"#))
        let over = String(repeating: "🙂", count: AppView.Caps.cardTitle / 2 + 1)
        XCTAssertThrowsError(
            try AppView.parse(
                try frame(#"{"vocabulary":1,"blocks":[{"kind":"card","title":"\#(over)"}]}"#)))
    }

    func testAnEmptyListIsRefused() throws {
        let payload = try frame(#"{"vocabulary":1,"blocks":[{"kind":"list","title":"Today","items":[]}]}"#)
        XCTAssertThrowsError(try AppView.parse(payload))
    }

    func testAListKeepsItsRowsInOrderWithTheirOptionalParts() throws {
        let payload = try frame("""
        {"vocabulary":1,"blocks":[{"kind":"list","title":"Today","items":[
          {"title":"Ship the fix","detail":"4pm"},
          {"title":"Call back","subtitle":"the supplier"}]}]}
        """)
        guard case .rows(let title, let items) = try AppView.parse(payload).blocks[0] else {
            return XCTFail("expected a list")
        }
        XCTAssertEqual(title, "Today")
        XCTAssertEqual(items.count, 2)
        XCTAssertEqual(items[0].detail, "4pm")
        XCTAssertNil(items[0].subtitle)
        XCTAssertEqual(items[1].subtitle, "the supplier")
    }

    func testTheTextProjectionIsWhatSomethingWithNoScreenReads() throws {
        let payload = try frame("""
        {"vocabulary":1,"blocks":[
          {"kind":"card","title":"Standup","fields":[{"label":"Blocked","value":"no"}]},
          {"kind":"list","items":[{"title":"Ship","detail":"4pm"}]}]}
        """)
        let text = try AppView.parse(payload).text()
        for want in ["Standup", "Blocked: no", "Ship", "4pm"] {
            XCTAssertTrue(text.contains(want), "missing \(want) in:\n\(text)")
        }
    }

    func testTheCapsMatchTheHost() {
        // Hand-checked against relayd/internal/apps/ui.go, which is itself
        // pinned to the SDK by a test that reads ui.ts. If one of these is wrong
        // the symptom is a card the host sent and this build refuses, so they
        // are written out rather than left to a reviewer's memory.
        XCTAssertEqual(AppView.vocabulary, 1)
        XCTAssertEqual(AppView.Caps.blocks, 8)
        XCTAssertEqual(AppView.Caps.cardTitle, 120)
        XCTAssertEqual(AppView.Caps.cardBody, 2000)
        XCTAssertEqual(AppView.Caps.cardFields, 12)
        XCTAssertEqual(AppView.Caps.fieldLabel, 60)
        XCTAssertEqual(AppView.Caps.fieldValue, 240)
        XCTAssertEqual(AppView.Caps.listTitle, 120)
        XCTAssertEqual(AppView.Caps.listItems, 50)
        XCTAssertEqual(AppView.Caps.itemTitle, 120)
        XCTAssertEqual(AppView.Caps.itemSubtitle, 240)
        XCTAssertEqual(AppView.Caps.itemDetail, 60)
        XCTAssertEqual(AppView.Caps.question, 240)
        XCTAssertEqual(AppView.Caps.buttonLabel, 32)
        XCTAssertEqual(AppView.Caps.confirmDetail, 600)
        XCTAssertEqual(AppView.Caps.speakText, 1000)
    }
}
