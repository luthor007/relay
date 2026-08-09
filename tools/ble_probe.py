#!/usr/bin/env python3
"""
ble_probe.py — enumerate and listen to an undocumented BLE device.

No SDK required. BLE mandates a discoverable GATT tree, so any device that talks
BLE will tell you its services and characteristics before you know anything else
about it.

    pip install bleak
    python ble_probe.py scan
    python ble_probe.py dump   AA:BB:CC:DD:EE:FF
    python ble_probe.py listen AA:BB:CC:DD:EE:FF
    python ble_probe.py write  AA:BB:CC:DD:EE:FF <char-uuid> 01ff00

On macOS, addresses are opaque CoreBluetooth UUIDs, not MACs. Use whatever
`scan` prints.
"""

import asyncio
import sys
import time
from bleak import BleakScanner, BleakClient

# Vendor UUIDs that show up constantly in cheap hardware. If you see one of
# these, you are almost certainly looking at a serial pipe, not a real profile —
# which means the payload is a custom byte protocol you'll have to diff out.
KNOWN = {
    "6e400001-b5a3-f393-e0a9-e50e24dcca9e": "Nordic UART Service (NUS)",
    "6e400002-b5a3-f393-e0a9-e50e24dcca9e": "  NUS RX (you write here)",
    "6e400003-b5a3-f393-e0a9-e50e24dcca9e": "  NUS TX (notifies come from here)",
    "0000fee7-0000-1000-8000-00805f9b34fb": "Telink / generic CN OTA",
    "0000ffe0-0000-1000-8000-00805f9b34fb": "HM-10 style serial",
    "0000ffe1-0000-1000-8000-00805f9b34fb": "  HM-10 serial data",
    "0000180f-0000-1000-8000-00805f9b34fb": "Battery Service (SIG standard)",
    "0000180a-0000-1000-8000-00805f9b34fb": "Device Information (SIG standard)",
    "0000180d-0000-1000-8000-00805f9b34fb": "Heart Rate (SIG standard)",
    "00001812-0000-1000-8000-00805f9b34fb": "HID over GATT",
}


def annotate(uuid: str) -> str:
    u = uuid.lower()
    if u in KNOWN:
        return f"  <-- {KNOWN[u]}"
    # 16-bit SIG-assigned UUIDs share this base; anything else is proprietary.
    if u.endswith("0000-1000-8000-00805f9b34fb"):
        return "  <-- SIG-assigned (look it up in the Bluetooth assigned numbers)"
    return "  <-- proprietary 128-bit (vendor-specific)"


async def scan(seconds: float = 8.0):
    print(f"scanning {seconds}s...\n")
    devices = await BleakScanner.discover(timeout=seconds, return_adv=True)
    for addr, (dev, adv) in sorted(
        devices.items(), key=lambda kv: kv[1][1].rssi or -999, reverse=True
    ):
        print(f"{addr}  rssi={adv.rssi:>4}  {dev.name or '(no name)'}")
        if adv.manufacturer_data:
            for cid, data in adv.manufacturer_data.items():
                # Company ID is the best free hint about who actually made the
                # module inside a white-labelled device.
                print(f"    mfr 0x{cid:04x}: {data.hex()}")
        if adv.service_uuids:
            for u in adv.service_uuids:
                print(f"    adv svc {u}{annotate(u)}")
        print()


async def dump(address: str):
    async with BleakClient(address) as client:
        print(f"connected: {client.is_connected}")
        print(f"mtu: {client.mtu_size}\n")
        for service in client.services:
            print(f"SERVICE {service.uuid}{annotate(service.uuid)}")
            print(f"        {service.description}")
            for ch in service.characteristics:
                props = ",".join(ch.properties)
                print(f"  CHAR  {ch.uuid}  [{props}]{annotate(ch.uuid)}")
                print(f"        {ch.description}")
                if "read" in ch.properties:
                    try:
                        val = await client.read_gatt_char(ch.uuid)
                        print(f"        value: {val.hex()}  ascii={val!r}")
                    except Exception as e:
                        print(f"        read failed: {e}")
                for d in ch.descriptors:
                    print(f"    DESC {d.uuid}")
            print()


async def listen(address: str, seconds: float = 60.0):
    """Subscribe to every notify/indicate characteristic and log framed hex.

    This is the money shot. Leave it running while you use the device normally
    (press its buttons, move it, start a recording) and watch which
    characteristic lights up and how the bytes change.
    """
    t0 = time.perf_counter()

    def make_cb(uuid):
        state = {"n": 0, "last": None}

        def cb(_, data: bytearray):
            state["n"] += 1
            dt = time.perf_counter() - t0
            line = f"[{dt:8.3f}s] {uuid[:8]} #{state['n']:<5} len={len(data):<3} {data.hex()}"
            # Byte-level diff against the previous frame — this is how you find
            # counters, timestamps, and the fields that actually vary.
            prev = state["last"]
            if prev is not None and len(prev) == len(data):
                diff = "".join(
                    "^^" if a != b else "  " for a, b in zip(prev, data)
                )
                line += f"\n{' ' * (len(line.split(chr(10))[0]) - len(data.hex()))}{diff}"
            state["last"] = bytes(data)
            print(line, flush=True)

        return cb

    async with BleakClient(address) as client:
        subscribed = []
        for service in client.services:
            for ch in service.characteristics:
                if "notify" in ch.properties or "indicate" in ch.properties:
                    try:
                        await client.start_notify(ch.uuid, make_cb(ch.uuid))
                        subscribed.append(ch.uuid)
                        print(f"subscribed: {ch.uuid}")
                    except Exception as e:
                        print(f"subscribe failed {ch.uuid}: {e}")
        if not subscribed:
            print("\nNo notify characteristics. This device likely needs a "
                  "command written to it first — capture the vendor app's "
                  "traffic (Android HCI snoop log) to learn the wake-up bytes.")
            return
        print(f"\nlistening {seconds}s — use the device now\n")
        await asyncio.sleep(seconds)


async def write(address: str, char_uuid: str, hexbytes: str):
    payload = bytes.fromhex(hexbytes)
    async with BleakClient(address) as client:
        ch = client.services.get_characteristic(char_uuid)
        if ch is None:
            print(f"no such characteristic: {char_uuid}")
            return
        resp = "write" in ch.properties  # vs write-without-response
        await client.write_gatt_char(char_uuid, payload, response=resp)
        print(f"wrote {payload.hex()} to {char_uuid} (response={resp})")
        await asyncio.sleep(2.0)


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return
    cmd = sys.argv[1]
    if cmd == "scan":
        asyncio.run(scan())
    elif cmd == "dump":
        asyncio.run(dump(sys.argv[2]))
    elif cmd == "listen":
        secs = float(sys.argv[3]) if len(sys.argv) > 3 else 60.0
        asyncio.run(listen(sys.argv[2], secs))
    elif cmd == "write":
        asyncio.run(write(sys.argv[2], sys.argv[3], sys.argv[4]))
    else:
        print(__doc__)


if __name__ == "__main__":
    main()
