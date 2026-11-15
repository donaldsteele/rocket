package io.github.donaldsteele.rocket.core

import org.json.JSONArray
import org.json.JSONObject
import java.io.File

/**
 * Keeps and releases long-lived read access to picked content URIs
 * (ContentResolver.takePersistableUriPermission), so an interrupted send
 * can be resumed after the app was closed.
 */
interface GrantKeeper {
    /** Takes persistable read access to uri; false when the provider does not allow it. */
    fun take(uri: String): Boolean

    /** Releases what [take] took. */
    fun release(uri: String)
}

/**
 * What an interrupted send needs to be resumed: its items (relative paths,
 * content URIs, sizes) and the URIs it took persistable access to.
 */
data class SavedSend(
    val id: String,
    val items: List<SendItem>,
    /** URIs with persistable read access (the picked files, or the picked folder's tree). */
    val grants: List<String>,
)

/**
 * The items of interrupted sends, by the Go core's transfer ID, one JSON
 * file each in [dir] (the app's private storage). The Go core keeps the
 * rest (peer, manifest hash) for 24 hours; [prune] drops what it no longer
 * has. Grants are released once no saved send uses them.
 */
class OutgoingStore(private val dir: File, private val grants: GrantKeeper) {
    @Synchronized
    fun save(s: SavedSend) {
        if (!validId(s.id)) return
        dir.mkdirs()
        val items = JSONArray()
        s.items.forEach { i ->
            items.put(
                JSONObject().put("path", i.path).put("ref", i.ref ?: JSONObject.NULL).put("size", i.size).put("dir", i.isDir),
            )
        }
        val o = JSONObject().put("id", s.id).put("items", items).put("grants", JSONArray(s.grants))
        val tmp = File(dir, "${s.id}.tmp")
        tmp.writeText(o.toString())
        if (!tmp.renameTo(File(dir, "${s.id}.json"))) {
            tmp.delete()
        }
    }

    @Synchronized
    fun load(id: String): SavedSend? {
        if (!validId(id)) return null
        val f = File(dir, "$id.json")
        if (!f.isFile) return null
        return runCatching { parse(f.readText()) }.getOrNull()?.takeIf { it.id == id }
    }

    /**
     * Forgets send id and, with release, the grants no other saved send
     * uses (a resume that took them over passes false).
     */
    @Synchronized
    fun remove(id: String, release: Boolean = true) {
        val gone = load(id) ?: return
        File(dir, "$id.json").delete()
        if (release) releaseUnused(gone.grants)
    }

    /** Forgets every saved send whose ID is not in live. */
    @Synchronized
    fun prune(live: Set<String>) {
        ids().filter { it !in live }.forEach { remove(it) }
    }

    /** Whether a saved send uses grant uri. */
    @Synchronized
    fun uses(uri: String): Boolean = ids().any { id -> load(id)?.grants?.contains(uri) == true }

    /** Releases the grants in list that no saved send uses. */
    @Synchronized
    fun releaseUnused(list: List<String>) {
        list.filter { !uses(it) }.forEach { runCatching { grants.release(it) } }
    }

    private fun ids(): List<String> =
        dir.listFiles { f -> f.isFile && f.name.endsWith(".json") }?.map { it.name.removeSuffix(".json") }?.filter(::validId).orEmpty()

    private fun parse(json: String): SavedSend {
        val o = JSONObject(json)
        val arr = o.getJSONArray("items")
        val items = (0 until arr.length()).map { k ->
            val i = arr.getJSONObject(k)
            SendItem(
                path = i.getString("path"),
                ref = if (i.isNull("ref")) null else i.getString("ref"),
                size = i.getLong("size"),
                isDir = i.getBoolean("dir"),
            )
        }
        val g = o.optJSONArray("grants")
        val list = if (g == null) emptyList() else (0 until g.length()).map { g.getString(it) }
        return SavedSend(o.getString("id"), items, list)
    }

    private companion object {
        /** Transfer IDs are 16 to 128 characters of [A-Za-z0-9_-] (docs/protocol.md). */
        fun validId(id: String) = id.length in 16..128 && id.all { it.isLetterOrDigit() && it.code < 128 || it == '_' || it == '-' }
    }
}
