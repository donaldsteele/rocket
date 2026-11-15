package io.github.donaldsteele.rocket.ui

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.PathEffect
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.semantics.clearAndSetSemantics
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import io.github.donaldsteele.rocket.R
import io.github.donaldsteele.rocket.core.Peer
import io.github.donaldsteele.rocket.ui.theme.Mono
import io.github.donaldsteele.rocket.ui.theme.Rocket

/** The app bar: the rocket mark and name, then the view's title. */
@Composable
fun TopBar(title: String, modifier: Modifier = Modifier) {
    val c = Rocket.colors
    Surface(color = c.bgElev, modifier = modifier.fillMaxWidth()) {
        Column(Modifier.statusBarsPadding()) {
            Row(
                verticalAlignment = Alignment.CenterVertically,
                modifier = Modifier.padding(horizontal = 16.dp, vertical = 10.dp).heightIn(min = 40.dp),
            ) {
                Image(
                    painter = painterResource(R.mipmap.ic_launcher_foreground),
                    contentDescription = null,
                    modifier = Modifier.size(36.dp).clip(RoundedCornerShape(8.dp)).background(c.bg),
                )
                Spacer(Modifier.width(10.dp))
                Text(
                    buildAnnotatedString {
                        withStyle(SpanStyle(fontWeight = FontWeight.Bold)) { append("rocket") }
                        withStyle(SpanStyle(color = c.link)) { append("▍") }
                    },
                    fontSize = 18.sp,
                    modifier = Modifier.clearAndSetSemantics { contentDescription = "rocket" },
                )
                Spacer(Modifier.width(12.dp))
                Text(
                    title,
                    style = MaterialTheme.typography.titleMedium,
                    color = c.fgMuted,
                    modifier = Modifier.semantics { heading() },
                )
            }
            Box(Modifier.fillMaxWidth().height(1.dp).background(c.line))
        }
    }
}

/** How a status reads: each tone has its own colour, never alone. */
enum class Tone { Ok, Warn, Bad, Info, Muted }

@Composable
fun Tone.color(): Color = when (this) {
    Tone.Ok -> Rocket.colors.ok
    Tone.Warn -> Rocket.colors.warn
    Tone.Bad -> Rocket.colors.bad
    Tone.Info -> Rocket.colors.info
    Tone.Muted -> Rocket.colors.fgMuted
}

/** A status as a word plus a shape (and a colour), like desktop's chips. */
@Composable
fun StatusChip(icon: ImageVector, word: String, tone: Tone, modifier: Modifier = Modifier) {
    val c = Rocket.colors
    Row(
        verticalAlignment = Alignment.CenterVertically,
        modifier = modifier
            .border(1.dp, c.lineStrong, RoundedCornerShape(10.dp))
            .padding(horizontal = 8.dp, vertical = 2.dp)
            .semantics(mergeDescendants = true) {},
    ) {
        Icon(icon, contentDescription = null, tint = tone.color(), modifier = Modifier.size(14.dp))
        Spacer(Modifier.width(4.dp))
        Text(word, style = MaterialTheme.typography.labelMedium, color = c.fg)
    }
}

/** The dashed "nothing here yet" box. */
@Composable
fun EmptyBox(text: String, modifier: Modifier = Modifier) {
    val c = Rocket.colors
    Box(
        modifier
            .fillMaxWidth()
            .background(c.bgElev, RoundedCornerShape(4.dp))
            .drawBehind {
                drawRoundRect(
                    color = c.lineStrong,
                    cornerRadius = CornerRadius(4.dp.toPx()),
                    style = Stroke(width = 1.dp.toPx(), pathEffect = PathEffect.dashPathEffect(floatArrayOf(6.dp.toPx(), 4.dp.toPx()))),
                )
            }
            .padding(16.dp),
    ) {
        Text(text, fontFamily = Mono, fontSize = 13.sp, color = c.fgMuted)
    }
}

/** A notice with a coloured edge, an icon and text. */
@Composable
fun Notice(text: String, tone: Tone, modifier: Modifier = Modifier) {
    val c = Rocket.colors
    val bg = when (tone) {
        Tone.Warn -> c.warnBg
        Tone.Bad -> c.badBg
        Tone.Ok -> c.okBg
        Tone.Info, Tone.Muted -> c.infoBg
    }
    val icon = when (tone) {
        Tone.Ok -> Lucide.CircleCheck
        Tone.Bad, Tone.Warn -> Lucide.TriangleAlert
        else -> Lucide.Info
    }
    Row(
        verticalAlignment = Alignment.CenterVertically,
        modifier = modifier
            .fillMaxWidth()
            .background(bg, RoundedCornerShape(4.dp))
            .drawBehind { drawRect(tone.colorFor(c), size = size.copy(width = 4.dp.toPx())) }
            .padding(start = 14.dp, end = 12.dp, top = 8.dp, bottom = 8.dp),
    ) {
        Icon(icon, contentDescription = null, tint = tone.colorFor(c), modifier = Modifier.size(18.dp))
        Spacer(Modifier.width(8.dp))
        Text(text, style = MaterialTheme.typography.bodyMedium, color = c.fg)
    }
}

private fun Tone.colorFor(c: io.github.donaldsteele.rocket.ui.theme.RocketColors): Color = when (this) {
    Tone.Ok -> c.ok
    Tone.Warn -> c.warn
    Tone.Bad -> c.bad
    Tone.Info -> c.info
    Tone.Muted -> c.fgMuted
}

/** A bordered panel, like desktop's cards. */
@Composable
fun Panel(modifier: Modifier = Modifier, content: @Composable () -> Unit) {
    val c = Rocket.colors
    Surface(
        color = c.bgElev,
        shape = RoundedCornerShape(6.dp),
        border = BorderStroke(1.dp, c.line),
        modifier = modifier.fillMaxWidth(),
    ) {
        Box(Modifier.padding(16.dp)) { content() }
    }
}

/** The shape that stands for a peer's kind of device. */
fun osIcon(os: String): ImageVector = when (os) {
    "android", "ios" -> Lucide.Smartphone
    "darwin" -> Lucide.Laptop
    else -> Lucide.Monitor
}

/** One discovered host, with the actions offered for it. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun PeerRow(peer: Peer, modifier: Modifier = Modifier, actions: @Composable () -> Unit) {
    val c = Rocket.colors
    Surface(
        color = c.bg,
        shape = RoundedCornerShape(4.dp),
        border = BorderStroke(1.dp, c.line),
        modifier = modifier.fillMaxWidth(),
    ) {
        Column(Modifier.padding(12.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(osIcon(peer.os), contentDescription = null, tint = c.link, modifier = Modifier.size(24.dp))
                Spacer(Modifier.width(12.dp))
                Column(Modifier.weight(1f).semantics(mergeDescendants = true) {}) {
                    Text(peer.name, style = MaterialTheme.typography.titleMedium, color = c.fg)
                    Text(
                        peerMeta(peer.fp, peer.os, peer.via),
                        fontFamily = Mono,
                        fontSize = 12.sp,
                        color = c.fgMuted,
                    )
                }
            }
            Spacer(Modifier.height(10.dp))
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                actions()
            }
        }
    }
}

@Composable
fun PrimaryAction(text: String, icon: ImageVector?, onClick: () -> Unit, modifier: Modifier = Modifier, enabled: Boolean = true) {
    Button(
        onClick = onClick,
        enabled = enabled,
        shape = RoundedCornerShape(4.dp),
        modifier = modifier.heightIn(min = 48.dp),
    ) {
        if (icon != null) {
            Icon(icon, contentDescription = null, modifier = Modifier.size(18.dp))
            Spacer(Modifier.width(6.dp))
        }
        Text(text)
    }
}

@Composable
fun SecondaryAction(
    text: String,
    icon: ImageVector?,
    onClick: () -> Unit,
    modifier: Modifier = Modifier,
    enabled: Boolean = true,
    danger: Boolean = false,
) {
    val c = Rocket.colors
    OutlinedButton(
        onClick = onClick,
        enabled = enabled,
        shape = RoundedCornerShape(4.dp),
        border = BorderStroke(1.dp, if (danger) c.bad else c.lineStrong),
        colors = ButtonDefaults.outlinedButtonColors(contentColor = if (danger) c.bad else c.fg),
        modifier = modifier.heightIn(min = 48.dp),
    ) {
        if (icon != null) {
            Icon(icon, contentDescription = null, modifier = Modifier.size(18.dp))
            Spacer(Modifier.width(6.dp))
        }
        Text(text)
    }
}

/**
 * Asks for a host:port to send to directly, without discovery. onSend gets
 * the address and whether a folder (rather than files) is to be picked; the
 * folder button shows only when allowFolder is set.
 */
@Composable
fun AddressDialog(
    onDismiss: () -> Unit,
    confirm: String,
    allowFolder: Boolean = false,
    onSend: (addr: String, folder: Boolean) -> Unit,
) {
    var text by rememberSaveable { mutableStateOf("") }
    val addr = parseAddress(text)
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Send to an address") },
        text = {
            Column {
                Text(
                    "For a host that discovery does not find. The other side is not checked against a " +
                        "fingerprint, so make sure the address is right.",
                    style = MaterialTheme.typography.bodyMedium,
                )
                Spacer(Modifier.height(12.dp))
                OutlinedTextField(
                    value = text,
                    onValueChange = { text = it },
                    label = { Text("Host and port") },
                    placeholder = { Text("192.168.1.20:29005") },
                    singleLine = true,
                    isError = text.isNotBlank() && addr == null,
                    supportingText = { Text(if (text.isNotBlank() && addr == null) "Not a host:port address" else "Port 29005 if left out") },
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri, imeAction = ImeAction.Go),
                    keyboardActions = KeyboardActions(onGo = { addr?.let { onSend(it, false) } }),
                    textStyle = MaterialTheme.typography.bodyLarge.copy(fontFamily = Mono),
                    modifier = Modifier.fillMaxWidth(),
                )
            }
        },
        confirmButton = {
            Row {
                if (allowFolder) {
                    TextButton(onClick = { addr?.let { onSend(it, true) } }, enabled = addr != null, modifier = Modifier.heightIn(min = 48.dp)) {
                        Text("Folder")
                    }
                }
                TextButton(onClick = { addr?.let { onSend(it, false) } }, enabled = addr != null, modifier = Modifier.heightIn(min = 48.dp)) {
                    Text(confirm)
                }
            }
        },
        dismissButton = {
            TextButton(onClick = onDismiss, modifier = Modifier.heightIn(min = 48.dp)) { Text("Cancel") }
        },
    )
}
