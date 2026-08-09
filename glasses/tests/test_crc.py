"""CRC tests.

The important one is `test_table_matches_bitwise_reference`: the shipped
implementation is table-driven, so a bug in table generation would be invisible
to any test that also uses the table. The bitwise version here is written
straight from the polynomial and shares no code with it.
"""

import random

import pytest

from glasses.protocol.crc import CRC16_INIT, CRC16_POLY_REFLECTED, CRC16_TABLE, crc16


def bitwise_crc16(data: bytes, crc: int = CRC16_INIT) -> int:
    """Independent reference: no lookup table, straight from the polynomial."""
    for byte in data:
        crc ^= byte
        for _ in range(8):
            if crc & 1:
                crc = (crc >> 1) ^ CRC16_POLY_REFLECTED
            else:
                crc >>= 1
    return crc


# First two rows of crc16_table[] exactly as printed in the spec appendix
# (附：CRC 校验参考). If our generator drifts, this fails first.
SPEC_TABLE_HEAD = [
    0x0000, 0xC0C1, 0xC181, 0x0140, 0xC301, 0x03C0, 0x0280, 0xC241,
    0xC601, 0x06C0, 0x0780, 0xC741, 0x0500, 0xC5C1, 0xC481, 0x0440,
]
# Last row from the appendix.
SPEC_TABLE_TAIL = [
    0x8201, 0x42C0, 0x4380, 0x8341, 0x4100, 0x81C1, 0x8081, 0x4040,
]


def test_table_head_matches_spec_appendix():
    assert list(CRC16_TABLE[: len(SPEC_TABLE_HEAD)]) == SPEC_TABLE_HEAD


def test_table_tail_matches_spec_appendix():
    assert list(CRC16_TABLE[-len(SPEC_TABLE_TAIL) :]) == SPEC_TABLE_TAIL


def test_table_is_256_entries_of_16_bits():
    assert len(CRC16_TABLE) == 256
    assert all(0 <= v <= 0xFFFF for v in CRC16_TABLE)


def test_table_matches_bitwise_reference():
    rng = random.Random(20260808)
    for _ in range(500):
        data = bytes(rng.randrange(256) for _ in range(rng.randrange(0, 64)))
        assert crc16(data) == bitwise_crc16(data), data.hex()


def test_modbus_check_value():
    """CRC-16/MODBUS is defined by check("123456789") == 0x4B37."""
    assert crc16(b"123456789") == 0x4B37


def test_is_not_crc16_arc():
    """Guard against someone "fixing" the init value back to the Linux default.

    The spec's reference implementation is Linux `lib/crc16.c`, which is always
    called with init 0 — that variant is CRC-16/ARC, check value 0xBB3D. The
    shipping iOS framework initialises to 0xFFFF instead (see crc.py). If this
    test ever fails, the wire format changed or someone regressed the constant.
    """
    assert CRC16_INIT == 0xFFFF
    assert crc16(b"123456789", crc=0x0000) == 0xBB3D  # what ARC would give
    assert crc16(b"123456789") != 0xBB3D


def test_empty_input_returns_init():
    assert crc16(b"") == CRC16_INIT


def test_incremental_equals_whole():
    """Needed if a checksum is ever continued across a chunked buffer."""
    rng = random.Random(7)
    data = bytes(rng.randrange(256) for _ in range(257))
    for split in (0, 1, 128, 256, 257):
        assert crc16(data[split:], crc16(data[:split])) == crc16(data)


@pytest.mark.parametrize("size", [1, 2, 3, 255, 256, 1024])
def test_output_always_16_bit(size):
    data = bytes(range(256)) * (size // 256 + 1)
    assert 0 <= crc16(data[:size]) <= 0xFFFF
