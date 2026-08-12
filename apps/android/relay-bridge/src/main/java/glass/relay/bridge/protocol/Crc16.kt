package glass.relay.bridge.protocol

/**
 * CRC-16/MODBUS — poly 0xA001 reflected, **init 0xFFFF**, no final XOR.
 *
 * Ported from `glasses/protocol/crc.py`, which is the authority. That file's
 * header records why: the vendor spec publishes the Linux `lib/crc16.c` table
 * and never states the initial value, and the Linux original is always called
 * with 0 — which would make this CRC-16/ARC. Disassembling
 * `-[NSData(CRC16Checksum) qg_crc16Checksum]` in the shipping `QCSDK.framework`
 * shows `mov w8, #0xffff` before the loop.
 *
 * Initialising to 0 produces a different checksum for every non-empty input, so
 * the glasses would reject every frame we ever sent. It looks like a broken BLE
 * stack for a day. `glass.relay.bridge.protocol.ProtocolCodecTest` pins this
 * against vectors taken from the Python — `crc vectors from the python
 * implementation` and `empty input returns the initial value, not zero`, which
 * is this file's whole argument as one assert.
 */
object Crc16 {

    /** Reversed form of 0x8005 (x^16 + x^15 + x^2 + 1). */
    const val POLY_REFLECTED: Int = 0xA001

    /** Verified against the shipping iOS framework. Not 0x0000. */
    const val INIT: Int = 0xFFFF

    private val TABLE: IntArray = IntArray(256) { index ->
        var crc = index
        repeat(8) {
            crc = (crc ushr 1) xor (if (crc and 1 != 0) POLY_REFLECTED else 0)
        }
        crc
    }

    /**
     * CRC-16/MODBUS of [data], optionally continuing from [seed].
     *
     * The seed is exposed for the same reason the Python exposes it: BLE hands
     * you a buffer in pieces, and `of(whole) == of(tail, of(head))` for any
     * split point.
     */
    fun of(data: ByteArray, seed: Int = INIT): Int {
        var crc = seed
        for (byte in data) {
            crc = (crc ushr 8) xor TABLE[(crc xor (byte.toInt() and 0xFF)) and 0xFF]
        }
        return crc and 0xFFFF
    }

    fun of(data: ByteArray, from: Int, until: Int, seed: Int = INIT): Int {
        var crc = seed
        for (index in from until until) {
            crc = (crc ushr 8) xor TABLE[(crc xor (data[index].toInt() and 0xFF)) and 0xFF]
        }
        return crc and 0xFFFF
    }
}
