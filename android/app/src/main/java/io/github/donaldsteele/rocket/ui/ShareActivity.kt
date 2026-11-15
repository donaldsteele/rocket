package io.github.donaldsteele.rocket.ui

import android.app.Application
import android.content.Intent
import android.net.Uri
import android.os.Bundle
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.consumeWindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.content.IntentCompat
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.compose.LifecycleStartEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import io.github.donaldsteele.rocket.core
import io.github.donaldsteele.rocket.core.SendItem
import io.github.donaldsteele.rocket.core.SendSummary
import io.github.donaldsteele.rocket.core.SendTarget
import io.github.donaldsteele.rocket.core.summarize
import io.github.donaldsteele.rocket.ui.theme.Mono
import io.github.donaldsteele.rocket.ui.theme.Rocket
import io.github.donaldsteele.rocket.ui.theme.RocketTheme
import io.github.donaldsteele.rocket.ui.theme.isDark
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** The share sheet target: what was shared, then pick a peer to send it to. */
class ShareActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val uris = sharedUris(intent)
        setContent {
            val mode by core.theme.collectAsStateWithLifecycle()
            RocketTheme(mode) {
                SystemBars(mode.isDark())
                ShareScreen(uris, onSent = { name ->
                    Toast.makeText(this, "Sending to $name", Toast.LENGTH_SHORT).show()
                    finish()
                })
            }
        }
    }
}

/** The content URIs of an ACTION_SEND or ACTION_SEND_MULTIPLE intent. */
private fun sharedUris(intent: Intent): List<Uri> {
    val out = LinkedHashSet<Uri>()
    when (intent.action) {
        Intent.ACTION_SEND ->
            IntentCompat.getParcelableExtra(intent, Intent.EXTRA_STREAM, Uri::class.java)?.let { out += it }
        Intent.ACTION_SEND_MULTIPLE ->
            IntentCompat.getParcelableArrayListExtra(intent, Intent.EXTRA_STREAM, Uri::class.java)?.let { out += it }
    }
    intent.clipData?.let { clip ->
        for (i in 0 until clip.itemCount) clip.getItemAt(i).uri?.let { out += it }
    }
    return out.toList()
}

sealed interface ShareState {
    data object Reading : ShareState
    data class Ready(val items: List<SendItem>, val summary: SendSummary) : ShareState
    data class Problem(val message: String) : ShareState
}

class ShareModel(app: Application) : AndroidViewModel(app) {
    private val _state = MutableStateFlow<ShareState>(ShareState.Reading)
    val state: StateFlow<ShareState> = _state.asStateFlow()
    private var loaded = false

    fun load(uris: List<Uri>) {
        if (loaded) return
        loaded = true
        if (uris.isEmpty()) {
            _state.value = ShareState.Problem("There are no files in what was shared. rocket sends files, not text or links.")
            return
        }
        viewModelScope.launch {
            _state.value = try {
                val items = withContext(Dispatchers.IO) { getApplication<Application>().core.items.fromUris(uris) }
                ShareState.Ready(items, summarize(items))
            } catch (e: Exception) {
                ShareState.Problem("Could not read what was shared: ${e.message ?: e}")
            }
        }
    }
}

@Composable
private fun ShareScreen(uris: List<Uri>, onSent: (String) -> Unit) {
    val context = LocalContext.current
    val core = context.core
    val model: ShareModel = viewModel()
    LaunchedEffect(model) { model.load(uris) }
    val state by model.state.collectAsStateWithLifecycle()
    val peers by core.discovery.peers.collectAsStateWithLifecycle()
    val problem by core.discovery.problem.collectAsStateWithLifecycle()
    var askAddress by rememberSaveable { mutableStateOf(false) }
    var error by rememberSaveable { mutableStateOf<String?>(null) }
    val c = Rocket.colors

    LifecycleStartEffect(Unit) {
        core.discovery.acquire()
        onStopOrDispose { core.discovery.release() }
    }

    val ready = state as? ShareState.Ready
    fun send(t: SendTarget) {
        val r = ready ?: return
        try {
            core.send(context, t, r.items, grants = uris)
            onSent(t.name)
        } catch (e: Exception) {
            error = "Could not send to ${t.name}: ${e.message ?: e}"
        }
    }

    if (askAddress) {
        AddressDialog(onDismiss = { askAddress = false }, confirm = "Send") { addr, _ ->
            askAddress = false
            send(SendTarget(addr = addr, fp = "", name = addr))
        }
    }

    Scaffold(containerColor = c.bg, topBar = { TopBar("Send with rocket") }) { padding ->
        LazyColumn(
            modifier = Modifier.fillMaxSize().padding(padding).consumeWindowInsets(padding),
            contentPadding = PaddingValues(16.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            item {
                when (val s = state) {
                    ShareState.Reading -> Row(verticalAlignment = Alignment.CenterVertically) {
                        CircularProgressIndicator(Modifier.size(20.dp), strokeWidth = 2.dp)
                        Spacer(Modifier.width(10.dp))
                        Text("Reading what was shared…")
                    }
                    is ShareState.Problem -> Notice(s.message, Tone.Bad)
                    is ShareState.Ready -> SharedSummary(s)
                }
            }
            error?.let { e -> item { Notice(e, Tone.Bad) } }
            if (ready != null) {
                item {
                    Text(
                        "Send to",
                        style = MaterialTheme.typography.titleMedium,
                        modifier = Modifier.padding(top = 8.dp),
                    )
                }
                problem?.let { p -> item { Notice(p, Tone.Warn) } }
                if (peers.isEmpty()) {
                    item { EmptyBox("Looking for rocket hosts on this network…") }
                }
                items(peers, key = { it.fp }) { p ->
                    PeerRow(p) {
                        PrimaryAction("Send", Lucide.Send, onClick = { send(SendTarget(p.addr, p.fp, p.name)) })
                    }
                }
                item {
                    SecondaryAction(
                        "Send to an address",
                        Lucide.Send,
                        onClick = { askAddress = true },
                        modifier = Modifier.fillMaxWidth().padding(top = 8.dp),
                    )
                }
            }
        }
    }
}

@Composable
private fun SharedSummary(s: ShareState.Ready) {
    val c = Rocket.colors
    Panel {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Icon(Lucide.File, contentDescription = null, tint = c.link, modifier = Modifier.size(24.dp))
            Spacer(Modifier.width(12.dp))
            Column(Modifier.weight(1f)) {
                Text(countWhat(s.summary.label, s.summary.files), style = MaterialTheme.typography.titleMedium)
                val size = if (s.summary.unknownSize) "size unknown" else formatSize(s.summary.bytes)
                Text(
                    "${s.summary.files} ${if (s.summary.files == 1) "file" else "files"} · $size",
                    fontFamily = Mono,
                    fontSize = 12.sp,
                    color = c.fgMuted,
                )
                if (s.items.size > 1) {
                    Spacer(Modifier.height(4.dp))
                    Text(
                        s.items.take(5).joinToString(", ") { it.path } + if (s.items.size > 5) ", …" else "",
                        fontFamily = Mono,
                        fontSize = 12.sp,
                        color = c.fgMuted,
                    )
                }
            }
        }
    }
}
