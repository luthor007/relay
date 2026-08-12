package glass.relay.bridge

import android.annotation.SuppressLint
import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.net.wifi.WifiNetworkSpecifier
import android.os.Build
import android.util.Log
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlin.coroutines.resume

/**
 * Joining the glasses' access point, and giving it back.
 *
 * `ARCHITECTURE.md` §2.1 and `APPS-SCOPE.md` §3.1: a day of audio cannot ride
 * BLE — ~173 MB at ~3 KB/s is about sixteen hours, longer than the day took to
 * record — so the nightly sync goes over the glasses' own AP. **The phone
 * cannot hold that AP and its own uplink at the same time**, which is why the
 * sync is two phases with a network change between them, and why
 * [uplinkAvailable] exists: anything that wants the box has to check it rather
 * than discover it as a timeout.
 *
 * ## The API this uses, and why
 *
 * From API 29 an app cannot simply connect to a network. `WifiManager.enableNetwork`
 * is deprecated and does nothing for third-party apps. The supported path is a
 * [WifiNetworkSpecifier] inside a [NetworkRequest] passed to
 * `ConnectivityManager.requestNetwork`, which shows the user a system dialog
 * naming the SSID and, on approval, hands back a [Network] **that only this app
 * can use**. It is not a system-wide join: other apps keep the normal uplink,
 * and traffic reaches the glasses only through the returned `Network` or after
 * `bindProcessToNetwork`.
 *
 * That is a better fit than it first appears — the sync is a foregrounded,
 * deliberate ritual, so a consent dialog on the first run is honest — but it
 * has consequences worth stating rather than discovering:
 *
 *  - The glasses' AP has no internet, so Android will not route general traffic
 *    over it. `bindProcessToNetwork` is required for the transfer, and must be
 *    undone afterwards or every later request in this process fails.
 *  - The request must be *released*, not merely ignored, or the phone stays on
 *    the glasses' network. [leave] is called from a `finally` for that reason.
 *
 * **Unbuilt and unverified.** Nothing in this file has been compiled — there is
 * no Android SDK on the machine it was written on — nor run against a phone.
 * Every symbol named here was chosen from the documented API surface and is
 * cited in the comments so that a reviewer with an SDK can check the names
 * rather than the intent.
 */
class AndroidSyncNetwork(context: Context) {

    private val connectivity =
        context.applicationContext.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager

    private var callback: ConnectivityManager.NetworkCallback? = null

    /** False exactly while the phone is on the glasses' network. */
    @Volatile
    var uplinkAvailable: Boolean = true
        private set

    /**
     * Join [ssid], returning the bound [Network] or null.
     *
     * Suspends until the system either grants the network or the user declines.
     * There is deliberately no timeout: the dialog is the user's, and cancelling
     * it out from under them would leave the request half-alive.
     */
    @SuppressLint("MissingPermission") // CHANGE_NETWORK_STATE, declared in the manifest
    suspend fun join(ssid: String, passphrase: String): Network? {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q) {
            // Below API 29 this needs the deprecated WifiManager path, which is
            // a different implementation entirely. minSdk is 26, so say so
            // rather than silently doing nothing.
            Log.w(TAG, "joining an access point needs API 29+; bulk sync is unavailable here")
            return null
        }

        val specifier = WifiNetworkSpecifier.Builder()
            .setSsid(ssid)
            .setWpa2Passphrase(passphrase)
            .build()

        val request = NetworkRequest.Builder()
            .addTransportType(NetworkCapabilities.TRANSPORT_WIFI)
            // The glasses' AP has no internet. Asking for INTERNET or
            // VALIDATED capability here would make the request never resolve.
            .removeCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .setNetworkSpecifier(specifier)
            .build()

        return suspendCancellableCoroutine { continuation ->
            val networkCallback = object : ConnectivityManager.NetworkCallback() {
                override fun onAvailable(network: Network) {
                    uplinkAvailable = false
                    // Everything in this process now goes to the glasses. This
                    // is the line that makes the file transfer work and the
                    // uplink stop working, in exactly that order.
                    connectivity.bindProcessToNetwork(network)
                    if (continuation.isActive) continuation.resume(network)
                }

                override fun onUnavailable() {
                    if (continuation.isActive) continuation.resume(null)
                }

                override fun onLost(network: Network) {
                    uplinkAvailable = true
                    connectivity.bindProcessToNetwork(null)
                }
            }

            callback = networkCallback
            connectivity.requestNetwork(request, networkCallback)
            continuation.invokeOnCancellation { leave() }
        }
    }

    /**
     * Give the uplink back.
     *
     * Idempotent, and safe to call when nothing was joined — which matters,
     * because the caller calls it from a `finally`. A phone left on the glasses'
     * network with no route to the internet is a worse outcome than a failed
     * sync.
     */
    fun leave() {
        connectivity.bindProcessToNetwork(null)
        callback?.let {
            runCatching { connectivity.unregisterNetworkCallback(it) }
            callback = null
        }
        uplinkAvailable = true
    }

    private companion object {
        const val TAG = "RelaySyncNetwork"
    }
}
