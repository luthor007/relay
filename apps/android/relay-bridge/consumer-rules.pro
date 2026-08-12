# Consumer rules for :relay-bridge.
#
# The vendor SDK is reflective in two places that R8 cannot see, and both fail
# silently at runtime rather than at build time — the failure mode is "the
# glasses connect but never report wear", which is very expensive to debug.

# Vendor SDK surface. Kept wholesale: the AAR is obfuscated already and its
# internal call graph is not ours to reason about.
-keep class com.glasses.** { *; }
-dontwarn com.glasses.**

# Jieli audio/OTA stack vendored inside the vendor AAR.
-keep class com.jieli.** { *; }
-dontwarn com.jieli.**

# Response objects are instantiated reflectively by LargeDataHandler when it
# demultiplexes an incoming action byte, so their no-arg constructors must
# survive even though nothing in our code calls them.
-keepclassmembers class * extends com.glasses.ble.base.communication.bigData.resp.BaseResponse {
    <init>();
}

# Callback interfaces are registered from our code but invoked from the vendor
# side, so the method names have to stay intact.
-keep interface com.glasses.ble.base.communication.ILargeDataResponse { *; }
-keep interface com.glasses.ble.base.communication.ICommandResponse { *; }
-keep interface com.glasses.ble.base.communication.file.IRecordCallback { *; }
-keep interface com.glasses.ble.base.bluetooth.OnGattEventCallback { *; }
