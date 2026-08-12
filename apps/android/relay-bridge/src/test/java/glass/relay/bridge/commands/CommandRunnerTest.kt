package glass.relay.bridge.commands

import glass.relay.bridge.protocol.Command
import glass.relay.bridge.protocol.FrameDecode
import glass.relay.bridge.protocol.Packet
import glass.relay.bridge.protocol.PacketType
import glass.relay.bridge.protocol.decodeFrame
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * What happens behind a tap on the command screen.
 */
class CommandRunnerTest {

    private class Wire(var reply: ByteArray? = null, var throws: Boolean = false) : CommandRunner.Sender {
        val sent = mutableListOf<ByteArray>()
        override suspend fun sendFrame(frame: ByteArray): ByteArray? {
            if (throws) throw IllegalStateException("not connected")
            sent += frame
            return reply
        }
    }

    @Test
    fun `a sendable command reaches the wire as a valid frame`() = runTest {
        val wire = Wire()
        val runner = CommandRunner(wire)

        val entry = runner.run(Command.WIFI_AP_CONTROL, CommandConsole.Input.Toggle(true))

        assertEquals(CommandRunner.Outcome.Sent, entry.outcome)
        assertEquals("WIFI_AP_CONTROL", entry.commandName)

        val decoded = decodeFrame(wire.sent.single())
        assertTrue(decoded is FrameDecode.Ok)
        assertEquals(Command.WIFI_AP_CONTROL, Packet.decode((decoded as FrameDecode.Ok).data).commandId)
    }

    @Test
    fun `a refusal never touches the wire`() = runTest {
        val wire = Wire()
        val runner = CommandRunner(wire)

        val entry = runner.run(Command.CLEAR_UNUPLOADED_FILES)

        assertEquals(CommandRunner.Outcome.Refused, entry.outcome)
        assertTrue("nothing may be sent unconfirmed", wire.sent.isEmpty())
    }

    @Test
    fun `a transport failure is recorded rather than thrown at the UI`() = runTest {
        val runner = CommandRunner(Wire(throws = true))
        val entry = runner.run(Command.GET_BATTERY)

        assertEquals(CommandRunner.Outcome.Failed, entry.outcome)
        assertTrue(entry.detail.contains("not connected"))
    }

    @Test
    fun `a reply is named but its payload is shown as hex, never as invented fields`() = runTest {
        // APPS-SCOPE.md §5.1: the framing is attested, most reply layouts are
        // not. Rendering a guessed field as a labelled value is how a guess
        // becomes a fact someone relies on.
        val reply = Packet(Command.GET_BATTERY, PacketType.RESPONSE, 0, byteArrayOf(0x5A, 0x01)).toFrame()
        val runner = CommandRunner(Wire(reply = reply))

        val entry = runner.run(Command.GET_BATTERY)

        assertEquals(CommandRunner.Outcome.Sent, entry.outcome)
        assertEquals("GET_BATTERY payload 5a01", entry.detail)
    }

    @Test
    fun `a corrupt reply is reported as a CRC failure, not decoded anyway`() = runTest {
        val reply = Packet(Command.GET_BATTERY, PacketType.RESPONSE, 0, byteArrayOf(1)).toFrame()
        reply[reply.size - 1] = (reply[reply.size - 1].toInt() xor 0xFF).toByte()

        val entry = CommandRunner(Wire(reply = reply)).run(Command.GET_BATTERY)

        assertTrue(entry.detail.contains("failed CRC"))
    }

    @Test
    fun `history is kept in order and bounded`() = runTest {
        val runner = CommandRunner(Wire(), historyLimit = 3)
        repeat(5) { runner.run(Command.HEARTBEAT) }

        assertEquals(3, runner.history.size)
        assertTrue(runner.history.all { it.commandName == "HEARTBEAT" })
    }
}
