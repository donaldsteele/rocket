package io.github.donaldsteele.rocket.ui

import io.github.donaldsteele.rocket.core.IncomingOffer
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
