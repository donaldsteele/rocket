package io.github.donaldsteele.rocket.ui

import io.github.donaldsteele.rocket.core.IncomingOffer
import io.github.donaldsteele.rocket.core.InterruptedSend
import io.github.donaldsteele.rocket.core.KeptReceive
import io.github.donaldsteele.rocket.core.Peer
import java.text.DateFormat
import java.util.Date
import java.util.Locale

/** "0 B", "512 B", "1.5 KB", "12.3 MB", as desktop rocket shows sizes (1024 steps). */
fun formatSize(n: Long): String {
    if (n <= 0) return "0 B"
    val units = arrayOf("B", "KB", "MB", "GB", "TB")
    var v = n.toDouble()
    var i = 0
    while (v >= 1024 && i < units.size - 1) {
        v /= 1024
        i++
    }
    return if (i == 0) "$n B" else String.format(Locale.ROOT, "%.1f %s", v, units[i])
}

/** "photos (12 files)" or just "report.pdf", as desktop rocket says it. */
fun countWhat(name: String, files: Int): String = if (files > 1) "$name ($files files)" else name

/** What an offer brings, as desktop rocket says it: "report.pdf" or "12 files". */
fun offerWhat(o: IncomingOffer): String = if (o.files > 1) "${o.files} files" else o.name.ifEmpty { "a file" }

/** The first few names of an offer: "a.jpg, b.jpg, …". */
fun namesLine(o: IncomingOffer): String = o.names.joinToString(", ") + if (o.names.size >= 5) ", …" else ""

/** Whole percent of a in b, 0..100. */
fun percent(a: Long, b: Long): Int = if (b > 0) (a.coerceAtLeast(0) * 100 / b).toInt().coerceIn(0, 100) else 0

/** A history time in the phone's locale; "" when unknown. */
fun formatWhen(ms: Long): String =
    if (ms <= 0) "" else DateFormat.getDateTimeInstance(DateFormat.MEDIUM, DateFormat.SHORT).format(Date(ms))

/** The second line of a peer row: fingerprint, OS and how it was found. */
fun peerMeta(fp: String, os: String, via: List<String>): String {
    val parts = mutableListOf(fp)
    if (os.isNotEmpty()) parts += os
    if (via.isNotEmpty()) parts += "via ${via.joinToString(" + ")}"
    return parts.joinToString(" · ")
}

/**
 * A host:port typed by the person, cleaned up: an IPv6 address needs
 * brackets, and a missing port means rocket's default. Null if it cannot be
 * an address.
 */
fun parseAddress(input: String, defaultPort: Int = 29005): String? {
    val s = input.trim()
    if (s.isEmpty() || s.any { it.isWhitespace() || it == '/' }) return null
    val host: String
    val port: Int
    when {
        s.startsWith("[") -> {
            val end = s.indexOf(']')
            if (end < 2) return null
            host = s.substring(0, end + 1)
            val rest = s.substring(end + 1)
            port = when {
                rest.isEmpty() -> defaultPort
                rest.startsWith(":") -> rest.substring(1).toIntOrNull() ?: return null
                else -> return null
            }
        }
        s.count { it == ':' } > 1 -> {
            // A bare IPv6 address.
            host = "[$s]"
            port = defaultPort
        }
        s.contains(':') -> {
            host = s.substringBefore(':')
            port = s.substringAfter(':').toIntOrNull() ?: return null
        }
        else -> {
            host = s
            port = defaultPort
        }
    }
    if (host.isEmpty() || port !in 1..65535) return null
    return "$host:$port"
}

/** How far the bottom bar's labels grow with the system font size. */
const val NAV_LABEL_MAX_SCALE = 1.15f

/**
 * The factor for a bottom-bar label's font size at system font scale
 * [fontScale]: five labels share the screen width, so past
 * [NAV_LABEL_MAX_SCALE] they stop growing instead of breaking mid-word
 * ("Trans-fers"). Everything else still follows the system size.
 */
fun navLabelFactor(fontScale: Float): Float =
    if (fontScale > NAV_LABEL_MAX_SCALE) NAV_LABEL_MAX_SCALE / fontScale else 1f

/** Why a peer cannot be sent to, when it speaks protocol [Peer.proto] and this phone [own]. */
fun peerUpdateText(p: Peer, own: Int): String {
    if (p.proto > own) return "Runs a newer rocket (protocol ${p.proto}). Update rocket on this phone to send to it."
    val ver = if (p.version.isNotEmpty()) "rocket ${p.version}" else "an older rocket"
    return "Runs $ver (protocol ${p.proto}). Update it to rocket 1.1 or later to send to it."
}

/** "Reconnecting 2/5", the word for a send waiting to retry. */
fun reconnectingWord(attempt: Int, attempts: Int): String =
    if (attempts > 0) "Reconnecting $attempt/$attempts" else "Reconnecting"

/** "Reconnecting, attempt 2 of 5", as a screen reader says it. */
fun reconnectingSpoken(attempt: Int, attempts: Int): String =
    if (attempts > 0) "Reconnecting, attempt $attempt of $attempts" else "Reconnecting"

/**
 * "Connection lost: the other device does not answer. rocket tries again by
 * itself and continues where it stopped." The Go core's error (a URL, a
 * socket address) is boiled down to what it means.
 */
fun reconnectingLine(error: String): String {
    val why = networkWhy(error)
    return "Connection lost" + (if (why.isNotEmpty()) ": $why" else "") +
        ". rocket tries again by itself and continues where it stopped."
}

/** What a network error from the core means, in a few words; "" when it is not clear. */
fun networkWhy(error: String): String {
    val e = error.lowercase(Locale.ROOT)
    return when {
        "refused" in e -> "the other device does not answer"
        "unreachable" in e || "no route" in e -> "the network cannot reach the other device"
        "timeout" in e || "timed out" in e || "no byte" in e || "stalled" in e -> "nothing moved for a while"
        "reset" in e || "forcibly closed" in e || "broken pipe" in e || "eof" in e ||
            "closed" in e || "final frame" in e -> "the connection was cut"
        else -> ""
    }
}

/** A time like 15:04 today, or "Mon 15:04" on another day; "" when unknown. */
fun untilText(ms: Long, now: Long = System.currentTimeMillis()): String {
    if (ms <= 0) return ""
    val d = Date(ms)
    val t = DateFormat.getTimeInstance(DateFormat.SHORT).format(d)
    val sameDay = java.text.SimpleDateFormat("yyyyMMdd", Locale.ROOT).let { it.format(d) == it.format(Date(now)) }
    return if (sameDay) t else "${java.text.SimpleDateFormat("EEE", Locale.getDefault()).format(d)} $t"
}

/** "3 files · 1.2 GB · kept until 15:04" for an interrupted send. */
fun interruptedMeta(s: InterruptedSend, now: Long = System.currentTimeMillis()): String =
    listOfNotNull(
        if (s.files > 1) "${s.files} files" else null,
        formatSize(s.bytes),
        untilText(s.expires, now).takeIf { it.isNotEmpty() }?.let { "kept until $it" },
    ).joinToString(" · ")

/** "42% · 410.0 MB of 980.0 MB · kept until 15:04" for a receive kept here. */
fun keptMeta(k: KeptReceive, now: Long = System.currentTimeMillis()): String =
    listOfNotNull(
        "${percent(k.done, k.total)}%",
        "${formatSize(k.done)} of ${formatSize(k.total)}",
        untilText(k.expires, now).takeIf { it.isNotEmpty() }?.let { "kept until $it" },
    ).joinToString(" · ")

/** Whole seconds left until [endsAt] (epoch ms), rounded up as the core does; 0 once past. */
fun secondsLeft(endsAt: Long, now: Long = System.currentTimeMillis()): Int =
    if (endsAt <= now) 0 else ((endsAt - now + 999) / 1000).coerceAtMost(Int.MAX_VALUE.toLong()).toInt()

/** A countdown as m:ss: "4:32", "0:07", "60:00". */
fun countdownText(seconds: Int): String {
    val s = seconds.coerceAtLeast(0)
    return String.format(Locale.ROOT, "%d:%02d", s / 60, s % 60)
}

/** "5 minutes", "1 minute" or "45 seconds", for the pairing explanation. */
fun durationWords(seconds: Int): String = when {
    seconds >= 60 && seconds % 60 == 0 -> (seconds / 60).let { if (it == 1) "1 minute" else "$it minutes" }
    seconds == 1 -> "1 second"
    else -> "$seconds seconds"
}
