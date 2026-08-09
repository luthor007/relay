#!/usr/bin/env python3
"""capture_trace.py — turn a real pair of glasses into replayable test fixtures.

This is the day-one-with-hardware instrument. It does three things:

    scan                    find glasses by their HSC advertisement
    probe   ADDR            connect, dump GATT, report capabilities
    record  ADDR -o FILE    log every frame of a session into a trace
    measure ADDR            walk through the open questions and print answers

The trace it writes is the format `glasses/bridge` replays, so a session
recorded once becomes a fixture the app is developed against forever after —
including on machines with no glasses attached, which is most of them.

Frames are recorded verbatim as ground truth. Decoded `events` are emitted only
where the payload layout is confirmed; everything else stays in `frames` for
later decoding rather than being guessed at now.

    pip install bleak
    python tools/capture_trace.py scan

On macOS, addresses are opaque CoreBluetooth UUIDs rather than MACs. Use
whatever `scan` prints.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import sys
import time
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from glasses.protocol import (  # noqa: E402
    Command,
    CommandType,
    FrameParser,
    NOTIFY_CHAR_UUID,
    Packet,
    SERVICE_UUID,
    SequenceCounter,
    WRITE_CHAR_UUID,
    parse_bleak_manufacturer_data,
)

TRACE_VERSION = 1


def _require_bleak():
    """Import bleak on demand so the rest of this module stays importable."""
    try:
        from bleak import BleakClient, BleakScanner
    except ImportError:  # pragma: no cover - environment dependent
        sys.exit("bleak is not installed.  pip install bleak")
    return BleakClient, BleakScanner


class TraceRecorder:
    """Accumulates frames and events with millisecond timestamps."""

    def __init__(self, notes: str = "") -> None:
        self._t0 = time.perf_counter()
        self.frames: list[dict[str, Any]] = []
        self.events: list[dict[str, Any]] = []
        self.notes = notes
        self.device: dict[str, str] = {}

    def _ms(self) -> int:
        return int((time.perf_counter() - self._t0) * 1000)

    def frame(self, direction: str, raw: bytes, note: str | None = None) -> None:
        entry: dict[str, Any] = {"tMs": self._ms(), "dir": direction, "hex": raw.hex()}
        if note:
            entry["note"] = note
        self.frames.append(entry)

    def event(self, name: str, payload: Any) -> None:
        self.events.append({"tMs": self._ms(), "event": name, "payload": payload})

    def to_json(self) -> str:
        return json.dumps(
            {
                "version": TRACE_VERSION,
                "recordedAt": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
                "device": self.device or None,
                "notes": self.notes or None,
                "frames": self.frames,
                "events": self.events,
            },
            indent=2,
        )

    def write(self, path: Path) -> None:
        path.write_text(self.to_json())
        print(
            f"\nwrote {path}  ({len(self.frames)} frames, {len(self.events)} events, "
            f"{self._ms() / 1000:.1f}s)"
        )


class GlassesSession:
    """A connected device: framed writes, matched responses, recorded traffic."""

    def __init__(self, client, recorder: TraceRecorder) -> None:
        self._client = client
        self._recorder = recorder
        self._parser = FrameParser()
        self._seq = SequenceCounter()
        self._waiters: dict[int, asyncio.Future] = {}
        self._loop = asyncio.get_running_loop()

    async def start(self) -> None:
        await self._client.start_notify(NOTIFY_CHAR_UUID, self._on_notify)

    def _on_notify(self, _sender, data: bytearray) -> None:
        raw = bytes(data)
        self._recorder.frame("rx", raw)
        for block in self._parser.feed(raw):
            try:
                packet = Packet.decode(block)
            except Exception as exc:  # noqa: BLE001 - log and keep the stream alive
                print(f"  ! undecodable packet: {exc}  {block.hex()}")
                continue
            print(f"  <- {packet}")
            waiter = self._waiters.pop(packet.seq, None)
            if waiter and not waiter.done():
                waiter.set_result(packet)

    async def request(self, command: Command, payload: bytes = b"", timeout: float = 5.0) -> Packet:
        """Send a request and wait for the response carrying the same sequence number."""
        seq = self._seq.next()
        packet = Packet(command, CommandType.REQUEST, seq, payload)
        wire = packet.to_frame()

        future: asyncio.Future = self._loop.create_future()
        self._waiters[seq] = future

        self._recorder.frame("tx", wire, command.name)
        print(f"  -> {packet}")
        await self._client.write_gatt_char(WRITE_CHAR_UUID, wire, response=False)

        try:
            return await asyncio.wait_for(future, timeout)
        except asyncio.TimeoutError:
            self._waiters.pop(seq, None)
            raise TimeoutError(f"no response to {command.name} within {timeout}s") from None

    async def try_request(self, command: Command, payload: bytes = b"", timeout: float = 5.0):
        """Like `request`, but returns None on timeout instead of raising."""
        try:
            return await self.request(command, payload, timeout)
        except TimeoutError:
            print(f"  .. {command.name} timed out")
            return None


# --- commands ---------------------------------------------------------------


async def cmd_scan(seconds: float) -> None:
    _, BleakScanner = _require_bleak()
    print(f"scanning {seconds}s for HSC advertisements...\n")

    devices = await BleakScanner.discover(timeout=seconds, return_adv=True)
    found = 0
    for address, (device, adv) in sorted(
        devices.items(), key=lambda kv: kv[1][1].rssi or -999, reverse=True
    ):
        parsed = parse_bleak_manufacturer_data(dict(adv.manufacturer_data or {}))
        is_ours = parsed is not None or SERVICE_UUID.lower() in {
            u.lower() for u in (adv.service_uuids or [])
        }
        if not is_ours:
            continue
        found += 1
        print(f"{address}  rssi={adv.rssi:>4}  {device.name or '(unnamed)'}")
        if parsed:
            print(
                f"    adv v{parsed.version}  mac={parsed.address_str}  "
                f"vendor=0x{parsed.vendor_id:04x}  product=0x{parsed.product_id:04x}  "
                f"color={parsed.color}"
            )
            if parsed.classic_bt_connected is not None:
                print(f"    classic BT connected: {parsed.classic_bt_connected}")

    if found == 0:
        print("no glasses found.  Are they powered on and out of their case?")
        print("(run tools/ble_probe.py scan to see every BLE device nearby)")


async def cmd_probe(address: str) -> None:
    BleakClient, _ = _require_bleak()
    recorder = TraceRecorder(notes="probe")

    async with BleakClient(address) as client:
        print(f"connected: {client.is_connected}   mtu: {client.mtu_size}\n")

        print("GATT services:")
        for service in client.services:
            marker = "  <-- glasses service" if service.uuid.lower() == SERVICE_UUID.lower() else ""
            print(f"  {service.uuid}{marker}")
            for ch in service.characteristics:
                print(f"    {ch.uuid}  [{','.join(ch.properties)}]")

        session = GlassesSession(client, recorder)
        await session.start()
        print("\ninterrogating device:")

        for command in (
            Command.GET_SUPPORTED_FEATURES,
            Command.GET_PRODUCT_INFO,
            Command.GET_VERSION,
            Command.GET_DEVICE_NAME,
            Command.GET_BATTERY,
            Command.GET_DISK_INFO,
        ):
            await session.try_request(command)
            await asyncio.sleep(0.3)

        print(
            "\nPayload layouts are not decoded yet — the hex above is what the "
            "device actually sent.\nDecode it into glasses/protocol/commands.py, "
            "then this becomes structured output."
        )


async def cmd_record(address: str, seconds: float, out: Path, notes: str) -> None:
    BleakClient, _ = _require_bleak()
    recorder = TraceRecorder(notes=notes or f"passive capture, {seconds}s")

    async with BleakClient(address) as client:
        session = GlassesSession(client, recorder)
        await session.start()
        recorder.event("connectionChanged", "connected")

        print(f"recording {seconds}s — use the glasses now.")
        print("tap, double-tap, put them on, take them off, start a recording...\n")
        await asyncio.sleep(seconds)

    recorder.write(out)
    print("Replay it with glasses/bridge:  new MockTransport({ trace })")


async def cmd_measure(address: str, out: Path | None) -> None:
    """Walk through the open questions from docs/APPS-SCOPE.md §8."""
    BleakClient, _ = _require_bleak()
    recorder = TraceRecorder(notes="measurement run")
    results: dict[str, Any] = {}

    async with BleakClient(address) as client:
        session = GlassesSession(client, recorder)
        await session.start()
        results["mtu"] = client.mtu_size
        print(f"negotiated MTU: {client.mtu_size}\n")

        # --- 1. capabilities and storage ------------------------------------
        print("[1/4] capabilities and storage")
        await session.try_request(Command.GET_SUPPORTED_FEATURES)
        await session.try_request(Command.GET_DISK_INFO)
        await session.try_request(Command.GET_BATTERY)

        # --- 2. recording format --------------------------------------------
        # The cheapest question with the biggest effect: Opus or PCM changes
        # storage headroom and sync duration by roughly 10x.
        print("\n[2/4] recording format — 60s local recording")
        started = time.perf_counter()
        if await session.try_request(Command.LOCAL_RECORDING_CONTROL, bytes([0x01])):
            await asyncio.sleep(60)
            await session.try_request(Command.LOCAL_RECORDING_CONTROL, bytes([0x00]))
            elapsed = time.perf_counter() - started
            await asyncio.sleep(1.0)
            await session.try_request(Command.GET_FILE_LIST)
            results["recording_seconds"] = round(elapsed, 1)
            print(
                "\n  -> read the file name and size out of the response above.\n"
                f"     bytes / {elapsed:.0f}s gives the rate; an .opus/.ogg name or\n"
                "     ~3 KB/s means Opus, ~32 KB/s means raw PCM."
            )
        else:
            print("  !! local recording control did not respond — check 0x0E04 support")

        # --- 3. photo throughput --------------------------------------------
        print("\n[3/4] BLE throughput via a photo")
        started = time.perf_counter()
        if await session.try_request(Command.AI_PHOTO_START, timeout=90.0):
            results["photo_seconds"] = round(time.perf_counter() - started, 1)
            print(f"  -> photo command round trip: {results['photo_seconds']}s")
            print("     divide the delivered byte count by this for real BLE throughput")

        # --- 4. passive mic ---------------------------------------------------
        print("\n[4/4] is the microphone continuous or session-gated?")
        print("  opening an AI chat session for 15s; watch for AUDIO_DATA frames")
        await session.try_request(Command.AI_CHAT_TRIGGER, bytes([0x01]))
        before = len(recorder.frames)
        await asyncio.sleep(15)
        during = len(recorder.frames) - before
        await session.try_request(Command.AI_CHAT_TRIGGER, bytes([0x00]))

        after_start = len(recorder.frames)
        await asyncio.sleep(15)
        after = len(recorder.frames) - after_start

        results["frames_during_session"] = during
        results["frames_after_session"] = after
        print(f"\n  frames while open: {during}   frames after closing: {after}")
        print(
            "  -> many during and ~none after means session-gated (capture Path A "
            "cannot run all day)\n"
            "  -> frames continuing after close would mean the mic streams "
            "independently"
        )

    print("\n" + "=" * 68)
    print("MEASUREMENTS")
    for key, value in results.items():
        print(f"  {key}: {value}")
    print("=" * 68)
    print("Update docs/APPS-SCOPE.md §8 and glasses/NOTES.md with these.")

    if out:
        recorder.notes = f"measurement run: {json.dumps(results)}"
        recorder.write(out)


def main() -> None:
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    sub = parser.add_subparsers(dest="command", required=True)

    p_scan = sub.add_parser("scan", help="find glasses nearby")
    p_scan.add_argument("--seconds", type=float, default=8.0)

    p_probe = sub.add_parser("probe", help="connect and report capabilities")
    p_probe.add_argument("address")

    p_record = sub.add_parser("record", help="record a session to a trace file")
    p_record.add_argument("address")
    p_record.add_argument("-o", "--out", type=Path, required=True)
    p_record.add_argument("--seconds", type=float, default=60.0)
    p_record.add_argument("--notes", default="")

    p_measure = sub.add_parser("measure", help="answer the open hardware questions")
    p_measure.add_argument("address")
    p_measure.add_argument("-o", "--out", type=Path, default=None)

    args = parser.parse_args()

    if args.command == "scan":
        asyncio.run(cmd_scan(args.seconds))
    elif args.command == "probe":
        asyncio.run(cmd_probe(args.address))
    elif args.command == "record":
        asyncio.run(cmd_record(args.address, args.seconds, args.out, args.notes))
    elif args.command == "measure":
        asyncio.run(cmd_measure(args.address, args.out))


if __name__ == "__main__":
    main()
