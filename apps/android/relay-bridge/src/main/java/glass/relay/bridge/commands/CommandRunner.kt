package glass.relay.bridge.commands

import glass.relay.bridge.protocol.Command
import glass.relay.bridge.protocol.FrameDecode
import glass.relay.bridge.protocol.Packet
import glass.relay.bridge.protocol.decodeFrame

/**
 * Issue a command and keep a record of what happened.
 *
 * The screen is a list generated from [CommandCatalog]; this is everything
 * behind the tap. Kept out of the UI layer so the interesting behaviour —
 * refusals, confirmations, what actually reached the wire — is testable with no
 * device and no Android runtime.
 *
 * [Sender] is one function wide, deliberately. Every command the app can issue
 * becomes a framed packet from [CommandConsole], so the vendor SDK surface this
 * needs is a single "write these bytes and give me the reply" call rather than
 * sixty typed methods that nobody can test until hardware arrives.
 */
class CommandRunner(
    private val send: Sender,
    private val console: CommandConsole = CommandConsole(),
    private val clock: () -> Long = System::currentTimeMillis,
    /** Keeps the log bounded; the console is a debugging tool, not a database. */
    private val historyLimit: Int = 200,
) {

    /** Writes one framed packet and returns the device's reply frame, if any. */
    fun interface Sender {
        suspend fun sendFrame(frame: ByteArray): ByteArray?
    }

    data class Entry(
        val atMs: Long,
        val commandName: String,
        val commandId: Int,
        val outcome: Outcome,
        val detail: String,
    )

    enum class Outcome { Sent, Refused, Failed }

    private val _history = ArrayDeque<Entry>()

    /** Newest last, so a UI can render it as a transcript. */
    val history: List<Entry> get() = _history.toList()

    suspend fun run(
        commandId: Int,
        input: CommandConsole.Input = CommandConsole.Input.None,
        confirmDestructive: String? = null,
    ): Entry {
        val built = console.build(commandId, input, confirmDestructive)

        val entry = when (built) {
            is CommandConsole.Outcome.Refused -> Entry(
                atMs = clock(),
                commandName = built.entry?.name ?: Command.nameOf(commandId),
                commandId = commandId,
                outcome = Outcome.Refused,
                detail = built.message,
            )

            is CommandConsole.Outcome.Ready -> try {
                val reply = send.sendFrame(built.frame)
                Entry(
                    atMs = clock(),
                    commandName = built.entry.name,
                    commandId = commandId,
                    outcome = Outcome.Sent,
                    detail = describeReply(reply),
                )
            } catch (error: Exception) {
                Entry(
                    atMs = clock(),
                    commandName = built.entry.name,
                    commandId = commandId,
                    outcome = Outcome.Failed,
                    detail = error.message ?: error.toString(),
                )
            }
        }

        _history.addLast(entry)
        while (_history.size > historyLimit) _history.removeFirst()
        return entry
    }

    /**
     * Say what came back without pretending to understand it.
     *
     * `APPS-SCOPE.md` §5.1: the framing and the CRC are attested, the byte
     * layout of most replies is not. So the console shows the command name it
     * decoded and the payload as hex. Rendering a guessed field as a labelled
     * value is how a guess becomes a fact somebody relies on.
     */
    private fun describeReply(reply: ByteArray?): String {
        if (reply == null || reply.isEmpty()) return "sent"
        return when (val decoded = decodeFrame(reply)) {
            is FrameDecode.Ok -> runCatching {
                val packet = Packet.decode(decoded.data)
                "${packet.name} payload ${packet.payload.hex()}"
            }.getOrElse { "reply ${decoded.data.hex()}" }

            is FrameDecode.ChecksumMismatch ->
                "reply failed CRC (carried 0x%04X, computed 0x%04X)".format(decoded.carried, decoded.computed)

            is FrameDecode.Incomplete -> "reply truncated, ${decoded.needBytes} bytes short"
            is FrameDecode.Malformed -> "reply malformed: ${decoded.message}"
        }
    }

    private fun ByteArray.hex(): String =
        if (isEmpty()) "(empty)" else joinToString("") { "%02x".format(it) }
}
