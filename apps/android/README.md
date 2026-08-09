# Android — `engram-bridge`

The always-on half of the Android host app. This is the piece the vendor SDK
does not provide at all: their sample is Activity-only, with **zero `<service>`
declarations**, so capture stops the moment the user switches apps.

```
engram-bridge/
  AndroidManifest.xml            typed FGS + the permissions API 34 requires
  EngramCaptureService.kt        the foreground service
  ConnectionSupervisor.kt        reconnect, heartbeat, wear→capture rules
  CaptureNotifications.kt        the standing recording indicator
  BootReceiver.kt                restart after reboot, if the user had it on
  BatteryOptimisation.kt         exemption flow + per-OEM escape hatches
  GlassesTransport.kt            the seam — mirrors glasses/bridge/src/transport.ts
  MockGlassesTransport.kt        glasses-free transport for debug builds
```

## Status — not yet compiled

There is no Gradle or Kotlin toolchain on the machine this was written on, so
**none of this has been built or run.** It is written against the documented
Android APIs and the vendor SDK surface, and the logic it encodes is the logic
the docs establish, but treat the first `./gradlew assembleDebug` as a real step
with real fixes in it rather than a formality.

The pure-logic parts — backoff, the wear→capture rules — have JVM unit tests
written and ready in `src/test/`.

## The things that are easy to get wrong

**Typed foreground services.** From API 34 a service must declare its type *and*
hold the matching permission, and `microphone` additionally requires
`RECORD_AUDIO` to be granted *before* `startForeground` is called. Getting it
wrong throws rather than degrading, so `EngramCaptureService.missingPermissions`
is checked first and the service refuses to start rather than crashing.

**`START_STICKY` is not enough.** Several OEM skins kill background work
regardless of foreground-service status. The wake lock, the boot receiver and the
battery-optimisation exemption all exist for that one reason, and
`BatteryOptimisation.manufacturerAdvice()` points the user at the specific
settings screen their device needs. This is the single largest source of "it
stopped recording overnight" reports for any capture app.

**`BLUETOOTH_SCAN` needs `neverForLocation`.** Without that flag, Android 12+
forces the app to hold location permission just to find the glasses — a prompt
with no honest justification.

**Recording survives disconnection.** The glasses write to their own 4 GB, so
losing the phone link does not stop the recording. The notification says so
explicitly, and a test pins it. Showing "not recording" there would push people
to restart a recording that never stopped.

## Building against real hardware

Debug builds link the mock (`USE_MOCK_GLASSES = true`), so the whole product
surface can be developed and instrumented with no glasses present. Release builds
use `VendorGlassesTransport`, which needs the vendor AAR at:

    apps/android/libs/LIB_GLASSES_SDK-release-20260709_8.aar

It is in this repo at `glasses/sdk/android/`. It is not copied into `libs/`
automatically because it is proprietary Shenzhen QC.wireless material and should
not end up in a public artifact by accident.

`VendorGlassesTransport` is the one file still to write — it wraps
`com.glasses.*` (`GlassesControl`, `LargeDataHandler`, `RecordHandle`) behind
`GlassesTransport`. Everything above the interface is done and testable now.
