"""M01 Pro smart-glasses BLE protocol.

A pure codec: bytes in, bytes out, no I/O and no bleak dependency, so it can be
tested exhaustively without hardware and dropped behind any transport later.

    from glasses.protocol import Packet, Command, CommandType, SequenceCounter
    from glasses.protocol import FrameParser

    seq = SequenceCounter()
    wire = Packet(Command.GET_SUPPORTED_FEATURES, CommandType.REQUEST, seq.next()).to_frame()
    # -> write `wire` to WRITE_CHAR_UUID

    parser = FrameParser()
    for data in parser.feed(notification_bytes):   # handles split/merged frames
        packet = Packet.decode(data)

Transcribed from 通信协议 v2.0.17 (2026-01-21, Shenzhen QC.wireless / 禾胜成),
cross-checked against the shipping iOS `QCSDK.framework`. See `NOTES.md` for
which constants are verified against the binary and which are read off the spec
and still need a live capture to confirm.
"""

from .ble import (
    NOTIFY_CHAR_UUID,
    SERVICE_UUID,
    SPP_UUID,
    WRITE_CHAR_UUID,
    Advertisement,
    parse_advertisement,
    parse_bleak_manufacturer_data,
)
from .commands import Command, CommandType, Packet, SequenceCounter
from .crc import CRC16_INIT, crc16
from .frame import (
    ChecksumError,
    FrameParser,
    MalformedFrame,
    ProtocolError,
    decode_frame,
    encode_frame,
)

__all__ = [
    "Command",
    "CommandType",
    "Packet",
    "SequenceCounter",
    "crc16",
    "CRC16_INIT",
    "encode_frame",
    "decode_frame",
    "FrameParser",
    "ProtocolError",
    "ChecksumError",
    "MalformedFrame",
    "SERVICE_UUID",
    "NOTIFY_CHAR_UUID",
    "WRITE_CHAR_UUID",
    "SPP_UUID",
    "Advertisement",
    "parse_advertisement",
    "parse_bleak_manufacturer_data",
]
