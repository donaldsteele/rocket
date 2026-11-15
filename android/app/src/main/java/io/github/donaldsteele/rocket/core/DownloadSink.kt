package io.github.donaldsteele.rocket.core

import android.content.ContentResolver
import android.content.ContentValues
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Environment
import android.provider.MediaStore
import android.webkit.MimeTypeMap
import androidx.annotation.RequiresApi
import androidx.core.net.toUri
import io.github.donaldsteele.rocket.core.rocketcore.Sink
import io.github.donaldsteele.rocket.core.rocketcore.SinkFile
import io.github.donaldsteele.rocket.core.rocketcore.SinkTransfer
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.io.FileOutputStream
import java.io.IOException
import java.io.OutputStream
import java.util.Locale

/**
 * Where accepted transfers are saved. The Go core checks every path against
 * the manifest and sanitizes it before a sink sees it; a sink only picks
 * fresh names and stores bytes (see rocketcore.Sink). A transfer cut off by
 * a lost connection is kept (SinkTransfer.keep) and continued by the
 * sender's next attempt (resume), or removed (discard).
 */
interface DownloadSink : Sink {
    /** Where files go, for display ("Download/rocket"). */
    val where: String

    /** Transfer [transferId] has ended: its fresh names need no claim any more. */
    fun finished(transferId: String)
}

/** The folder under Downloads that receives go to. */
const val DOWNLOAD_FOLDER = "rocket"

/** Suffix of a file still being written (as desktop rocket's .rocket-part). */
private const val PART = ".rocket-part"

private const val MAX_TRIES = 10_000

/**
 * name with " (n)" before its extension, as desktop rocket numbers taken
 * names (transfer.UniquePath): "photos (1)", "a (2).txt". n = 0 is name.
 */
fun numberedName(name: String, n: Int): String {
    if (n == 0) return name
    val dot = name.lastIndexOf('.')
    return if (dot >= 0) "${name.substring(0, dot)} ($n)${name.substring(dot)}" else "$name ($n)"
}

/**
 * Top-level names claimed by transfers in flight, so two transfers running
 * at once never both take the same fresh folder name. Case-blind, as shared
 * storage is.
 */
class Claims {
    private val byTransfer = HashMap<String, MutableSet<String>>()

    @Synchronized
    fun claim(transferId: String, name: String): Boolean {
        val key = name.lowercase(Locale.ROOT)
        if (byTransfer.values.any { key in it }) return false
        byTransfer.getOrPut(transferId) { HashSet() }.add(key)
        return true
    }

    @Synchronized
    fun release(transferId: String) {
        byTransfer.remove(transferId)
    }
}

/**
 * Saves into a folder on disk: the public Download/rocket folder on Android
 * 8 and 9 (with WRITE_EXTERNAL_STORAGE), any folder in tests. Like desktop
 * rocket, a file is written as "<name>.rocket-part" and renamed when
 * complete, a top-level folder always gets a fresh name, nothing existing is
 * replaced, and an aborted transfer removes everything it wrote.
 *
 * [openFd] returns a detached writable descriptor for a file, or -1 to take
 * the bytes through SinkFile.write; [openRw] a readable and writable one
 * (not truncated) to continue a kept part file, or -1 when it cannot;
 * [onSaved] hears of each finished file. A kept transfer leaves its part
 * file and finished files in place.
 */
class FileSink(
    private val root: File,
    private val openFd: (File) -> Int = { -1 },
    private val onSaved: (File) -> Unit = {},
    private val claims: Claims = Claims(),
    override val where: String = root.path,
    private val openRw: (File) -> Int = { -1 },
) : DownloadSink {
    override fun begin(transferID: String, manifestJSON: String): SinkTransfer {
        if (!root.isDirectory && !root.mkdirs()) throw IOException("cannot create the folder $where")
        return Tx(transferID)
    }

    override fun resume(transferID: String, manifestJSON: String, state: String): SinkTransfer {
        val k = KeptFiles.parse(state)
        return Tx(transferID).apply {
            made += k.dirs.map(::inside)
            saved += k.files.map(::inside)
            parts += k.parts.map(::inside)
        }
    }

    override fun check(state: String) {
        val k = KeptFiles.parse(state)
        (k.dirs + k.files + k.parts).map(::inside).firstOrNull { !it.exists() }?.let {
            throw IOException("${it.name} is gone")
        }
    }

    override fun discard(state: String) = goCallback("discard", Unit) {
        val k = runCatching { KeptFiles.parse(state) }.getOrNull() ?: return@goCallback
        (k.parts + k.files).forEach { p -> runCatching { inside(p).delete() } }
        k.dirs.asReversed().forEach { p -> runCatching { inside(p).delete() } }
    }

    override fun finished(transferId: String) = claims.release(transferId)

    /** A kept path, which must be inside [root]. */
    private fun inside(path: String): File {
        val f = File(path)
        if (!isUnderAny(f.path, listOf(root.path)) || f.normalize() == root.normalize()) {
            throw IOException("kept path $path is not in $where")
        }
        return f
    }

    private inner class Tx(private val id: String) : SinkTransfer {
        /** Folders this transfer created, parents first. */
        val made = mutableListOf<File>()
        val saved = mutableListOf<File>()
        private val open = mutableListOf<Out>()

        /** Part files kept by an earlier attempt that this one continues. */
        val parts = mutableListOf<File>()

        override fun mkdir(rel: String): String {
            if ('/' !in rel) {
                for (n in 0 until MAX_TRIES) {
                    val cand = numberedName(rel, n)
                    val dir = File(root, cand)
                    if (dir.exists() || File(root, cand + PART).exists() || !claims.claim(id, cand)) continue
                    if (dir.mkdir()) {
                        synchronized(this) { made += dir }
                        return cand
                    }
                }
                throw IOException("no free name for $rel in $where")
            }
            makeDirs(File(root, rel))
            return rel
        }

        private fun makeDirs(dir: File) {
            val missing = generateSequence(dir) { it.parentFile }.takeWhile { !it.exists() }.toList().asReversed()
            for (d in missing) {
                if (!d.mkdir() && !d.isDirectory) throw IOException("cannot create the folder ${d.name}")
                synchronized(this) { made += d }
            }
            if (!dir.isDirectory) throw IOException("${dir.name} is not a folder")
        }

        override fun create(rel: String, size: Long): SinkFile {
            val target = File(root, rel)
            val parent = target.parentFile ?: root
            makeDirs(parent)
            for (n in 0 until MAX_TRIES) {
                val cand = numberedName(target.name, n)
                val final = File(parent, cand)
                if (final.exists()) continue
                val part = File(parent, cand + PART)
                if (part.createNewFile()) return Out(part, final).also { synchronized(this) { open += it } }
            }
            throw IOException("no free name for ${target.name}")
        }

        override fun abort() = goCallback("abort", Unit) {
            val (outs, files, dirs) = synchronized(this) { Triple(open.toList(), saved.toList() + parts, made.toList()) }
            outs.forEach { it.abort() }
            files.forEach { runCatching { it.delete() } }
            // Children before parents; delete() leaves a folder that is not
            // empty, so nothing that was there before goes.
            dirs.asReversed().forEach { runCatching { it.delete() } }
        }

        override fun location(): String = goCallback("location", "") { where }

        override fun keep(): String {
            val outs = synchronized(this) { open.toList() }
            outs.forEach { it.close() }
            return synchronized(this) {
                KeptFiles(
                    dirs = made.map { it.path },
                    files = saved.map { it.path },
                    parts = (parts + outs.map { it.part }).distinct().map { it.path },
                ).json()
            }
        }

        override fun reopen(ref: String): SinkFile {
            val part = inside(ref)
            val known = synchronized(this) { part in parts || open.any { it.part == part } }
            if (!known || !part.name.endsWith(PART) || !part.isFile) throw IOException("no kept part file $ref")
            val final = File(part.parentFile, part.name.removeSuffix(PART))
            return Out(part, final, rw = true).also {
                synchronized(this) {
                    parts -= part
                    open += it
                }
            }
        }

        inner class Out(val part: File, private val final: File, private val rw: Boolean = false) : SinkFile {
            private var stream: OutputStream? = null

            // No error result: a descriptor that cannot be opened means -1,
            // and the bytes come through write(), which can report it.
            override fun fd(): Long = goCallback("fd", -1L) { (if (rw) openRw(part) else openFd(part)).toLong() }

            override fun ref(): String = goCallback("ref", "") { part.path }

            /** Stops writing, leaving the part file (keep). */
            fun close() {
                runCatching { stream?.close() }
                stream = null
            }

            override fun write(data: ByteArray) {
                // A reopened part file is continued, never cut short here.
                val s = stream ?: FileOutputStream(part, rw).also { stream = it }
                s.write(data)
            }

            override fun commit(): String {
                stream?.close()
                stream = null
                var dest = final
                var n = 1
                while (dest.exists() && n < MAX_TRIES) dest = File(final.parentFile, numberedName(final.name, n++))
                if (dest.exists() || !part.renameTo(dest)) {
                    part.delete()
                    throw IOException("cannot save ${final.name}")
                }
                synchronized(this@Tx) {
                    open -= this
                    saved += dest
                }
                onSaved(dest)
                return dest.path
            }

            override fun abort() = goCallback("abort", Unit) {
                runCatching { stream?.close() }
                stream = null
                runCatching { part.delete() }
                synchronized(this@Tx) { open -= this }
            }
        }
    }
}

/**
 * Saves into Download/rocket through MediaStore (Android 10 and later; no
 * permission needed). Each file is inserted pending, written through a file
 * descriptor, and published when complete. MediaStore itself never replaces
 * a file: a taken name becomes "a (1).txt". Top-level folders get a fresh
 * name here; an aborted transfer deletes every item it inserted.
 */
@RequiresApi(Build.VERSION_CODES.Q)
class MediaStoreSink(
    private val resolver: ContentResolver,
    private val claims: Claims = Claims(),
) : DownloadSink {
    override val where = "${Environment.DIRECTORY_DOWNLOADS}/$DOWNLOAD_FOLDER"

    private val base = "$where/"
    private val collection: Uri = MediaStore.Downloads.getContentUri(MediaStore.VOLUME_EXTERNAL_PRIMARY)

    // Folders cannot be made through MediaStore. They appear with their
    // first file; this path is used to see what is there and to make empty
    // folders where the platform allows it.
    @Suppress("DEPRECATION")
    private val disk = File(Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DOWNLOADS), DOWNLOAD_FOLDER)

    override fun begin(transferID: String, manifestJSON: String): SinkTransfer = Tx(transferID)

    override fun resume(transferID: String, manifestJSON: String, state: String): SinkTransfer {
        val k = KeptFiles.parse(state)
        return Tx(transferID).apply {
            uris += k.files.map { ownItem(it) }
            dirs += k.dirs.map(::insideDisk)
        }
    }

    override fun check(state: String) {
        val k = KeptFiles.parse(state)
        k.files.map { ownItem(it) }.firstOrNull { !exists(it) }?.let { throw IOException("a kept file is gone ($it)") }
    }

    override fun discard(state: String) = goCallback("discard", Unit) {
        val k = runCatching { KeptFiles.parse(state) }.getOrNull() ?: return@goCallback
        k.files.forEach { u -> runCatching { resolver.delete(ownItem(u), null, null) } }
        k.dirs.asReversed().forEach { d -> runCatching { insideDisk(d).delete() } }
    }

    override fun finished(transferId: String) = claims.release(transferId)

    /** A kept item, which must be one of the Downloads collection's. */
    private fun ownItem(s: String): Uri {
        val uri = s.toUri()
        val base = collection.toString().trimEnd('/')
        if (uri.scheme != ContentResolver.SCHEME_CONTENT || !uri.toString().startsWith("$base/")) {
            throw IOException("kept item $s is not in $where")
        }
        return uri
    }

    /** A kept folder, which must be inside Download/rocket. */
    private fun insideDisk(path: String): File {
        val f = File(path)
        if (!isUnderAny(f.path, listOf(disk.path)) || f.normalize() == disk.normalize()) {
            throw IOException("kept folder $path is not in $where")
        }
        return f
    }

    /** Whether item uri is still there, pending or not. */
    private fun exists(uri: Uri): Boolean {
        val cols = arrayOf(MediaStore.MediaColumns._ID)
        val cursor = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            val q = Bundle().apply { putInt(MediaStore.QUERY_ARG_MATCH_PENDING, MediaStore.MATCH_INCLUDE) }
            resolver.query(uri, cols, q, null)
        } else {
            @Suppress("DEPRECATION")
            resolver.query(MediaStore.setIncludePending(uri), cols, null, null, null)
        }
        return cursor?.use { it.count > 0 } ?: false
    }

    /**
     * Whether top-level name is used in Download/rocket, as a folder or a
     * file. The file system shows every folder from Android 11 on; MediaStore
     * adds this app's own items, pending and trashed ones too (a receive cut
     * off by a crash leaves pending items behind, and those still hold their
     * folder). Android 10 hides other apps' folders from both, so a folder
     * another app made there can still be written into; names never clash,
     * as MediaStore numbers a taken file name itself.
     */
    private fun taken(name: String): Boolean {
        if (runCatching { File(disk, name).exists() }.getOrDefault(false)) return true
        val esc = likeEscape(name)
        val sel = "${MediaStore.MediaColumns.RELATIVE_PATH} LIKE ? ESCAPE '\\' OR " +
            "(${MediaStore.MediaColumns.RELATIVE_PATH} LIKE ? ESCAPE '\\' AND ${MediaStore.MediaColumns.DISPLAY_NAME} LIKE ? ESCAPE '\\')"
        val args = arrayOf("${likeEscape(base)}$esc/%", likeEscape(base), esc)
        val cols = arrayOf(MediaStore.MediaColumns._ID)
        val cursor = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            val q = Bundle().apply {
                putString(ContentResolver.QUERY_ARG_SQL_SELECTION, sel)
                putStringArray(ContentResolver.QUERY_ARG_SQL_SELECTION_ARGS, args)
                putInt(MediaStore.QUERY_ARG_MATCH_PENDING, MediaStore.MATCH_INCLUDE)
                putInt(MediaStore.QUERY_ARG_MATCH_TRASHED, MediaStore.MATCH_INCLUDE)
            }
            resolver.query(collection, cols, q, null)
        } else {
            @Suppress("DEPRECATION")
            resolver.query(MediaStore.setIncludePending(collection), cols, sel, args, null)
        }
        return cursor?.use { it.count > 0 } ?: false
    }

    private inner class Tx(private val id: String) : SinkTransfer {
        /** Every item this transfer inserted (or the kept one it continues), pending or published. */
        val uris = mutableListOf<Uri>()
        val dirs = mutableListOf<File>()
        private val open = mutableListOf<Item>()

        override fun mkdir(rel: String): String {
            if ('/' !in rel) {
                for (n in 0 until MAX_TRIES) {
                    val cand = numberedName(rel, n)
                    if (taken(cand) || !claims.claim(id, cand)) continue
                    makeDir(cand)
                    return cand
                }
                throw IOException("no free name for $rel in $where")
            }
            makeDir(rel)
            return rel
        }

        /** Best effort: an empty folder may not be possible through the file system. */
        private fun makeDir(rel: String) {
            runCatching {
                val missing = generateSequence(File(disk, rel)) { it.parentFile }.takeWhile { !it.exists() }.toList().asReversed()
                for (d in missing) {
                    if (!d.mkdir()) break
                    synchronized(this) { dirs += d }
                }
            }
        }

        override fun create(rel: String, size: Long): SinkFile {
            val folder = rel.substringBeforeLast('/', "")
            val name = rel.substringAfterLast('/')
            val values = ContentValues().apply {
                put(MediaStore.MediaColumns.DISPLAY_NAME, name)
                put(MediaStore.MediaColumns.RELATIVE_PATH, if (folder.isEmpty()) base else "$base$folder/")
                put(MediaStore.MediaColumns.MIME_TYPE, mimeOf(name))
                put(MediaStore.MediaColumns.IS_PENDING, 1)
            }
            val uri = resolver.insert(collection, values) ?: throw IOException("cannot save $name in $where")
            synchronized(this) { uris += uri }
            return Item(uri).also { synchronized(this) { open += it } }
        }

        override fun keep(): String {
            val items = synchronized(this) { open.toList() }
            items.forEach { it.close() }
            return synchronized(this) { KeptFiles(dirs = dirs.map { it.path }, files = uris.map { it.toString() }, parts = emptyList()).json() }
        }

        override fun reopen(ref: String): SinkFile {
            val uri = ownItem(ref)
            if (synchronized(this) { uri !in uris }) throw IOException("no kept item $ref")
            return Item(uri, rw = true).also { synchronized(this) { open += it } }
        }

        override fun abort() = goCallback("abort", Unit) {
            val (items, folders) = synchronized(this) { uris.toList() to dirs.toList() }
            items.forEach { runCatching { resolver.delete(it, null, null) } }
            folders.asReversed().forEach { runCatching { it.delete() } }
        }

        override fun location(): String = goCallback("location", "") { where }

        private inner class Item(private val uri: Uri, private val rw: Boolean = false) : SinkFile {
            private var stream: OutputStream? = null

            // The descriptor is detached for Go, which closes it; close()
            // on the emptied ParcelFileDescriptor is then a no-op. fd() has
            // no error result: when the pending item cannot be opened, -1
            // sends the bytes through write(), which reports the failure.
            // A kept item is reopened "rw", so it is not truncated (Go cuts
            // it to where it continues).
            override fun fd(): Long = goCallback("fd", -1L) {
                resolver.openFileDescriptor(uri, if (rw) "rw" else "w")?.use { it.detachFd().toLong() } ?: -1L
            }

            override fun ref(): String = goCallback("ref", "") { uri.toString() }

            /** Stops writing, leaving the pending item (keep). */
            fun close() {
                runCatching { stream?.close() }
                stream = null
                synchronized(this@Tx) { open -= this }
            }

            override fun write(data: ByteArray) {
                val s = stream ?: (resolver.openOutputStream(uri, "w") ?: throw IOException("cannot write $uri")).also { stream = it }
                s.write(data)
            }

            override fun commit(): String {
                stream?.close()
                stream = null
                val done = ContentValues().apply { put(MediaStore.MediaColumns.IS_PENDING, 0) }
                if (resolver.update(uri, done, null, null) < 1) throw IOException("cannot publish $uri")
                synchronized(this@Tx) { open -= this }
                return uri.toString()
            }

            override fun abort() = goCallback("abort", Unit) {
                runCatching { stream?.close() }
                stream = null
                runCatching { resolver.delete(uri, null, null) }
                synchronized(this@Tx) {
                    uris -= uri
                    open -= this
                }
            }
        }
    }
}

/**
 * What a kept transfer stored, as SinkTransfer.keep describes it: folders
 * made (parents first), files (paths or content URIs) and part files.
 */
internal data class KeptFiles(val dirs: List<String>, val files: List<String>, val parts: List<String>) {
    fun json(): String = JSONObject()
        .put("dirs", JSONArray(dirs))
        .put("files", JSONArray(files))
        .put("parts", JSONArray(parts))
        .toString()

    companion object {
        fun parse(state: String): KeptFiles {
            val o = JSONObject(state)
            fun list(key: String): List<String> {
                val a = o.optJSONArray(key) ?: return emptyList()
                return (0 until a.length()).map { a.getString(it) }
            }
            return KeptFiles(list("dirs"), list("files"), list("parts"))
        }
    }
}

/** A MIME type from a file name's extension; octet-stream when unknown. */
private fun mimeOf(name: String): String {
    val ext = name.substringAfterLast('.', "").lowercase(Locale.ROOT)
    if (ext.isEmpty()) return "application/octet-stream"
    return MimeTypeMap.getSingleton().getMimeTypeFromExtension(ext) ?: "application/octet-stream"
}

/** Escapes a literal for SQL LIKE ... ESCAPE '\'. */
internal fun likeEscape(s: String): String =
    s.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
