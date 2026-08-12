package glass.relay.bridge

import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import glass.relay.bridge.oem.OemPolicy

/**
 * Surviving aggressive OEM battery management.
 *
 * On stock Android a foreground service with a wake lock stays alive. On several
 * popular skins it does not: the vendor's own power manager kills background
 * work regardless of foreground-service status, and the only fix is a per-OEM
 * settings screen the user has to visit by hand. This is well documented by the
 * dontkillmyapp project and is the single largest source of "it stopped
 * recording overnight" reports for any capture app.
 *
 * So: ask for the standard exemption, and if the device is one of the known
 * offenders, also point the user at the specific screen that matters. Both are
 * user-initiated — the exemption dialog is a policy-sensitive prompt and
 * spamming it is grounds for Play Store removal.
 */
object BatteryOptimisation {

    fun isExempt(context: Context): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M) return true
        val power = context.getSystemService(Context.POWER_SERVICE) as PowerManager
        return power.isIgnoringBatteryOptimizations(context.packageName)
    }

    /**
     * Opens the system exemption prompt.
     *
     * Call this from a screen that has already explained why an always-on
     * capture device needs it — never on first launch, and never twice.
     */
    @Suppress("BatteryLife") // justified: all-day capture is the product
    fun requestExemption(context: Context): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M || isExempt(context)) return false
        val intent = Intent(
            Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
            Uri.parse("package:${context.packageName}"),
        ).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        return context.tryStart(intent)
    }

    /**
     * The extra, OEM-specific screen this device needs, or null if it behaves.
     *
     * The table moved to [OemPolicy], which is plain Kotlin and therefore
     * testable on a machine with no Android SDK — which, until one is
     * available, is the only place it gets tested at all. This function is now
     * just `Build.MANUFACTURER` plus that lookup.
     */
    fun manufacturerAdvice(): OemPolicy.Advice? = OemPolicy.adviceFor(Build.MANUFACTURER)

    /**
     * Whether this device needs an explicit autostart grant on top of the
     * exemption.
     *
     * Where true, the boot receiver cannot fire at all without it, so a UI that
     * only offers the standard battery dialog is offering a fix that does not
     * work.
     */
    fun needsAutostartGrant(): Boolean = manufacturerAdvice()?.requiresAutostart == true

    fun openManufacturerSettings(context: Context): Boolean {
        val advice = manufacturerAdvice() ?: return false
        for (component in advice.components) {
            val intent = Intent().setClassName(component.first, component.second)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            if (context.tryStart(intent)) return true
        }
        // The component moved in this OS version — the generic screen is still
        // better than doing nothing.
        return context.tryStart(
            Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS)
                .setData(Uri.parse("package:${context.packageName}"))
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
        )
    }

    private fun Context.tryStart(intent: Intent): Boolean = try {
        startActivity(intent)
        true
    } catch (_: ActivityNotFoundException) {
        false
    } catch (_: SecurityException) {
        false
    }
}
