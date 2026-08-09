package glass.relay.bridge

import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings

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
     * Component names are the ones these skins have shipped; they move between
     * versions, which is why [openManufacturerSettings] falls back to the
     * generic battery-settings screen rather than throwing.
     */
    fun manufacturerAdvice(): ManufacturerAdvice? {
        val manufacturer = Build.MANUFACTURER.lowercase()
        return KNOWN.entries.firstOrNull { manufacturer.contains(it.key) }?.value
    }

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

    private val KNOWN: Map<String, ManufacturerAdvice> = mapOf(
        "xiaomi" to ManufacturerAdvice(
            instruction = "Set Relay's battery saver to \"No restrictions\" and enable Autostart.",
            components = listOf(
                "com.miui.securitycenter" to "com.miui.permcenter.autostart.AutoStartManagementActivity",
            ),
        ),
        "huawei" to ManufacturerAdvice(
            instruction = "Add Relay to protected apps so it keeps running when the screen is off.",
            components = listOf(
                "com.huawei.systemmanager" to "com.huawei.systemmanager.startupmgr.ui.StartupNormalAppListActivity",
                "com.huawei.systemmanager" to "com.huawei.systemmanager.optimize.process.ProtectActivity",
            ),
        ),
        "oppo" to ManufacturerAdvice(
            instruction = "Allow Relay to run in the background and start automatically.",
            components = listOf(
                "com.coloros.safecenter" to "com.coloros.safecenter.permission.startup.StartupAppListActivity",
            ),
        ),
        "vivo" to ManufacturerAdvice(
            instruction = "Allow high background power use for Relay.",
            components = listOf(
                "com.vivo.permissionmanager" to "com.vivo.permissionmanager.activity.BgStartUpManagerActivity",
            ),
        ),
        "oneplus" to ManufacturerAdvice(
            instruction = "Turn off battery optimisation for Relay and disable Deep Optimisation.",
            components = listOf(
                "com.oneplus.security" to "com.oneplus.security.chainlaunch.view.ChainLaunchAppListActivity",
            ),
        ),
        "samsung" to ManufacturerAdvice(
            instruction = "Remove Relay from Sleeping apps in Device care.",
            components = listOf(
                "com.samsung.android.lool" to "com.samsung.android.sm.ui.battery.BatteryActivity",
            ),
        ),
    )
}

data class ManufacturerAdvice(
    /** Shown to the user in plain language before opening the screen. */
    val instruction: String,
    val components: List<Pair<String, String>>,
)
