package io.github.donaldsteele.rocket.ui

import android.content.ClipData
import android.content.ClipboardManager
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.Surface
import androidx.compose.runtime.produceState
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.selection.toggleable
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Switch
import androidx.compose.material3.SwitchDefaults
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.remember
import androidx.compose.ui.semantics.contentDescription
import io.github.donaldsteele.rocket.core.TrustedSender
import io.github.donaldsteele.rocket.core.rocketcore.Rocketcore
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import io.github.donaldsteele.rocket.BuildConfig
import io.github.donaldsteele.rocket.core
import io.github.donaldsteele.rocket.core.ThemeMode
import io.github.donaldsteele.rocket.ui.theme.Mono
import io.github.donaldsteele.rocket.ui.theme.Rocket

private const val PROJECT_URL = "https://github.com/donaldsteele/rocket"

/** Device name, trusted senders, appearance, security note and about. */
@Composable
fun SettingsScreen(onMessage: (String) -> Unit, modifier: Modifier = Modifier) {
    val context = LocalContext.current
    val core = context.core
    val name by core.name.collectAsStateWithLifecycle()
    val theme by core.theme.collectAsStateWithLifecycle()
    val trusted by core.trusted.collectAsStateWithLifecycle()
    val autoAccept by core.autoAccept.collectAsStateWithLifecycle()
    val trustChanged by core.receiver.trustChanged.collectAsStateWithLifecycle()
    var revoking by remember { mutableStateOf<TrustedSender?>(null) }
    var showLicenses by rememberSaveable { mutableStateOf(false) }
    if (showLicenses) LicensesDialog(onDismiss = { showLicenses = false })
    LaunchedEffect(trustChanged) { core.reloadTrusted() }
    var draft by rememberSaveable(name) { mutableStateOf(name) }
    val focus = LocalFocusManager.current
    val uri = LocalUriHandler.current
    val c = Rocket.colors

    fun saveName() {
        focus.clearFocus()
        if (draft.trim() == name) return
        val announced = core.setName(draft)
        draft = core.name.value
        onMessage(
            if (announced) {
                "Others now see this phone as ${core.name.value}."
            } else {
                "Saved. Receiving announces the new name once the current offer or receive is over and you turn it off and on."
            },
        )
    }

    revoking?.let { t ->
        RevokeDialog(
            t,
            onConfirm = {
                revoking = null
                runCatching { core.revoke(t.fingerprint) }
                    .onSuccess { onMessage("${t.name} is no longer trusted.") }
                    .onFailure { onMessage("Could not revoke: ${it.message ?: it}") }
            },
            onDismiss = { revoking = null },
        )
    }

    Column(
        modifier = modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        Panel {
            Column {
                SectionTitle(Lucide.Smartphone, "This device")
                OutlinedTextField(
                    value = draft,
                    onValueChange = { draft = it.take(64) },
                    label = { Text("Device name") },
                    singleLine = true,
                    supportingText = { Text("Others see this name. It is a label, not proof of who you are: the fingerprint is.") },
                    keyboardOptions = KeyboardOptions(imeAction = ImeAction.Done),
                    keyboardActions = KeyboardActions(onDone = { saveName() }),
                    modifier = Modifier.fillMaxWidth(),
                )
                Spacer(Modifier.height(4.dp))
                PrimaryAction("Save name", Lucide.Check, onClick = { saveName() }, enabled = draft.trim() != name)
                Spacer(Modifier.height(12.dp))
                Text("Fingerprint", style = MaterialTheme.typography.bodySmall, color = c.fgMuted)
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Icon(Lucide.Fingerprint, contentDescription = null, tint = c.link, modifier = Modifier.size(18.dp))
                    Spacer(Modifier.width(8.dp))
                    Text(core.fingerprint, fontFamily = Mono, fontSize = 15.sp, modifier = Modifier.weight(1f))
                    IconButton(onClick = {
                        context.getSystemService(ClipboardManager::class.java)
                            ?.setPrimaryClip(ClipData.newPlainText("rocket fingerprint", core.fingerprint))
                        onMessage("Fingerprint copied.")
                    }) {
                        Icon(Lucide.Copy, contentDescription = "Copy fingerprint", modifier = Modifier.size(20.dp))
                    }
                }
                Text(
                    "Compare it with what the other device shows before you choose \"Always accept\" there.",
                    style = MaterialTheme.typography.bodySmall,
                    color = c.fgMuted,
                )
            }
        }

        Panel {
            Column {
                SectionTitle(Lucide.ShieldCheck, "Trusted senders")
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    modifier = Modifier
                        .fillMaxWidth()
                        .heightIn(min = 48.dp)
                        .toggleable(
                            value = autoAccept,
                            role = Role.Switch,
                            onValueChange = { on ->
                                runCatching { core.setAutoAccept(on) }
                                    .onFailure { onMessage("Could not save the setting: ${it.message ?: it}") }
                            },
                        ),
                ) {
                    Text("Accept trusted senders without asking", style = MaterialTheme.typography.bodyLarge, modifier = Modifier.weight(1f))
                    Spacer(Modifier.width(8.dp))
                    Switch(
                        checked = autoAccept,
                        onCheckedChange = null,
                        colors = SwitchDefaults.colors(checkedTrackColor = c.accent, checkedThumbColor = c.accentFg),
                    )
                }
                Text(
                    "A sender becomes trusted when you choose \"Always accept from this device\". Its key is pinned: " +
                        "another device using the same name is still asked. Turn this off to be asked every time.",
                    style = MaterialTheme.typography.bodySmall,
                    color = c.fgMuted,
                )
                Spacer(Modifier.height(8.dp))
                if (trusted.isEmpty()) {
                    Text("No trusted senders yet.", style = MaterialTheme.typography.bodyMedium, color = c.fgMuted)
                }
                trusted.forEach { t ->
                    Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.fillMaxWidth().padding(vertical = 4.dp)) {
                        Column(Modifier.weight(1f).semantics(mergeDescendants = true) {}) {
                            Text(t.name, style = MaterialTheme.typography.bodyLarge)
                            Text(t.fingerprint, fontFamily = Mono, fontSize = 12.sp, color = c.fgMuted)
                            val since = formatWhen(t.approvedAt)
                            if (since.isNotEmpty()) {
                                Text("Trusted since $since", style = MaterialTheme.typography.bodySmall, color = c.fgMuted)
                            }
                        }
                        Spacer(Modifier.width(8.dp))
                        SecondaryAction(
                            "Revoke",
                            Lucide.X,
                            onClick = { revoking = t },
                            danger = true,
                            modifier = Modifier.semantics { contentDescription = "Revoke ${t.name}" },
                        )
                    }
                }
            }
        }

        Panel {
            Column(Modifier.selectableGroup()) {
                SectionTitle(Lucide.Sun, "Appearance")
                ThemeOption(ThemeMode.Dark, "Dark", Lucide.Moon, theme) { core.setTheme(it) }
                ThemeOption(ThemeMode.Light, "Light", Lucide.Sun, theme) { core.setTheme(it) }
                ThemeOption(ThemeMode.System, "Same as the system", Lucide.Smartphone, theme) { core.setTheme(it) }
            }
        }

        Panel {
            Column {
                SectionTitle(Lucide.ShieldCheck, "Security")
                Text(
                    "Every offer is signed by the sender's key, and \"Always accept from this device\" pins that key, " +
                        "so another device cannot pass itself off as one you trust. The files themselves travel as " +
                        "plain HTTP on your network: someone who can watch the network could read them. Use rocket " +
                        "on networks you trust.",
                    style = MaterialTheme.typography.bodyMedium,
                )
                TextButton(onClick = { uri.openUri("$PROJECT_URL/blob/main/docs/security.md") }, modifier = Modifier.heightIn(min = 48.dp)) {
                    Text("Read the security notes")
                }
            }
        }

        Panel {
            Column {
                SectionTitle(Lucide.Info, "About")
                Text("rocket ${BuildConfig.VERSION_NAME} for Android · protocol v${Rocketcore.ProtoVersion}", style = MaterialTheme.typography.bodyMedium)
                Spacer(Modifier.height(4.dp))
                Text(
                    "LAN file transfer between rocket hosts. The wire protocol is rocket's Go code, the same as on desktop.",
                    style = MaterialTheme.typography.bodySmall,
                    color = c.fgMuted,
                )
                Spacer(Modifier.height(4.dp))
                Text(
                    "Includes the Go standard library and Go modules (BSD, MIT, Apache 2.0), AndroidX, Jetpack " +
                        "Compose and Kotlin (Apache 2.0), the Inter and JetBrains Mono fonts (SIL Open Font " +
                        "License 1.1) and Lucide icons (ISC).",
                    style = MaterialTheme.typography.bodySmall,
                    color = c.fgMuted,
                )
                TextButton(onClick = { showLicenses = true }, modifier = Modifier.heightIn(min = 48.dp)) {
                    Text("Open-source licences")
                }
                TextButton(onClick = { uri.openUri(PROJECT_URL) }, modifier = Modifier.heightIn(min = 48.dp)) {
                    Text("Project page")
                }
            }
        }
    }
}

/** The licence texts of everything the app bundles (assets/licenses, made by tools/licenses). */
@Composable
private fun LicensesDialog(onDismiss: () -> Unit) {
    val context = LocalContext.current
    val c = Rocket.colors
    val paragraphs by produceState<List<String>?>(initialValue = null) {
        value = withContext(Dispatchers.IO) {
            runCatching {
                context.assets.open(LICENSES_ASSET).bufferedReader().use { it.readText() }
                    .replace("\r\n", "\n")
                    .split(Regex("\n{2,}"))
                    .map { it.trimEnd() }
                    .filter { it.isNotBlank() }
            }.getOrElse { listOf("The licence texts could not be read: ${it.message}. They are in THIRD_PARTY_LICENSES.txt on the project page.") }
        }
    }
    Dialog(onDismissRequest = onDismiss, properties = DialogProperties(usePlatformDefaultWidth = false)) {
        Surface(color = c.bg, modifier = Modifier.fillMaxSize()) {
            Column(Modifier.fillMaxSize().safeDrawingPadding()) {
                Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.padding(start = 16.dp, end = 4.dp)) {
                    Text(
                        "Open-source licences",
                        style = MaterialTheme.typography.titleLarge,
                        modifier = Modifier.weight(1f).semantics { heading() },
                    )
                    TextButton(onClick = onDismiss, modifier = Modifier.heightIn(min = 48.dp)) { Text("Close") }
                }
                val list = paragraphs
                if (list == null) {
                    Text("Loading…", modifier = Modifier.padding(16.dp))
                } else {
                    LazyColumn(
                        contentPadding = PaddingValues(16.dp),
                        verticalArrangement = Arrangement.spacedBy(10.dp),
                        modifier = Modifier.fillMaxSize(),
                    ) {
                        items(list) { p -> Text(p, fontFamily = Mono, fontSize = 11.sp, color = c.fg) }
                    }
                }
            }
        }
    }
}

private const val LICENSES_ASSET = "licenses/THIRD_PARTY_LICENSES.txt"

/** Confirms revoking a trusted sender. */
@Composable
private fun RevokeDialog(t: TrustedSender, onConfirm: () -> Unit, onDismiss: () -> Unit) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Revoke ${t.name}?") },
        text = { Text("Its next offer asks you again. Fingerprint ${t.fingerprint}.") },
        confirmButton = {
            TextButton(onClick = onConfirm, modifier = Modifier.heightIn(min = 48.dp)) { Text("Revoke") }
        },
        dismissButton = {
            TextButton(onClick = onDismiss, modifier = Modifier.heightIn(min = 48.dp)) { Text("Cancel") }
        },
    )
}

@Composable
private fun SectionTitle(icon: ImageVector, text: String) {
    Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.padding(bottom = 8.dp)) {
        Icon(icon, contentDescription = null, tint = Rocket.colors.link, modifier = Modifier.size(18.dp))
        Spacer(Modifier.width(8.dp))
        Text(text, style = MaterialTheme.typography.titleMedium, modifier = Modifier.semantics { heading() })
    }
}

@Composable
private fun ThemeOption(mode: ThemeMode, label: String, icon: ImageVector, current: ThemeMode, onPick: (ThemeMode) -> Unit) {
    Row(
        verticalAlignment = Alignment.CenterVertically,
        modifier = Modifier
            .fillMaxWidth()
            .heightIn(min = 48.dp)
            .selectable(selected = mode == current, role = Role.RadioButton, onClick = { onPick(mode) }),
    ) {
        RadioButton(selected = mode == current, onClick = null)
        Spacer(Modifier.width(12.dp))
        Icon(icon, contentDescription = null, tint = Rocket.colors.fgMuted, modifier = Modifier.size(18.dp))
        Spacer(Modifier.width(8.dp))
        Text(label, style = MaterialTheme.typography.bodyLarge)
    }
}
