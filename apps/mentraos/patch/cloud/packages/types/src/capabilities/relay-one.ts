/**
 * @fileoverview Relay One Hardware Capabilities
 *
 * Capability profile for Relay One (UU Lab), built on Shenzhen QC.wireless
 * M01 Pro class hardware.
 *
 * Every value here comes from the vendor specification or the shipping SDK
 * headers, not from estimation. Anything unconfirmed on a physical unit is
 * called out in a comment rather than guessed — a capability profile is what
 * other people's apps branch on, so an optimistic entry here becomes a runtime
 * failure in someone else's code.
 *
 * Drop into: cloud/packages/types/src/capabilities/relay-one.ts
 */

import type { Capabilities } from "../hardware";

export const relayOne: Capabilities = {
  modelName: "Relay One",

  // --- camera --------------------------------------------------------------
  // Stills and video, recorded to the device's own 4 GB. Immediate delivery
  // over BLE is possible but slow, so apps should prefer capture-then-sync for
  // anything nobody is waiting on.
  hasCamera: true,
  camera: {
    resolution: { width: 1920, height: 1080 },
    hasHDR: false,
    hasFocus: false,
    video: {
      canRecord: true,
      // No RTMP. The device serves RTSP from its own access point at
      // 192.168.31.1:8554, so the phone has to join that network and gives up
      // its own WiFi uplink while streaming. That makes it a deliberate,
      // foregrounded action rather than something an app can do in the
      // background — hence canStream: false.
      canStream: false,
      supportedStreamTypes: [],
      supportedResolutions: [
        { width: 1920, height: 1080 },
        { width: 1280, height: 720 },
      ],
    },
  },

  // --- display -------------------------------------------------------------
  // None. Text-first apps should fall back to the speaker rather than no-op.
  hasDisplay: false,
  display: null,

  // --- microphone ----------------------------------------------------------
  // Two mics with hardware noise cancellation. Audio arrives as Opus or PCM at
  // 16 kHz mono. The device also runs its own recogniser and can return
  // recognised text directly, so an app may not need a cloud STT at all.
  hasMicrophone: true,
  microphone: {
    count: 2,
    hasVAD: true,
  },

  // --- speaker -------------------------------------------------------------
  // Open-ear, and an ordinary Bluetooth audio sink, so TTS needs no special
  // transport. Output routing between glasses and phone is switchable.
  hasSpeaker: true,
  speaker: {
    count: 1,
    isPrivate: true,
  },

  // --- IMU -----------------------------------------------------------------
  // Gyroscope and accelerometer, exposed through the vendor stabilisation
  // pipeline with a pre-calibrated gyro-to-frame delay. No magnetometer.
  hasIMU: true,
  imu: {
    axisCount: 6,
    hasAccelerometer: true,
    hasGyroscope: true,
    hasCompass: false,
  },

  // --- buttons -------------------------------------------------------------
  // Two physical buttons plus a capacitive strip along the right temple. The
  // strip reports single, double and triple tap, long press, and swipe in both
  // directions — a richer surface than most glasses in this list.
  hasButton: true,
  button: {
    count: 3,
    buttons: [
      {
        type: "press",
        events: ["press", "long_press"],
        isCapacitive: false,
      },
      {
        type: "swipe1d",
        events: [
          "press",
          "double_press",
          "triple_press",
          "long_press",
          "swipe_forward",
          "swipe_backward",
        ],
        isCapacitive: true,
      },
    ],
  },

  // --- lights --------------------------------------------------------------
  // Front-facing indicators, lit while recording. Deliberately not addressable
  // by apps: a capture indicator an app can switch off is not an indicator.
  hasLight: true,
  light: {
    count: 2,
    lights: [
      {
        id: "privacy",
        purpose: "privacy",
        isFullColor: false,
        color: "white",
        position: "front_facing",
      },
      {
        id: "status",
        purpose: "user_feedback",
        isFullColor: false,
        color: "white",
        position: "front_facing",
      },
    ],
  },

  // --- power ---------------------------------------------------------------
  // Charges over magnetic USB-C and records while charging, which is what makes
  // all-day desk capture viable. No external battery pack or case.
  //
  // UNMEASURED: unplugged runtime under continuous recording. Deliberately not
  // asserted anywhere; measure on hardware before this profile is merged.
  hasPower: true,
  power: {
    hasExternalBattery: false,
  },

  // OTA firmware update is supported over both BLE and the device access point.
  hasOta: true,
};
