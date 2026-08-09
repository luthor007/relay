"""BLE transport constants and advertisement parsing.

Spec §二 蓝牙连接. The glasses expose one proprietary GATT service with a
notify characteristic (device->app) and a write characteristic (app->device),
plus a classic-Bluetooth SPP profile over RFCOMM for bulk transfer.

Note the SPP UUID begins `48534300` — that is ASCII "HSC\\0", the same magic that
prefixes the advertisement. 禾胜成 (He Sheng Cheng) is the protocol vendor.
"""

from __future__ import annotations

from dataclasses import dataclass

__all__ = [
    "SERVICE_UUID",
    "NOTIFY_CHAR_UUID",
    "WRITE_CHAR_UUID",
    "SPP_UUID",
    "ADV_MAGIC",
    "Advertisement",
    "parse_advertisement",
    "parse_bleak_manufacturer_data",
]

SERVICE_UUID = "01000100-0000-2000-8000-009078563412"
NOTIFY_CHAR_UUID = "02000200-0000-2000-8000-009178563412"  # READ — device notifies here
WRITE_CHAR_UUID = "03000300-0000-2000-8000-009278563412"   # WRITE — send commands here

# Classic Bluetooth SPP over RFCOMM.
SPP_UUID = "48534300-0000-2000-8000-0058494F4E47"

# The 0xFF manufacturer-data payload starts with this, not a Bluetooth SIG
# company identifier — see `parse_bleak_manufacturer_data`.
ADV_MAGIC = b"HSC"


@dataclass(frozen=True)
class Advertisement:
    """Decoded 0xFF manufacturer-specific advertising payload.

    Spec 广播协议 V01/V02:

        magic       3   0x48 0x53 0x43 ("HSC")
        version     1   0x01 or 0x02
        address     6   addr[0-5]
        vendor_id   2
        product_id  2
        color       1
        conn_state  1   V02 only — bit0: classic Bluetooth connected
    """

    version: int
    address: bytes
    vendor_id: int
    product_id: int
    color: int
    classic_bt_connected: bool | None = None

    @property
    def address_str(self) -> str:
        """Colon-separated MAC, as printed by every other Bluetooth tool."""
        return ":".join(f"{b:02X}" for b in self.address)


_V01_LEN = 3 + 1 + 6 + 2 + 2 + 1  # 15
_V02_LEN = _V01_LEN + 1           # 16


def parse_advertisement(payload: bytes) -> Advertisement:
    """Parse a complete 0xFF AD payload, magic included.

    Raises `ValueError` if this is not an HSC advertisement or is truncated.
    """
    if not payload.startswith(ADV_MAGIC):
        raise ValueError(f"not an HSC advertisement: starts with {payload[:3]!r}")
    if len(payload) < _V01_LEN:
        raise ValueError(f"advertisement truncated: {len(payload)} bytes, need >= {_V01_LEN}")

    version = payload[3]
    if version not in (0x01, 0x02):
        raise ValueError(f"unknown advertisement version 0x{version:02x}")
    if version == 0x02 and len(payload) < _V02_LEN:
        raise ValueError(f"V02 advertisement truncated: {len(payload)} bytes, need {_V02_LEN}")

    address = payload[4:10]
    vendor_id = int.from_bytes(payload[10:12], "little")
    product_id = int.from_bytes(payload[12:14], "little")
    color = payload[14]
    connected = bool(payload[15] & 0x01) if version == 0x02 else None

    return Advertisement(
        version=version,
        address=address,
        vendor_id=vendor_id,
        product_id=product_id,
        color=color,
        classic_bt_connected=connected,
    )


def parse_bleak_manufacturer_data(manufacturer_data: dict[int, bytes]) -> Advertisement | None:
    """Recover an `Advertisement` from bleak's `AdvertisementData.manufacturer_data`.

    bleak follows the Bluetooth SIG convention and splits the 0xFF payload into
    {company_id: rest}, taking the first two bytes as a little-endian company
    identifier. This vendor does not use a company identifier — it puts "HSC"
    there — so the payload arrives keyed by 0x5348 ("HS" read little-endian)
    with the remainder starting at "C". This reassembles it.

    Returns None if no HSC advertisement is present, so it can be used as a
    filter while scanning.
    """
    for company_id, rest in manufacturer_data.items():
        reassembled = company_id.to_bytes(2, "little") + rest
        if reassembled.startswith(ADV_MAGIC):
            try:
                return parse_advertisement(reassembled)
            except ValueError:
                continue
    return None
