package io.github.donaldsteele.rocket.core

import org.json.JSONArray
import org.json.JSONObject
import java.time.OffsetDateTime

// The Go core hands lists and events over as JSON in the shapes desktop
// rocket's HTTP API uses (see mobile/rocketcore/doc.go). These parse them.

/** A discovered rocket host, as in desktop rocket's GET /api/peers. */
data class Peer(
    val name: String,
    val fp: String,
    val addr: String,
    val os: String,
    val version: String,
    val via: List<String>,
)

fun parsePeers(json: String): List<Peer> {
    val arr = runCatching { JSONArray(json) }.getOrNull() ?: return emptyList()
    return (0 until arr.length()).mapNotNull { i ->
        val o = arr.optJSONObject(i) ?: return@mapNotNull null
        val fp = o.optString("fp")
        if (fp.isEmpty()) return@mapNotNull null
        Peer(
            name = o.optString("name").ifEmpty { fp },
            fp = fp,
            addr = o.optString("addr"),
            os = o.optString("os"),
            version = o.optString("version"),
            via = o.optJSONArray("via").strings(),
        )
    }
}

/** A finished transfer, as in desktop rocket's GET /api/history. */
data class HistoryEntry(
    val id: String,
    /** "in" or "out". */
    val dir: String,
    val name: String,
    val peerName: String,
    val peerFp: String,
    val files: Int,
    val bytes: Long,
    /** Epoch milliseconds, or 0 when unknown. */
    val started: Long,
    val ended: Long,
    /** ok, declined, cancelled, error or timeout. */
    val result: String,
    val error: String,
)

fun parseHistory(json: String): List<HistoryEntry> {
    val arr = runCatching { JSONArray(json) }.getOrNull() ?: return emptyList()
    return (0 until arr.length()).mapNotNull { i ->
        val o = arr.optJSONObject(i) ?: return@mapNotNull null
        HistoryEntry(
            id = o.optString("id"),
            dir = o.optString("dir"),
            name = o.optString("name"),
            peerName = o.optString("peer_name"),
            peerFp = o.optString("peer_fp"),
            files = o.optInt("files"),
            bytes = o.optLong("bytes"),
            started = parseTime(o.optString("started")),
            ended = parseTime(o.optString("ended")),
            result = o.optString("result"),
            error = o.optString("error"),
        )
    }
}

/** An event of one transfer (receiver.Event in the Go core). */
sealed interface TransferEvent {
    val transferId: String
    val name: String

    data class Progress(
        override val transferId: String,
        override val name: String,
        val sent: Long,
        val total: Long,
        val file: String,
        /** 1-based. */
        val fileIndex: Int,
        val files: Int,
    ) : TransferEvent

    /** A send finished: the receiver has every byte. */
    data class Sent(
        override val transferId: String,
        override val name: String,
        val files: Int,
        val bytes: Long,
    ) : TransferEvent

    data class Failed(
        override val transferId: String,
        override val name: String,
        val error: String,
    ) : TransferEvent

    /** Stopped on this phone ("local") or by the other side ("peer"). */
    data class Cancelled(
        override val transferId: String,
        override val name: String,
        val by: String,
        val error: String,
    ) : TransferEvent
}

/** Parses one event; null for event types a send does not use or bad JSON. */
fun parseTransferEvent(json: String): TransferEvent? {
    val o = runCatching { JSONObject(json) }.getOrNull() ?: return null
    val d = o.optJSONObject("data") ?: return null
    val id = d.optString("transfer_id")
    if (id.isEmpty()) return null
    val name = d.optString("name")
    return when (o.optString("type")) {
        "progress" -> TransferEvent.Progress(
            transferId = id,
            name = name,
            sent = d.optLong("sent"),
            total = d.optLong("total"),
            file = d.optString("file"),
            fileIndex = d.optInt("file_index"),
            files = d.optInt("files"),
        )
        "sent", "done" -> TransferEvent.Sent(id, name, d.optInt("files"), d.optLong("bytes"))
        "error" -> TransferEvent.Failed(id, name, d.optString("error").ifEmpty { "failed" })
        "cancelled" -> TransferEvent.Cancelled(id, name, d.optString("by"), d.optString("error"))
        else -> null
    }
}

/** Go's time.Time JSON (RFC 3339 with nanoseconds) to epoch ms; 0 if absent. */
internal fun parseTime(s: String): Long {
    if (s.isEmpty() || s.startsWith("0001-01-01")) return 0
    return runCatching { OffsetDateTime.parse(s).toInstant().toEpochMilli() }.getOrDefault(0)
}

private fun JSONArray?.strings(): List<String> {
    if (this == null) return emptyList()
    return (0 until length()).map { optString(it) }.filter { it.isNotEmpty() }
}
