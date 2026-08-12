package glass.relay.bridge.oem

/**
 * What this particular phone will do to a background service, and what to do
 * about it.
 *
 * On stock Android a foreground service with a wake lock stays alive. On
 * several popular skins it does not: the vendor's own power manager kills
 * background work regardless of foreground-service status, and the fix is a
 * settings screen the user has to visit by hand. This is the single largest
 * source of "it stopped recording overnight" reports for any capture app, and
 * it is documented at length by the dontkillmyapp project.
 *
 * Pure Kotlin on purpose. `BatteryOptimisation` supplies `Build.MANUFACTURER`
 * and launches the intents; the decision of *what a manufacturer does and what
 * the user must be told* lives here, where it is testable on a machine with no
 * Android at all — which is the only place it will be tested for a while.
 */
object OemPolicy {

    enum class Severity {
        /** Stock behaviour. The standard exemption is enough. */
        Standard,

        /** Restricts aggressively; the exemption helps but is not sufficient. */
        Restricts,

        /** Kills foreground services outright without the vendor's own opt-in. */
        Kills,
    }

    data class Advice(
        val manufacturer: String,
        val severity: Severity,
        /** Shown verbatim, before anything is launched. */
        val instruction: String,
        /**
         * Whether the vendor requires an explicit "autostart" grant *in addition*
         * to the battery exemption. Where true, the app cannot restart itself
         * after a reboot or a kill without it, and the boot receiver is a
         * decoration.
         */
        val requiresAutostart: Boolean,
        /** Settings components, most specific first. They move between versions. */
        val components: List<Pair<String, String>>,
    )

    /**
     * The advice for [manufacturer], or null if it behaves like stock.
     *
     * Matching is a lowercase substring test because `Build.MANUFACTURER` is not
     * a controlled vocabulary: Xiaomi ships "Xiaomi" and "xiaomi", OnePlus has
     * shipped both "OnePlus" and "Oneplus", and Redmi and POCO devices report
     * their own names while running MIUI.
     */
    fun adviceFor(manufacturer: String?): Advice? {
        val key = manufacturer?.lowercase()?.trim().orEmpty()
        if (key.isEmpty()) return null
        return KNOWN.entries.firstOrNull { key.contains(it.key) }?.value
    }

    /** Every manufacturer this build knows about. Useful for a test, and for a log. */
    val known: Set<String> get() = KNOWN.keys

    private val KNOWN: Map<String, Advice> = linkedMapOf(
        // MIUI/HyperOS is the worst of the set: without Autostart the process is
        // not restarted after a kill or a reboot at all, and "battery saver:
        // restricted" kills a foreground service within minutes of screen-off.
        "xiaomi" to Advice(
            manufacturer = "Xiaomi",
            severity = Severity.Kills,
            instruction = "Set Relay's battery saver to \"No restrictions\" and turn on Autostart. " +
                "Without Autostart, MIUI will not restart Relay after a reboot.",
            requiresAutostart = true,
            components = listOf(
                "com.miui.securitycenter" to "com.miui.permcenter.autostart.AutoStartManagementActivity",
                "com.miui.powerkeeper" to "com.miui.powerkeeper.ui.HiddenAppsConfigActivity",
            ),
        ),
        "redmi" to Advice(
            manufacturer = "Redmi",
            severity = Severity.Kills,
            instruction = "Set Relay's battery saver to \"No restrictions\" and turn on Autostart.",
            requiresAutostart = true,
            components = listOf(
                "com.miui.securitycenter" to "com.miui.permcenter.autostart.AutoStartManagementActivity",
            ),
        ),
        "poco" to Advice(
            manufacturer = "POCO",
            severity = Severity.Kills,
            instruction = "Set Relay's battery saver to \"No restrictions\" and turn on Autostart.",
            requiresAutostart = true,
            components = listOf(
                "com.miui.securitycenter" to "com.miui.permcenter.autostart.AutoStartManagementActivity",
            ),
        ),
        "huawei" to Advice(
            manufacturer = "Huawei",
            severity = Severity.Kills,
            instruction = "Add Relay to Protected apps and set it to \"Manage manually\" with " +
                "auto-launch, secondary launch and run in background all on.",
            requiresAutostart = true,
            components = listOf(
                "com.huawei.systemmanager" to "com.huawei.systemmanager.startupmgr.ui.StartupNormalAppListActivity",
                "com.huawei.systemmanager" to "com.huawei.systemmanager.optimize.process.ProtectActivity",
            ),
        ),
        "honor" to Advice(
            manufacturer = "Honor",
            severity = Severity.Kills,
            instruction = "Add Relay to Protected apps so it keeps running with the screen off.",
            requiresAutostart = true,
            components = listOf(
                "com.huawei.systemmanager" to "com.huawei.systemmanager.startupmgr.ui.StartupNormalAppListActivity",
            ),
        ),
        "oppo" to Advice(
            manufacturer = "OPPO",
            severity = Severity.Kills,
            instruction = "Allow Relay to run in the background and start automatically, and turn " +
                "off \"Sleep standby optimisation\".",
            requiresAutostart = true,
            components = listOf(
                "com.coloros.safecenter" to "com.coloros.safecenter.permission.startup.StartupAppListActivity",
                "com.coloros.safecenter" to "com.coloros.safecenter.startupapp.StartupAppListActivity",
            ),
        ),
        "realme" to Advice(
            manufacturer = "realme",
            severity = Severity.Kills,
            instruction = "Allow Relay to run in the background and start automatically.",
            requiresAutostart = true,
            components = listOf(
                "com.coloros.safecenter" to "com.coloros.safecenter.permission.startup.StartupAppListActivity",
            ),
        ),
        "vivo" to Advice(
            manufacturer = "vivo",
            severity = Severity.Kills,
            instruction = "Allow high background power use for Relay and turn on background " +
                "auto-start.",
            requiresAutostart = true,
            components = listOf(
                "com.vivo.permissionmanager" to "com.vivo.permissionmanager.activity.BgStartUpManagerActivity",
                "com.iqoo.secure" to "com.iqoo.secure.ui.phoneoptimize.BgStartUpManager",
            ),
        ),
        "oneplus" to Advice(
            manufacturer = "OnePlus",
            severity = Severity.Restricts,
            instruction = "Turn off battery optimisation for Relay and disable \"Deep optimisation\" " +
                "and \"Sleep standby optimisation\" in Battery settings.",
            requiresAutostart = false,
            components = listOf(
                "com.oneplus.security" to "com.oneplus.security.chainlaunch.view.ChainLaunchAppListActivity",
            ),
        ),
        // Samsung does not kill outright, but "Sleeping apps" and "Deep sleeping
        // apps" will put Relay to sleep after a few days of not being opened —
        // which is exactly what an always-on background app looks like.
        "samsung" to Advice(
            manufacturer = "Samsung",
            severity = Severity.Restricts,
            instruction = "In Device care → Battery, remove Relay from \"Sleeping apps\" and turn " +
                "off \"Put unused apps to sleep\".",
            requiresAutostart = false,
            components = listOf(
                "com.samsung.android.lool" to "com.samsung.android.sm.ui.battery.BatteryActivity",
                "com.samsung.android.sm" to "com.samsung.android.sm.ui.battery.BatteryActivity",
            ),
        ),
        "asus" to Advice(
            manufacturer = "ASUS",
            severity = Severity.Restricts,
            instruction = "Add Relay to the Auto-start manager in Mobile Manager.",
            requiresAutostart = true,
            components = listOf(
                "com.asus.mobilemanager" to "com.asus.mobilemanager.autostart.AutoStartActivity",
            ),
        ),
        "meizu" to Advice(
            manufacturer = "Meizu",
            severity = Severity.Restricts,
            instruction = "Allow Relay to run in the background in Security → Permissions.",
            requiresAutostart = true,
            components = listOf(
                "com.meizu.safe" to "com.meizu.safe.security.SHOW_APPSEC",
            ),
        ),
    )
}
