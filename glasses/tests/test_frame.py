"""Framing tests, with emphasis on the streaming parser.

BLE delivers no message boundaries, so `FrameParser` is where a real link will
actually break: notifications split frames, coalesce them, drop them, and arrive
mid-frame after a device reboot. Each of those has a test here.
"""

import random

import pytest

from glasses.protocol.crc import crc16
from glasses.protocol.frame import (
    CRC_LEN,
    FRAME_PREFIX,
    HEADER_LEN,
    ChecksumError,
    FrameParser,
    MalformedFrame,
    decode_frame,
    encode_frame,
)


def test_frame_layout_is_exactly_the_spec_table():
    data = bytes([0x05, 0x00, 0x01, 0x00, 0x00, 0x00])
    frame = encode_frame(data)

    assert frame[0] == 0xA5                                    # offset 0: prefix
    assert frame[1:3] == len(data).to_bytes(2, "little")       # offset 1: length, LE
    assert frame[3 : 3 + len(data)] == data                    # offset 3: data
    assert frame[3 + len(data) :] == crc16(data).to_bytes(2, "little")  # CRC, LE
    assert len(frame) == HEADER_LEN + len(data) + CRC_LEN


def test_crc_covers_data_only_not_the_header():
    """Documents the spec reading: "data 数据 CRC-16的值".

    If a live capture ever shows otherwise, this is the test that should change
    first, and frame.CRC_COVERS with it.
    """
    data = b"\xde\xad\xbe\xef"
    frame = encode_frame(data)
    carried = int.from_bytes(frame[-2:], "little")
    assert carried == crc16(data)
    assert carried != crc16(frame[:-2])  # not over prefix+length+data


@pytest.mark.parametrize("size", [0, 1, 2, 6, 255, 256, 1024])
def test_round_trip(size):
    data = bytes(random.Random(size).randrange(256) for _ in range(size))
    decoded, consumed = decode_frame(encode_frame(data))
    assert decoded == data
    assert consumed == HEADER_LEN + size + CRC_LEN


def test_decode_rejects_bad_prefix():
    frame = bytearray(encode_frame(b"\x01\x02"))
    frame[0] = 0xA6
    with pytest.raises(MalformedFrame):
        decode_frame(bytes(frame))


def test_decode_raises_value_error_when_incomplete():
    """Incomplete is not malformed — the caller should wait for more bytes."""
    frame = encode_frame(b"\x01\x02\x03")
    for cut in range(1, len(frame)):
        with pytest.raises(ValueError) as exc:
            decode_frame(frame[:cut])
        assert not isinstance(exc.value, MalformedFrame)


def test_decode_detects_corrupt_payload():
    frame = bytearray(encode_frame(b"\x01\x02\x03\x04"))
    frame[4] ^= 0xFF
    with pytest.raises(ChecksumError) as exc:
        decode_frame(bytes(frame))
    assert exc.value.expected != exc.value.actual


def test_decode_detects_corrupt_checksum():
    frame = bytearray(encode_frame(b"\x01\x02\x03\x04"))
    frame[-1] ^= 0xFF
    with pytest.raises(ChecksumError):
        decode_frame(bytes(frame))


def test_trailing_bytes_are_left_for_the_caller():
    frame = encode_frame(b"\xaa")
    data, consumed = decode_frame(frame + b"leftover")
    assert data == b"\xaa"
    assert consumed == len(frame)


def test_oversized_data_is_rejected():
    with pytest.raises(MalformedFrame):
        encode_frame(b"\x00" * 0x10000)


# --- streaming parser -------------------------------------------------------


def test_parser_reassembles_a_frame_split_one_byte_at_a_time():
    frame = encode_frame(b"hello glasses")
    parser = FrameParser()
    out = []
    for i in range(len(frame)):
        out += parser.feed(frame[i : i + 1])
    assert out == [b"hello glasses"]
    assert parser.pending == 0


def test_parser_handles_several_frames_in_one_notification():
    payloads = [b"", b"\x01", b"\x02\x03", b"x" * 40]
    blob = b"".join(encode_frame(p) for p in payloads)
    assert FrameParser().feed(blob) == payloads


def test_parser_handles_a_frame_plus_the_head_of_the_next():
    first, second = encode_frame(b"AAAA"), encode_frame(b"BBBB")
    parser = FrameParser()
    assert parser.feed(first + second[:3]) == [b"AAAA"]
    assert parser.pending == 3
    assert parser.feed(second[3:]) == [b"BBBB"]


def test_parser_resyncs_past_leading_garbage():
    """A device that reboots mid-stream leaves junk in front of the next frame."""
    parser = FrameParser()
    assert parser.feed(b"\x11\x22\x33" + encode_frame(b"ok")) == [b"ok"]
    assert parser.resyncs == 1


def test_parser_recovers_after_a_corrupt_frame():
    good = encode_frame(b"good")
    bad = bytearray(encode_frame(b"bad!"))
    bad[4] ^= 0xFF
    parser = FrameParser()
    out = parser.feed(bytes(bad) + good)
    assert out == [b"good"]
    assert parser.checksum_errors == 1


def test_parser_does_not_stall_on_a_prefix_byte_inside_a_payload():
    """0xA5 is a legal payload byte; the length field, not the prefix, delimits."""
    payload = bytes([0xA5, 0xA5, 0xA5, 0x00, 0xA5])
    assert FrameParser().feed(encode_frame(payload)) == [payload]


def test_parser_survives_random_chunking():
    rng = random.Random(4242)
    payloads = [bytes(rng.randrange(256) for _ in range(rng.randrange(0, 50))) for _ in range(30)]
    blob = b"".join(encode_frame(p) for p in payloads)

    parser, out, i = FrameParser(), [], 0
    while i < len(blob):
        n = rng.randrange(1, 21)  # MTU-ish chunks
        out += parser.feed(blob[i : i + n])
        i += n
    assert out == payloads
    assert parser.pending == 0


def test_reset_drops_partial_state():
    parser = FrameParser()
    parser.feed(encode_frame(b"xyz")[:4])
    assert parser.pending > 0
    parser.reset()
    assert parser.pending == 0
    assert parser.feed(encode_frame(b"fresh")) == [b"fresh"]


def test_parser_discards_a_chunk_with_no_prefix_at_all():
    parser = FrameParser()
    assert parser.feed(bytes([FRAME_PREFIX ^ 0xFF]) * 100) == []
    assert parser.pending == 0
