package io.github.donaldsteele.rocket.ui

import android.app.Application
import android.net.Uri
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
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
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.ProgressBarRangeInfo
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.progressBarRangeInfo
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.semantics.stateDescription
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.compose.LifecycleResumeEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import io.github.donaldsteele.rocket.core
import io.github.donaldsteele.rocket.core.InterruptedSend
import io.github.donaldsteele.rocket.core.KeptReceive
import io.github.donaldsteele.rocket.core.SendItem
import io.github.donaldsteele.rocket.core.Transfer
import io.github.donaldsteele.rocket.core.TransferState
import io.github.donaldsteele.rocket.ui.theme.Mono
import io.github.donaldsteele.rocket.ui.theme.Rocket
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/**
 * Resumes and discards interrupted transfers. A Resume uses the saved
 * items when rocket can still read them, else asks the person to pick the
 * files again ([pickAgain]).
 */
class ResumeModel(app: Application) : AndroidViewModel(app) {
    private val core = app.core

    private val _busy = MutableStateFlow<Set<String>>(emptySet())

    /** IDs being resumed or discarded right now. */
    val busy: StateFlow<Set<String>> = _busy.asStateFlow()

    private val _pickAgain = MutableStateFlow<InterruptedSend?>(null)

    /** An interrupted send whose files must be picked again to resume it. */
    val pickAgain: StateFlow<InterruptedSend?> = _pickAgain.asStateFlow()

    private val _messages = MutableSharedFlow<String>(extraBufferCapacity = 4)
    val messages: SharedFlow<String> = _messages.asSharedFlow()

    fun resume(s: InterruptedSend) = work(s.transferId) {
        val saved = withContext(Dispatchers.IO) { core.transfers.saved(s.transferId)?.takeIf { core.items.canRead(it.items) } }
        if (saved == null) {
            _pickAgain.value = s
            return@work
        }
        start(s, saved.items, saved.grants)
    }

    fun cancelPick() {
        _pickAgain.value = null
    }

    fun resumeWithFiles(s: InterruptedSend, uris: List<Uri>) = resumeWith(s, uris) { core.items.fromUris(uris) }

    fun resumeWithFolder(s: InterruptedSend, tree: Uri) = resumeWith(s, listOf(tree)) { core.items.fromTree(tree) }

    private fun resumeWith(s: InterruptedSend, picked: List<Uri>, read: () -> List<SendItem>) = work(s.transferId) {
        _pickAgain.value = null
        var kept = emptyList<String>()
        try {
            val items = withContext(Dispatchers.IO) {
                kept = picked.map { it.toString() }.filter { core.items.take(it) }
                read()
            }
            if (items.isEmpty()) {
                kept.forEach { core.items.release(it) }
                _messages.tryEmit("Nothing to send.")
                return@work
            }
            start(s, items, kept)
        } catch (e: Exception) {
            kept.forEach { core.items.release(it) }
            throw e
        }
    }

    private fun start(s: InterruptedSend, items: List<SendItem>, grants: List<String>) {
        core.resume(getApplication(), s, items, grants)
        _messages.tryEmit("Resuming ${countWhat(s.name, s.files)} to ${s.peerName}.")
    }

    fun discard(id: String, what: String) = work(id) {
        val err = withContext(Dispatchers.IO) { core.transfers.discard(id) }
        _messages.tryEmit(if (err == null) "Discarded $what." else "Could not discard $what: $err")
    }

    private fun work(id: String, body: suspend () -> Unit) {
        if (id in _busy.value) return
        _busy.update { it + id }
        viewModelScope.launch {
            try {
                body()
            } catch (e: Exception) {
                _messages.tryEmit("Could not resume: ${e.message ?: e}")
            } finally {
                _busy.update { it - id }
            }
        }
    }
}

/** The sends and receives of this session: progress, cancel, how each ended; interrupted ones to resume. */
@Composable
fun TransfersScreen(onMessage: (String) -> Unit, modifier: Modifier = Modifier) {
    val core = LocalContext.current.core
    val transfers = core.transfers
    val model: ResumeModel = viewModel()
    val list by transfers.list.collectAsStateWithLifecycle()
    val starting by transfers.starting.collectAsStateWithLifecycle()
    val resumable by transfers.resumable.collectAsStateWithLifecycle()
    val busy by model.busy.collectAsStateWithLifecycle()
    val pickAgain by model.pickAgain.collectAsStateWithLifecycle()
    val c = Rocket.colors

    LifecycleResumeEffect(Unit) {
        transfers.reloadResumable()
        onPauseOrDispose {}
    }
    LaunchedEffect(model) {
        model.messages.collect { onMessage(it) }
    }

    val pickFiles = rememberLauncherForActivityResult(ActivityResultContracts.OpenMultipleDocuments()) { uris ->
        val s = model.pickAgain.value
        if (s != null && uris.isNotEmpty()) model.resumeWithFiles(s, uris) else model.cancelPick()
    }
    val pickFolder = rememberLauncherForActivityResult(ActivityResultContracts.OpenDocumentTree()) { tree ->
        val s = model.pickAgain.value
        if (s != null && tree != null) model.resumeWithFolder(s, tree) else model.cancelPick()
    }
    pickAgain?.let { s ->
        PickAgainDialog(
            s,
            onFiles = { pickFiles.launch(arrayOf("*/*")) },
            onFolder = { pickFolder.launch(null) },
            onDismiss = { model.cancelPick() },
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
                    "Sends and receives since rocket started. Finished ones are also in History.",
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
        if (list.isEmpty() && starting == 0 && resumable.isEmpty) {
            item { EmptyBox("No transfers yet. Send something from the Send tab or the share sheet, or turn on receiving.") }
        }
        items(list, key = { it.id }) { t ->
            TransferCard(t, onCancel = { transfers.cancel(t.id) })
        }
        if (!resumable.isEmpty) {
            item {
                Column(Modifier.padding(top = 8.dp)) {
                    Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.semantics { heading() }) {
                        Icon(Lucide.Unplug, contentDescription = null, tint = c.warn, modifier = Modifier.size(20.dp))
                        Spacer(Modifier.width(8.dp))
                        Text("Interrupted transfers", style = MaterialTheme.typography.titleMedium)
                    }
                    Spacer(Modifier.height(4.dp))
                    Text(
                        "When a connection breaks, what arrived is kept for 24 hours, and the transfer continues where it stopped.",
                        style = MaterialTheme.typography.bodySmall,
                        color = c.fgMuted,
                    )
                }
            }
        }
        if (resumable.outgoing.isNotEmpty()) {
            item { SubHeading("Sends to resume") }
            items(resumable.outgoing, key = { "out-" + it.transferId }) { s ->
                InterruptedSendRow(
                    s,
                    busy = s.transferId in busy,
                    onResume = { model.resume(s) },
                    onDiscard = { model.discard(s.transferId, "the send of ${countWhat(s.name, s.files)}") },
                )
            }
        }
        if (resumable.incoming.isNotEmpty()) {
            item {
                Column {
                    SubHeading("Kept on this phone")
                    Text(
                        "These continue by themselves when the sender tries again, while receiving is on.",
                        style = MaterialTheme.typography.bodySmall,
                        color = c.fgMuted,
                    )
                }
            }
            items(resumable.incoming, key = { "in-" + it.id }) { k ->
                KeptReceiveRow(
                    k,
                    busy = k.id in busy,
                    onDiscard = { model.discard(k.id, "what arrived of ${countWhat(k.name, k.files)}") },
                )
            }
        }
    }
}

@Composable
private fun SubHeading(text: String) {
    Text(
        text,
        style = MaterialTheme.typography.titleSmall,
        color = Rocket.colors.fg,
        modifier = Modifier.padding(top = 4.dp).semantics { heading() },
    )
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun InterruptedSendRow(s: InterruptedSend, busy: Boolean, onResume: () -> Unit, onDiscard: () -> Unit) {
    val c = Rocket.colors
    Panel {
        Column {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(Lucide.ArrowUp, contentDescription = null, tint = c.link, modifier = Modifier.size(22.dp))
                Spacer(Modifier.width(10.dp))
                Column(Modifier.weight(1f).semantics(mergeDescendants = true) {}) {
                    Text(countWhat(s.name, s.files), style = MaterialTheme.typography.titleMedium)
                    Text("to ${s.peerName}", style = MaterialTheme.typography.bodySmall, color = c.fgMuted)
                }
                Spacer(Modifier.width(8.dp))
                StatusChip(Lucide.Unplug, "Interrupted", Tone.Warn)
            }
            Spacer(Modifier.height(6.dp))
            Text(interruptedMeta(s), fontFamily = Mono, fontSize = 12.sp, color = c.fgMuted)
            Spacer(Modifier.height(8.dp))
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                PrimaryAction(
                    "Resume",
                    Lucide.Play,
                    onClick = onResume,
                    enabled = !busy,
                    modifier = Modifier.semantics { contentDescription = "Resume sending ${countWhat(s.name, s.files)} to ${s.peerName}" },
                )
                SecondaryAction(
                    "Discard",
                    Lucide.Trash,
                    onClick = onDiscard,
                    enabled = !busy,
                    danger = true,
                    modifier = Modifier.semantics { contentDescription = "Discard the interrupted send of ${countWhat(s.name, s.files)}" },
                )
            }
        }
    }
}

@Composable
private fun KeptReceiveRow(k: KeptReceive, busy: Boolean, onDiscard: () -> Unit) {
    val c = Rocket.colors
    Panel {
        Column {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(Lucide.ArrowDown, contentDescription = null, tint = c.link, modifier = Modifier.size(22.dp))
                Spacer(Modifier.width(10.dp))
                Column(Modifier.weight(1f).semantics(mergeDescendants = true) {}) {
                    Text(countWhat(k.name, k.files), style = MaterialTheme.typography.titleMedium)
                    Text("from ${k.senderName}", style = MaterialTheme.typography.bodySmall, color = c.fgMuted)
                }
                Spacer(Modifier.width(8.dp))
                StatusChip(Lucide.Unplug, "Partly received", Tone.Warn)
            }
            Spacer(Modifier.height(6.dp))
            Text(keptMeta(k), fontFamily = Mono, fontSize = 12.sp, color = c.fgMuted)
            Spacer(Modifier.height(8.dp))
            SecondaryAction(
                "Discard",
                Lucide.Trash,
                onClick = onDiscard,
                enabled = !busy,
                danger = true,
                modifier = Modifier.semantics { contentDescription = "Discard what arrived of ${countWhat(k.name, k.files)}" },
            )
        }
    }
}

/** Asks for the files of an interrupted send again, when rocket can no longer read them. */
@Composable
private fun PickAgainDialog(s: InterruptedSend, onFiles: () -> Unit, onFolder: () -> Unit, onDismiss: () -> Unit) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Pick the files again") },
        text = {
            Text(
                "rocket can no longer open ${countWhat(s.name, s.files)}. Pick the same files or folder again to " +
                    "resume. Unchanged files continue where they stopped; anything else is sent again.",
                style = MaterialTheme.typography.bodyMedium,
            )
        },
        confirmButton = {
            Row {
                TextButton(onClick = onFolder, modifier = Modifier.heightIn(min = 48.dp)) { Text("Folder") }
                TextButton(onClick = onFiles, modifier = Modifier.heightIn(min = 48.dp)) { Text("Files") }
            }
        },
        dismissButton = {
            TextButton(onClick = onDismiss, modifier = Modifier.heightIn(min = 48.dp)) { Text("Cancel") }
        },
    )
}

/** The status of a transfer as a shape, a word, a tone and (when it differs) what a screen reader says. */
internal data class Look(val icon: androidx.compose.ui.graphics.vector.ImageVector, val word: String, val tone: Tone, val spoken: String? = null)

internal fun transferLook(t: Transfer): Look = when (t.state) {
    TransferState.Waiting -> Look(Lucide.Clock, "Waiting", Tone.Info)
    TransferState.Sending -> if (t.incoming) Look(Lucide.ArrowDown, "Receiving", Tone.Info) else Look(Lucide.ArrowUp, "Sending", Tone.Info)
    TransferState.Reconnecting -> Look(
        Lucide.RefreshCw,
        reconnectingWord(t.attempt, t.attempts),
        Tone.Warn,
        reconnectingSpoken(t.attempt, t.attempts),
    )
    TransferState.Done -> Look(Lucide.CircleCheck, "Done", Tone.Ok)
    TransferState.Failed -> Look(Lucide.TriangleAlert, "Failed", Tone.Bad)
    TransferState.Cancelled -> Look(Lucide.Square, "Cancelled", Tone.Warn)
    TransferState.Interrupted -> Look(Lucide.Unplug, "Interrupted", Tone.Warn)
}

@Composable
private fun TransferCard(t: Transfer, onCancel: () -> Unit) {
    val c = Rocket.colors
    val look = transferLook(t)
    Panel {
        Column {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(if (t.incoming) Lucide.ArrowDown else Lucide.ArrowUp, contentDescription = null, tint = c.link, modifier = Modifier.size(22.dp))
                Spacer(Modifier.width(10.dp))
                Column(Modifier.weight(1f).semantics(mergeDescendants = true) {}) {
                    Text(countWhat(t.name, t.files), style = MaterialTheme.typography.titleMedium)
                    Text(
                        if (t.incoming) "from ${t.peerName}" else "to ${t.peerName}",
                        style = MaterialTheme.typography.bodySmall,
                        color = c.fgMuted,
                    )
                }
                Spacer(Modifier.width(8.dp))
                StatusChip(look.icon, look.word, look.tone, spoken = look.spoken)
            }
            // Encrypted once bytes can move: not on a send refused before any did.
            if (t.encrypted && (t.state.active || t.state == TransferState.Done || t.sent > 0)) {
                Spacer(Modifier.height(6.dp))
                EncryptedChip()
            }
            if (t.state.active) {
                Spacer(Modifier.height(10.dp))
                val pct = percent(t.sent, t.total)
                val desc = when (t.state) {
                    TransferState.Waiting -> "waiting for the receiver"
                    TransferState.Reconnecting -> "reconnecting, $pct percent"
                    else -> "$pct percent"
                }
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
                        color = if (t.state == TransferState.Reconnecting) c.warn else c.accent,
                        trackColor = c.bgElev2,
                    )
                }
                Spacer(Modifier.height(6.dp))
                if (t.state == TransferState.Reconnecting) {
                    Text(reconnectingLine(t.error), style = MaterialTheme.typography.bodySmall, color = c.fgMuted)
                    Spacer(Modifier.height(2.dp))
                }
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
                    TransferState.Done -> doneLine(t)
                    TransferState.Interrupted -> interruptedLine(t)
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

/** "12.3 MB sent", or "12.3 MB received, saved in Download/rocket". */
internal fun doneLine(t: Transfer, where: String = "Download/rocket"): String =
    if (t.incoming) "${formatSize(t.total)} received, saved in $where" else "${formatSize(t.total)} sent"

/** What happens to an interrupted transfer. */
internal fun interruptedLine(t: Transfer): String =
    if (t.incoming) {
        "The connection was lost. What arrived is kept for 24 hours and continues when ${t.peerName} sends again."
    } else {
        "The connection was lost. Resume it under Interrupted transfers within 24 hours."
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
