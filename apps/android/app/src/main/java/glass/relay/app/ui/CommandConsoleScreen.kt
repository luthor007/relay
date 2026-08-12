package glass.relay.app.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import glass.relay.bridge.GlassesRawCommands
import glass.relay.bridge.TransportProvider
import glass.relay.bridge.commands.CommandCatalog
import glass.relay.bridge.commands.CommandConsole
import glass.relay.bridge.commands.CommandRunner
import kotlinx.coroutines.launch

/**
 * Every glasses command, by hand.
 *
 * `ORCHESTRATOR.md` §5, job 2: "a product where the only input is speech fails
 * in a quiet room, a loud room, and on a bad day." So every command in the spec
 * gets a row.
 *
 * The list is **generated from `CommandCatalog`**, not written out. That is the
 * point: a command cannot be added to the protocol and quietly not appear here,
 * because there is no per-command UI code to forget to write. Rows the app must
 * not send — device reports, the spec's retired commands, and anything whose
 * request layout is not attested — are shown greyed with the reason, rather
 * than hidden. A missing row looks like a bug in the app; a row that says
 * "已弃用" is the truth about the device.
 *
 * Destructive commands need a second tap that names the command. `0x0911`
 * deletes exactly the audio that has not been synced yet.
 */
@Composable
fun CommandConsoleScreen() {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()

    val runner = remember {
        val transport = TransportProvider.get(context)
        val raw = transport as? GlassesRawCommands
        raw?.let { channel -> CommandRunner({ frame -> channel.sendFrame(frame) }) }
    }

    var lastEntry by remember { mutableStateOf<CommandRunner.Entry?>(null) }
    var confirming by remember { mutableStateOf<Int?>(null) }

    Box(
        Modifier
            .fillMaxSize()
            .background(Ground),
    ) {
        LazyColumn(
            Modifier
                .fillMaxSize()
                .padding(horizontal = 20.dp),
            contentPadding = androidx.compose.foundation.layout.PaddingValues(vertical = 32.dp),
        ) {
            item {
                Text(
                    "Device commands",
                    style = MaterialTheme.typography.headlineSmall,
                    color = Ink,
                )
                Spacer(Modifier.height(4.dp))
                Text(
                    "All ${CommandCatalog.ENTRIES.size} commands in the protocol. " +
                        if (runner == null) {
                            "This build's transport has no raw command channel, so nothing here can be sent."
                        } else {
                            "Greyed rows are ones the device or the spec will not accept."
                        },
                    style = MaterialTheme.typography.bodySmall,
                    color = InkDim,
                )
                Spacer(Modifier.height(16.dp))
            }

            lastEntry?.let { entry ->
                item {
                    ConsoleCard {
                        Text(entry.commandName, style = MaterialTheme.typography.labelLarge, color = Ink)
                        Spacer(Modifier.height(4.dp))
                        Text(
                            entry.detail,
                            style = MaterialTheme.typography.bodySmall,
                            color = when (entry.outcome) {
                                CommandRunner.Outcome.Sent -> InkMid
                                else -> InkDim
                            },
                        )
                    }
                    Spacer(Modifier.height(16.dp))
                }
            }

            for ((category, entries) in CommandCatalog.byCategory()) {
                item(key = "header-$category") {
                    Text(
                        category.name.uppercase(),
                        style = MaterialTheme.typography.labelSmall,
                        color = InkDim,
                        modifier = Modifier.padding(top = 20.dp, bottom = 8.dp),
                    )
                }
                items(entries, key = { it.id }) { entry ->
                    CommandRow(
                        entry = entry,
                        enabled = runner != null && entry.sendable,
                        awaitingConfirmation = confirming == entry.id,
                        onTap = {
                            if (entry.destructive && confirming != entry.id) {
                                confirming = entry.id
                                return@CommandRow
                            }
                            confirming = null
                            scope.launch {
                                lastEntry = runner?.run(
                                    commandId = entry.id,
                                    input = defaultInput(entry),
                                    confirmDestructive = entry.name.takeIf { entry.destructive },
                                )
                            }
                        },
                    )
                    Spacer(Modifier.height(8.dp))
                }
            }
        }
    }
}

/**
 * The argument the row sends when tapped.
 *
 * A one-tap default for the shapes that have an obvious one, and nothing for
 * the rest. Where a command needs a real choice — a device mode, a wake-word
 * selection — the console refuses a wrong argument rather than guessing, so the
 * worst case is a visible refusal rather than an unintended command.
 */
private fun defaultInput(entry: CommandCatalog.Entry): CommandConsole.Input =
    when (val spec = entry.args) {
        is CommandCatalog.ArgSpec.Toggle -> CommandConsole.Input.Toggle(on = true)
        is CommandCatalog.ArgSpec.Choice -> CommandConsole.Input.Choice(spec.options.first().value)
        is CommandCatalog.ArgSpec.WakeWordSelection ->
            CommandConsole.Input.WakeWords(
                listOf(CommandConsole.Input.WakeWords.Selection(0, enabled = true)),
            )
        else -> CommandConsole.Input.None
    }

@Composable
private fun CommandRow(
    entry: CommandCatalog.Entry,
    enabled: Boolean,
    awaitingConfirmation: Boolean,
    onTap: () -> Unit,
) {
    ConsoleCard(onClick = if (enabled) onTap else null) {
        Row(
            Modifier.fillMaxWidth(),
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(Modifier.fillMaxWidth(0.8f)) {
                Text(
                    entry.label,
                    style = MaterialTheme.typography.bodyMedium,
                    color = if (enabled) Ink else InkDim,
                )
                Text(
                    "0x%04X · %s".format(entry.id, entry.specName),
                    style = MaterialTheme.typography.bodySmall,
                    color = InkDim,
                )
            }
            Text(
                "0x%04X".format(entry.id),
                style = MaterialTheme.typography.labelSmall,
                color = InkDim,
            )
        }

        val reason = when {
            entry.role == CommandCatalog.CommandRole.Report -> "device report — nothing to send"
            entry.role == CommandCatalog.CommandRole.Unused -> "未使用 — the device does not implement it"
            entry.role == CommandCatalog.CommandRole.Deprecated -> "已弃用 — retired by the spec"
            entry.args is CommandCatalog.ArgSpec.Unattested ->
                "payload layout not attested — see APPS-SCOPE.md §5.1"
            else -> null
        }

        if (reason != null) {
            Spacer(Modifier.height(6.dp))
            Text(reason, style = MaterialTheme.typography.bodySmall, color = InkDim)
        }

        entry.note?.let {
            Spacer(Modifier.height(6.dp))
            Text(it, style = MaterialTheme.typography.bodySmall, color = InkMid)
        }

        if (awaitingConfirmation) {
            Spacer(Modifier.height(10.dp))
            Text(
                "This deletes data on the glasses. Tap again to send ${entry.name}.",
                style = MaterialTheme.typography.bodySmall,
                color = Live,
            )
            Spacer(Modifier.height(6.dp))
            Row {
                OutlinedButton(onClick = onTap) { Text("Send ${entry.name}", color = Ink) }
                Spacer(Modifier.fillMaxWidth(0.05f))
                TextButton(onClick = {}) { Text("Cancel", color = InkDim) }
            }
        }
    }
}

@Composable
private fun ConsoleCard(onClick: (() -> Unit)? = null, content: @Composable () -> Unit) {
    val base = Modifier
        .fillMaxWidth()
        .clip(RoundedCornerShape(12.dp))
        .background(Surface)
        .border(1.dp, Line, RoundedCornerShape(12.dp))
    Column(
        (if (onClick != null) base.clickable(onClick = onClick) else base).padding(14.dp),
    ) {
        content()
    }
}
