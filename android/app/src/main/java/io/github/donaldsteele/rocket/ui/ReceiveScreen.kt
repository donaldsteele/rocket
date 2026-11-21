package io.github.donaldsteele.rocket.ui

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
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
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.toggleable
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Switch
import androidx.compose.material3.SwitchDefaults
import androidx.compose.material3.Text
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.content.ContextCompat
import androidx.lifecycle.compose.LifecycleResumeEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import io.github.donaldsteele.rocket.core
import io.github.donaldsteele.rocket.core.IncomingOffer
import io.github.donaldsteele.rocket.core.PairedDevice
import io.github.donaldsteele.rocket.core.PairingWindow
import io.github.donaldsteele.rocket.core.ReceiverState
import io.github.donaldsteele.rocket.core.rocketcore.Rocketcore
import io.github.donaldsteele.rocket.service.OfferNotifications
import io.github.donaldsteele.rocket.ui.theme.Mono
import io.github.donaldsteele.rocket.ui.theme.Rocket
import kotlinx.coroutines.delay

/** Receiving on or off, how others see this phone, and the offers waiting. */
@Composable
fun ReceiveScreen(onMessage: (String) -> Unit, modifier: Modifier = Modifier) {
    val context = LocalContext.current
    val core = context.core
    val receiver = core.receiver
    val state by receiver.state.collectAsStateWithLifecycle()
    val port by receiver.port.collectAsStateWithLifecycle()
    val offers by receiver.offers.collectAsStateWithLifecycle()
    val problem by receiver.problem.collectAsStateWithLifecycle()
    val name by core.name.collectAsStateWithLifecycle()
    val pairing by receiver.pairing.collectAsStateWithLifecycle()
    val paired by receiver.paired.collectAsStateWithLifecycle()
    val c = Rocket.colors

    // A short message for each device paired while this screen shows (the
    // count is saved, so a rotation does not say it again).
    var announced by rememberSaveable { mutableIntStateOf(paired.size) }
    LaunchedEffect(paired) {
        if (paired.size < announced) announced = 0
        paired.drop(announced).forEach { onMessage("Paired with ${it.name}. It is now trusted. Progress is in Transfers.") }
        announced = paired.size
    }

    // Re-read the notification permission whenever the screen comes back.
    var resumed by remember { mutableIntStateOf(0) }
    LifecycleResumeEffect(Unit) {
        resumed++
        onPauseOrDispose { }
    }
    val notificationsOff = remember(resumed, state) { !OfferNotifications.allowed(context) }

    val askNotifications = rememberNotificationAsk()
    val storage = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        if (granted) {
            core.startReceiving(context)
            askNotifications()
        } else {
            onMessage("rocket needs storage access to save received files on this Android version.")
        }
    }

    fun turnOn() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q &&
            ContextCompat.checkSelfPermission(context, Manifest.permission.WRITE_EXTERNAL_STORAGE) != PackageManager.PERMISSION_GRANTED
        ) {
            storage.launch(Manifest.permission.WRITE_EXTERNAL_STORAGE)
            return
        }
        try {
            core.startReceiving(context)
            askNotifications()
        } catch (e: Exception) {
            onMessage("Receiving could not start: ${e.message ?: e}")
        }
    }

    val on = state == ReceiverState.On || state == ReceiverState.Starting
    val busy = state == ReceiverState.Starting || state == ReceiverState.Stopping

    LazyColumn(
        modifier = modifier.fillMaxSize(),
        contentPadding = PaddingValues(16.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        item {
            Panel {
                Column {
                    Row(
                        verticalAlignment = Alignment.CenterVertically,
                        modifier = Modifier
                            .fillMaxWidth()
                            .heightIn(min = 48.dp)
                            .toggleable(
                                value = on,
                                enabled = !busy,
                                role = Role.Switch,
                                onValueChange = { if (it) turnOn() else core.stopReceiving() },
                            ),
                    ) {
                        Icon(Lucide.Download, contentDescription = null, tint = c.link, modifier = Modifier.size(22.dp))
                        Spacer(Modifier.width(10.dp))
                        Column(Modifier.weight(1f)) {
                            Text("Receive files", style = MaterialTheme.typography.titleMedium)
                            Text(
                                when (state) {
                                    ReceiverState.On -> "On: devices on this network can offer you files"
                                    ReceiverState.Starting -> "Turning on…"
                                    ReceiverState.Stopping -> "Turning off…"
                                    ReceiverState.Off -> "Off"
                                },
                                style = MaterialTheme.typography.bodySmall,
                                color = c.fgMuted,
                            )
                        }
                        Spacer(Modifier.width(8.dp))
                        Switch(
                            checked = on,
                            onCheckedChange = null,
                            enabled = !busy,
                            colors = SwitchDefaults.colors(checkedTrackColor = c.accent, checkedThumbColor = c.accentFg),
                        )
                    }
                    Spacer(Modifier.height(8.dp))
                    if (state == ReceiverState.On) {
                        Text(
                            "Visible as $name · ${core.fingerprint}",
                            fontFamily = Mono,
                            fontSize = 12.sp,
                            color = c.fg,
                        )
                        Text("Port $port · files are saved in ${receiver.where}", fontFamily = Mono, fontSize = 12.sp, color = c.fgMuted)
                        Spacer(Modifier.height(4.dp))
                        Text(
                            "Every offer asks you first, unless you chose \"Always accept\" for its sender. " +
                                "Receiving stays on in the background until you turn it off; a notification shows it.",
                            style = MaterialTheme.typography.bodySmall,
                            color = c.fgMuted,
                        )
                        Spacer(Modifier.height(12.dp))
                        PairingSection(
                            window = pairing,
                            paired = paired,
                            onStart = { receiver.startPairing()?.let { onMessage("Pairing could not start: $it") } },
                            onStop = { receiver.stopPairing() },
                        )
                    } else {
                        Text(
                            "Turn on to let other rocket devices on this Wi-Fi network find this phone and offer it files. " +
                                "Nothing is saved until you accept.",
                            style = MaterialTheme.typography.bodySmall,
                            color = c.fgMuted,
                        )
                    }
                }
            }
        }
        problem?.let { p -> item { Notice(p, Tone.Warn) } }
        if (on && notificationsOff) {
            item { Notice("Notifications are off, so offers only show while rocket is open.", Tone.Info) }
        }
        if (offers.isNotEmpty()) {
            item {
                Text(
                    "Waiting for you",
                    style = MaterialTheme.typography.titleMedium,
                    modifier = Modifier.padding(top = 8.dp).semantics { heading() },
                )
            }
        }
        items(offers, key = { it.id }) { o ->
            Panel {
                OfferBody(o, onAnswer = { accept, trust -> answerOffer(core.receiver::answer, o, accept, trust, onMessage) })
            }
        }
        if (state == ReceiverState.On && offers.isEmpty()) {
            item { EmptyBox("No offers right now. Others see this phone as $name.") }
        }
    }
}

/**
 * "Pair new devices": a button, or while the window is open a countdown,
 * what it means, Stop, and the devices paired so far.
 */
@Composable
fun PairingSection(window: PairingWindow, paired: List<PairedDevice>, onStart: () -> Unit, onStop: () -> Unit) {
    val c = Rocket.colors
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    LaunchedEffect(window) {
        while (window.open) {
            now = System.currentTimeMillis()
            val left = window.endsAt - now
            if (left <= 0) break
            // Wake when the seconds shown change.
            delay(left % 1000 + 1)
        }
    }
    val left = if (window.open) secondsLeft(window.endsAt, now) else 0
    if (left <= 0) {
        SecondaryAction("Pair new devices", Lucide.ShieldCheck, onClick = onStart)
        return
    }
    Column {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Icon(Lucide.Clock, contentDescription = null, tint = c.warn, modifier = Modifier.size(18.dp))
            Spacer(Modifier.width(8.dp))
            Text(
                "Pairing: ${countdownText(left)} left",
                style = MaterialTheme.typography.titleSmall,
                color = c.fg,
                modifier = Modifier.weight(1f),
            )
            Spacer(Modifier.width(8.dp))
            SecondaryAction(
                "Stop",
                Lucide.X,
                onClick = onStop,
                modifier = Modifier.semantics { this.contentDescription = "Stop pairing" },
            )
        }
        Spacer(Modifier.height(4.dp))
        Text(
            "For the next ${durationWords(Rocketcore.DefaultPairingSeconds.toInt())}, rocket devices that send to this phone " +
                "are trusted without asking. Only use it when you are pairing your own devices.",
            style = MaterialTheme.typography.bodySmall,
            color = c.fgMuted,
        )
        Spacer(Modifier.height(8.dp))
        if (paired.isEmpty()) {
            Text("Nothing paired yet. Send a file from your other device to this phone.", fontFamily = Mono, fontSize = 12.sp, color = c.fgMuted)
        }
        paired.forEach { p ->
            Row(
                verticalAlignment = Alignment.CenterVertically,
                modifier = Modifier.padding(vertical = 4.dp).semantics(mergeDescendants = true) {},
            ) {
                Icon(Lucide.ShieldCheck, contentDescription = null, tint = c.ok, modifier = Modifier.size(18.dp))
                Spacer(Modifier.width(8.dp))
                Column {
                    Text("Paired: ${p.name}", style = MaterialTheme.typography.bodyMedium, color = c.fg)
                    Text(p.fingerprint, fontFamily = Mono, fontSize = 12.sp, color = c.fgMuted)
                }
            }
        }
    }
}

/** Answers an offer and says what happened. */
fun answerOffer(
    answer: (String, Boolean, Boolean) -> Boolean,
    o: IncomingOffer,
    accept: Boolean,
    trust: Boolean,
    onMessage: (String) -> Unit,
) {
    val ok = answer(o.id, accept, trust)
    onMessage(
        when {
            !ok -> "That offer is no longer waiting."
            !accept -> "Declined ${offerWhat(o)} from ${o.senderName}."
            trust -> "Accepted. ${o.senderName} is now trusted. Progress is in Transfers."
            else -> "Accepted ${offerWhat(o)} from ${o.senderName}. Progress is in Transfers."
        },
    )
}

/** An offer as text plus Accept, Always accept and Decline, as on desktop. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun OfferBody(o: IncomingOffer, onAnswer: (accept: Boolean, trust: Boolean) -> Unit) {
    val c = Rocket.colors
    val what = offerWhat(o)
    Column {
        Row(verticalAlignment = Alignment.Top) {
            Icon(Lucide.Download, contentDescription = null, tint = c.warn, modifier = Modifier.size(22.dp))
            Spacer(Modifier.width(10.dp))
            Column(Modifier.weight(1f).semantics(mergeDescendants = true) {}) {
                Text(
                    buildAnnotatedString {
                        withStyle(SpanStyle(fontWeight = FontWeight.Bold)) { append(o.senderName) }
                        append(" wants to send you ")
                        withStyle(SpanStyle(fontWeight = FontWeight.Bold)) { append(what) }
                        if (o.size > 0) {
                            withStyle(SpanStyle(color = c.fgMuted)) { append(" (${formatSize(o.size)})") }
                        }
                    },
                    style = MaterialTheme.typography.bodyLarge,
                )
                if (o.files > 1 && o.names.isNotEmpty()) {
                    Spacer(Modifier.height(4.dp))
                    Text(namesLine(o), fontFamily = Mono, fontSize = 12.sp, color = c.fgMuted)
                }
                if (o.senderFp.isNotEmpty()) {
                    Spacer(Modifier.height(4.dp))
                    Text("Fingerprint ${o.senderFp}", fontFamily = Mono, fontSize = 13.sp, color = c.fg)
                }
                if (o.addr.isNotEmpty()) {
                    Text("from ${o.addr}", fontFamily = Mono, fontSize = 12.sp, color = c.fgMuted)
                }
            }
        }
        // Every offer the phone gets is from a rocket app (there is no drop
        // page on Android), so its files arrive encrypted.
        Spacer(Modifier.height(8.dp))
        EncryptedChip(Modifier.padding(start = 32.dp))
        Spacer(Modifier.height(10.dp))
        FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            PrimaryAction(
                "Accept",
                Lucide.Check,
                onClick = { onAnswer(true, false) },
                modifier = Modifier.semantics { this.contentDescription = "Accept $what from ${o.senderName}" },
            )
            if (o.senderFp.isNotEmpty()) {
                SecondaryAction("Always accept from this device", Lucide.ShieldCheck, onClick = { onAnswer(true, true) })
            }
            SecondaryAction(
                "Decline",
                Lucide.X,
                onClick = { onAnswer(false, false) },
                danger = true,
                modifier = Modifier.semantics { this.contentDescription = "Decline $what from ${o.senderName}" },
            )
        }
    }
}

/** The incoming-offer sheet shown over the other tabs. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun OfferSheet(o: IncomingOffer, onAnswer: (accept: Boolean, trust: Boolean) -> Unit, onDismiss: () -> Unit) {
    val c = Rocket.colors
    val sheet = rememberModalBottomSheetState(skipPartiallyExpanded = true)
    ModalBottomSheet(onDismissRequest = onDismiss, sheetState = sheet, containerColor = c.bgElev) {
        // Scrolls so the answers stay reachable with large fonts (200%).
        Column(
            Modifier
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 16.dp)
                .padding(bottom = 16.dp)
                .navigationBarsPadding(),
        ) {
            Text(
                "Incoming offer",
                style = MaterialTheme.typography.titleMedium,
                modifier = Modifier.padding(bottom = 12.dp).semantics { heading() },
            )
            OfferBody(o, onAnswer)
            Spacer(Modifier.height(8.dp))
            Text(
                "Names and the device name come from the other device. Compare the fingerprint before you choose \"Always accept\".",
                style = MaterialTheme.typography.bodySmall,
                color = c.fgMuted,
            )
        }
    }
}
