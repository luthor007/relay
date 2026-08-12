import Foundation

/// The declarative UI vocabulary, the phone's side.
///
/// `ORCHESTRATOR.md` §5's two sentences are the whole design, and they only look
/// like a contradiction:
///
/// - **App code runs on the server**, sandboxed, never on the phone. That is
///   what keeps the author from ever seeing your transcript.
/// - **App UI renders in the phone app**, through a small declarative vocabulary
///   the host draws natively.
///
/// This file is what makes the second true without breaking the first: a
/// `ui.render` frame is **data**, and nothing in it is executed. No script, no
/// URL, no view identifier, no layout — four block kinds, all text, every string
/// capped. An app cannot draw arbitrary pixels here, which is the trade
/// `APP-PLATFORM.md` §7 states outright.
///
/// ## Why this parses strictly rather than drawing what it recognises
///
/// A *confirmation* is one of the four kinds, so "draw the parts you understand"
/// is how a question reaches someone's screen with its buttons missing, or with
/// its card of context silently dropped. A view this build cannot draw whole is
/// refused whole and reported, which is also what the Android side does and what
/// the host does in the other direction.
///
/// ## This is the third copy, and it is not the authority
///
/// `apps/sdk/src/ui.ts` gives an app author their error while they are writing.
/// `relayd/internal/apps/ui.go` is the enforcement that counts and validates
/// every view before one reaches a wire. This copy exists because a renderer
/// needs typed data. What it catches is a *newer* daemon, a corrupted frame, and
/// drift between the three files — which is why the caps are repeated here
/// rather than assumed.
public enum AppView {

    /// The vocabulary version this build draws. Must equal the host's.
    public static let vocabulary = 1

    /// Every cap, mirroring `LIMITS` in the SDK and `ViewCaps` in relayd.
    public enum Caps {
        public static let blocks = 8
        public static let cardTitle = 120
        public static let cardBody = 2000
        public static let cardFields = 12
        public static let fieldLabel = 60
        public static let fieldValue = 240
        public static let listTitle = 120
        public static let listItems = 50
        public static let itemTitle = 120
        public static let itemSubtitle = 240
        public static let itemDetail = 60
        public static let question = 240
        public static let buttonLabel = 32
        public static let confirmDetail = 600
        public static let speakText = 1000
    }

    /// A view that will not render, with the reason a log should carry.
    public struct Malformed: Error, CustomStringConvertible, Equatable {
        public let reason: String
        public var description: String { reason }
    }

    /// One labelled value on a card.
    public struct Field: Equatable, Sendable {
        public let label: String
        public let value: String
    }

    /// One row of a list.
    public struct Item: Equatable, Sendable {
        public let title: String
        public let subtitle: String?
        public let detail: String?
    }

    /// One block.
    ///
    /// An enum rather than a struct with optional everything: this side is the
    /// renderer, and a `switch` over a closed enum is what makes "every kind has
    /// a drawing" a compile error rather than a blank space on someone's screen.
    public enum Block: Equatable, Sendable {
        case card(title: String, body: String?, fields: [Field])
        case rows(title: String?, items: [Item])
        case confirm(question: String, confirmLabel: String, cancelLabel: String, detail: String?)
        case speak(text: String)
    }

    /// A view with the app that drew it.
    ///
    /// `app` is not decoration. "Which of my apps is asking me this" is the first
    /// question a confirmation raises and the blocks cannot answer it, so a card
    /// drawn without attribution is one the user cannot act on.
    public struct Rendered: Equatable, Sendable {
        public let app: String
        public let appName: String?
        /// Non-null when an answer is expected.
        public let actionId: String?
        /// Unix milliseconds, or 0 when the question stands until retracted.
        public let deadlineMs: Int
        public let blocks: [Block]

        /// The confirmation, if this view asks something. At most one.
        public var question: Block? {
            blocks.first { if case .confirm = $0 { return true } else { return false } }
        }

        /// What something with no screen would read — for logs and VoiceOver.
        public func text() -> String {
            blocks.map { block in
                switch block {
                case let .card(title, body, fields):
                    return ([title] + [body].compactMap { $0 }
                        + fields.map { "\($0.label): \($0.value)" }).joined(separator: "\n")
                case let .rows(title, items):
                    return ([title].compactMap { $0 } + items.map { item in
                        var line = "- " + item.title
                        if let subtitle = item.subtitle { line += " — " + subtitle }
                        if let detail = item.detail { line += " (" + detail + ")" }
                        return line
                    }).joined(separator: "\n")
                case let .confirm(question, _, _, detail):
                    return ([question] + [detail].compactMap { $0 }).joined(separator: "\n")
                case let .speak(text):
                    return text
                }
            }.joined(separator: "\n")
        }
    }

    /// Parse a `ui.render` payload.
    public static func parse(_ payload: JSONValue) throws -> Rendered {
        guard let app = payload["app"]?.stringValue, !app.isEmpty else {
            throw Malformed(reason: "a view arrived with no app id")
        }
        guard let view = payload["view"], case .object = view else {
            throw Malformed(reason: "a ui.render frame with no view")
        }
        let declared = view["vocabulary"]?.intValue ?? -1
        guard declared == vocabulary else {
            throw Malformed(reason:
                "this view says vocabulary \(declared) and this build draws \(vocabulary). "
                + "Refusing it whole rather than drawing the parts it recognises: a confirmation "
                + "with a question and no buttons is worse than a screen that says the app needs "
                + "a newer Relay")
        }
        guard let raw = view["blocks"]?.arrayValue else {
            throw Malformed(reason: "a view with no blocks")
        }
        guard !raw.isEmpty else { throw Malformed(reason: "a view with no blocks renders nothing") }
        guard raw.count <= Caps.blocks else {
            throw Malformed(reason: "a view has \(raw.count) blocks; the limit is \(Caps.blocks)")
        }

        var blocks: [Block] = []
        for (i, entry) in raw.enumerated() {
            blocks.append(try parseBlock(entry, at: i))
        }
        let questions = blocks.filter { if case .confirm = $0 { return true } else { return false } }
        guard questions.count <= 1 else {
            throw Malformed(reason: "a view asks at most one question")
        }
        let speaks = blocks.filter { if case .speak = $0 { return true } else { return false } }
        guard speaks.count <= 1 else {
            throw Malformed(reason: "a view speaks at most once")
        }

        let actionId = payload["action_id"]?.stringValue.flatMap { $0.isEmpty ? nil : $0 }
        if actionId == nil, !questions.isEmpty {
            // A question with no id is one this phone could draw and never
            // answer, because the answer is keyed by the id.
            throw Malformed(reason: "a view asks a question and carries no action id to answer it with")
        }
        return Rendered(
            app: app,
            appName: payload["appName"]?.stringValue.flatMap { $0.isEmpty ? nil : $0 },
            actionId: actionId,
            deadlineMs: payload["deadline"]?.intValue ?? 0,
            blocks: blocks)
    }

    private static func parseBlock(_ entry: JSONValue, at i: Int) throws -> Block {
        switch entry["kind"]?.stringValue ?? "" {
        case "card":
            return .card(
                title: try text(entry, "title", "blocks[\(i)].title", Caps.cardTitle),
                body: try optional(entry, "body", "blocks[\(i)].body", Caps.cardBody),
                fields: try fields(entry, at: i))

        case "list":
            guard let rows = entry["items"]?.arrayValue else {
                throw Malformed(reason: "blocks[\(i)].items is missing")
            }
            guard !rows.isEmpty else { throw Malformed(reason: "blocks[\(i)].items is empty") }
            guard rows.count <= Caps.listItems else {
                throw Malformed(reason: "blocks[\(i)].items has \(rows.count); the limit is \(Caps.listItems)")
            }
            return .rows(
                title: try optional(entry, "title", "blocks[\(i)].title", Caps.listTitle),
                items: try rows.enumerated().map { j, row in
                    Item(
                        title: try text(row, "title", "blocks[\(i)].items[\(j)].title", Caps.itemTitle),
                        subtitle: try optional(row, "subtitle", "blocks[\(i)].items[\(j)].subtitle", Caps.itemSubtitle),
                        detail: try optional(row, "detail", "blocks[\(i)].items[\(j)].detail", Caps.itemDetail))
                })

        case "confirm":
            return .confirm(
                question: try text(entry, "question", "blocks[\(i)].question", Caps.question),
                // The defaults live here rather than on the wire, so an app that
                // sends nothing gets the platform's words and an empty label
                // cannot produce a button with no text on it.
                confirmLabel: try optional(entry, "confirmLabel", "blocks[\(i)].confirmLabel", Caps.buttonLabel) ?? "Yes",
                cancelLabel: try optional(entry, "cancelLabel", "blocks[\(i)].cancelLabel", Caps.buttonLabel) ?? "No",
                detail: try optional(entry, "detail", "blocks[\(i)].detail", Caps.confirmDetail))

        case "speak":
            return .speak(text: try text(entry, "text", "blocks[\(i)].text", Caps.speakText))

        case let kind:
            throw Malformed(reason:
                "blocks[\(i)].kind is \"\(kind)\"; this build draws card, list, confirm, speak")
        }
    }

    private static func fields(_ entry: JSONValue, at i: Int) throws -> [Field] {
        guard let raw = entry["fields"]?.arrayValue else { return [] }
        guard raw.count <= Caps.cardFields else {
            throw Malformed(reason: "blocks[\(i)].fields has \(raw.count); the limit is \(Caps.cardFields)")
        }
        return try raw.enumerated().map { j, f in
            Field(
                label: try text(f, "label", "blocks[\(i)].fields[\(j)].label", Caps.fieldLabel),
                value: try text(f, "value", "blocks[\(i)].fields[\(j)].value", Caps.fieldValue))
        }
    }

    private static func text(_ obj: JSONValue, _ key: String, _ where_: String, _ max: Int) throws -> String {
        guard let value = try optional(obj, key, where_, max) else {
            throw Malformed(reason: "\(where_) is required")
        }
        return value
    }

    /// One string check for every field.
    ///
    /// Control characters are refused rather than stripped, matching the host: a
    /// card is text a phone draws, not a terminal, and silently removing an
    /// escape sequence hides from everyone that one arrived. Tab and newline are
    /// allowed only in the fields the host allows them in, identified by their
    /// cap because the caps are the one thing the three copies agree on by
    /// construction.
    private static func optional(_ obj: JSONValue, _ key: String, _ where_: String, _ max: Int) throws -> String? {
        guard let value = obj[key]?.stringValue, !value.isEmpty else { return nil }
        guard !value.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            throw Malformed(reason: "\(where_) is blank")
        }
        // UTF-16 units, matching JavaScript's String.length, which is what the
        // caps were written against. Counting Characters would accept a view the
        // host refused; either way an emoji moves the boundary.
        guard value.utf16.count <= max else {
            throw Malformed(reason: "\(where_) is \(value.utf16.count) characters; the limit is \(max)")
        }
        let multiline = max == Caps.cardBody || max == Caps.confirmDetail || max == Caps.speakText
        for scalar in value.unicodeScalars {
            if scalar == "\n" || scalar == "\t" {
                if !multiline { throw Malformed(reason: "\(where_) contains a line break and is one line") }
                continue
            }
            if scalar.value < 0x20 || scalar.value == 0x7F {
                throw Malformed(reason: "\(where_) contains a control character; a view is text, not a terminal")
            }
        }
        return value
    }
}
