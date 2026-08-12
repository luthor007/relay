package glass.relay.bridge.commands

import glass.relay.bridge.protocol.Command
import glass.relay.bridge.protocol.FrameDecode
import glass.relay.bridge.protocol.Packet
import glass.relay.bridge.protocol.decodeFrame
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The tappable command surface refuses more than it sends, on purpose.
 */
class CommandConsoleTest {

    private fun ready(outcome: CommandConsole.Outcome): CommandConsole.Outcome.Ready {
        assertTrue("expected Ready, got $outcome", outcome is CommandConsole.Outcome.Ready)
        return outcome as CommandConsole.Outcome.Ready
    }

    private fun refused(outcome: CommandConsole.Outcome): CommandConsole.Outcome.Refused {
        assertTrue("expected Refused, got $outcome", outcome is CommandConsole.Outcome.Refused)
        return outcome as CommandConsole.Outcome.Refused
    }

    @Test
    fun `a toggle becomes one byte and a valid frame`() {
        val console = CommandConsole()
        val outcome = ready(
            console.build(Command.LOCAL_RECORDING_CONTROL, CommandConsole.Input.Toggle(on = true)),
        )

        assertEquals(Command.LOCAL_RECORDING_CONTROL, outcome.packet.commandId)
        assertEquals(1, outcome.packet.payload.size)
        assertEquals(1, outcome.packet.payload[0].toInt())

        // The frame must survive the real decoder, CRC and all.
        val decoded = decodeFrame(outcome.frame)
        assertTrue(decoded is FrameDecode.Ok)
        val packet = Packet.decode((decoded as FrameDecode.Ok).data)
        assertEquals(Command.LOCAL_RECORDING_CONTROL, packet.commandId)
    }

    @Test
    fun `sequence numbers advance so a reply can be matched to its request`() {
        val console = CommandConsole()
        val first = ready(console.build(Command.GET_BATTERY)).packet.sequence
        val second = ready(console.build(Command.GET_BATTERY)).packet.sequence
        assertEquals((first + 1) and 0xFF, second)
    }

    @Test
    fun `unused commands are refused rather than hidden`() {
        val outcome = refused(CommandConsole().build(Command.AI_CHAT_MODE_UNUSED))
        assertEquals(CommandConsole.Reason.Unused, outcome.reason)
        assertTrue(outcome.message.contains("未使用"))
    }

    @Test
    fun `deprecated access-point credentials are refused, because there is no station mode`() {
        val outcome = refused(CommandConsole().build(Command.SET_WIFI_SSID_DEPRECATED))
        assertEquals(CommandConsole.Reason.Deprecated, outcome.reason)
    }

    @Test
    fun `a device report has nothing to send`() {
        val outcome = refused(CommandConsole().build(Command.BATTERY_REPORT))
        assertEquals(CommandConsole.Reason.ReportOnly, outcome.reason)
    }

    @Test
    fun `an unattested payload is refused rather than guessed`() {
        // APPS-SCOPE.md §5.1. 0x091D's request layout is not attested anywhere,
        // so the console will not invent one.
        val outcome = refused(CommandConsole().build(Command.SET_VIDEO_PARAMS))
        assertEquals(CommandConsole.Reason.PayloadUnattested, outcome.reason)
        assertTrue(outcome.message.contains("APPS-SCOPE.md §5.1"))
    }

    @Test
    fun `clearing un-uploaded files needs the command named, not a boolean`() {
        val console = CommandConsole()

        val without = refused(console.build(Command.CLEAR_UNUPLOADED_FILES))
        assertEquals(CommandConsole.Reason.NeedsConfirmation, without.reason)

        val wrongName = refused(
            console.build(Command.CLEAR_UNUPLOADED_FILES, confirmDestructive = "DELETE_ALL_FILES"),
        )
        assertEquals(CommandConsole.Reason.NeedsConfirmation, wrongName.reason)

        val confirmed = ready(
            console.build(
                Command.CLEAR_UNUPLOADED_FILES,
                confirmDestructive = "CLEAR_UNUPLOADED_FILES",
            ),
        )
        assertEquals(Command.CLEAR_UNUPLOADED_FILES, confirmed.packet.commandId)
    }

    @Test
    fun `every destructive command needs confirmation, not just the one we remembered`() {
        val console = CommandConsole()
        for (entry in CommandCatalog.destructive()) {
            if (!entry.sendable) continue
            val outcome = refused(console.build(entry.id))
            assertEquals("${entry.name} went through unconfirmed", CommandConsole.Reason.NeedsConfirmation, outcome.reason)
        }
    }

    @Test
    fun `a choice outside the device's own vocabulary is refused`() {
        val console = CommandConsole()
        val outcome = refused(
            console.build(Command.DEVICE_CONTROL, CommandConsole.Input.Choice(0x99)),
        )
        assertEquals(CommandConsole.Reason.WrongArgument, outcome.reason)
    }

    @Test
    fun `a valid device mode encodes to its SDK value`() {
        val console = CommandConsole()
        val outcome = ready(
            console.build(
                Command.DEVICE_CONTROL,
                CommandConsole.Input.Choice(CommandCatalog.DeviceMode.RESTART),
            ),
        )
        assertEquals(0x0E, outcome.packet.payload[0].toInt())
    }

    @Test
    fun `the wrong kind of argument is refused rather than coerced`() {
        val console = CommandConsole()
        val outcome = refused(
            console.build(Command.LOCAL_RECORDING_CONTROL, CommandConsole.Input.Choice(1)),
        )
        assertEquals(CommandConsole.Reason.WrongArgument, outcome.reason)
    }

    @Test
    fun `wake words are selected by index, never by phrase`() {
        val console = CommandConsole()
        val outcome = ready(
            console.build(
                Command.SET_WAKEWORD_SETTING,
                CommandConsole.Input.WakeWords(
                    listOf(
                        CommandConsole.Input.WakeWords.Selection(0, enabled = true),
                        CommandConsole.Input.WakeWords.Selection(1, enabled = false),
                    ),
                ),
            ),
        )
        assertEquals(
            listOf(0, 1, 1, 0),
            outcome.packet.payload.map { it.toInt() },
        )
    }

    @Test
    fun `the wake word list decodes the spec's worked example`() {
        // ARCHITECTURE.md §5.2b: Index, Type, Len, Value. The spec's example
        // phrase is "hey chatgpt".
        val phrase = "hey chatgpt".toByteArray()
        val payload = byteArrayOf(0, 0, phrase.size.toByte()) + phrase
        val words = CommandConsole.decodeWakeWordList(payload)

        assertEquals(1, words.size)
        assertEquals("hey chatgpt", words[0].phrase)
        assertEquals(0, words[0].index)
    }

    @Test
    fun `a truncated wake word list is rejected rather than half-read`() {
        val payload = byteArrayOf(0, 0, 20, 'h'.code.toByte())
        val error = runCatching { CommandConsole.decodeWakeWordList(payload) }.exceptionOrNull()
        assertTrue("expected a decode failure, got $error", error is IllegalArgumentException)
    }

    @Test
    fun `an id the spec has never heard of is refused`() {
        val outcome = refused(CommandConsole().build(0x4242))
        assertEquals(CommandConsole.Reason.UnknownCommand, outcome.reason)
    }

    @Test
    fun `every sendable command in the catalog can actually be built`() {
        // The coverage proof: if a command claims to be sendable, the console
        // must be able to produce bytes for it with the input its ArgSpec names.
        val console = CommandConsole()
        for (entry in CommandCatalog.ENTRIES.filter { it.sendable }) {
            val input = when (val spec = entry.args) {
                is CommandCatalog.ArgSpec.None -> CommandConsole.Input.None
                is CommandCatalog.ArgSpec.Toggle -> CommandConsole.Input.Toggle(true)
                is CommandCatalog.ArgSpec.Choice -> CommandConsole.Input.Choice(spec.options.first().value)
                is CommandCatalog.ArgSpec.WakeWordSelection -> CommandConsole.Input.WakeWords(
                    listOf(CommandConsole.Input.WakeWords.Selection(0, true)),
                )
                is CommandCatalog.ArgSpec.Unattested -> error("unreachable: filtered above")
            }
            val outcome = console.build(entry.id, input, confirmDestructive = entry.name)
            assertTrue("${entry.name} could not be built: $outcome", outcome is CommandConsole.Outcome.Ready)
        }
    }
}
