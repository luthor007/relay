# MentraOS support — status: not yet

**Relay One is not MentraOS compatible today.** This directory is the work to
make it so. Nothing here should be used to claim otherwise on a page that takes
money.

## Where it actually stands

MentraOS supports G1, Vuzix, Even Realities and NIMO for display glasses, and
Mentra Live for camera glasses. Relay One is on none of those lists.

But it is an open, documented path — MentraOS is MIT licensed and ships a
contributing guide for exactly this. Adding a device means four things:

1. A `SmartGlassesCommunicator` subclass in `android_core`
2. A device entry in `supportedglasses/`
3. A case in `SmartGlassesRepresentative.createCommunicator()`
4. The model added to the mobile app's glasses picker

`RelayGlassesCommunicator.java` is step 1, written against the protocol work in
`glasses/protocol/` — the framing, the CRC-16/MODBUS variant, and the command
IDs, all of which have tests here.

## What is left

- Reconcile signatures against the real superclass in the MentraOS tree
- Port the frame codec to Java (`RelayFrameCodec`), or JNI the Python one
- Wire the BLE plumbing to their helper rather than raw GATT
- Map their event callbacks onto our notifications
- Test on hardware
- Open the PR

The protocol half — the part that took reverse engineering — is done and
portable. The rest is glue against someone else's interfaces, which is
mechanical but cannot be written blind.

## Why bother

MentraOS users are exactly our buyer, already own glasses, and already write
apps. Being a supported device there is distribution we do not have to build.
AGENT-BRIEF §8-B called this correctly: write the driver, open the PR, do not
fork.

## The claim rule

Until this is merged and someone has run it on a real pair:

- the landing page does not say "MentraOS compatible"
- it may say a driver is in progress, because that is true and this file is the evidence
