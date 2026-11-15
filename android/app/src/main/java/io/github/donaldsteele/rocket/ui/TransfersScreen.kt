package io.github.donaldsteele.rocket.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.Icon
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.ProgressBarRangeInfo
import androidx.compose.ui.semantics.progressBarRangeInfo
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.semantics.stateDescription
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import io.github.donaldsteele.rocket.core
import io.github.donaldsteele.rocket.core.Transfer
import io.github.donaldsteele.rocket.core.TransferState
import io.github.donaldsteele.rocket.ui.theme.Mono
import io.github.donaldsteele.rocket.ui.theme.Rocket

/** The sends of this session: progress, cancel, how each ended. */
@Composable
fun TransfersScreen(modifier: Modifier = Modifier) {
    val transfers = LocalContext.current.core.transfers
    val list by transfers.list.collectAsStateWithLifecycle()
    val starting by transfers.starting.collectAsStateWithLifecycle()
    val c = Rocket.colors

    LazyColumn(
        modifier = modifier.fillMaxSize(),
        contentPadding = PaddingValues(16.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        item {
            Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.fillMaxWidth()) {
                Text(
                    "Sends since rocket started. Finished ones are also in History.",
                    style = MaterialTheme.typography.bodyMedium,
                    color = c.fgMuted,
                    modifier = Modifier.weight(1f),
                )
                if (list.any { !it.state.active }) {
                    Spacer(Modifier.width(8.dp))
                    SecondaryAction("Clear finished", Lucide.Trash, onClick = { transfers.clearFinished() })
                }
            }
        }
        if (starting > 0) {
            item { Notice("Reading the files for ${if (starting == 1) "a send" else "$starting sends"}…", Tone.Info) }
        }
        if (list.isEmpty() && starting == 0) {
            item { EmptyBox("No transfers yet. Send something from the Send tab or the share sheet.") }
        }
        items(list, key = { it.id }) { t ->
            TransferCard(t, onCancel = { transfers.cancel(t.id) })
        }
    }
}

@Composable
private fun TransferCard(t: Transfer, onCancel: () -> Unit) {
    val c = Rocket.colors
    val (icon, word, tone) = when (t.state) {
        TransferState.Waiting -> Triple(Lucide.Clock, "Waiting", Tone.Info)
        TransferState.Sending -> Triple(Lucide.ArrowUp, "Sending", Tone.Info)
        TransferState.Done -> Triple(Lucide.CircleCheck, "Done", Tone.Ok)
        TransferState.Failed -> Triple(Lucide.TriangleAlert, "Failed", Tone.Bad)
        TransferState.Cancelled -> Triple(Lucide.Square, "Cancelled", Tone.Warn)
    }
    Panel {
        Column {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(Lucide.ArrowUp, contentDescription = null, tint = c.link, modifier = Modifier.size(22.dp))
                Spacer(Modifier.width(10.dp))
                Column(Modifier.weight(1f).semantics(mergeDescendants = true) {}) {
                    Text(countWhat(t.name, t.files), style = MaterialTheme.typography.titleMedium)
                    Text("to ${t.peerName}", style = MaterialTheme.typography.bodySmall, color = c.fgMuted)
                }
                Spacer(Modifier.width(8.dp))
                StatusChip(icon, word, tone)
            }
            if (t.state.active) {
                Spacer(Modifier.height(10.dp))
                val pct = percent(t.sent, t.total)
                val desc = if (t.state == TransferState.Waiting) "waiting for the receiver" else "$pct percent"
                if (t.state == TransferState.Waiting) {
                    LinearProgressIndicator(
                        modifier = Modifier.fillMaxWidth().semantics { stateDescription = desc },
                        color = c.accent,
                        trackColor = c.bgElev2,
                    )
                } else {
                    LinearProgressIndicator(
                        progress = { pct / 100f },
                        modifier = Modifier.fillMaxWidth().semantics {
                            stateDescription = desc
                            progressBarRangeInfo = ProgressBarRangeInfo(pct / 100f, 0f..1f)
                        },
                        color = c.accent,
                        trackColor = c.bgElev2,
                    )
                }
                Spacer(Modifier.height(6.dp))
                Text(
                    progressLine(t),
                    fontFamily = Mono,
                    fontSize = 12.sp,
                    color = c.fgMuted,
                )
                if (t.cancellable) {
                    Spacer(Modifier.height(8.dp))
                    SecondaryAction("Cancel", Lucide.X, onClick = onCancel, danger = true)
                }
            } else {
                val line = when (t.state) {
                    TransferState.Done -> "${formatSize(t.total)} sent"
                    else -> t.error
                }
                if (line.isNotEmpty()) {
                    Spacer(Modifier.height(6.dp))
                    Text(line, style = MaterialTheme.typography.bodySmall, color = c.fgMuted)
                }
            }
        }
    }
}

/** "12.3 MB of 40.0 MB · 2 of 5: beach.jpg" */
internal fun progressLine(t: Transfer): String {
    if (t.state == TransferState.Waiting) return "Waiting for ${t.peerName} to accept"
    val parts = mutableListOf("${formatSize(t.sent)} of ${formatSize(t.total)}")
    if (t.files > 1 && t.fileIndex > 0) {
        parts += "${t.fileIndex} of ${t.files}" + if (t.file.isNotEmpty()) ": ${t.file}" else ""
    }
    return parts.joinToString(" · ")
}
