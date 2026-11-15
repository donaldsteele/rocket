package io.github.donaldsteele.rocket.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import io.github.donaldsteele.rocket.core
import io.github.donaldsteele.rocket.core.HistoryEntry
import io.github.donaldsteele.rocket.core.parseHistory
import io.github.donaldsteele.rocket.ui.theme.Mono
import io.github.donaldsteele.rocket.ui.theme.Rocket
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** The history of finished transfers, newest first (kept by the Go core). */
@Composable
fun HistoryScreen(onMessage: (String) -> Unit, modifier: Modifier = Modifier) {
    val core = LocalContext.current.core
    val ended by core.transfers.ended.collectAsStateWithLifecycle()
    var reload by remember { mutableIntStateOf(0) }
    var entries by remember { mutableStateOf<List<HistoryEntry>?>(null) }
    var confirmClear by rememberSaveable { mutableStateOf(false) }
    val scope = rememberCoroutineScope()
    val c = Rocket.colors

    LaunchedEffect(ended, reload) {
        entries = withContext(Dispatchers.IO) {
            runCatching { parseHistory(core.node.historyJSON(HISTORY_LIMIT)) }.getOrElse {
                onMessage("Could not read the history: ${it.message}")
                emptyList()
            }
        }
    }

    if (confirmClear) {
        AlertDialog(
            onDismissRequest = { confirmClear = false },
            title = { Text("Clear the history?") },
            text = { Text("This forgets every finished transfer on this phone. Files are not touched.") },
            confirmButton = {
                TextButton(
                    onClick = {
                        confirmClear = false
                        scope.launch {
                            withContext(Dispatchers.IO) { runCatching { core.node.clearHistory() } }
                                .onFailure { onMessage("Could not clear the history: ${it.message}") }
                            reload++
                        }
                    },
                    modifier = Modifier.heightIn(min = 48.dp),
                ) { Text("Clear") }
            },
            dismissButton = {
                TextButton(onClick = { confirmClear = false }, modifier = Modifier.heightIn(min = 48.dp)) { Text("Keep") }
            },
        )
    }

    LazyColumn(
        modifier = modifier.fillMaxSize(),
        contentPadding = PaddingValues(16.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        item {
            Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.fillMaxWidth()) {
                Text(
                    "Finished transfers, newest first.",
                    style = MaterialTheme.typography.bodyMedium,
                    color = c.fgMuted,
                    modifier = Modifier.weight(1f),
                )
                if (!entries.isNullOrEmpty()) {
                    SecondaryAction("Clear", Lucide.Trash, onClick = { confirmClear = true })
                }
            }
        }
        val list = entries
        if (list != null && list.isEmpty()) {
            item { EmptyBox("No finished transfers yet.") }
        }
        items(list.orEmpty(), key = { it.id + it.ended }) { e -> HistoryRow(e) }
    }
}

private const val HISTORY_LIMIT = 200L

@Composable
private fun HistoryRow(e: HistoryEntry) {
    val c = Rocket.colors
    val (icon, word, tone) = resultLook(e.result)
    val incoming = e.dir == "in"
    Panel {
        Column {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(
                    if (incoming) Lucide.ArrowDown else Lucide.ArrowUp,
                    contentDescription = if (incoming) "Received" else "Sent",
                    tint = c.link,
                    modifier = Modifier.size(22.dp),
                )
                Spacer(Modifier.width(10.dp))
                Column(Modifier.weight(1f).semantics(mergeDescendants = true) {}) {
                    Text(countWhat(e.name, e.files), style = MaterialTheme.typography.titleMedium)
                    Text(
                        (if (incoming) "from " else "to ") + e.peerName.ifEmpty { "unknown" },
                        style = MaterialTheme.typography.bodySmall,
                        color = c.fgMuted,
                    )
                }
                Spacer(Modifier.width(8.dp))
                StatusChip(icon, word, tone)
            }
            if (showSecurity(e.result)) {
                Spacer(Modifier.height(6.dp))
                if (e.encrypted) EncryptedChip() else StatusChip(Lucide.LockOpen, "Not encrypted", Tone.Muted)
            }
            val meta = listOf(formatSize(e.bytes), formatWhen(e.ended.takeIf { it > 0 } ?: e.started))
                .filter { it.isNotEmpty() }
                .joinToString(" · ")
            Text(meta, fontFamily = Mono, fontSize = 12.sp, color = c.fgMuted, modifier = Modifier.padding(top = 6.dp))
            val why = if (e.result == "interrupted") interruptedHistoryLine(e) else e.error
            if (why.isNotEmpty() && e.result != "ok") {
                Text(why, style = MaterialTheme.typography.bodySmall, color = c.fgMuted)
            }
        }
    }
}

/**
 * Whether a history entry says if its bytes were encrypted: only when bytes
 * could have moved (not for a declined or unanswered offer), as on desktop.
 */
internal fun showSecurity(result: String): Boolean = result != "declined" && result != "timeout"

/** What an interrupted transfer's history entry says, instead of the raw network error. */
internal fun interruptedHistoryLine(e: HistoryEntry): String {
    val why = networkWhy(e.error)
    val lost = "The connection was lost" + (if (why.isNotEmpty()) " ($why)" else "") + "."
    return if (e.dir == "in") {
        "$lost What arrived was kept for 24 hours, to continue when the sender tries again."
    } else {
        "$lost It could be resumed from Transfers for 24 hours."
    }
}

/** A history result as a shape, a word and a tone, as on desktop. */
internal fun resultLook(result: String): Triple<ImageVector, String, Tone> = when (result) {
    "ok" -> Triple(Lucide.CircleCheck, "Done", Tone.Ok)
    "declined" -> Triple(Lucide.CircleX, "Declined", Tone.Warn)
    "cancelled" -> Triple(Lucide.Square, "Cancelled", Tone.Warn)
    "error" -> Triple(Lucide.TriangleAlert, "Failed", Tone.Bad)
    "timeout" -> Triple(Lucide.Clock, "No answer", Tone.Warn)
    "interrupted" -> Triple(Lucide.Unplug, "Interrupted", Tone.Warn)
    else -> Triple(Lucide.Info, result.ifEmpty { "Unknown" }, Tone.Muted)
}
