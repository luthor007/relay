package glass.relay.app.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import glass.relay.bridge.BatteryOptimisation
import glass.relay.bridge.CaptureDiagnostics
import glass.relay.bridge.DiskProbe
import glass.relay.bridge.TransportProvider
import glass.relay.bridge.oem.CaptureWatchdog
import glass.relay.bridge.storage.StoragePolicy

/**
 * The two screens that exist because of things that go wrong quietly:
 * the glasses filling up, and the phone killing the service.
 */
@Composable
fun DeviceScreen() {
    Box(
        Modifier
            .fillMaxSize()
            .background(Ground),
    ) {
        Column(
            Modifier
                .fillMaxSize()
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 24.dp, vertical = 40.dp),
        ) {
            Text("Device", style = MaterialTheme.typography.headlineSmall, color = Ink)
            Spacer(Modifier.height(20.dp))
            StoragePanel()
            Spacer(Modifier.height(16.dp))
            ReliabilityPanel()
        }
    }
}

/**
 * The 4 GB, in the unit that means something.
 *
 * "62% free" tells nobody anything. "Four days of audio, or 25 minutes of
 * video" is a sentence someone can act on — and the ratio between those two
 * numbers is the whole argument of `APPS-SCOPE.md` §3.2.
 */
@Composable
private fun StoragePanel() {
    val context = LocalContext.current
    var assessment by remember { mutableStateOf<StoragePolicy.Assessment?>(null) }

    LaunchedEffect(Unit) {
        val probe = TransportProvider.get(context) as? DiskProbe ?: return@LaunchedEffect
        runCatching { StoragePolicy.assess(probe.diskInfo(), probe.inventory()) }
            .onSuccess { assessment = it }
    }

    Panel("Storage on the glasses") {
        val current = assessment
        if (current == null) {
            Text("Reading…", style = MaterialTheme.typography.bodyMedium, color = InkDim)
            return@Panel
        }

        Fact("Free", "%.1f GB".format(current.freeBytes / 1024.0 / 1024.0 / 1024.0))
        Spacer(Modifier.height(6.dp))
        Fact("Audio left", "%.0f hours".format(current.audioHoursRemaining))
        Spacer(Modifier.height(6.dp))
        Fact(
            "Video left",
            if (current.videoAllowed) "${current.videoMinutesBeforeReserveLost} minutes" else "none",
        )
        Spacer(Modifier.height(6.dp))
        Fact("State", current.level.name)

        for (action in current.actions) {
            Spacer(Modifier.height(10.dp))
            when (action) {
                is StoragePolicy.Action.SyncNow ->
                    Text("Sync now — ${action.why}", style = MaterialTheme.typography.bodySmall, color = Live)

                is StoragePolicy.Action.BlockVideo ->
                    Text("Video is off: ${action.why}", style = MaterialTheme.typography.bodySmall, color = Live)

                is StoragePolicy.Action.WarnBeforeVideo ->
                    Text(
                        "Recording ${action.minutesAvailable} minutes of video would leave " +
                            "%.0f hours of audio.".format(action.audioHoursAfter),
                        style = MaterialTheme.typography.bodySmall,
                        color = InkMid,
                    )

                is StoragePolicy.Action.FreeUploadedAudio ->
                    Text(
                        "%.0f MB of audio is already on the box and can be freed. ".format(
                            action.bytes / 1024.0 / 1024.0,
                        ) + "Nothing un-synced is ever deleted.",
                        style = MaterialTheme.typography.bodySmall,
                        color = InkMid,
                    )
            }
        }

        for (warning in current.warnings) {
            Spacer(Modifier.height(8.dp))
            Text(warning, style = MaterialTheme.typography.bodySmall, color = InkDim)
        }
    }
}

/**
 * Whether this phone has been stopping capture behind our back.
 *
 * Advice about OEM battery managers is easy to ignore until it has already
 * cost someone a day. This panel is the evidence: it says what happened, when,
 * and what to change — and it only appears once there is something to say.
 */
@Composable
private fun ReliabilityPanel() {
    val context = LocalContext.current
    val diagnostics = remember { CaptureDiagnostics(context) }
    val report = remember { diagnostics.report() }
    val advice = remember { diagnostics.oemAdvice() }

    if (report.healthy && advice == null) return

    Panel("Reliability") {
        Text(report.message, style = MaterialTheme.typography.bodyMedium, color = Ink)

        if (report.verdict != CaptureWatchdog.Verdict.Healthy && report.gaps.isNotEmpty()) {
            Spacer(Modifier.height(8.dp))
            Text(
                "${report.gaps.size} interruption${if (report.gaps.size == 1) "" else "s"} " +
                    "in the last few hours, longest ${report.longestGapMs / 60_000} minutes.",
                style = MaterialTheme.typography.bodySmall,
                color = InkDim,
            )
        }

        if (advice != null) {
            Spacer(Modifier.height(12.dp))
            Text(advice.instruction, style = MaterialTheme.typography.bodySmall, color = InkMid)
            if (advice.requiresAutostart) {
                Spacer(Modifier.height(6.dp))
                Text(
                    "Without the autostart grant, Relay cannot restart itself after a reboot at all.",
                    style = MaterialTheme.typography.bodySmall,
                    color = Live,
                )
            }
            Spacer(Modifier.height(12.dp))
            OutlinedButton(
                onClick = { BatteryOptimisation.openManufacturerSettings(context) },
                modifier = Modifier.fillMaxWidth(),
            ) {
                Text("Open ${advice.manufacturer} settings", color = Ink)
            }
        }
    }
}

@Composable
private fun Panel(title: String, content: @Composable () -> Unit) {
    Column(
        Modifier
            .fillMaxWidth()
            .clip(RoundedCornerShape(12.dp))
            .background(Surface)
            .border(1.dp, Line, RoundedCornerShape(12.dp))
            .padding(16.dp),
    ) {
        Text(title, style = MaterialTheme.typography.labelLarge, color = Ink)
        Spacer(Modifier.height(12.dp))
        content()
    }
}

@Composable
private fun Fact(label: String, value: String) {
    Row(
        modifier = Modifier.fillMaxWidth(),
        horizontalArrangement = Arrangement.SpaceBetween,
    ) {
        Text(label, style = MaterialTheme.typography.bodyMedium, color = InkDim)
        Text(value, style = MaterialTheme.typography.bodyMedium, color = Ink)
    }
}
