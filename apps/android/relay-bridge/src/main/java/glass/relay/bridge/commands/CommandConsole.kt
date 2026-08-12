package glass.relay.bridge.commands

import glass.relay.bridge.protocol.Packet
import glass.relay.bridge.protocol.PacketType
import glass.relay.bridge.protocol.SequenceCounter

/**
 * The tappable command surface, as logic rather than as screens.
 *
 * `ORCHESTRATOR.md` §5 wants every glasses command reachable by hand. The
 * screen that does that is thin Compose over this class, and everything worth
 * getting right lives here, where it can be tested with no device, no SDK and
 * no Android runtime:
 *
 *  - **Unused and deprecated commands are refused locally**, not hidden. The
 *    spec retired `0x0901`/`0x0902` and marked `0x0801`-`0x0804` unused; a UI
 *    that silently omits them is a UI that cannot explain why the thing the
 *    vendor documented does not work. It shows the row and says "retired".
 *  - **Reports have no send affordance.** `0x0102` is the device telling us the
 *    battery; there is nothing to send.
 *  - **Destructive commands need a matching confirmation token.** Not a boolean
 *    flag that a refactor can default to true — the caller has to name the
 *    command it means to destroy with. `0x0911` deletes exactly the audio that
 *    has not been synced yet.
 *  - **Unattested payloads are refused, not guessed.** `APPS-SCOPE.md` §5.1:
 *    the ids, framing, CRC and enumerated control bytes are attested; most
 *    request and response layouts are not. A guess that reaches the wire looks
 *    like a firmware bug for a day.
 *
 * What comes out is a [Packet] — id, type, sequence, payload — which
 * [Packet.toFrame] turns into the exact bytes for the characteristic. That is
 * the whole reason the codec is ported into this module: without it, "every
 * command by hand" is sixty vendor SDK calls nobody can test, and with it the
 * SDK surface is one call wide.
 */
class CommandConsole(
    private val sequence: SequenceCounter = SequenceCounter(),
) {

    /** What the UI collected, before it is turned into bytes. */
    sealed interface Input {
        data object None : Input
        data class Toggle(val on: Boolean) : Input
        data class Choice(val value: Int) : Input
        data class WakeWords(val selections: List<Selection>) : Input {
            data class Selection(val index: Int, val enabled: Boolean)
        }
    }

    sealed interface Outcome {
        /** Ready for the wire. [frame] is the complete framed, CRC'd byte string. */
        data class Ready(val entry: CommandCatalog.Entry, val packet: Packet) : Outcome {
            val frame: ByteArray get() = packet.toFrame()
        }

        data class Refused(
            val entry: CommandCatalog.Entry?,
            val reason: Reason,
            val message: String,
        ) : Outcome
    }

    enum class Reason {
        /** No such command in the spec. */
        UnknownCommand,

        /** 未使用 — the spec says the device does not implement it. */
        Unused,

        /** 已弃用 — retired by the spec. */
        Deprecated,

        /** A device report. There is no request to send. */
        ReportOnly,

        /** The request payload layout is not attested; we will not invent one. */
        PayloadUnattested,

        /** The UI supplied an argument of the wrong shape. */
        WrongArgument,

        /** Destroys data and was not confirmed by name. */
        NeedsConfirmation,
    }

    /**
     * Build the packet for [commandId], or refuse and say why.
     *
     * [confirmDestructive] must equal the command's spec name for a destructive
     * command to go through. A `Boolean` would be one careless default away from
     * deleting a day of un-uploaded audio.
     */
    fun build(
        commandId: Int,
        input: Input = Input.None,
        confirmDestructive: String? = null,
    ): Outcome {
        val entry = CommandCatalog.describe(commandId)
            ?: return Outcome.Refused(
                null,
                Reason.UnknownCommand,
                "0x%04X is not in 通信协议 v2.0.17".format(commandId),
            )

        when (entry.role) {
            CommandCatalog.CommandRole.Unused -> return Outcome.Refused(
                entry,
                Reason.Unused,
                "${entry.name} is 未使用 in v2.0.17 — the device does not implement it",
            )
            CommandCatalog.CommandRole.Deprecated -> return Outcome.Refused(
                entry,
                Reason.Deprecated,
                "${entry.name} is 已弃用 in v2.0.17",
            )
            CommandCatalog.CommandRole.Report -> return Outcome.Refused(
                entry,
                Reason.ReportOnly,
                "${entry.name} is a device report; there is nothing to send",
            )
            else -> Unit
        }

        if (entry.destructive && confirmDestructive != entry.name) {
            return Outcome.Refused(
                entry,
                Reason.NeedsConfirmation,
                "${entry.name} destroys data — pass confirmDestructive = \"${entry.name}\"",
            )
        }

        val payload = when (val spec = entry.args) {
            is CommandCatalog.ArgSpec.Unattested -> return Outcome.Refused(
                entry,
                Reason.PayloadUnattested,
                "${entry.name}: ${spec.whatIsMissing} — see APPS-SCOPE.md §5.1",
            )

            is CommandCatalog.ArgSpec.None -> ByteArray(0)

            is CommandCatalog.ArgSpec.Toggle -> {
                val toggle = input as? Input.Toggle
                    ?: return wrongArgument(entry, "a Toggle")
                byteArrayOf(if (toggle.on) CommandCatalog.Toggle.ON.toByte() else CommandCatalog.Toggle.OFF.toByte())
            }

            is CommandCatalog.ArgSpec.Choice -> {
                val choice = input as? Input.Choice
                    ?: return wrongArgument(entry, "a Choice")
                if (spec.options.none { it.value == choice.value }) {
                    return Outcome.Refused(
                        entry,
                        Reason.WrongArgument,
                        "0x%02X is not one of the values ${entry.name} accepts".format(choice.value),
                    )
                }
                byteArrayOf((choice.value and 0xFF).toByte())
            }

            is CommandCatalog.ArgSpec.WakeWordSelection -> {
                val words = input as? Input.WakeWords
                    ?: return wrongArgument(entry, "a WakeWords selection")
                if (words.selections.isEmpty()) {
                    return Outcome.Refused(
                        entry,
                        Reason.WrongArgument,
                        "${entry.name} needs at least one index/enabled pair",
                    )
                }
                encodeWakeWordSettings(words.selections)
            }
        }

        return Outcome.Ready(
            entry,
            Packet(
                commandId = entry.id,
                type = PacketType.REQUEST,
                sequence = sequence.next(),
                payload = payload,
            ),
        )
    }

    private fun wrongArgument(entry: CommandCatalog.Entry, expected: String) = Outcome.Refused(
        entry,
        Reason.WrongArgument,
        "${entry.name} expects $expected",
    )

    companion object {

        /**
         * `0x0F02` / `0x0F03` payload: repeating `Index(1) Enabled(1)`.
         *
         * Mirrors `encodeWakeWordSettings` in `glasses/bridge/src/commands.ts`.
         * Selection by index only — there is no command anywhere in the spec
         * that accepts a wake *phrase*, because the spotter is a trained DSP
         * model (`ARCHITECTURE.md` §5.2b).
         */
        fun encodeWakeWordSettings(selections: List<Input.WakeWords.Selection>): ByteArray {
            val out = ByteArray(selections.size * 2)
            selections.forEachIndexed { i, selection ->
                require(selection.index in 0..0xFF) { "wake word index out of range: ${selection.index}" }
                out[i * 2] = selection.index.toByte()
                out[i * 2 + 1] = if (selection.enabled) 1 else 0
            }
            return out
        }

        /**
         * `0x0F01` payload: repeating `Index(1) Type(1) Len(1) Value(Len)`.
         *
         * Mirrors `decodeWakeWordList`. Values are UTF-8 phrases as the firmware
         * spells them; the spec's worked example is `"hey chatgpt"`.
         */
        fun decodeWakeWordList(payload: ByteArray): List<WakeWord> {
            val out = mutableListOf<WakeWord>()
            var offset = 0
            while (offset + 3 <= payload.size) {
                val index = payload[offset].toInt() and 0xFF
                val kind = payload[offset + 1].toInt() and 0xFF
                val length = payload[offset + 2].toInt() and 0xFF
                require(offset + 3 + length <= payload.size) {
                    "wake word entry $index declares $length bytes but only " +
                        "${payload.size - offset - 3} remain"
                }
                out += WakeWord(
                    index = index,
                    kind = kind,
                    phrase = String(payload, offset + 3, length, Charsets.UTF_8),
                )
                offset += 3 + length
            }
            require(offset == payload.size) {
                "wake word list has ${payload.size - offset} trailing bytes"
            }
            return out
        }
    }

    /** Type 0 is an AI wake phrase, 1 a Bluetooth control, 2 a device control. */
    data class WakeWord(val index: Int, val kind: Int, val phrase: String)
}
