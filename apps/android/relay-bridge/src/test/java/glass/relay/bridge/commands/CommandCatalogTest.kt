package glass.relay.bridge.commands

import glass.relay.bridge.protocol.Command
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Test
import java.io.File

/**
 * "Every glasses command, by hand", made mechanical.
 *
 * `ORCHESTRATOR.md` §5 asks for a product where every command is reachable
 * without speaking. The way that requirement rots is silently: firmware gains a
 * command, someone adds the id, and no screen ever grows a control for it. So
 * this suite checks the table against the two files that own the facts, rather
 * than against a reviewer's memory.
 *
 *  1. **Ids** come from `glasses/protocol/commands.py`, which the 92 Python
 *     tests cover. Re-parsed here on every run.
 *  2. **Roles and destructive flags** come from `COMMAND_CATALOG` in
 *     `glasses/bridge/src/commands.ts`, the TypeScript bridge both apps drive.
 *     Also re-parsed. Two apps that disagree about whether `0x0911` destroys
 *     data is not a difference of opinion.
 *
 * When the repository is not on disk — a source drop with only `apps/android/`
 * in it — the parsing tests skip rather than fail, and the structural tests
 * still run. A skip is visible in the report; a green suite that quietly
 * checked nothing is not.
 */
class CommandCatalogTest {

    // --- structure ----------------------------------------------------------

    @Test
    fun `the catalog covers every command id exactly once`() {
        val ids = CommandCatalog.ENTRIES.map { it.id }
        assertEquals("duplicate ids in the catalog", ids.size, ids.toSet().size)
        assertEquals(
            "catalog and Command table disagree",
            Command.ALL.toSet(),
            ids.toSet(),
        )
        assertEquals(92, CommandCatalog.ENTRIES.size)
    }

    @Test
    fun `every entry names itself the way the protocol does`() {
        for (entry in CommandCatalog.ENTRIES) {
            assertEquals(Command.nameOf(entry.id), entry.name)
        }
    }

    @Test
    fun `every entry has a label and the spec's own term`() {
        for (entry in CommandCatalog.ENTRIES) {
            assertTrue("${entry.name} has no label", entry.label.isNotBlank())
            assertTrue("${entry.name} has no spec name", entry.specName.isNotBlank())
        }
    }

    @Test
    fun `every category has at least one command`() {
        val grouped = CommandCatalog.byCategory()
        for (category in CommandCatalog.Category.entries) {
            assertTrue("$category is empty", grouped[category]?.isNotEmpty() == true)
        }
    }

    @Test
    fun `the destructive set is exactly the three commands that delete things`() {
        // Deliberately spelled out. A fourth appearing here should be a decision
        // someone made on purpose, visible in a diff.
        assertEquals(
            setOf(
                Command.DELETE_FILE,
                Command.DELETE_ALL_FILES,
                Command.CLEAR_UNUPLOADED_FILES,
            ),
            CommandCatalog.destructive().map { it.id }.toSet(),
        )
    }

    @Test
    fun `clearing un-uploaded files is flagged and explained`() {
        val entry = CommandCatalog.describe(Command.CLEAR_UNUPLOADED_FILES)
        assertNotNull(entry)
        assertTrue("0x0911 must be destructive", entry!!.destructive)
        assertTrue(
            "0x0911 needs a note saying what it deletes",
            entry.note?.contains("NOT been synced") == true,
        )
    }

    @Test
    fun `unused and deprecated commands are never sendable`() {
        val unsendable = CommandCatalog.ENTRIES.filter {
            it.role == CommandCatalog.CommandRole.Unused ||
                it.role == CommandCatalog.CommandRole.Deprecated
        }
        assertTrue("expected the spec's retired commands to be listed", unsendable.size >= 5)
        for (entry in unsendable) {
            assertTrue("${entry.name} must not be sendable", !entry.sendable)
        }
    }

    @Test
    fun `the two access-point credential commands stay refused, because there is no station mode`() {
        // ARCHITECTURE.md §2. These set the glasses' OWN hotspot and the spec
        // retired them; a UI that offered "join a WiFi network" would be selling
        // a capability the hardware does not have.
        for (id in listOf(Command.SET_WIFI_SSID_DEPRECATED, Command.SET_WIFI_PASSWORD_DEPRECATED)) {
            val entry = CommandCatalog.describe(id)!!
            assertEquals(CommandCatalog.CommandRole.Deprecated, entry.role)
            assertTrue(!entry.sendable)
        }
    }

    @Test
    fun `no command asks the glasses to transcribe`() {
        // SYSTEM.md §7b: the device is a microphone and a button. The app
        // recognises. Nothing in the catalog may suggest otherwise.
        val suspicious = CommandCatalog.ENTRIES.filter {
            it.sendable && (
                it.label.contains("transcri", ignoreCase = true) ||
                    it.label.contains("recognise", ignoreCase = true) ||
                    it.label.contains("recognize", ignoreCase = true)
                )
        }
        assertTrue("nothing sendable may claim device-side ASR: $suspicious", suspicious.isEmpty())
    }

    @Test
    fun `0x0A02 is uplink only and 0x0A03 goes both ways`() {
        assertEquals(
            CommandCatalog.CommandRole.Command,
            CommandCatalog.describe(Command.AUDIO_CONTROL)!!.role,
        )
        assertEquals(
            CommandCatalog.CommandRole.Both,
            CommandCatalog.describe(Command.AUDIO_DATA)!!.role,
        )
    }

    @Test
    fun `every sendable command has a payload the console can actually build`() {
        for (entry in CommandCatalog.ENTRIES.filter { it.sendable }) {
            assertTrue(
                "${entry.name} claims to be sendable with an unattested payload",
                entry.args !is CommandCatalog.ArgSpec.Unattested,
            )
        }
    }

    // --- drift, against the files that own the facts -------------------------

    @Test
    fun `ids match glasses protocol commands py`() {
        val source = repoFile("glasses/protocol/commands.py") ?: return skip()
        val fromPython = parsePythonCommands(source.readText())

        assertEquals("expected 92 commands in the spec", 92, fromPython.size)
        for ((name, id) in fromPython) {
            val entry = CommandCatalog.ENTRIES.firstOrNull { it.name == name }
            assertNotNull("$name is in commands.py but not in the Android catalog", entry)
            assertEquals("$name has drifted", id, entry!!.id)
        }
        assertEquals(
            "the Android catalog has commands the spec does not",
            fromPython.keys,
            CommandCatalog.ENTRIES.map { it.name }.toSet(),
        )
    }

    @Test
    fun `roles and destructive flags match the typescript catalog`() {
        val source = repoFile("glasses/bridge/src/commands.ts") ?: return skip()
        val fromTypeScript = parseTypeScriptCatalog(source.readText())
        assumeTrue("commands.ts has no COMMAND_CATALOG", fromTypeScript.isNotEmpty())

        for ((name, row) in fromTypeScript) {
            val entry = CommandCatalog.ENTRIES.firstOrNull { it.name == name }
            assertNotNull("$name is in commands.ts but not here", entry)
            assertEquals(
                "$name: role disagrees with the TypeScript bridge",
                row.role.lowercase(),
                entry!!.role.name.lowercase(),
            )
            assertEquals(
                "$name: destructive flag disagrees with the TypeScript bridge",
                row.destructive,
                entry.destructive,
            )
        }
        assertEquals(fromTypeScript.keys.size, CommandCatalog.ENTRIES.size)
    }

    private fun skip() = assumeTrue(
        "repository sources not present; run this from a full checkout to check for drift",
        false,
    )

    private data class TsRow(val role: String, val destructive: Boolean)

    private companion object {

        fun parsePythonCommands(source: String): Map<String, Int> {
            val start = source.indexOf("class Command(IntEnum):")
            require(start >= 0) { "commands.py must still define class Command(IntEnum)" }
            val body = source.substring(start).split("\n\n\n").first()
            val pattern = Regex("""^ {4}([A-Z0-9_]+) = (0x[0-9A-Fa-f]{4})\b""")
            return body.lineSequence()
                .mapNotNull { pattern.find(it) }
                .associate { it.groupValues[1] to it.groupValues[2].removePrefix("0x").toInt(16) }
        }

        fun parseTypeScriptCatalog(source: String): Map<String, TsRow> {
            val start = source.indexOf("export const COMMAND_CATALOG")
            if (start < 0) return emptyMap()
            val body = source.substring(start, source.indexOf("];", start))
            val pattern = Regex("""\{\s*name:\s*"([A-Z0-9_]+)",[^}]*?role:\s*CommandRole\.(\w+)([^}]*)}""")
            return pattern.findAll(body).associate { match ->
                match.groupValues[1] to TsRow(
                    role = match.groupValues[2],
                    destructive = match.groupValues[3].contains("destructive: true"),
                )
            }
        }

        /**
         * Walk up from the working directory to find a repository file.
         *
         * Gradle runs unit tests with the module directory as the working
         * directory and `tools/verify-jvm-logic.sh` runs them from
         * `apps/android`, so neither can be hard-coded.
         */
        fun repoFile(relative: String): File? {
            var directory: File? = File(System.getProperty("user.dir")).absoluteFile
            repeat(8) {
                val candidate = File(directory, relative)
                if (candidate.isFile) return candidate
                directory = directory?.parentFile ?: return null
            }
            return null
        }
    }
}
