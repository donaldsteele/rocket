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
    /** The wire protocol the peer announces; 1 (rocket 1.0) when it announces none. */
    val proto: Int = 1,
)

/** Whether this phone can send to the peer: it speaks the same protocol. */
fun Peer.canSend(own: Int): Boolean = proto == own

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
            proto = o.optInt("proto", 1).coerceAtLeast(1),
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
    /** ok, declined, cancelled, error, timeout or interrupted. */
    val result: String,
    val error: String,
    /** Whether the file bytes travelled encrypted (every transfer between rocket 1.1 apps). */
    val encrypted: Boolean = false,
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
            encrypted = o.optBoolean("encrypted"),
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

    /**
     * A transfer finished: for a send ("sent") the receiver has every byte;
     * for a receive ("done") everything is saved, at [path].
     */
    data class Sent(
        override val transferId: String,
        override val name: String,
        val files: Int,
        val bytes: Long,
        /** Where a receive was saved (a content URI or a folder name); "" for a send. */
        val path: String = "",
    ) : TransferEvent

    /**
     * A transfer failed. [interrupted]: a send that lost its connection and
     * gave up after its retries, kept to be resumed; or a receive whose
     * connection was lost, kept to be continued.
     */
    data class Failed(
        override val transferId: String,
        override val name: String,
        val error: String,
        val interrupted: Boolean = false,
    ) : TransferEvent

    /** A send lost its connection and is offered again after [delayMs]: retry [attempt] of [attempts]. */
    data class Retrying(
        override val transferId: String,
        override val name: String,
        val attempt: Int,
        val attempts: Int,
        val delayMs: Long,
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
        "sent", "done" -> TransferEvent.Sent(id, name, d.optInt("files"), d.optLong("bytes"), d.optString("path"))
        "error" -> TransferEvent.Failed(id, name, d.optString("error").ifEmpty { "failed" }, d.optString("result") == "interrupted")
        "retrying" -> TransferEvent.Retrying(
            transferId = id,
            name = name,
            attempt = d.optInt("attempt"),
            attempts = d.optInt("attempts"),
            delayMs = d.optLong("delay_ms"),
            error = d.optString("error"),
        )
        "cancelled" -> TransferEvent.Cancelled(id, name, d.optString("by"), d.optString("error"))
        else -> null
    }
}

/**
 * An offer waiting for this phone's decision (receiver.IncomingOffer in the
 * Go core, as in desktop rocket's GET /api/incoming). Names are as they will
 * be saved; all of it comes from the other device.
 */
data class IncomingOffer(
    /** Answer with this ID; the same as [transferId]. */
    val id: String,
    val transferId: String,
    /** "file" or "multi". */
    val kind: String,
    /** The first entry's path. */
    val name: String,
    val size: Long,
    /** Files, not counting folders. */
    val files: Int,
    /** The first few entry paths. */
    val names: List<String>,
    val senderName: String,
    val senderFp: String,
    /** The sender's IP address. */
    val addr: String,
)

fun parseOffer(json: String): IncomingOffer? {
    val o = runCatching { JSONObject(json) }.getOrNull() ?: return null
    return offerFrom(o)
}

/** Parses IncomingJSON (a list of offers). */
fun parseOffers(json: String): List<IncomingOffer> {
    val arr = runCatching { JSONArray(json) }.getOrNull() ?: return emptyList()
    return (0 until arr.length()).mapNotNull { i -> arr.optJSONObject(i)?.let(::offerFrom) }
}

private fun offerFrom(o: JSONObject): IncomingOffer? {
    val id = o.optString("id")
    if (id.isEmpty()) return null
    return IncomingOffer(
        id = id,
        transferId = o.optString("transfer_id").ifEmpty { id },
        kind = o.optString("kind"),
        name = o.optString("name"),
        size = o.optLong("size"),
        files = o.optInt("files"),
        names = o.optJSONArray("names").strings(),
        senderName = o.optString("sender_name").ifEmpty { "Unknown device" },
        senderFp = o.optString("sender_fp"),
        addr = o.optString("addr"),
    )
}

/** An "offer_gone" event: the offer no longer waits (answered, timed out, withdrawn). */
data class OfferGone(val transferId: String, val reason: String)

/** Parses an "offer_gone" event; null for any other event. */
fun parseOfferGone(json: String): OfferGone? {
    val o = runCatching { JSONObject(json) }.getOrNull() ?: return null
    if (o.optString("type") != "offer_gone") return null
    val d = o.optJSONObject("data") ?: return null
    val id = d.optString("transfer_id")
    if (id.isEmpty()) return null
    return OfferGone(id, d.optString("reason"))
}

/** Whether an event changes what is kept for a resume ("kept" or "kept_gone"). */
fun isKeptEvent(json: String): Boolean {
    val o = runCatching { JSONObject(json) }.getOrNull() ?: return false
    return o.optString("type") == "kept" || o.optString("type") == "kept_gone"
}

/** The kept receive a "kept_gone" event says was completed by a later attempt, or null. */
fun keptCompleted(json: String): String? {
    val o = runCatching { JSONObject(json) }.getOrNull() ?: return null
    val d = o.optJSONObject("data") ?: return null
    if (o.optString("type") != "kept_gone" || d.optString("reason") != "completed") return null
    return d.optString("id").ifEmpty { null }
}

/** A send that lost its connection and gave up after its retries (ResumableJSON "outgoing"). */
data class InterruptedSend(
    val transferId: String,
    val name: String,
    val peerName: String,
    val peerFp: String,
    val files: Int,
    val bytes: Long,
    val error: String,
    /** Epoch milliseconds when it is forgotten, or 0. */
    val expires: Long,
)

/** What arrived of a receive whose connection was lost, kept on this phone (ResumableJSON "incoming"). */
data class KeptReceive(
    val id: String,
    val name: String,
    val senderName: String,
    val senderFp: String,
    val files: Int,
    val total: Long,
    val done: Long,
    val location: String,
    /** Epoch milliseconds when it is removed, or 0. */
    val expires: Long,
)

/** Everything that can be resumed (ResumableJSON). */
data class Resumable(
    val outgoing: List<InterruptedSend> = emptyList(),
    val incoming: List<KeptReceive> = emptyList(),
) {
    val isEmpty: Boolean get() = outgoing.isEmpty() && incoming.isEmpty()
}

fun parseResumable(json: String): Resumable {
    val o = runCatching { JSONObject(json) }.getOrNull() ?: return Resumable()
    val out = o.optJSONArray("outgoing").objects().mapNotNull { r ->
        val id = r.optString("transfer_id")
        if (id.isEmpty()) return@mapNotNull null
        InterruptedSend(
            transferId = id,
            name = r.optString("name"),
            peerName = r.optString("peer_name").ifEmpty { r.optString("addr") },
            peerFp = r.optString("peer_fp"),
            files = r.optInt("files"),
            bytes = r.optLong("bytes"),
            error = r.optString("error"),
            expires = parseTime(r.optString("expires")),
        )
    }
    val inc = o.optJSONArray("incoming").objects().mapNotNull { r ->
        val id = r.optString("id")
        if (id.isEmpty()) return@mapNotNull null
        KeptReceive(
            id = id,
            name = r.optString("name"),
            senderName = r.optString("sender_name").ifEmpty { "another device" },
            senderFp = r.optString("sender_fp"),
            files = r.optInt("files"),
            total = r.optLong("total"),
            done = r.optLong("done"),
            location = r.optString("location"),
            expires = parseTime(r.optString("expires")),
        )
    }
    return Resumable(out, inc)
}

/** A transfer in flight, as in desktop rocket's GET /api/transfers. */
data class TransferInfo(
    val id: String,
    /** "in" or "out". */
    val dir: String,
    val name: String,
    val peerName: String,
    val files: Int,
    val total: Long,
    val sent: Long,
)

fun parseTransferInfos(json: String): List<TransferInfo> {
    val arr = runCatching { JSONArray(json) }.getOrNull() ?: return emptyList()
    return (0 until arr.length()).mapNotNull { i ->
        val o = arr.optJSONObject(i) ?: return@mapNotNull null
        val id = o.optString("transfer_id")
        if (id.isEmpty()) return@mapNotNull null
        TransferInfo(
            id = id,
            dir = o.optString("dir"),
            name = o.optString("name"),
            peerName = o.optString("peer_name"),
            files = o.optInt("files"),
            total = o.optLong("total"),
            sent = o.optLong("sent"),
        )
    }
}

/** A sender whose key is pinned ("Always accept from this device"). */
data class TrustedSender(
    val fingerprint: String,
    val name: String,
    /** Epoch milliseconds, or 0 when unknown. */
    val approvedAt: Long,
)

fun parseTrusted(json: String): List<TrustedSender> {
    val arr = runCatching { JSONArray(json) }.getOrNull() ?: return emptyList()
    return (0 until arr.length()).mapNotNull { i ->
        val o = arr.optJSONObject(i) ?: return@mapNotNull null
        val fp = o.optString("fingerprint")
        if (fp.isEmpty()) return@mapNotNull null
        TrustedSender(fp, o.optString("name").ifEmpty { fp }, parseTime(o.optString("approved_at")))
    }
}

/**
 * The pairing window (PairingJSON, or a "pairing" event's data): while it
 * is open, rocket senders not yet trusted are accepted without asking and
 * trusted from then on.
 */
data class PairingWindow(
    val open: Boolean,
    /** When it closes, epoch milliseconds; 0 while closed. */
    val endsAt: Long,
) {
    companion object {
        val Closed = PairingWindow(false, 0)
    }
}

/**
 * Parses the pairing state {open, until, seconds}, bare or as a "pairing"
 * event; Closed for anything else. Without a usable until, the end is
 * seconds from now.
 */
fun parsePairing(json: String, now: Long = System.currentTimeMillis()): PairingWindow {
    val o = runCatching { JSONObject(json) }.getOrNull() ?: return PairingWindow.Closed
    val d = if (o.has("type")) {
        if (o.optString("type") != "pairing") return PairingWindow.Closed
        o.optJSONObject("data") ?: return PairingWindow.Closed
    } else {
        o
    }
    if (!d.optBoolean("open")) return PairingWindow.Closed
    val until = parseTime(d.optString("until"))
    val seconds = d.optLong("seconds")
    val endsAt = if (until > 0) until else now + seconds * 1000
    return if (endsAt > now) PairingWindow(true, endsAt) else PairingWindow.Closed
}

/** Whether an event is a "pairing" event (the window opened or closed). */
fun isPairingEvent(json: String): Boolean =
    runCatching { JSONObject(json).optString("type") == "pairing" }.getOrDefault(false)

/** A sender paired (accepted and trusted) while the pairing window was open. */
data class PairedDevice(val fingerprint: String, val name: String, val addr: String)

/** Parses a "paired" event {fingerprint, name, addr}; null for any other event. */
fun parsePaired(json: String): PairedDevice? {
    val o = runCatching { JSONObject(json) }.getOrNull() ?: return null
    if (o.optString("type") != "paired") return null
    val d = o.optJSONObject("data") ?: return null
    val fp = d.optString("fingerprint")
    if (fp.isEmpty()) return null
    return PairedDevice(fp, d.optString("name").ifEmpty { "Unknown device" }, d.optString("addr"))
}

/** Go's time.Time JSON (RFC 3339 with nanoseconds) to epoch ms; 0 if absent. */
internal fun parseTime(s: String): Long {
    if (s.isEmpty() || s.startsWith("0001-01-01")) return 0
    return runCatching { OffsetDateTime.parse(s).toInstant().toEpochMilli() }.getOrDefault(0)
}

private fun JSONArray?.objects(): List<JSONObject> {
    if (this == null) return emptyList()
    return (0 until length()).mapNotNull { optJSONObject(it) }
}

private fun JSONArray?.strings(): List<String> {
    if (this == null) return emptyList()
    return (0 until length()).map { optString(it) }.filter { it.isNotEmpty() }
}
