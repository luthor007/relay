package glass.relay.bridge

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log
import glass.relay.bridge.consent.ConsentGate
import glass.relay.bridge.consent.ConsentPolicy
import glass.relay.bridge.link.BoxAddress
import glass.relay.bridge.link.canOpenLink

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
 *
 * **Restarting the service is not the same as resuming recording.** A boot is
 * not a user action, so `ConsentGate` sees no `startSession()` and the phone
 * has no idea where the device woke up. Under
 * [ConsentPolicy.Scope.FamiliarPlaces] that means the link comes back, the
 * notification comes back, and capture waits for one tap — which is
 * `ARCHITECTURE.md` §6's "capture defaults to off in a new location ... until
 * confirmed" applied to the case where the location is simply unknown. Under
 * [ConsentPolicy.Scope.Always] it resumes on its own. That difference is the
 * scope doing its job, not a bug.
 */
class BootReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        val action = intent.action ?: return
        if (action !in BOOT_ACTIONS) return

        if (!CapturePreferences(context).captureEnabled) {
            Log.d(TAG, "capture was off before reboot; staying off")
            return
        }

        // Blocking, not merely missing: a user who declined the microphone
        // still wants their day recorded, and all-day capture does not use the
        // phone's microphone. Checking the wider list here would silently stop
        // restarting capture for them.
        val missing = RelayCaptureService.blockingPermissions(context)
        if (missing.isNotEmpty()) {
            Log.w(TAG, "not restarting after boot, missing: $missing")
            return
        }

        runCatching { RelayCaptureService.start(context) }
            .onFailure { Log.e(TAG, "failed to restart after boot", it) }
    }

    private companion object {
        const val TAG = "RelayBoot"
        val BOOT_ACTIONS = setOf(
            Intent.ACTION_BOOT_COMPLETED,
            "android.intent.action.QUICKBOOT_POWERON",
            "com.htc.intent.action.QUICKBOOT_POWERON",
        )
    }
}

/**
 * The facts that have to outlive the process.
 *
 * Public and in this module on purpose. Consent used to live only in the app
 * module's `RelayPrefs`, which meant the always-on service — the thing that
 * actually records — could not read it. Two stores for one fact is how a
 * revoked consent stops applying to the only component that matters.
 *
 * **What is stored is a scope and a set of confirmed places, not a boolean.**
 * `ARCHITECTURE.md` §6 requires capture to default off in a new location, and a
 * boolean cannot say where it was granted. [ConsentGate] is what reads this and
 * decides; nothing else should. [consentGranted] survives as the onboarding
 * screen's on/off switch and is derived from the scope, so there is still only
 * one stored fact.
 */
class CapturePreferences(context: Context) {
    private val prefs = context.applicationContext
        .getSharedPreferences(FILE, Context.MODE_PRIVATE)

    /** Whether the user wants capture running. Read by [BootReceiver]. */
    var captureEnabled: Boolean
        get() = prefs.getBoolean(KEY_ENABLED, false)
        set(value) = prefs.edit().putBoolean(KEY_ENABLED, value).apply()

    /**
     * How far the user's consent reaches. `ARCHITECTURE.md` §6.
     *
     * Falls back to the old boolean when no scope has been written yet, so an
     * install that predates this migrates to [ConsentPolicy.Scope.FamiliarPlaces]
     * rather than losing consent — and to `None` if consent was never given. An
     * unparseable value reads as `None`: failing closed is the only safe
     * direction for this particular field.
     */
    var consentScope: ConsentPolicy.Scope
        get() {
            val name = prefs.getString(KEY_SCOPE, null)
                ?: return if (prefs.getBoolean(KEY_CONSENT, false)) {
                    ConsentPolicy.Scope.FamiliarPlaces
                } else {
                    ConsentPolicy.Scope.None
                }
            return runCatching { ConsentPolicy.Scope.valueOf(name) }
                .getOrDefault(ConsentPolicy.Scope.None)
        }
        set(value) = prefs.edit()
            .putString(KEY_SCOPE, value.name)
            .putBoolean(KEY_CONSENT, value != ConsentPolicy.Scope.None)
            .apply()

    /** Places the user has confirmed, by the id the box uses for them. */
    var confirmedPlaces: Set<String>
        get() = prefs.getStringSet(KEY_PLACES, emptySet()) ?: emptySet()
        // A defensive copy: SharedPreferences does not copy the set it is
        // given, and mutating it afterwards corrupts the stored value.
        set(value) = prefs.edit().putStringSet(KEY_PLACES, LinkedHashSet(value)).apply()

    /**
     * Where the user's box is, and the token it printed on start.
     *
     * `SYSTEM.md` §6.1: "one authenticated socket means one token, printed on
     * start like the pairing code". Both empty until pairing writes them, and
     * the service simply does not open a link in that state — an unconfigured
     * phone must not spend its battery retrying a URL that does not exist.
     *
     * **The screen that fills these in does not exist yet.** That is a named
     * gap, not a hidden one: `APPS-SCOPE.md` §4.3 makes PAKE-based pairing a
     * build blocker, and inventing a text box for a bearer token here would be
     * the thing that ships instead of pairing.
     */
    var boxUrl: String
        get() = prefs.getString(KEY_BOX_URL, "").orEmpty()
        set(value) = prefs.edit().putString(KEY_BOX_URL, value).apply()

    var boxToken: String
        get() = prefs.getString(KEY_BOX_TOKEN, "").orEmpty()
        set(value) = prefs.edit().putString(KEY_BOX_TOKEN, value).apply()

    /**
     * The rendezvous relay, so the phone can reach the box from outside the
     * house. `SYSTEM.md` §7.
     *
     * Both empty is the ordinary state of a box on a LAN with no relay
     * configured, and it stays working: [boxAddress] simply produces one
     * endpoint instead of two. Neither value is a secret — the box id in
     * particular must never become one, because anyone who learns it gets
     * exactly as far as a stranger on the LAN, which is nowhere.
     */
    var relayUrl: String
        get() = prefs.getString(KEY_RELAY_URL, "").orEmpty()
        set(value) = prefs.edit().putString(KEY_RELAY_URL, value).apply()

    var boxId: String
        get() = prefs.getString(KEY_BOX_ID, "").orEmpty()
        set(value) = prefs.edit().putString(KEY_BOX_ID, value).apply()

    /** Everywhere this box can be reached, for [glass.relay.bridge.link.endpoints]. */
    val boxAddress: BoxAddress
        get() = BoxAddress(direct = boxUrl, relayUrl = relayUrl, boxId = boxId)

    /**
     * Whether there is a box to open a link to at all.
     *
     * A token and *somewhere to send it*. The relay counts as somewhere: a
     * cloud box has no LAN address, and requiring one would mean a paying
     * customer's phone never opens a link.
     */
    val boxConfigured: Boolean
        get() = canOpenLink(boxToken, boxAddress)

    var consentState: ConsentGate.Stored
        get() = ConsentGate.Stored(scope = consentScope, confirmedPlaces = confirmedPlaces)
        set(value) {
            consentScope = value.scope
            confirmedPlaces = value.confirmedPlaces
        }

    /**
     * Whether the user has consented to being recorded at all.
     *
     * The onboarding screen's switch, and nothing finer. Granting it means
     * [ConsentPolicy.Scope.FamiliarPlaces] — "record where I have confirmed" —
     * which is §6's rule rather than a blanket yes. Withdrawing it forgets every
     * confirmed place, because otherwise re-granting silently restores every yes
     * the user has ever given.
     */
    var consentGranted: Boolean
        get() = consentScope != ConsentPolicy.Scope.None
        set(value) {
            consentScope = if (value) {
                ConsentPolicy.Scope.FamiliarPlaces
            } else {
                ConsentPolicy.Scope.None
            }
            if (!value) confirmedPlaces = emptySet()
        }

    private companion object {
        const val FILE = "relay.capture"
        const val KEY_ENABLED = "capture_enabled"
        const val KEY_CONSENT = "consent_given"
        const val KEY_SCOPE = "consent_scope"
        const val KEY_PLACES = "consent_places"
        const val KEY_BOX_URL = "box_url"
        const val KEY_BOX_TOKEN = "box_token"
        const val KEY_RELAY_URL = "relay_url"
        const val KEY_BOX_ID = "box_id"
    }
}
