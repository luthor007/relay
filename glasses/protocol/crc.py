"""CRC-16 as the M01 Pro glasses actually compute it.

The vendor spec (通信协议 v2.0.17, appendix 附：CRC 校验参考) publishes a
reference implementation lifted verbatim from the Linux kernel's `lib/crc16.c`:
a table for the reflected polynomial 0xA001 (that is, 0x8005 reversed), applied
as `crc = (crc >> 8) ^ table[(crc ^ byte) & 0xff]`.

What the spec does NOT publish is the initial value — its reference function
takes `crc` as a parameter and the document never shows a worked example. The
Linux original is always called with 0, which would make this CRC-16/ARC.

It is not. Disassembling `-[NSData(CRC16Checksum) qg_crc16Checksum]` in the
shipping `QCSDK.framework` shows the initial value loaded as a literal:

    mov   w8, #0xffff
    sturh w8, [x29, #-0x12]        ; crc = 0xFFFF
    ...
    asr   w8, w8, #8               ; crc >> 8
    ldurb w9, [x29, #-0x12]        ; crc & 0xff
    eor   w10, w9, w10             ; (crc & 0xff) ^ byte
    ldrh  w9, [x9, w10, sxtw #1]   ; table[...]
    eor   w8, w8, w9
    ...
    ldurh w0, [x29, #-0x12]        ; return crc, no final XOR

So the wire algorithm is **CRC-16/MODBUS**: poly 0xA001 reflected, init 0xFFFF,
no final XOR, reflected in and out. Initialising to 0 instead produces a
different checksum for every non-empty input, and the glasses would reject every
frame — which is exactly the kind of thing that looks like "the BLE stack is
broken" for a day.
"""

from __future__ import annotations

__all__ = ["CRC16_INIT", "CRC16_POLY_REFLECTED", "crc16", "CRC16_TABLE"]

# Reversed form of 0x8005 (x^16 + x^15 + x^2 + 1).
CRC16_POLY_REFLECTED = 0xA001

# Verified against the shipping iOS framework — see module docstring.
CRC16_INIT = 0xFFFF


def _build_table() -> tuple[int, ...]:
    table = []
    for byte in range(256):
        crc = byte
        for _ in range(8):
            crc = (crc >> 1) ^ (CRC16_POLY_REFLECTED if crc & 1 else 0)
        table.append(crc)
    return tuple(table)


CRC16_TABLE = _build_table()


def crc16(data: bytes, crc: int = CRC16_INIT) -> int:
    """Return the CRC-16/MODBUS of `data`.

    `crc` is exposed so a checksum can be continued across a buffer that arrives
    in pieces — `crc16(b) == crc16(b[n:], crc16(b[:n]))` for any split point.
    """
    for byte in data:
        crc = (crc >> 8) ^ CRC16_TABLE[(crc ^ byte) & 0xFF]
    return crc
