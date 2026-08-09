package glass.engram.bridge

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log

/**
 * Brings capture back after a reboot — but only if the user had it running.
 *
 * Two things this deliberately does not do:
 *
 *  - It does not start capture just because the app is installed. Restoring a
 *    session the user stopped is the behaviour that makes people uninstall a
 *    recording app.
 *  - It does not assume `startForegroundService` will succeed. On API 31+ the
 *    boot broadcast is a permitted background-start window, but permissions may
 *    have been revoked while the device was off, so the state check runs first
 *    and the call is guarded.
 */
class BootReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        val action = intent.action ?: return
        if (action !in BOOT_ACTIONS) return

        if (!CapturePreferences(context).captureEnabled) {
            Log.d(TAG, "capture was off before reboot; staying off")
            return
        }

        val missing = EngramCaptureService.missingPermissions(context)
        if (missing.isNotEmpty()) {
            Log.w(TAG, "not restarting after boot, missing: $missing")
            return
        }

        runCatching { EngramCaptureService.start(context) }
            .onFailure { Log.e(TAG, "failed to restart after boot", it) }
    }

    private companion object {
        const val TAG = "EngramBoot"
        val BOOT_ACTIONS = setOf(
            Intent.ACTION_BOOT_COMPLETED,
            "android.intent.action.QUICKBOOT_POWERON",
            "com.htc.intent.action.QUICKBOOT_POWERON",
        )
    }
}

/** Whether the user wants capture running, persisted across reboots and kills. */
internal class CapturePreferences(context: Context) {
    private val prefs = context.getSharedPreferences("engram.capture", Context.MODE_PRIVATE)

    var captureEnabled: Boolean
        get() = prefs.getBoolean(KEY_ENABLED, false)
        set(value) = prefs.edit().putBoolean(KEY_ENABLED, value).apply()

    private companion object {
        const val KEY_ENABLED = "capture_enabled"
    }
}
