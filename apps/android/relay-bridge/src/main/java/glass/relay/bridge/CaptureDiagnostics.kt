package glass.relay.bridge

import android.content.Context
import android.os.Build
import android.os.SystemClock
import glass.relay.bridge.oem.CaptureWatchdog
import glass.relay.bridge.oem.OemPolicy

/**
 * Proof of whether capture actually ran.
 *
 * Telling a user that their phone might kill background services is worth very
 * little: they have no reason to believe it applies to them until it already
 * has, and when it does the failure is silent — the service simply stops
 * existing. So the service leaves a trail, and this reads it back.
 *
 * The decision logic is in [CaptureWatchdog], which is plain Kotlin and tested.
 * This class is only the two things that need Android: where the beats are
 * stored, and how a reboot is detected.
 *
 * **Reboot detection** compares `System.currentTimeMillis()` with
 * `SystemClock.elapsedRealtime()`. The second counts milliseconds since boot
 * including deep sleep, so `now - elapsedRealtime` is the wall-clock instant the
 * device booted, and it changes only when it actually reboots. There is no
 * broadcast that tells you "you missed BOOT_COMPLETED", which is exactly the
 * case that matters: on MIUI and EMUI without an Autostart grant, the boot
 * broadcast never reaches us at all.
 */
class CaptureDiagnostics(context: Context) {

    private val prefs = context.applicationContext
        .getSharedPreferences("relay.diagnostics", Context.MODE_PRIVATE)

    /**
     * Record that capture is alive.
     *
     * Called from the service on the same cadence as the connection heartbeat.
     * Cheap: a bounded ring of longs in SharedPreferences, not a database.
     */
    fun beat(captureIntended: Boolean = true) {
        val now = System.currentTimeMillis()
        val encoded = if (captureIntended) now else -now
        val beats = (readLongs(KEY_BEATS) + encoded).takeLast(MAX_BEATS)
        prefs.edit().putString(KEY_BEATS, beats.joinToString(",")).apply()
        noteBoot()
    }

    /**
     * What happened while nobody was watching.
     *
     * Safe to call from the UI: it reads, it does not write.
     */
    fun report(): CaptureWatchdog.Report = CaptureWatchdog.analyse(
        beats = readLongs(KEY_BEATS).map {
            CaptureWatchdog.Beat(kotlin.math.abs(it), captureIntended = it > 0)
        },
        expectedIntervalMs = BEAT_INTERVAL_MS,
        nowMs = System.currentTimeMillis(),
        rebootsAtMs = readLongs(KEY_BOOTS),
        manufacturer = Build.MANUFACTURER,
    )

    /** The manufacturer's own settings screen, if this phone needs one. */
    fun oemAdvice(): OemPolicy.Advice? = OemPolicy.adviceFor(Build.MANUFACTURER)

    fun clear() = prefs.edit().clear().apply()

    /**
     * Remember when this boot started, once per boot.
     *
     * Clock changes make the computed instant drift by a few seconds, so two
     * timestamps within a minute of each other are treated as the same boot.
     */
    private fun noteBoot() {
        val bootedAt = System.currentTimeMillis() - SystemClock.elapsedRealtime()
        val known = readLongs(KEY_BOOTS)
        if (known.any { kotlin.math.abs(it - bootedAt) < BOOT_TOLERANCE_MS }) return
        val boots = (known + bootedAt).takeLast(MAX_BOOTS)
        prefs.edit().putString(KEY_BOOTS, boots.joinToString(",")).apply()
    }

    private fun readLongs(key: String): List<Long> =
        prefs.getString(key, "")
            ?.split(',')
            ?.mapNotNull { it.trim().toLongOrNull() }
            ?: emptyList()

    companion object {
        /** Matches the supervisor's heartbeat, so one missed beat means one gap. */
        const val BEAT_INTERVAL_MS = 60_000L

        /** ~8 hours of history at one a minute. Enough to explain a night. */
        private const val MAX_BEATS = 480
        private const val MAX_BOOTS = 16
        private const val BOOT_TOLERANCE_MS = 60_000L

        private const val KEY_BEATS = "beats"
        private const val KEY_BOOTS = "boots"
    }
}
