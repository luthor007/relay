# M01 Pro protocol — what is verified and what is not

Sources: `sdk/protocol/通信协议v2.0.17(1)(4).pdf` (109 pages, Shenzhen QC.wireless /
禾胜成, revised 2026-01-21) and the shipping `sdk/ios/QCSDKDemo/SDK/QCSDK.framework`
(arm64 static archive).

The protocol package was written without the glasses in hand. This file records
exactly how much confidence each constant deserves, so nobody has to re-derive it
and nobody trusts it further than it earned.

## Verified against the compiled SDK

**CRC-16 initial value = `0xFFFF`.** This is the one that would have cost a day.

The spec's appendix (附：CRC 校验参考) publishes a reference implementation copied
verbatim from the Linux kernel's `lib/crc16.c` — table for reflected polynomial
0xA001, applied as `crc = (crc >> 8) ^ table[(crc ^ byte) & 0xff]`. It never
states an initial value; the function takes it as an argument, and the Linux
original is always called with 0, which would make this CRC-16/ARC.

Disassembling `-[NSData(CRC16Checksum) qg_crc16Checksum]` shows otherwise:

```
mov   w8, #0xffff
sturh w8, [x29, #-0x12]        ; crc = 0xFFFF
...
ldurh w0, [x29, #-0x12]        ; return crc — no final XOR
```

So the wire algorithm is **CRC-16/MODBUS**, not ARC. Initialising to 0 produces a
different checksum for every non-empty input, and the glasses would silently
reject every frame. `test_is_not_crc16_arc` guards the constant.

The vendor's 256-entry table is also embedded in the framework binary at file
offset `0x77270`, byte-identical to the one printed in the appendix.

## Read off the spec, not yet confirmed on hardware

These follow the document; none has been checked against a live capture, because
the five units are in Quebec. Each is a one-line change.

| Assumption | Source | Where to change |
|---|---|---|
| CRC covers **`data` only**, not prefix or length | literal reading of "data 数据 CRC-16的值" (§三) | `frame.CRC_COVERS`, `test_crc_covers_data_only_not_the_header` |
| CRC is transmitted **little-endian** | inherits the blanket §一 字节序 rule; spec shows no worked example | `frame.encode_frame` / `decode_frame` |
| Advertisement `vendor_id`/`product_id` are little-endian | same blanket rule | `ble.parse_advertisement` |

The spec contains **no worked example frame anywhere** — no hex dump to check a
codec against. The first real capture should be diffed against
`test_golden_frame_for_get_supported_features`.

Golden frames this codec currently produces (seq starting at 0):

```
GET_SUPPORTED_FEATURES   a5 0600 050001000000 01b2
GET_BATTERY              a5 0600 010101010000 6c36
AI_PHOTO_START           a5 0600 060901020000 7c40
PREVIEW_CONTROL          a5 0600 0a0901030000 2d4c
```

## Command IDs

All 92 in the document are transcribed into `Command`, including those marked
已弃用 (deprecated) and 未使用 (unused) — older firmware may still emit them.
`test_command_ids_are_unique` asserts the count and that no two alias.

Three TOC entries extracted as truncated text and were resolved from their body
sections rather than the contents page: `0x090B` = WIFI AP 控制, `0x0919` =
WIFI P2P 控制, `0x0D01` = 设备控制命令.

## The station-mode question, settled

AGENT-BRIEF §8-D flagged this as deciding two weeks of work: *do the glasses
support WiFi station mode?*

**No.** Searching all 109 pages, the only WiFi configuration commands are
`0x0901` 设置 WIFI SSID and `0x0902` 设置 WIFI 密码 — both marked 已弃用, and both
setting the glasses' **own hotspot** (设置热点 SSID / 设置热点密码). There is no
join-an-access-point command. `0x090B` opens and closes the device's AP;
`0x0919` does the same for WiFi P2P.

The iOS SDK agrees: `QCSDKWiFiManager` connects *the phone* to the glasses' SSID
and defaults `serverIP` to `192.168.31.1`, with RTSP served at `:8554`. Android
uses `WifiP2pManagerSingleton` for the same purpose.

Consequences:

1. A hosted VPS can never reach the glasses directly. Any always-on product needs
   a local bridge holding a radio that joins the glasses' AP — and, since joining
   it costs that host its normal WiFi uplink, a second interface for internet.
2. **BLE alone carries photo, microphone, speaker routing and touch events.**
   Only live video needs the AP. An MCP server built on BLE needs no WiFi, no
   phone, and no App Store review — which is why the protocol layer came first.

## Not yet implemented

This package is the codec only: framing, checksums, packet structure, command
IDs, advertisement parsing. Deliberately no I/O, so it stays testable without
hardware.

Still to build:
- Transport — bleak client, connect/notify/write, request-response matching on
  the sequence number, heartbeat (`0x0007`)
- Payload codecs for individual commands (only the 6-byte headers are modelled;
  each command's payload body still needs transcribing from its spec section)
- Capability gating on `0x0005` 获取支持功能 before issuing anything else
- The MCP server proper — `look()` / `listen()` / `say()`
