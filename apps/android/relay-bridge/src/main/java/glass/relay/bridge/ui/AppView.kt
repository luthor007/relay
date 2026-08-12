package glass.relay.bridge.ui

import org.json.JSONObject

/**
 * The declarative UI vocabulary, the phone's side.
 *
 * `ORCHESTRATOR.md` §5's two sentences are the whole design, and they only look
 * like a contradiction:
 *
 *  - **App code runs on the server**, sandboxed, never on the phone. That is
 *    what keeps the author from ever seeing your transcript.
 *  - **App UI renders in the phone app**, through a small declarative
 *    vocabulary the host draws natively.
 *
 * This file is what makes the second one true without breaking the first: a
 * `ui.render` frame is **data**, and nothing in it is executed. There is no
 * script, no URL, no view id, no layout — four block kinds, all text, every
 * string capped. An app cannot draw arbitrary pixels here and that is the
 * trade `APP-PLATFORM.md` §7 states outright.
 *
 * ## Why this parses strictly rather than rendering what it can
 *
 * The temptation with a wire format is to draw the parts you understand. It is
 * the wrong call for this one. A view from a newer daemon may contain a block
 * kind this build has no renderer for, and a *confirmation* is one of the four
 * — so "draw what you recognise" is how a question reaches someone's screen
 * with its buttons missing, or with a card of context silently dropped and only
 * the question left. Refusing the view whole and saying so is the honest
 * failure, and it is the same rule the host applies in the other direction.
 *
 * ## This is a mirror, and the other two copies are the authority
 *
 * `apps/sdk/src/ui.ts` gives an app author their error while they are writing.
 * `relayd/internal/apps/ui.go` is the enforcement that counts — it validates
 * every view before one is ever put on the wire. This copy exists because a
 * renderer needs typed data, not because the phone is a checkpoint: a view that
 * got here already passed the host. What this catches is a *newer* daemon, a
 * corrupted frame, and drift between the three files, which is why the caps are
 * repeated rather than assumed.
 */
object AppView {

    /** The vocabulary version this build draws. Must equal the host's. */
    const val VOCABULARY = 1

    /** Every cap, mirroring `LIMITS` in the SDK and `ViewCaps` in relayd. */
    object Caps {
        const val BLOCKS = 8
        const val CARD_TITLE = 120
        const val CARD_BODY = 2000
        const val CARD_FIELDS = 12
        const val FIELD_LABEL = 60
        const val FIELD_VALUE = 240
        const val LIST_TITLE = 120
        const val LIST_ITEMS = 50
        const val ITEM_TITLE = 120
        const val ITEM_SUBTITLE = 240
        const val ITEM_DETAIL = 60
        const val QUESTION = 240
        const val BUTTON_LABEL = 32
        const val CONFIRM_DETAIL = 600
        const val SPEAK_TEXT = 1000
    }

    /** A view that will not render, with the reason a log should carry. */
    class Malformed(message: String) : IllegalArgumentException(message)

    /** One labelled value on a card. */
    data class Field(val label: String, val value: String)

    /** One row of a list. */
    data class Item(val title: String, val subtitle: String? = null, val detail: String? = null)

    /**
     * One block.
     *
     * A sealed hierarchy rather than one struct with nullable everything: this
     * side is the renderer, and a `when` over a sealed type is what makes
     * "every kind has a drawing" a compile error rather than a blank space.
     */
    sealed interface Block {
        data class Card(
            val title: String,
            val body: String? = null,
            val fields: List<Field> = emptyList(),
        ) : Block

        data class Rows(val title: String?, val items: List<Item>) : Block

        data class Confirm(
            val question: String,
            val confirmLabel: String,
            val cancelLabel: String,
            val detail: String? = null,
        ) : Block

        data class Speak(val text: String) : Block
    }

    /**
     * A view with the app that drew it.
     *
     * `app` is not decoration. "Which of my apps is asking me this" is the first
     * question a confirmation raises and the blocks cannot answer it, so a card
     * drawn without attribution is one the user cannot act on.
     */
    data class Rendered(
        val app: String,
        val appName: String?,
        /** Non-null when an answer is expected — see [Block.Confirm]. */
        val actionId: String?,
        /** Unix milliseconds, or 0 when the question stands until retracted. */
        val deadlineMs: Long,
        val blocks: List<Block>,
    ) {
        /** The confirmation, if this view asks something. At most one. */
        val question: Block.Confirm? get() = blocks.filterIsInstance<Block.Confirm>().firstOrNull()

        /** What something with no screen would read out — for logs and for TalkBack. */
        fun text(): String = blocks.joinToString("\n") { block ->
            when (block) {
                is Block.Card -> (listOf(block.title) + listOfNotNull(block.body) +
                    block.fields.map { "${it.label}: ${it.value}" }).joinToString("\n")
                is Block.Rows -> (listOfNotNull(block.title) + block.items.map { item ->
                    buildString {
                        append("- ").append(item.title)
                        item.subtitle?.let { append(" — ").append(it) }
                        item.detail?.let { append(" (").append(it).append(")") }
                    }
                }).joinToString("\n")
                is Block.Confirm -> (listOf(block.question) + listOfNotNull(block.detail)).joinToString("\n")
                is Block.Speak -> block.text
            }
        }
    }

    /**
     * Parse a `ui.render` payload.
     *
     * @throws Malformed when the frame is not a view this build can draw whole.
     */
    fun parse(payload: JSONObject?): Rendered {
        if (payload == null) throw Malformed("a ui.render frame with no payload")
        val app = payload.optString("app")
        if (app.isEmpty()) {
            // Unattributed, so refused: see [Rendered.app].
            throw Malformed("a view arrived with no app id")
        }
        val view = payload.optJSONObject("view")
            ?: throw Malformed("a ui.render frame with no view")

        val vocabulary = view.optInt("vocabulary", -1)
        if (vocabulary != VOCABULARY) {
            throw Malformed(
                "this view says vocabulary $vocabulary and this build draws $VOCABULARY. " +
                    "Refusing it whole rather than drawing the parts it recognises: a " +
                    "confirmation with a question and no buttons is worse than a screen that " +
                    "says the app needs a newer Relay",
            )
        }

        val raw = view.optJSONArray("blocks") ?: throw Malformed("a view with no blocks")
        if (raw.length() == 0) throw Malformed("a view with no blocks renders nothing")
        if (raw.length() > Caps.BLOCKS) {
            throw Malformed("a view has ${raw.length()} blocks; the limit is ${Caps.BLOCKS}")
        }

        val blocks = ArrayList<Block>(raw.length())
        for (i in 0 until raw.length()) {
            val obj = raw.optJSONObject(i) ?: throw Malformed("blocks[$i] is not an object")
            blocks += parseBlock(obj, i)
        }
        if (blocks.count { it is Block.Confirm } > 1) {
            throw Malformed("a view asks at most one question")
        }
        if (blocks.count { it is Block.Speak } > 1) {
            throw Malformed("a view speaks at most once")
        }

        val actionId = payload.optString("action_id").takeIf { it.isNotEmpty() }
        if (actionId == null && blocks.any { it is Block.Confirm }) {
            // A question with no id is one this phone could draw and never
            // answer, because the answer is keyed by the id. Refusing beats
            // showing two buttons that lead nowhere.
            throw Malformed("a view asks a question and carries no action id to answer it with")
        }
        return Rendered(
            app = app,
            appName = payload.optString("appName").takeIf { it.isNotEmpty() },
            actionId = actionId,
            deadlineMs = payload.optLong("deadline", 0),
            blocks = blocks,
        )
    }

    private fun parseBlock(obj: JSONObject, i: Int): Block = when (val kind = obj.optString("kind")) {
        "card" -> Block.Card(
            title = text(obj, "title", "blocks[$i].title", Caps.CARD_TITLE),
            body = optional(obj, "body", "blocks[$i].body", Caps.CARD_BODY),
            fields = fields(obj, i),
        )

        "list" -> {
            val rows = obj.optJSONArray("items")
                ?: throw Malformed("blocks[$i].items is missing")
            if (rows.length() == 0) throw Malformed("blocks[$i].items is empty")
            if (rows.length() > Caps.LIST_ITEMS) {
                throw Malformed("blocks[$i].items has ${rows.length()}; the limit is ${Caps.LIST_ITEMS}")
            }
            Block.Rows(
                title = optional(obj, "title", "blocks[$i].title", Caps.LIST_TITLE),
                items = (0 until rows.length()).map { j ->
                    val row = rows.optJSONObject(j)
                        ?: throw Malformed("blocks[$i].items[$j] is not an object")
                    Item(
                        title = text(row, "title", "blocks[$i].items[$j].title", Caps.ITEM_TITLE),
                        subtitle = optional(row, "subtitle", "blocks[$i].items[$j].subtitle", Caps.ITEM_SUBTITLE),
                        detail = optional(row, "detail", "blocks[$i].items[$j].detail", Caps.ITEM_DETAIL),
                    )
                },
            )
        }

        "confirm" -> Block.Confirm(
            question = text(obj, "question", "blocks[$i].question", Caps.QUESTION),
            // The defaults live here rather than on the wire so an app that
            // sends nothing gets the platform's words, and so a `confirmLabel`
            // of "" cannot produce a button with no text on it.
            confirmLabel = optional(obj, "confirmLabel", "blocks[$i].confirmLabel", Caps.BUTTON_LABEL) ?: "Yes",
            cancelLabel = optional(obj, "cancelLabel", "blocks[$i].cancelLabel", Caps.BUTTON_LABEL) ?: "No",
            detail = optional(obj, "detail", "blocks[$i].detail", Caps.CONFIRM_DETAIL),
        )

        "speak" -> Block.Speak(text(obj, "text", "blocks[$i].text", Caps.SPEAK_TEXT))

        else -> throw Malformed(
            "blocks[$i].kind is \"$kind\"; this build draws card, list, confirm, speak",
        )
    }

    private fun fields(obj: JSONObject, i: Int): List<Field> {
        val raw = obj.optJSONArray("fields") ?: return emptyList()
        if (raw.length() > Caps.CARD_FIELDS) {
            throw Malformed("blocks[$i].fields has ${raw.length()}; the limit is ${Caps.CARD_FIELDS}")
        }
        return (0 until raw.length()).map { j ->
            val f = raw.optJSONObject(j) ?: throw Malformed("blocks[$i].fields[$j] is not an object")
            Field(
                label = text(f, "label", "blocks[$i].fields[$j].label", Caps.FIELD_LABEL),
                value = text(f, "value", "blocks[$i].fields[$j].value", Caps.FIELD_VALUE),
            )
        }
    }

    private fun text(obj: JSONObject, key: String, where: String, max: Int): String =
        optional(obj, key, where, max) ?: throw Malformed("$where is required")

    /**
     * One string check for every field.
     *
     * Control characters are refused rather than stripped, matching the host: a
     * card is text a phone draws, not a terminal, and silently removing an
     * escape sequence hides from everyone that one arrived. Tab and newline are
     * allowed only where the host allows them, which is the two fields that are
     * paragraphs — checked by [max] being one of the two paragraph caps rather
     * than by a flag, because the caps are the only thing the three copies of
     * this file agree on by construction.
     */
    private fun optional(obj: JSONObject, key: String, where: String, max: Int): String? {
        if (!obj.has(key) || obj.isNull(key)) return null
        val value = obj.optString(key)
        if (value.isEmpty()) return null
        if (value.isBlank()) throw Malformed("$where is blank")
        if (value.length > max) {
            throw Malformed("$where is ${value.length} characters; the limit is $max")
        }
        val multiline = max == Caps.CARD_BODY || max == Caps.CONFIRM_DETAIL || max == Caps.SPEAK_TEXT
        for (ch in value) {
            if (ch == '\n' || ch == '\t') {
                if (!multiline) throw Malformed("$where contains a line break and is one line")
                continue
            }
            if (ch.code < 0x20 || ch.code == 0x7F) {
                throw Malformed("$where contains a control character; a view is text, not a terminal")
            }
        }
        return value
    }
}
