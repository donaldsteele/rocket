package io.github.donaldsteele.rocket.ui

import android.app.Application
import android.net.Uri
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
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
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.listSaver
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.LiveRegionMode
import androidx.compose.ui.semantics.liveRegion
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.compose.LifecycleStartEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import io.github.donaldsteele.rocket.core
import io.github.donaldsteele.rocket.core.SendItem
import io.github.donaldsteele.rocket.core.SendTarget
import io.github.donaldsteele.rocket.core.canSend
import io.github.donaldsteele.rocket.core.summarize
import io.github.donaldsteele.rocket.ui.theme.Mono
import io.github.donaldsteele.rocket.ui.theme.Rocket
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** Reads the picked files or folder (off the main thread) and starts the send. */
class SendModel(app: Application) : AndroidViewModel(app) {
    private val core = app.core
    private val _preparing = MutableStateFlow(false)

    /** True while a pick is being read (a big folder takes a moment). */
    val preparing: StateFlow<Boolean> = _preparing.asStateFlow()

    private val _messages = MutableSharedFlow<String>(extraBufferCapacity = 4)
    val messages: SharedFlow<String> = _messages.asSharedFlow()

    fun sendFiles(target: SendTarget, uris: List<Uri>) = send(target, uris) { core.items.fromUris(uris) }

    fun sendFolder(target: SendTarget, tree: Uri) = send(target, listOf(tree)) { core.items.fromTree(tree) }

    /**
     * Reads the picked items and sends them. picked are the URIs the picker
     * returned: rocket takes persistable access to them, so the send can be
     * resumed after the app was closed (released when it is not needed).
     */
    private fun send(target: SendTarget, picked: List<Uri>, read: () -> List<SendItem>) {
        viewModelScope.launch {
            _preparing.value = true
            var kept = emptyList<String>()
            try {
                val items = withContext(Dispatchers.IO) {
                    kept = picked.map { it.toString() }.filter { core.items.take(it) }
                    read()
                }
                if (items.isEmpty()) {
                    _messages.tryEmit("Nothing to send.")
                    kept.forEach { core.items.release(it) }
                    return@launch
                }
                core.send(getApplication(), target, items, kept = kept)
                val s = summarize(items)
                _messages.tryEmit("Sending ${countWhat(s.label, s.files)} to ${target.name}. Progress is in Transfers.")
            } catch (e: Exception) {
                kept.forEach { core.items.release(it) }
                _messages.tryEmit("Could not send to ${target.name}: ${e.message ?: e}")
            } finally {
                _preparing.value = false
            }
        }
    }
}

private val TargetSaver = listSaver<SendTarget?, String>(
    save = { t -> if (t == null) emptyList() else listOf(t.addr, t.fp, t.name) },
    restore = { l -> if (l.size == 3) SendTarget(l[0], l[1], l[2]) else null },
)

/** Pick a peer, then files or a folder. */
@Composable
fun SendScreen(onMessage: (String) -> Unit, modifier: Modifier = Modifier) {
    val context = LocalContext.current
    val core = context.core
    val model: SendModel = viewModel()
    val peers by core.discovery.peers.collectAsStateWithLifecycle()
    val problem by core.discovery.problem.collectAsStateWithLifecycle()
    val preparing by model.preparing.collectAsStateWithLifecycle()
    val name by core.name.collectAsStateWithLifecycle()
    var target by rememberSaveable(stateSaver = TargetSaver) { mutableStateOf<SendTarget?>(null) }
    var askAddress by rememberSaveable { mutableStateOf(false) }

    LifecycleStartEffect(Unit) {
        core.discovery.acquire()
        onStopOrDispose { core.discovery.release() }
    }
    LaunchedEffect(model) {
        model.messages.collect { onMessage(it) }
    }

    val askNotifications = rememberNotificationAsk()
    val pickFiles = rememberLauncherForActivityResult(ActivityResultContracts.OpenMultipleDocuments()) { uris ->
        val t = target
        if (t != null && uris.isNotEmpty()) {
            model.sendFiles(t, uris)
            askNotifications()
        }
    }
    val pickFolder = rememberLauncherForActivityResult(ActivityResultContracts.OpenDocumentTree()) { tree ->
        val t = target
        if (t != null && tree != null) {
            model.sendFolder(t, tree)
            askNotifications()
        }
    }
    fun files(t: SendTarget) {
        target = t
        pickFiles.launch(arrayOf("*/*"))
    }
    fun folder(t: SendTarget) {
        target = t
        pickFolder.launch(null)
    }

    if (askAddress) {
        AddressDialog(
            onDismiss = { askAddress = false },
            confirm = "Files",
            allowFolder = true,
        ) { addr, isFolder ->
            askAddress = false
            val t = SendTarget(addr = addr, fp = "", name = addr)
            if (isFolder) folder(t) else files(t)
        }
    }

    val c = Rocket.colors
    LazyColumn(
        modifier = modifier.fillMaxSize(),
        contentPadding = PaddingValues(16.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        item {
            Column {
                Text("Pick a device on this network, then files or a folder.", style = MaterialTheme.typography.bodyMedium, color = c.fgMuted)
                Spacer(Modifier.height(4.dp))
                Text(
                    "This phone: $name · ${core.fingerprint}",
                    fontFamily = Mono,
                    fontSize = 12.sp,
                    color = c.fgMuted,
                )
                Spacer(Modifier.height(8.dp))
            }
        }
        if (preparing) {
            item {
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    modifier = Modifier.semantics { liveRegion = LiveRegionMode.Polite },
                ) {
                    CircularProgressIndicator(modifier = Modifier.size(20.dp), strokeWidth = 2.dp)
                    Spacer(Modifier.width(10.dp))
                    Text("Reading the files…", style = MaterialTheme.typography.bodyMedium)
                }
            }
        }
        problem?.let { p -> item { Notice(p, Tone.Warn) } }
        if (peers.any { it.canSend(PROTO) }) {
            item { EncryptedNote() }
        }
        if (peers.isEmpty()) {
            item {
                EmptyBox(
                    "Looking for rocket hosts… Both devices need to be on the same Wi-Fi network, " +
                        "with rocket running on the other one.",
                )
            }
        }
        items(peers, key = { it.fp }) { p ->
            PeerRow(p) {
                val t = SendTarget(addr = p.addr, fp = p.fp, name = p.name)
                PrimaryAction("Files", Lucide.Upload, onClick = { files(t) }, enabled = !preparing)
                SecondaryAction("Folder", Lucide.Folder, onClick = { folder(t) }, enabled = !preparing)
            }
        }
        item {
            Spacer(Modifier.height(8.dp))
            SecondaryAction(
                "Send to an address",
                Lucide.Send,
                onClick = { askAddress = true },
                enabled = !preparing,
                modifier = Modifier.fillMaxWidth(),
            )
        }
    }
}

