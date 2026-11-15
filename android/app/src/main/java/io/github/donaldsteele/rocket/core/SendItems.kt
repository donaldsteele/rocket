package io.github.donaldsteele.rocket.core

import java.io.IOException
import java.io.InputStream

/**
 * One file or folder to send. [path] is relative and "/"-separated, rooted at
 * the picked file or folder's own name; [ref] is the content URI (as a
 * string) a file is read from, null for a folder; [size] is -1 when the
 * provider does not know it (the Go core then stats the descriptor).
 */
data class SendItem(
    val path: String,
    val ref: String?,
    val size: Long,
    val isDir: Boolean,
)

/** What the items add up to, for a line like "3 files · 12.4 MB". */
data class SendSummary(val label: String, val files: Int, val bytes: Long, val unknownSize: Boolean)

fun summarize(items: List<SendItem>): SendSummary {
    val files = items.filter { !it.isDir }
    val tops = items.map { it.path.substringBefore('/') }.distinct()
    val label = when {
        tops.isEmpty() -> ""
        tops.size == 1 -> tops[0]
        else -> "${tops[0]} and ${tops.size - 1} more"
    }
    return SendSummary(
        label = label,
        files = files.size,
        bytes = files.sumOf { it.size.coerceAtLeast(0) },
        unknownSize = files.any { it.size < 0 },
    )
}

/**
 * The most items one send can hold: the protocol's manifest limit
 * (docs/protocol.md). Checked early so a huge folder fails before it is
 * walked to the end.
 */
const val MAX_ITEMS = 10_000

private const val MAX_NAME_BYTES = 255

/**
 * Turns a provider's display name into one path segment the protocol accepts:
 * no "/", "\", ":" or control characters, not "." or "..", at most 255 UTF-8
 * bytes (the extension kept when it is shortened). The receiver makes the
 * name safe for its own file system as well; this only keeps the manifest
 * valid.
 */
fun safeName(display: String?, fallback: String = "file"): String {
    val cleaned = buildString {
        for (ch in display.orEmpty()) {
            append(if (ch == '/' || ch == '\\' || ch == ':' || ch.isISOControl()) '_' else ch)
        }
    }.trim()
    val name = if (cleaned.isEmpty() || cleaned == "." || cleaned == "..") fallback else cleaned
    return truncateName(name)
}

private fun truncateName(name: String): String {
    if (name.toByteArray(Charsets.UTF_8).size <= MAX_NAME_BYTES) return name
    val dot = name.lastIndexOf('.')
    val ext = if (dot > 0 && name.length - dot <= 16) name.substring(dot) else ""
    var stem = name.substring(0, name.length - ext.length)
    while (stem.isNotEmpty() && (stem + ext).toByteArray(Charsets.UTF_8).size > MAX_NAME_BYTES) {
        stem = stem.dropLast(1)
        // Never leave half a surrogate pair at the end.
        if (stem.isNotEmpty() && stem.last().isHighSurrogate()) stem = stem.dropLast(1)
    }
    return stem + ext
}

/**
 * Makes names unique among siblings, compared case-insensitively as the
 * protocol does: a repeat of "a.jpg" becomes "a (1).jpg", then "a (2).jpg".
 */
fun uniqueNames(names: List<String>): List<String> {
    val taken = HashSet<String>()
    return names.map { name ->
        var candidate = name
        var n = 1
        while (!taken.add(candidate.lowercase())) {
            val dot = name.lastIndexOf('.')
            candidate = if (dot > 0) {
                "${name.substring(0, dot)} ($n)${name.substring(dot)}"
            } else {
                "$name ($n)"
            }
            n++
        }
        candidate
    }
}

/** Opens what a [SendItem.ref] names. */
interface ItemOpener {
    /**
     * Opens ref for reading and returns a detached file descriptor the
     * caller now owns, or -1 when the provider has no plain descriptor.
     */
    fun openFd(ref: String): Int

    /** Opens ref as a stream (the fallback when there is no descriptor). */
    fun openStream(ref: String): InputStream
}

/**
 * Serves ReadChunk calls (the Go core's fallback for providers without file
 * descriptors) from one open stream per item, read forward. A read at any
 * other offset reopens the stream and skips to it.
 */
class ChunkReader(private val opener: ItemOpener) {
    private class Open(val stream: InputStream, var pos: Long)

    private val open = HashMap<Int, Open>()

    @Synchronized
    fun read(index: Int, ref: String, offset: Long, max: Int): ByteArray {
        var cur = open[index]
        if (cur == null || cur.pos != offset) {
            cur?.stream?.closeQuietly()
            val s = opener.openStream(ref)
            skipFully(s, offset)
            cur = Open(s, offset)
            open[index] = cur
        }
        val buf = ByteArray(max.coerceIn(0, 1 shl 20))
        var n = 0
        while (n < buf.size) {
            val k = cur.stream.read(buf, n, buf.size - n)
            if (k < 0) break
            n += k
        }
        cur.pos += n
        if (n == 0) {
            cur.stream.closeQuietly()
            open.remove(index)
            return ByteArray(0)
        }
        return if (n == buf.size) buf else buf.copyOf(n)
    }

    /** Closes every stream still open. */
    @Synchronized
    fun close() {
        open.values.forEach { it.stream.closeQuietly() }
        open.clear()
    }

    private fun skipFully(s: InputStream, n: Long) {
        var left = n
        while (left > 0) {
            val k = s.skip(left)
            if (k > 0) {
                left -= k
                continue
            }
            // skip may return 0 before the end; read a byte to tell.
            if (s.read() < 0) throw IOException("the file is shorter than expected")
            left--
        }
    }
}

internal fun AutoCloseable.closeQuietly() {
    try {
        close()
    } catch (_: Exception) {
    }
}
