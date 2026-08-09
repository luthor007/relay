# MentraOS support — status: not yet

**Relay One is not MentraOS compatible today.** This directory is the work to
make it so. Nothing here is grounds for claiming otherwise on a page that takes
money.

## What MentraOS actually requires

Their own compatibility doc is unambiguous:

> A Bluetooth name or manufacturer-data prefix is a compatibility claim; do not
> add a prefix unless the hardware is validated and listed here.

Supported today: Even Realities G1, Mentra Live, Mentra Mach 1, Vuzix Z100,
Xingyi AR99. Unlisted project identifiers **must be rejected** — pairing fails
closed. So this is not a case of "it might work"; an unmodified Mentra app will
actively refuse Relay One, by design.

**Their public docs are stale.** The contributing guide still describes
`android_core` and a `SmartGlassesCommunicator` subclass. That layer no longer
exists on `dev`. The real integration surface today is:

```
cloud/packages/types/src/enums.ts                     DeviceTypes enum
cloud/packages/types/src/capabilities/<model>.ts      capability profile
cloud/packages/types/src/hardware.ts                  HARDWARE_CAPABILITIES map
mobile/src/app/pairing/select-glasses-model.tsx       the picker
mobile/src/components/brands/<Brand>Logo.tsx          brand mark
mobile/modules/bluetooth-sdk/                         native scan + transport
glasses-compatibility.md                              the claim, last
```

An earlier version of this directory held a `SmartGlassesCommunicator` subclass.
It was deleted rather than kept: writing against an architecture that no longer
exists is worse than writing nothing, because it looks finished.

## What is here

`patch/cloud/packages/types/src/capabilities/relay-one.ts` — a complete, honest
capability profile, written against their real `Capabilities` interface. Every
field is sourced from the vendor spec or the shipping SDK headers. The one
figure nobody has measured (unplugged runtime while recording) is marked
unmeasured instead of invented.

Two entries in it are deliberate and worth defending in review:

- `canStream: false` — the device does serve RTSP, but only from its own access
  point, which costs the phone its uplink. Advertising it as streamable would
  make apps reach for something that cannot run in the background.
- lights are not app-addressable — a capture indicator an app can switch off is
  not an indicator.

## What is left

1. `DeviceTypes.RELAY = "Relay One"` in `enums.ts`
2. `[relayOne.modelName]: relayOne` in `HARDWARE_CAPABILITIES`
3. Scan recognition in `mobile/modules/bluetooth-sdk` — Relay advertises a
   `0xFF` payload starting `"HSC"` rather than a SIG company identifier, so every
   BLE stack mis-splits it the same way and the filter has to account for that
4. Transport: framing, CRC-16/MODBUS, command IDs — all already implemented and
   tested in `glasses/protocol/`, needing a port to their Kotlin/Swift module
5. Picker entry plus a Relay brand mark
6. `glasses-compatibility.md` row — **last**, and only after step 7
7. Validation on physical hardware

Steps 1–5 are mechanical. Step 7 is the gate, and it is theirs, not ours.

## The claim rule

Until this is merged and run on a real pair:

- the landing page does not say "MentraOS compatible"
- it may say a driver is in progress, because that is true and this is the evidence
