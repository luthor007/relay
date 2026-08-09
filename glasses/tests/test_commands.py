"""Command packet tests."""

import random

import pytest

from glasses.protocol.commands import Command, CommandType, Packet, SequenceCounter
from glasses.protocol.crc import crc16
from glasses.protocol.frame import ProtocolError, decode_frame


def test_packet_header_layout_is_exactly_the_spec_table():
    p = Packet(Command.GET_SUPPORTED_FEATURES, CommandType.REQUEST, seq=0x2A, payload=b"\xff")
    raw = p.encode()

    assert raw[0:2] == (0x0005).to_bytes(2, "little")  # offset 0: command id, LE
    assert raw[2] == 1                                 # offset 2: type (request)
    assert raw[3] == 0x2A                              # offset 3: sequence number
    assert raw[4:6] == (1).to_bytes(2, "little")       # offset 4: payload length, LE
    assert raw[6:] == b"\xff"                          # offset 6: payload


def test_golden_frame_for_get_supported_features():
    """A full request as it goes on the wire, end to end.

    0x0005 is the first call any client should make — it returns the capability
    bitmap that says which of the other 91 commands this firmware honours.
    """
    wire = Packet(Command.GET_SUPPORTED_FEATURES, CommandType.REQUEST, seq=0).to_frame()
    data = bytes.fromhex("050001000000")

    assert wire == bytes([0xA5]) + b"\x06\x00" + data + crc16(data).to_bytes(2, "little")
    assert decode_frame(wire)[0] == data
    assert Packet.decode(data).command == Command.GET_SUPPORTED_FEATURES


@pytest.mark.parametrize("ctype", list(CommandType))
@pytest.mark.parametrize("payload_len", [0, 1, 17, 500])
def test_round_trip(ctype, payload_len):
    rng = random.Random(payload_len)
    payload = bytes(rng.randrange(256) for _ in range(payload_len))
    original = Packet(Command.GET_BATTERY, ctype, seq=rng.randrange(256), payload=payload)
    assert Packet.decode(original.encode()) == original


def test_round_trip_through_a_frame():
    original = Packet(Command.SET_TIME, CommandType.REQUEST, seq=9, payload=b"\x01\x02\x03\x04")
    data, _ = decode_frame(original.to_frame())
    assert Packet.decode(data) == original


def test_decode_rejects_truncated_header():
    with pytest.raises(ProtocolError):
        Packet.decode(b"\x05\x00\x01\x00\x00")  # 5 bytes, header needs 6


def test_decode_rejects_length_field_disagreeing_with_payload():
    """Catches a truncated transfer that still passed CRC on a short read."""
    raw = bytearray(Packet(Command.GET_BATTERY, CommandType.REQUEST, 0, b"\x01\x02").encode())
    raw[4] = 99  # claim 99 payload bytes, carry 2
    with pytest.raises(ProtocolError, match="length field"):
        Packet.decode(bytes(raw))


def test_decode_rejects_unknown_command_type():
    raw = bytearray(Packet(Command.GET_BATTERY, CommandType.REQUEST, 0).encode())
    raw[2] = 7
    with pytest.raises(ProtocolError, match="unknown command type"):
        Packet.decode(bytes(raw))


def test_unknown_command_id_decodes_but_is_labelled():
    """Newer firmware may send commands this spec revision does not list."""
    raw = Packet(0xBEEF, CommandType.NOTIFY, seq=1).encode()
    packet = Packet.decode(raw)
    assert packet.command == 0xBEEF
    assert packet.name == "UNKNOWN_0xBEEF"


def test_known_command_is_named():
    assert Packet(Command.RTSP_URL, CommandType.NOTIFY, 0).name == "RTSP_URL"


@pytest.mark.parametrize("seq", [-1, 256])
def test_sequence_number_must_fit_a_byte(seq):
    with pytest.raises(ValueError):
        Packet(Command.HEARTBEAT, CommandType.REQUEST, seq=seq)


def test_command_ids_are_unique():
    """A copy-paste duplicate in a 92-entry enum would silently alias two commands.

    IntEnum aliases a duplicate value to the first name instead of raising, so
    this compares declared members against distinct values.
    """
    values = [c.value for c in Command]
    assert len(values) == len(set(values))
    assert len(values) == 92


def test_command_ids_are_16_bit():
    assert all(0 <= c.value <= 0xFFFF for c in Command)


def test_spot_check_ids_against_the_spec():
    """The handful this project actually depends on, per AGENT-BRIEF §5."""
    assert Command.GET_SUPPORTED_FEATURES == 0x0005
    assert Command.SET_TIME == 0x0903
    assert Command.PREVIEW_CONTROL == 0x090A
    assert Command.RTSP_URL == 0x0908
    assert Command.WIFI_AP_CONTROL == 0x090B
    assert Command.LOCAL_VIDEO_STATE_REPORT == 0x0D02
    assert Command.AI_PHOTO_START == 0x0906
    assert Command.AI_CHAT_TRIGGER == 0x0805


# --- sequence numbers -------------------------------------------------------


def test_sequence_counter_increments_and_wraps():
    counter = SequenceCounter(start=254)
    assert [counter.next() for _ in range(4)] == [254, 255, 0, 1]


def test_sequence_counter_covers_every_value_before_repeating():
    counter = SequenceCounter()
    assert sorted(counter.next() for _ in range(256)) == list(range(256))


def test_sequence_counter_rejects_bad_start():
    with pytest.raises(ValueError):
        SequenceCounter(start=256)
