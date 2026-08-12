package glass.relay.bridge.ui

import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

/**
 * The vocabulary is defined three times and this is the third
 *
 * `apps/sdk/src/ui.ts`, `relayd/internal/apps/ui.go` and this file. The Go and
 * TypeScript copies are pinned to each other by a test that reads one from the
 * other; this one cannot be, because nothing on this side can read a file from
 * the daemon's tree at build time. What it has instead is the same fixtures: a
 * frame here is written the way relayd writes one, so drift shows up as a
 * refusal rather than as a card that silently loses a field.
 */
class AppViewTest {

    private fun frame(view: String, extra: String = ""): JSONObject =
        JSONObject("""{"app":"dev.test.standup","appName":"Standup"$extra,"view":$view}""")

    private val card = """{"vocabulary":1,"blocks":[{"kind":"card","title":"Standup"}]}"""

    @Test
    fun `a card arrives with the app that drew it`() {
        val v = AppView.parse(frame(card))
        assertEquals("dev.test.standup", v.app)
        assertEquals("Standup", v.appName)
        assertEquals(1, v.blocks.size)
        assertTrue(v.blocks[0] is AppView.Block.Card)
    }

    @Test
    fun `an unattributed view is refused`() {
        // "Which of my apps is asking me this" is the first question a card
        // raises and the blocks cannot answer it.
        try {
            AppView.parse(JSONObject("""{"view":$card}"""))
            fail("a view with no app id was accepted")
        } catch (e: AppView.Malformed) {
            assertTrue(e.message!!.contains("app id"))
        }
    }

    @Test
    fun `a view from a newer daemon is refused whole`() {
        // Not partially drawn. A confirmation with a question and no buttons is
        // worse than a screen that says the app needs a newer Relay.
        try {
            AppView.parse(frame("""{"vocabulary":2,"blocks":[{"kind":"card","title":"Standup"}]}"""))
            fail("a view from the future was drawn")
        } catch (e: AppView.Malformed) {
            assertTrue(e.message!!.contains("vocabulary 2"))
        }
    }

    @Test
    fun `a block kind this build cannot draw refuses the view rather than skipping it`() {
        try {
            AppView.parse(
                frame(
                    """{"vocabulary":1,"blocks":[
                        {"kind":"card","title":"Standup"},
                        {"kind":"chart","title":"Burndown"}]}""",
                ),
            )
            fail("an unknown block was skipped and the rest drawn")
        } catch (e: AppView.Malformed) {
            assertTrue(e.message!!.contains("chart"))
        }
    }

    @Test
    fun `a question carries the id that answers it`() {
        val v = AppView.parse(
            frame(
                """{"vocabulary":1,"blocks":[
                    {"kind":"card","title":"About to send"},
                    {"kind":"confirm","question":"Send it?","detail":"Four lines."}]}""",
                extra = ""","action_id":"act-1","deadline":123456""",
            ),
        )
        assertEquals("act-1", v.actionId)
        assertEquals(123456L, v.deadlineMs)
        val q = v.question
        assertNotNull(q)
        assertEquals("Send it?", q!!.question)
        // The platform's words when the app sends none, so a button is never blank.
        assertEquals("Yes", q.confirmLabel)
        assertEquals("No", q.cancelLabel)
    }

    @Test
    fun `a question with no id is refused because nothing could answer it`() {
        try {
            AppView.parse(frame("""{"vocabulary":1,"blocks":[{"kind":"confirm","question":"Send it?"}]}"""))
            fail("a question with no action id was drawn")
        } catch (e: AppView.Malformed) {
            assertTrue(e.message!!.contains("action id"))
        }
    }

    @Test
    fun `a view that only draws carries no action id`() {
        assertNull(AppView.parse(frame(card)).actionId)
    }

    @Test
    fun `two questions in one view are refused`() {
        try {
            AppView.parse(
                frame(
                    """{"vocabulary":1,"blocks":[
                        {"kind":"confirm","question":"A?"},{"kind":"confirm","question":"B?"}]}""",
                    extra = ""","action_id":"act-1"""",
                ),
            )
            fail("two questions were accepted and one answer would have to serve both")
        } catch (e: AppView.Malformed) {
            assertTrue(e.message!!.contains("at most one question"))
        }
    }

    @Test
    fun `a control character is refused rather than drawn`() {
        // Refused, not stripped: a card is text a phone draws, not a terminal,
        // and quietly removing an escape hides from everyone that one arrived.
        // Built through the JSON API rather than a literal, because the check is
        // on the decoded string and a raw Kotlin string would carry the two
        // characters that spell an escape rather than one.
        val view = JSONObject()
            .put("vocabulary", 1)
            .put(
                "blocks",
                JSONArray().put(
                    JSONObject().put("kind", "card").put("title", "Stand\u001b[31mup"),
                ),
            )
        try {
            AppView.parse(JSONObject().put("app", "dev.test.standup").put("view", view))
            fail("an escape sequence was accepted")
        } catch (e: AppView.Malformed) {
            assertTrue(e.message!!.contains("control character"))
        }
    }

    @Test
    fun `a newline is allowed in a body and refused in a title`() {
        AppView.parse(frame("""{"vocabulary":1,"blocks":[{"kind":"card","title":"S","body":"one\ntwo"}]}"""))
        try {
            AppView.parse(frame("""{"vocabulary":1,"blocks":[{"kind":"card","title":"one\ntwo"}]}"""))
            fail("a title spanning two lines was accepted")
        } catch (e: AppView.Malformed) {
            assertTrue(e.message!!.contains("one line"))
        }
    }

    @Test
    fun `a string past its cap is refused`() {
        val long = "x".repeat(AppView.Caps.CARD_TITLE + 1)
        try {
            AppView.parse(frame("""{"vocabulary":1,"blocks":[{"kind":"card","title":"$long"}]}"""))
            fail("a title over the cap was accepted")
        } catch (e: AppView.Malformed) {
            assertTrue(e.message!!.contains("the limit is ${AppView.Caps.CARD_TITLE}"))
        }
    }

    @Test
    fun `an empty list is refused`() {
        try {
            AppView.parse(frame("""{"vocabulary":1,"blocks":[{"kind":"list","title":"Today","items":[]}]}"""))
            fail("an empty list was accepted; it draws as a heading with nothing under it")
        } catch (e: AppView.Malformed) {
            assertTrue(e.message!!.contains("empty"))
        }
    }

    @Test
    fun `a list keeps its rows in order with their optional parts`() {
        val v = AppView.parse(
            frame(
                """{"vocabulary":1,"blocks":[{"kind":"list","title":"Today","items":[
                    {"title":"Ship the fix","detail":"4pm"},
                    {"title":"Call back","subtitle":"the supplier"}]}]}""",
            ),
        )
        val rows = v.blocks[0] as AppView.Block.Rows
        assertEquals("Today", rows.title)
        assertEquals(2, rows.items.size)
        assertEquals("4pm", rows.items[0].detail)
        assertNull(rows.items[0].subtitle)
        assertEquals("the supplier", rows.items[1].subtitle)
    }

    @Test
    fun `the text projection is what something with no screen reads`() {
        val v = AppView.parse(
            frame(
                """{"vocabulary":1,"blocks":[
                    {"kind":"card","title":"Standup","fields":[{"label":"Blocked","value":"no"}]},
                    {"kind":"list","items":[{"title":"Ship","detail":"4pm"}]}]}""",
            ),
        )
        val text = v.text()
        for (want in listOf("Standup", "Blocked: no", "Ship", "4pm")) {
            assertTrue("missing $want in:\n$text", text.contains(want))
        }
    }

    @Test
    fun `the caps match the host`() {
        // Hand-checked against relayd/internal/apps/ui.go, which is itself
        // pinned to the SDK by a test that reads ui.ts. If one of these is ever
        // wrong the symptom is a card the host sent and this build refuses, so
        // it is written out rather than left to a reviewer's memory.
        assertEquals(8, AppView.Caps.BLOCKS)
        assertEquals(120, AppView.Caps.CARD_TITLE)
        assertEquals(2000, AppView.Caps.CARD_BODY)
        assertEquals(12, AppView.Caps.CARD_FIELDS)
        assertEquals(60, AppView.Caps.FIELD_LABEL)
        assertEquals(240, AppView.Caps.FIELD_VALUE)
        assertEquals(120, AppView.Caps.LIST_TITLE)
        assertEquals(50, AppView.Caps.LIST_ITEMS)
        assertEquals(120, AppView.Caps.ITEM_TITLE)
        assertEquals(240, AppView.Caps.ITEM_SUBTITLE)
        assertEquals(60, AppView.Caps.ITEM_DETAIL)
        assertEquals(240, AppView.Caps.QUESTION)
        assertEquals(32, AppView.Caps.BUTTON_LABEL)
        assertEquals(600, AppView.Caps.CONFIRM_DETAIL)
        assertEquals(1000, AppView.Caps.SPEAK_TEXT)
        assertEquals(1, AppView.VOCABULARY)
    }
}
