"""Advertisement parsing tests.

The bleak reassembly case is the one that matters in practice: the vendor puts
"HSC" where the Bluetooth SIG expects a 2-byte company identifier, so every
BLE library will mis-split the payload the same way.
"""

import pytest

from glasses.protocol.ble import (
    NOTIFY_CHAR_UUID,
    SERVICE_UUID,
    SPP_UUID,
    WRITE_CHAR_UUID,
    parse_advertisement,
    parse_bleak_manufacturer_data,
)

ADDRESS = bytes([0x11, 0x22, 0x33, 0x44, 0x55, 0x66])

V01 = b"HSC" + bytes([0x01]) + ADDRESS + b"\x34\x12" + b"\x78\x56" + b"\x02"
V02_CONNECTED = b"HSC" + bytes([0x02]) + ADDRESS + b"\x34\x12" + b"\x78\x56" + b"\x02\x01"
V02_DISCONNECTED = V02_CONNECTED[:-1] + b"\x00"


def test_parse_v01():
    adv = parse_advertisement(V01)
    assert adv.version == 1
    assert adv.address == ADDRESS
    assert adv.vendor_id == 0x1234    # little-endian
    assert adv.product_id == 0x5678
    assert adv.color == 2
    assert adv.classic_bt_connected is None  # V01 has no connection-state byte


def test_parse_v02_connection_state():
    assert parse_advertisement(V02_CONNECTED).classic_bt_connected is True
    assert parse_advertisement(V02_DISCONNECTED).classic_bt_connected is False


def test_address_str_matches_conventional_formatting():
    assert parse_advertisement(V01).address_str == "11:22:33:44:55:66"


def test_rejects_foreign_advertisement():
    with pytest.raises(ValueError, match="not an HSC"):
        parse_advertisement(b"\x4c\x00" + b"\x00" * 20)  # Apple company id


@pytest.mark.parametrize("cut", range(1, 15))
def test_rejects_truncated_v01(cut):
    with pytest.raises(ValueError):
        parse_advertisement(V01[:cut])


def test_rejects_v02_missing_its_extra_byte():
    with pytest.raises(ValueError, match="truncated"):
        parse_advertisement(V02_CONNECTED[:-1])


def test_rejects_unknown_version():
    with pytest.raises(ValueError, match="version"):
        parse_advertisement(b"HSC" + bytes([0x09]) + ADDRESS + b"\x00" * 5)


def test_bleak_manufacturer_data_reassembly():
    """bleak reads "HS" as company id 0x5348 and hands back the rest."""
    company_id = int.from_bytes(b"HS", "little")
    assert company_id == 0x5348
    adv = parse_bleak_manufacturer_data({company_id: V01[2:]})
    assert adv is not None
    assert adv.address == ADDRESS
    assert adv.product_id == 0x5678


def test_bleak_manufacturer_data_ignores_other_vendors():
    assert parse_bleak_manufacturer_data({0x004C: b"\x02\x15" + b"\x00" * 20}) is None


def test_bleak_manufacturer_data_picks_hsc_out_of_a_mixed_dict():
    company_id = int.from_bytes(b"HS", "little")
    mixed = {0x004C: b"\x02\x15", company_id: V02_CONNECTED[2:]}
    adv = parse_bleak_manufacturer_data(mixed)
    assert adv is not None and adv.version == 2


def test_bleak_manufacturer_data_on_empty_dict():
    assert parse_bleak_manufacturer_data({}) is None


def test_uuids_are_distinct_and_well_formed():
    uuids = [SERVICE_UUID, NOTIFY_CHAR_UUID, WRITE_CHAR_UUID, SPP_UUID]
    assert len(set(uuids)) == 4
    for u in uuids:
        assert len(u) == 36 and u.count("-") == 4


def test_spp_uuid_carries_the_hsc_magic():
    """48 53 43 00 is "HSC\\0" — same vendor magic as the advertisement."""
    assert SPP_UUID.startswith("48534300")
    assert bytes.fromhex("48534300")[:3] == b"HSC"
