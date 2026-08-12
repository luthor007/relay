package glass.relay.app.ui

import android.Manifest
import androidx.compose.animation.core.RepeatMode
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
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
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.clip
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import glass.relay.app.R
import glass.relay.bridge.BatteryOptimisation
import glass.relay.bridge.CaptureBus
import glass.relay.bridge.CaptureState
import glass.relay.bridge.ConnectionState
import glass.relay.bridge.RelayCaptureService

// --------------------------------------------------------------------- consent

@Composable
fun ConsentScreen(onAccept: () -> Unit) {
    Screen {
        Text(
            text = stringResource(R.string.consent_title),
            style = androidx.compose.material3.MaterialTheme.typography.displaySmall,
            color = Ink,
        )
        Spacer(Modifier.height(20.dp))
        Text(
            text = stringResource(R.string.consent_body).trimIndent(),
            style = androidx.compose.material3.MaterialTheme.typography.bodyLarge,
            color = InkMid,
        )
        Spacer(Modifier.height(32.dp))
        PrimaryButton(
            text = stringResource(R.string.consent_accept),
            onClick = onAccept,
        )
    }
}

// ----------------------------------------------------------------- permissions

@Composable
fun PermissionsScreen(missing: List<String>, onGrant: () -> Unit) {
    Screen {
        Text(
            text = stringResource(R.string.perm_title),
            style = androidx.compose.material3.MaterialTheme.typography.displaySmall,
            color = Ink,
        )
        Spacer(Modifier.height(12.dp))
        Text(
            text = stringResource(R.string.perm_body),
            style = androidx.compose.material3.MaterialTheme.typography.bodyLarge,
            color = InkMid,
        )
        Spacer(Modifier.height(24.dp))

        missing.forEach { permission ->
            PermissionRow(permission)
            Spacer(Modifier.height(12.dp))
        }

        Spacer(Modifier.height(20.dp))
        PrimaryButton(text = stringResource(R.string.perm_grant), onClick = onGrant)
    }
}

@Composable
private fun PermissionRow(permission: String) {
    // Only the permissions this app requests are mapped. An unmapped one showing
    // its raw name is a bug worth seeing rather than hiding behind a generic
    // string — it means the service started asking for something unexplained.
    val label = when (permission) {
        Manifest.permission.RECORD_AUDIO -> stringResource(R.string.perm_mic)
        Manifest.permission.BLUETOOTH_CONNECT,
        Manifest.permission.BLUETOOTH_SCAN,
        -> stringResource(R.string.perm_bt)
        Manifest.permission.POST_NOTIFICATIONS -> stringResource(R.string.perm_notif)
        else -> permission
    }
    Card {
        Text(
            text = label,
            style = androidx.compose.material3.MaterialTheme.typography.bodyMedium,
            color = Ink,
        )
    }
}

// ------------------------------------------------------------------------ home

@Composable
fun HomeScreen(onRevokeConsent: () -> Unit) {
    val context = LocalContext.current
    val state by CaptureBus.state.collectAsStateWithLifecycle()
    val live by CaptureBus.live.collectAsStateWithLifecycle()
    val approvals by CaptureBus.approvals.collectAsStateWithLifecycle()

    Screen {
        StatusHeader(state = state, live = live)

        // The consent question, above everything else on the screen. It is the
        // only thing here that is holding capture off, and burying it under the
        // status card is how "capture defaults to off until confirmed"
        // (ARCHITECTURE.md §6) turns into "capture mysteriously stopped".
        state.consentQuestion?.let { question ->
            Spacer(Modifier.height(20.dp))
            ConsentPrompt(
                question = question,
                why = state.consentWhy,
                onAnswer = { approve -> RelayCaptureService.answerConsent(context, approve) },
            )
        }

        // `ORCHESTRATOR.md` §5 job 3: an agent that wants to run something
        // dangerous has to be able to ask. A `confirm.request` that reaches the
        // phone and is never drawn is the same as one that never arrived, and
        // the session on the other end stays blocked either way.
        approvals.forEach { request ->
            Spacer(Modifier.height(20.dp))
            ConsentPrompt(
                question = request.summary,
                why = if (request.consequential) {
                    "this has effects outside the machine, so it is asked every time"
                } else {
                    null
                },
                onAnswer = { approve ->
                    RelayCaptureService.answerApproval(context, request.actionId, approve)
                },
                yes = "Allow",
                no = "Deny",
            )
        }

        Spacer(Modifier.height(28.dp))

        if (live) {
            OutlinedButton(
                onClick = { RelayCaptureService.stop(context) },
                modifier = Modifier.fillMaxWidth(),
            ) {
                Text("Stop capture", color = Ink)
            }
        } else {
            PrimaryButton(
                text = "Start capture",
                // Tapping this is the wearer starting a conversation, which is
                // the one consent signal the phone can observe for itself.
                onClick = { RelayCaptureService.start(context, userInitiated = true) },
            )
        }

        Spacer(Modifier.height(16.dp))
        DeviceFacts(state)

        Spacer(Modifier.height(16.dp))
        BatteryOptimisationCard()

        Spacer(Modifier.height(24.dp))
        TextButton(onClick = onRevokeConsent, modifier = Modifier.fillMaxWidth()) {
            Text("Turn off capture and withdraw consent", color = InkDim)
        }
    }
}

/**
 * The recording indicator.
 *
 * Two rules this composable exists to enforce, both from ARCHITECTURE.md §6:
 *
 *  - It never says "not recording" on the strength of a lost connection. The
 *    glasses record to their own storage, so a dropped link means *we* stopped
 *    knowing, not that recording stopped. Saying otherwise would push someone to
 *    restart a recording that never ended.
 *  - Before the service has published anything, it says so rather than guessing.
 */
/**
 * The one question that stops capture, and the two answers to it.
 *
 * Both buttons are the same weight on purpose. A prompt whose "yes" is a filled
 * button and whose "no" is grey text is a prompt designed to be agreed with, and
 * in a two-party consent jurisdiction that is the wrong design.
 */
@Composable
private fun ConsentPrompt(
    question: String,
    why: String?,
    onAnswer: (Boolean) -> Unit,
    yes: String = stringResource(R.string.consent_prompt_yes),
    no: String = stringResource(R.string.consent_prompt_no),
) {
    Card {
        Text(
            text = question,
            style = androidx.compose.material3.MaterialTheme.typography.titleMedium,
            color = Ink,
        )
        if (why != null) {
            Spacer(Modifier.height(8.dp))
            Text(
                text = why,
                style = androidx.compose.material3.MaterialTheme.typography.bodySmall,
                color = InkDim,
            )
        }
        Spacer(Modifier.height(16.dp))
        Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            OutlinedButton(onClick = { onAnswer(false) }, modifier = Modifier.weight(1f)) {
                Text(no, color = Ink)
            }
            OutlinedButton(onClick = { onAnswer(true) }, modifier = Modifier.weight(1f)) {
                Text(yes, color = Ink)
            }
        }
    }
}

@Composable
private fun StatusHeader(state: CaptureState, live: Boolean) {
    val recording = live && state.recording
    val connectionLost = live && state.connection == ConnectionState.Reconnecting

    Row(verticalAlignment = Alignment.CenterVertically) {
        LiveDot(active = recording)
        Spacer(Modifier.size(12.dp))
        Column {
            Text(
                text = when {
                    !live -> "Capture off"
                    recording && connectionLost -> "Recording on the glasses"
                    recording -> "Recording"
                    state.connection == ConnectionState.Connected && !state.worn ->
                        "Connected — put them on to start"
                    state.connection == ConnectionState.Connected -> "Connected"
                    state.connection == ConnectionState.Connecting -> "Connecting…"
                    state.connection == ConnectionState.Reconnecting -> "Reconnecting…"
                    else -> "Not connected"
                },
                style = androidx.compose.material3.MaterialTheme.typography.headlineSmall,
                color = Ink,
            )
            if (recording && connectionLost) {
                Text(
                    text = "Phone out of range — the day is safe on the glasses and will sync later",
                    style = androidx.compose.material3.MaterialTheme.typography.bodyMedium,
                    color = InkDim,
                )
            }
        }
    }
}

@Composable
private fun LiveDot(active: Boolean) {
    // A slow pulse, not a blink. Blinking reads as an error state; this has to
    // read as "alive".
    val transition = rememberInfiniteTransition(label = "live")
    val pulse by transition.animateFloat(
        initialValue = 1f,
        targetValue = 0.35f,
        animationSpec = infiniteRepeatable(tween(1400), RepeatMode.Reverse),
        label = "pulse",
    )
    Box(
        Modifier
            .size(14.dp)
            .alpha(if (active) pulse else 1f)
            .clip(CircleShape)
            .background(if (active) Live else Line),
    )
}

@Composable
private fun DeviceFacts(state: CaptureState) {
    Card {
        Fact("Worn", if (state.worn) "Yes" else "No")
        Spacer(Modifier.height(8.dp))
        Fact("Battery", state.batteryPercent?.let { "$it%" } ?: "—")
        Spacer(Modifier.height(8.dp))
        // Two links, named separately. Glasses down means no new audio; box
        // down means the day is piling up on the phone. One "Connected" row
        // would hide whichever of them is actually broken.
        Fact("Glasses", state.connection.name)
        Spacer(Modifier.height(8.dp))
        Fact("Box", state.boxConnection.name)
        state.lastFromBox?.let { line ->
            Spacer(Modifier.height(12.dp))
            Text(
                text = line,
                style = androidx.compose.material3.MaterialTheme.typography.bodySmall,
                color = InkMid,
            )
        }
    }
}

@Composable
private fun Fact(label: String, value: String) {
    Row(
        modifier = Modifier.fillMaxWidth(),
        horizontalArrangement = Arrangement.SpaceBetween,
    ) {
        Text(label, style = androidx.compose.material3.MaterialTheme.typography.bodyMedium, color = InkDim)
        Text(value, style = androidx.compose.material3.MaterialTheme.typography.bodyMedium, color = Ink)
    }
}

/**
 * The single largest cause of "it stopped recording overnight" — see
 * apps/android/README.md. Shown until the exemption is actually granted.
 */
@Composable
private fun BatteryOptimisationCard() {
    val context = LocalContext.current
    val exempt = remember { BatteryOptimisation.isExempt(context) }
    val advice = remember { BatteryOptimisation.manufacturerAdvice() }

    if (exempt && advice == null) return

    Card {
        Text(
            text = stringResource(R.string.battery_title),
            style = androidx.compose.material3.MaterialTheme.typography.labelLarge,
            color = Ink,
        )
        Spacer(Modifier.height(8.dp))
        Text(
            text = if (exempt) {
                stringResource(R.string.battery_ok)
            } else {
                stringResource(R.string.battery_body).trimIndent()
            },
            style = androidx.compose.material3.MaterialTheme.typography.bodyMedium,
            color = InkMid,
        )

        if (!exempt) {
            Spacer(Modifier.height(12.dp))
            OutlinedButton(
                onClick = { BatteryOptimisation.requestExemption(context) },
                modifier = Modifier.fillMaxWidth(),
            ) {
                Text(stringResource(R.string.battery_exempt), color = Ink)
            }
        }

        // Some OEM skins kill foreground services regardless of the exemption, so
        // this stays available even once Android itself is satisfied.
        if (advice != null) {
            Spacer(Modifier.height(8.dp))
            Text(
                text = advice.instruction,
                style = androidx.compose.material3.MaterialTheme.typography.bodyMedium,
                color = InkDim,
            )
            Spacer(Modifier.height(8.dp))
            TextButton(
                onClick = { BatteryOptimisation.openManufacturerSettings(context) },
                modifier = Modifier.fillMaxWidth(),
            ) {
                Text(stringResource(R.string.battery_oem_open, deviceMaker()), color = InkMid)
            }
        }
    }
}

/** "xiaomi" → "Xiaomi". Only ever used inside a sentence about their settings app. */
private fun deviceMaker(): String =
    android.os.Build.MANUFACTURER.replaceFirstChar { it.uppercase() }

// ------------------------------------------------------------------ components

@Composable
private fun Screen(content: @Composable () -> Unit) {
    Box(
        Modifier
            .fillMaxSize()
            .background(Ground),
    ) {
        Column(
            Modifier
                .fillMaxSize()
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 24.dp, vertical = 48.dp),
        ) {
            content()
        }
    }
}

@Composable
private fun Card(content: @Composable () -> Unit) {
    Column(
        Modifier
            .fillMaxWidth()
            .clip(RoundedCornerShape(12.dp))
            .background(Surface)
            .border(1.dp, Line, RoundedCornerShape(12.dp))
            .padding(16.dp),
    ) {
        content()
    }
}

@Composable
private fun PrimaryButton(text: String, onClick: () -> Unit) {
    Button(
        onClick = onClick,
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(10.dp),
        colors = ButtonDefaults.buttonColors(containerColor = Ink, contentColor = Ground),
    ) {
        Text(
            text = text,
            style = androidx.compose.material3.MaterialTheme.typography.labelLarge,
            textAlign = TextAlign.Center,
            modifier = Modifier.padding(vertical = 6.dp),
        )
    }
}
