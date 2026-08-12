package glass.relay.bridge.protocol

/**
 * Wire framing and packet layout for the M01 Pro BLE link.
 *
 * Ported from `glasses/protocol/frame.py` and `glasses/protocol/commands.py`.
 * Those are the authority; this exists so the phone can *encode a command
 * without the vendor SDK's help* — which matters more than it sounds, because
 * it is the difference between "every glasses command by hand" being ~60
 * unverifiable SDK calls and being one call plus a codec with tests.
 *
 * Frame, spec §三:
 * ```
 *   0      1   0xA5   命令前缀
 *   1      2   --     数据长度 (length of data)
 *   3      N   --     数据
 *   3+N    2   --     CRC-16 of data
 * ```
 *
 * Packet — the `data` block — spec §四:
 * ```
 *   0      2   命令 ID       little-endian
 *   2      1   命令类型       1=request 2=response 3=notify
 *   3      1   Sequence      request increments 0-255, response echoes
 *   4      2   数据长度       little-endian
 *   6      N   payload
 * ```
 *
 * Two things inherited from the Python and still unattested against a live
 * device, both flagged there too: the CRC covers `data` only (the literal
 * reading of "data 数据 CRC-16的值"), and it is transmitted little-endian per the
 * blanket §一 byte-order rule. Neither has a worked example in the spec.
 */

const val FRAME_PREFIX: Int = 0xA5
const val FRAME_HEADER_LEN: Int = 3
const val FRAME_CRC_LEN: Int = 2
const val FRAME_MIN_LEN: Int = FRAME_HEADER_LEN + FRAME_CRC_LEN

/** The length field is 16 bits, so this is a protocol ceiling, not a policy. */
const val MAX_DATA_LEN: Int = 0xFFFF

const val PACKET_HEADER_LEN: Int = 6

/** 1=request, 2=response, 3=notify. Mirrors `CommandType` in commands.py. */
object PacketType {
    const val REQUEST: Int = 1
    const val RESPONSE: Int = 2
    const val NOTIFY: Int = 3

    fun name(value: Int): String = when (value) {
        REQUEST -> "request"
        RESPONSE -> "response"
        NOTIFY -> "notify"
        else -> "unknown($value)"
    }
}

/**
 * One decode attempt.
 *
 * Modelled as a result rather than an exception because `Incomplete` is the
 * ordinary case on a BLE link — a notification carrying half a frame is not an
 * error, and making it one turns normal traffic into a stack trace.
 */
sealed interface FrameDecode {
    data class Ok(val data: ByteArray, val consumed: Int) : FrameDecode {
        override fun equals(other: Any?): Boolean =
            this === other ||
                (other is Ok && consumed == other.consumed && data.contentEquals(other.data))

        override fun hashCode(): Int = 31 * consumed + data.contentHashCode()
    }

    /** Structurally plausible, just not all here yet. Wait for more bytes. */
    data class Incomplete(val needBytes: Int) : FrameDecode

    /** Cannot be a frame at all: bad prefix. */
    data class Malformed(val message: String) : FrameDecode

    /** Right shape, wrong contents. */
    data class ChecksumMismatch(val carried: Int, val computed: Int) : FrameDecode
}

/** Wrap [data] in prefix, length and CRC, ready for the BLE characteristic. */
fun encodeFrame(data: ByteArray): ByteArray {
    require(data.size <= MAX_DATA_LEN) {
        "data is ${data.size} bytes, protocol allows at most $MAX_DATA_LEN"
    }
    val crc = Crc16.of(data)
    val out = ByteArray(FRAME_HEADER_LEN + data.size + FRAME_CRC_LEN)
    out[0] = FRAME_PREFIX.toByte()
    out[1] = (data.size and 0xFF).toByte()
    out[2] = ((data.size ushr 8) and 0xFF).toByte()
    data.copyInto(out, FRAME_HEADER_LEN)
    out[FRAME_HEADER_LEN + data.size] = (crc and 0xFF).toByte()
    out[FRAME_HEADER_LEN + data.size + 1] = ((crc ushr 8) and 0xFF).toByte()
    return out
}

/** Decode one frame from the front of [buf]. */
fun decodeFrame(buf: ByteArray, offset: Int = 0): FrameDecode {
    val available = buf.size - offset
    if (available < FRAME_HEADER_LEN) return FrameDecode.Incomplete(FRAME_HEADER_LEN - available)
    if ((buf[offset].toInt() and 0xFF) != FRAME_PREFIX) {
        return FrameDecode.Malformed(
            "expected prefix 0x%02x, got 0x%02x".format(FRAME_PREFIX, buf[offset].toInt() and 0xFF),
        )
    }

    val declared = readU16Le(buf, offset + 1)
    val total = FRAME_HEADER_LEN + declared + FRAME_CRC_LEN
    if (available < total) return FrameDecode.Incomplete(total - available)

    val data = buf.copyOfRange(offset + FRAME_HEADER_LEN, offset + FRAME_HEADER_LEN + declared)
    val carried = readU16Le(buf, offset + FRAME_HEADER_LEN + declared)
    val computed = Crc16.of(data)
    if (carried != computed) return FrameDecode.ChecksumMismatch(carried, computed)
    return FrameDecode.Ok(data, total)
}

/**
 * Reassembles frames from an arbitrarily chunked BLE notification stream.
 *
 * BLE gives no message boundaries: one notification can carry a partial frame,
 * several frames, or a frame plus the head of the next. The MTU is negotiated
 * per connection. Anything that assumes one notification is one frame works on
 * the bench and fails under load.
 *
 * On corruption it resynchronises by discarding one byte and hunting for the
 * next 0xA5, exactly as the Python does. A device that reboots mid-frame would
 * otherwise wedge the link permanently.
 *
 * One parser per connection — it owns a buffer. Call [reset] on reconnect.
 */
class FrameParser {

    private var buffer = ByteArray(0)

    var checksumErrors: Int = 0
        private set

    var resyncs: Int = 0
        private set

    val pending: Int get() = buffer.size

    fun feed(chunk: ByteArray): List<ByteArray> {
        buffer = if (buffer.isEmpty()) chunk.copyOf() else buffer + chunk
        val frames = mutableListOf<ByteArray>()

        var cursor = 0
        loop@ while (true) {
            if (cursor >= buffer.size) break
            if ((buffer[cursor].toInt() and 0xFF) != FRAME_PREFIX) {
                val next = indexOfPrefix(buffer, cursor + 1)
                resyncs += 1
                if (next < 0) {
                    cursor = buffer.size
                    break@loop
                }
                cursor = next
                continue@loop
            }

            when (val decoded = decodeFrame(buffer, cursor)) {
                is FrameDecode.Ok -> {
                    frames += decoded.data
                    cursor += decoded.consumed
                }
                is FrameDecode.Incomplete -> break@loop
                is FrameDecode.ChecksumMismatch -> {
                    // Structure was right, contents were not. Step past this
                    // prefix byte so the hunt above finds the next candidate.
                    checksumErrors += 1
                    cursor += 1
                }
                is FrameDecode.Malformed -> cursor += 1
            }
        }

        buffer = if (cursor == 0) buffer else buffer.copyOfRange(cursor, buffer.size)
        return frames
    }

    fun reset() {
        buffer = ByteArray(0)
    }

    private fun indexOfPrefix(bytes: ByteArray, from: Int): Int {
        for (index in from until bytes.size) {
            if ((bytes[index].toInt() and 0xFF) == FRAME_PREFIX) return index
        }
        return -1
    }
}

/** The `data` block of a frame: a command with its payload. */
data class Packet(
    val commandId: Int,
    val type: Int = PacketType.REQUEST,
    val sequence: Int = 0,
    val payload: ByteArray = ByteArray(0),
) {
    init {
        require(commandId in 0..0xFFFF) { "command id out of range: $commandId" }
        require(sequence in 0..0xFF) { "sequence must fit one byte, got $sequence" }
        require(payload.size <= MAX_DATA_LEN - PACKET_HEADER_LEN) {
            "payload is ${payload.size} bytes, too large for one frame"
        }
    }

    /** Matches `Packet.name` in the Python so the two logs diff cleanly. */
    val name: String get() = commandName(commandId)

    fun encode(): ByteArray {
        val out = ByteArray(PACKET_HEADER_LEN + payload.size)
        out[0] = (commandId and 0xFF).toByte()
        out[1] = ((commandId ushr 8) and 0xFF).toByte()
        out[2] = type.toByte()
        out[3] = sequence.toByte()
        out[4] = (payload.size and 0xFF).toByte()
        out[5] = ((payload.size ushr 8) and 0xFF).toByte()
        payload.copyInto(out, PACKET_HEADER_LEN)
        return out
    }

    /** The whole thing, framed and checksummed, ready to write. */
    fun toFrame(): ByteArray = encodeFrame(encode())

    override fun equals(other: Any?): Boolean =
        this === other || (
            other is Packet &&
                commandId == other.commandId &&
                type == other.type &&
                sequence == other.sequence &&
                payload.contentEquals(other.payload)
            )

    override fun hashCode(): Int {
        var result = commandId
        result = 31 * result + type
        result = 31 * result + sequence
        result = 31 * result + payload.contentHashCode()
        return result
    }

    companion object {
        fun decode(data: ByteArray): Packet {
            require(data.size >= PACKET_HEADER_LEN) {
                "packet needs $PACKET_HEADER_LEN header bytes, got ${data.size}"
            }
            val declared = readU16Le(data, 4)
            require(data.size == PACKET_HEADER_LEN + declared) {
                "packet declares $declared payload bytes but carries ${data.size - PACKET_HEADER_LEN}"
            }
            return Packet(
                commandId = readU16Le(data, 0),
                type = data[2].toInt() and 0xFF,
                sequence = data[3].toInt() and 0xFF,
                payload = data.copyOfRange(PACKET_HEADER_LEN, data.size),
            )
        }
    }
}

/**
 * Request sequence numbers, 0-255 wrapping.
 *
 * The device echoes it on the response, so this is how a reply is matched to
 * the request that caused it. Wrapping is the spec's, not a shortcut.
 */
class SequenceCounter(private var value: Int = 0) {
    fun next(): Int {
        val current = value
        value = (value + 1) and 0xFF
        return current
    }

    fun peek(): Int = value
}

internal fun readU16Le(bytes: ByteArray, offset: Int): Int =
    (bytes[offset].toInt() and 0xFF) or ((bytes[offset + 1].toInt() and 0xFF) shl 8)
