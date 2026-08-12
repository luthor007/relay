package glass.relay.app

import android.content.Context
import glass.relay.bridge.CapturePreferences

/**
 * The app's view of what has to outlive the process.
 *
 * Consent is **not** stored here any more. It lives in
 * [glass.relay.bridge.CapturePreferences], in the module that contains the
 * service that actually records, because that component has to be able to read
 * it — and previously could not. Two stores for one fact is how a revoked
 * consent stops applying to the only thing it was about. This class delegates.
 *
 * Consent is stored rather than re-asked because a consent dialog shown every
 * launch trains people to dismiss it, which is the opposite of consent. It is
 * revocable from the home screen instead.
 */
class RelayPrefs(context: Context) {

    private val prefs = context.applicationContext
        .getSharedPreferences("relay", Context.MODE_PRIVATE)

    private val capture = CapturePreferences(context)

    var consentGiven: Boolean
        get() = capture.consentGranted
        set(value) { capture.consentGranted = value }

    /** Set once the user has been shown the battery-exemption rationale. */
    var batteryPromptSeen: Boolean
        get() = prefs.getBoolean(KEY_BATTERY_PROMPT, false)
        set(value) = prefs.edit().putBoolean(KEY_BATTERY_PROMPT, value).apply()

    private companion object {
        const val KEY_BATTERY_PROMPT = "battery_prompt_seen"
    }
}
