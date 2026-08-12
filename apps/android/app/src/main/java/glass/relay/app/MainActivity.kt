package glass.relay.app

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.compose.LocalLifecycleOwner
import glass.relay.app.ui.CommandConsoleScreen
import glass.relay.app.ui.ConsentScreen
import glass.relay.app.ui.DeviceScreen
import glass.relay.app.ui.Ground
import glass.relay.app.ui.HomeScreen
import glass.relay.app.ui.Ink
import glass.relay.app.ui.InkDim
import glass.relay.app.ui.PermissionsScreen
import glass.relay.app.ui.RelayTheme
import glass.relay.app.ui.Surface
import glass.relay.bridge.RelayCaptureService

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            RelayTheme {
                RelayApp()
            }
        }
    }
}

/**
 * Three gates, in order, and none of them is skippable:
 *
 *   consent → permissions → home
 *
 * The order is the point. Asking for a microphone before explaining what will be
 * recorded gets a grant that means nothing, and the permission dialog is not a
 * place to explain it — it is a system dialog with no room for the part that
 * matters.
 */
@Composable
fun RelayApp() {
    val context = LocalContext.current
    val prefs = remember { RelayPrefs(context) }

    var consentGiven by remember { mutableStateOf(prefs.consentGiven) }

    // Two lists, not one. `missing` is everything the app would like and is
    // what the permission screen asks for; `blocking` is the subset without
    // which capture cannot legally start. Gating on the first would leave a
    // user who declines the microphone permanently stuck on a screen asking for
    // it — and all-day capture does not use the phone's microphone at all.
    var missingPermissions by remember {
        mutableStateOf(RelayCaptureService.missingPermissions(context))
    }
    var blockingPermissions by remember {
        mutableStateOf(RelayCaptureService.blockingPermissions(context))
    }

    val lifecycleOwner = LocalLifecycleOwner.current
    DisposableEffect(lifecycleOwner) {
        val observer = LifecycleEventObserver { _, event ->
            if (event == Lifecycle.Event.ON_RESUME) {
                missingPermissions = RelayCaptureService.missingPermissions(context)
                blockingPermissions = RelayCaptureService.blockingPermissions(context)
            }
        }
        lifecycleOwner.lifecycle.addObserver(observer)
        onDispose { lifecycleOwner.lifecycle.removeObserver(observer) }
    }

    val permissionLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestMultiplePermissions(),
    ) {
        missingPermissions = RelayCaptureService.missingPermissions(context)
        blockingPermissions = RelayCaptureService.blockingPermissions(context)
    }

    when {
        !consentGiven -> ConsentScreen(
            onAccept = {
                prefs.consentGiven = true
                consentGiven = true
            },
        )

        blockingPermissions.isNotEmpty() -> PermissionsScreen(
            missing = missingPermissions,
            onGrant = { permissionLauncher.launch(missingPermissions.toTypedArray()) },
        )

        else -> RelayShell(
            onRevokeConsent = {
                // Through the service, because the service owns the
                // `ConsentGate` and the gate is what has to stop a recording
                // already in progress and forget every confirmed place. The
                // preference write that follows is the same value by a second
                // route: if the service cannot be started for any reason, the
                // withdrawal must still be the thing that survives a restart.
                RelayCaptureService.revokeConsent(context)
                prefs.consentGiven = false
                consentGiven = false
            },
        )
    }
}

/**
 * Three tabs, once the gates are passed.
 *
 * Home is capture. Device is the two things that go wrong quietly — the glasses
 * filling up, and the phone killing the service. Commands is every command in
 * the protocol, by hand (`ORCHESTRATOR.md` §5).
 *
 * A plain row rather than a navigation library: three destinations, no
 * back-stack worth preserving, and one fewer dependency in the module that has
 * to keep working at 3 a.m.
 */
@Composable
private fun RelayShell(onRevokeConsent: () -> Unit) {
    var tab by remember { mutableStateOf(Tab.Home) }

    Column(Modifier.fillMaxSize().background(Ground)) {
        Box(Modifier.weight(1f)) {
            when (tab) {
                Tab.Home -> HomeScreen(onRevokeConsent = onRevokeConsent)
                Tab.Device -> DeviceScreen()
                Tab.Commands -> CommandConsoleScreen()
            }
        }
        Row(
            Modifier
                .fillMaxWidth()
                .background(Surface)
                .padding(vertical = 8.dp),
            horizontalArrangement = Arrangement.SpaceEvenly,
        ) {
            for (entry in Tab.entries) {
                TextButton(onClick = { tab = entry }) {
                    Text(
                        entry.label,
                        style = MaterialTheme.typography.labelLarge,
                        color = if (tab == entry) Ink else InkDim,
                    )
                }
            }
        }
    }
}

private enum class Tab(val label: String) {
    Home("Capture"),
    Device("Device"),
    Commands("Commands"),
}
