"""Outer wire framing for the M01 Pro BLE link.

Spec §三 连接交互 (数据交互格式):

    Offset  Length  Value   Description
    0       1       0xa5    命令前缀   (command prefix)
    1       2       --      数据长度   (length of `data`)
    3       N       --      数据       (data)
    3+N     2       --      data 数据 CRC-16 的值

Spec §一 字节序: multi-byte fields are little-endian unless stated otherwise.

Two details the spec states but which have not been confirmed against a live
device, because the glasses are not physically here yet. Both are one-line
changes if a capture disagrees:

  1. The CRC covers **`data` only** — not the prefix and not the length field.
     That is the literal reading of "data 数据 CRC-16的值", and it is what this
     module implements. See `CRC_COVERS` if it turns out to span the header.
  2. The CRC is transmitted little-endian, per the blanket §一 rule. The spec
     never shows a worked example frame, so this inherits the general rule
     rather than being independently attested.

`FrameParser` exists because BLE gives you no message boundaries. A single
notification can carry a partial frame, several frames, or a frame plus the head
of the next one, and the MTU (see `+[QGSDKManager setSendPocketMTU:]`) is
negotiated per connection. Anything that assumes one notification is one frame
will work on the bench and fail under load.
"""

from __future__ import annotations

from typing import Iterator

from .crc import crc16

__all__ = [
    "FRAME_PREFIX",
    "HEADER_LEN",
    "CRC_LEN",
    "MIN_FRAME_LEN",
    "MAX_DATA_LEN",
    "ProtocolError",
    "ChecksumError",
    "MalformedFrame",
    "encode_frame",
    "decode_frame",
    "FrameParser",
]

FRAME_PREFIX = 0xA5
HEADER_LEN = 3  # prefix (1) + length (2)
CRC_LEN = 2
MIN_FRAME_LEN = HEADER_LEN + CRC_LEN

# The length field is 16 bits, so this is a protocol ceiling, not a policy.
MAX_DATA_LEN = 0xFFFF

# Documents assumption (1) above in one place. If a capture shows the checksum
# spanning the header, this becomes slice(0, HEADER_LEN + n) and the tests that
# reference it will tell you what else needs to move.
CRC_COVERS = "data"


class ProtocolError(Exception):
    """Base class for anything wrong on the wire."""


class ChecksumError(ProtocolError):
    """A structurally valid frame whose CRC did not match."""

    def __init__(self, expected: int, actual: int) -> None:
        super().__init__(f"CRC mismatch: frame carries 0x{expected:04x}, computed 0x{actual:04x}")
        self.expected = expected
        self.actual = actual


class MalformedFrame(ProtocolError):
    """Bytes that cannot be a frame at all (bad prefix, impossible length)."""


def encode_frame(data: bytes) -> bytes:
    """Wrap `data` in prefix, length and CRC, ready to write to the BLE characteristic."""
    if len(data) > MAX_DATA_LEN:
        raise MalformedFrame(f"data is {len(data)} bytes, protocol allows at most {MAX_DATA_LEN}")
    return b"".join(
        (
            bytes([FRAME_PREFIX]),
            len(data).to_bytes(2, "little"),
            data,
            crc16(data).to_bytes(2, "little"),
        )
    )


def decode_frame(buf: bytes) -> tuple[bytes, int]:
    """Decode one frame from the front of `buf`.

    Returns `(data, consumed)`. Raises `MalformedFrame` if `buf` does not start
    with a plausible frame header, or `ChecksumError` if it does but the CRC is
    wrong. Raises `ValueError` if the frame is structurally fine but `buf` is
    simply too short to hold all of it yet — callers streaming from BLE should
    use `FrameParser`, which treats that as "wait for more" rather than an error.
    """
    if len(buf) < HEADER_LEN:
        raise ValueError(f"need at least {HEADER_LEN} bytes for a header, have {len(buf)}")
    if buf[0] != FRAME_PREFIX:
        raise MalformedFrame(f"expected prefix 0x{FRAME_PREFIX:02x}, got 0x{buf[0]:02x}")

    n = int.from_bytes(buf[1:3], "little")
    total = HEADER_LEN + n + CRC_LEN
    if len(buf) < total:
        raise ValueError(f"frame declares {n} data bytes, need {total} total, have {len(buf)}")

    data = buf[HEADER_LEN : HEADER_LEN + n]
    carried = int.from_bytes(buf[HEADER_LEN + n : total], "little")
    computed = crc16(data)
    if carried != computed:
        raise ChecksumError(carried, computed)
    return data, total


class FrameParser:
    """Reassembles frames from an arbitrarily chunked BLE notification stream.

    Feed it whatever bytes arrive; it yields the `data` block of each complete,
    CRC-valid frame. It owns a buffer, so one parser belongs to one connection.

    On corruption it resynchronises by discarding one byte and hunting for the
    next 0xA5, rather than giving up on the stream. A device that reboots
    mid-frame, or a dropped notification, would otherwise wedge the link
    permanently.
    """

    def __init__(self) -> None:
        self._buf = bytearray()
        self.checksum_errors = 0
        self.resyncs = 0

    def feed(self, chunk: bytes) -> list[bytes]:
        """Append `chunk` and return every complete frame's data block, in order."""
        self._buf.extend(chunk)
        return list(self._drain())

    def _drain(self) -> Iterator[bytes]:
        while True:
            # Hunt for a prefix. Anything before it is unrecoverable garbage.
            if not self._buf:
                return
            if self._buf[0] != FRAME_PREFIX:
                idx = self._buf.find(bytes([FRAME_PREFIX]))
                if idx == -1:
                    self._buf.clear()
                    self.resyncs += 1
                    return
                del self._buf[:idx]
                self.resyncs += 1
                continue

            try:
                data, consumed = decode_frame(bytes(self._buf))
            except ValueError:
                return  # incomplete — wait for the next notification
            except ChecksumError:
                # Structure was right, contents were not. Skip this prefix byte
                # so the hunt above can find the next candidate.
                self.checksum_errors += 1
                del self._buf[:1]
                continue

            del self._buf[:consumed]
            yield data

    @property
    def pending(self) -> int:
        """Bytes buffered but not yet forming a complete frame."""
        return len(self._buf)

    def reset(self) -> None:
        """Drop buffered bytes. Call on reconnect."""
        self._buf.clear()
