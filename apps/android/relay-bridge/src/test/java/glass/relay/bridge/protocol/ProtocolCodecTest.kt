package glass.relay.bridge.protocol

import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The codec, pinned against the implementation that owns it.
 *
 * Every expected value in this file was produced by running
 * `glasses/protocol/{crc,frame,commands}.py` — the vectors are in the test
 * names and comments so a reader can regenerate them. A Kotlin port that
 * "looks right" and disagrees by one byte produces a device that ignores every
 * frame, which presents as a broken BLE stack rather than as a bug in this file.
 */
class ProtocolCodecTest {

    // --- CRC ----------------------------------------------------------------

    @Test
    fun `crc matches the python for the vendor's own check string`() {
        // crc16(b"123456789") == 0x4B37 — the standard CRC-16/MODBUS check value.
        assertEquals(0x4B37, Crc16.of("123456789".toByteArray()))
    }

    @Test
    fun `empty input returns the initial value, not zero`() {
        // This is the whole argument of glasses/protocol/crc.py in one assert.
        // CRC-16/ARC would give 0x0000 here and disagree on every other input.
        assertEquals(0xFFFF, Crc16.of(ByteArray(0)))
        assertEquals(Crc16.INIT, Crc16.of(ByteArray(0)))
    }

    @Test
    fun `crc vectors from the python implementation`() {
        assertEquals(0x40BF, Crc16.of(byteArrayOf(0x00)))
        assertEquals(0x466C, Crc16.of(byteArrayOf(0xA5.toByte(), 0x01, 0x00, 0x0E, 0x04)))
        assertEquals(0x4FA5, Crc16.of("hey chatgpt".toByteArray()))
    }

    @Test
    fun `a checksum can be continued across a split, because BLE splits things`() {
        val whole = "the quick brown fox".toByteArray()
        for (split in 0..whole.size) {
            val head = Crc16.of(whole.copyOfRange(0, split))
            val tail = Crc16.of(whole.copyOfRange(split, whole.size), seed = head)
            assertEquals("split at $split", Crc16.of(whole), tail)
        }
    }

    // --- frames -------------------------------------------------------------

    @Test
    fun `frame vectors from the python implementation`() {
        // encode_frame(b"") == a5 0000 ffff
        assertEquals("a50000ffff", encodeFrame(ByteArray(0)).hex())
        // encode_frame(bytes.fromhex("010e0100010001"))
        assertEquals(
            "a50700010e0100010001a72e",
            encodeFrame("010e0100010001".unhex()).hex(),
        )
    }

    @Test
    fun `packet and frame vectors for the commands the capture fork actually uses`() {
        // Packet(LOCAL_RECORDING_CONTROL, REQUEST, seq=7, payload=01) — Path B on.
        val recordOn = Packet(Command.LOCAL_RECORDING_CONTROL, PacketType.REQUEST, 7, byteArrayOf(1))
        assertEquals("040e0107010001", recordOn.encode().hex())
        assertEquals("a50700040e0107010001f35a", recordOn.toFrame().hex())

        // Packet(WIFI_AP_CONTROL, REQUEST, seq=0, payload=01) — open the AP.
        val apOn = Packet(Command.WIFI_AP_CONTROL, PacketType.REQUEST, 0, byteArrayOf(1))
        assertEquals("a507000b0901000100010c99", apOn.toFrame().hex())

        // Packet(GET_BATTERY, REQUEST, seq=255, payload=empty) — sequence wraps here.
        val battery = Packet(Command.GET_BATTERY, PacketType.REQUEST, 255)
        assertEquals("a50600010101ff00000dc6", battery.toFrame().hex())

        // Packet(SET_WAKEWORD_SETTING, REQUEST, seq=3, payload=00 01 01 00)
        val wake = Packet(Command.SET_WAKEWORD_SETTING, PacketType.REQUEST, 3, "00010100".unhex())
        assertEquals("a50a00030f010304000001010062e0", wake.toFrame().hex())
    }

    @Test
    fun `a frame decodes back to what went in`() {
        val data = "010e0100010001".unhex()
        val decoded = decodeFrame(encodeFrame(data))
        assertTrue(decoded is FrameDecode.Ok)
        assertArrayEquals(data, (decoded as FrameDecode.Ok).data)
        assertEquals(data.size + 5, decoded.consumed)
    }

    @Test
    fun `a flipped bit is a checksum error, not silent corruption`() {
        val frame = encodeFrame("0102030405".unhex())
        frame[5] = (frame[5].toInt() xor 0x01).toByte()
        val decoded = decodeFrame(frame)
        assertTrue("expected a checksum error, got $decoded", decoded is FrameDecode.ChecksumMismatch)
    }

    @Test
    fun `a short buffer is incomplete rather than wrong`() {
        // BLE hands you half a frame constantly. Treating that as an error turns
        // ordinary traffic into a stack trace.
        val frame = encodeFrame("0102030405".unhex())
        val decoded = decodeFrame(frame.copyOfRange(0, 6))
        assertTrue(decoded is FrameDecode.Incomplete)
    }

    @Test
    fun `a packet that lies about its payload length is rejected`() {
        val bad = "010101ff0400".unhex() + byteArrayOf(1) // declares 4, carries 1
        val error = runCatching { Packet.decode(bad) }.exceptionOrNull()
        assertTrue("expected a decode failure, got $error", error is IllegalArgumentException)
    }

    // --- the parser ---------------------------------------------------------

    @Test
    fun `the parser reassembles frames split across notifications`() {
        val one = encodeFrame("0102".unhex())
        val two = encodeFrame("030405".unhex())
        val stream = one + two

        val parser = FrameParser()
        val collected = mutableListOf<ByteArray>()
        // One byte at a time is the worst case, and it is the one that matters:
        // an MTU is negotiated per connection and nothing guarantees alignment.
        for (byte in stream) collected += parser.feed(byteArrayOf(byte))

        assertEquals(2, collected.size)
        assertArrayEquals("0102".unhex(), collected[0])
        assertArrayEquals("030405".unhex(), collected[1])
        assertEquals(0, parser.pending)
    }

    @Test
    fun `the parser resynchronises after garbage instead of wedging`() {
        val parser = FrameParser()
        val frames = parser.feed("00ff11".unhex() + encodeFrame("0102".unhex()))
        assertEquals(1, frames.size)
        assertArrayEquals("0102".unhex(), frames[0])
        assertTrue("should have counted a resync", parser.resyncs >= 1)
    }

    @Test
    fun `a corrupt frame does not eat the next one`() {
        // A device that reboots mid-frame must not wedge the link permanently.
        val corrupt = encodeFrame("0102".unhex()).also { it[4] = 0x77 }
        val good = encodeFrame("0304".unhex())

        val parser = FrameParser()
        val frames = parser.feed(corrupt + good)

        assertEquals(1, frames.size)
        assertArrayEquals("0304".unhex(), frames[0])
        assertEquals(1, parser.checksumErrors)
    }

    @Test
    fun `several frames in one notification all come out`() {
        val parser = FrameParser()
        val payloads = listOf("01", "0203", "040506").map { it.unhex() }
        val stream = payloads.map { encodeFrame(it) }.reduce { a, b -> a + b }
        val frames = parser.feed(stream)
        assertEquals(3, frames.size)
        payloads.forEachIndexed { index, expected -> assertArrayEquals(expected, frames[index]) }
    }

    // --- sequence -----------------------------------------------------------

    @Test
    fun `the sequence counter wraps at 255, as the spec says`() {
        val counter = SequenceCounter(254)
        assertEquals(254, counter.next())
        assertEquals(255, counter.next())
        assertEquals(0, counter.next())
        assertEquals(1, counter.peek())
    }

    // --- names --------------------------------------------------------------

    @Test
    fun `unknown ids are named the same way the python names them`() {
        assertEquals("UNKNOWN_0x1234", commandName(0x1234))
        assertEquals("LOCAL_RECORDING_CONTROL", commandName(0x0E04))
        assertTrue(Command.isKnown(0x0A03))
        assertTrue(!Command.isKnown(0x1234))
    }
}

internal fun ByteArray.hex(): String = joinToString("") { "%02x".format(it) }

internal fun String.unhex(): ByteArray =
    ByteArray(length / 2) {
        ((Character.digit(this[it * 2], 16) shl 4) or Character.digit(this[it * 2 + 1], 16)).toByte()
    }
