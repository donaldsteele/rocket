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
import io.github.donaldsteele.rocket.core.rocketcore.Sink
import io.github.donaldsteele.rocket.core.rocketcore.SinkFile
import io.github.donaldsteele.rocket.core.rocketcore.SinkTransfer
import java.io.File
import java.io.IOException
import java.io.OutputStream
import java.util.Locale

/**
 * Where accepted transfers are saved. The Go core checks every path against
 * the manifest and sanitizes it before a sink sees it; a sink only picks
 * fresh names and stores bytes (see rocketcore.Sink).
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
 * the bytes through SinkFile.write; [onSaved] hears of each finished file.
 */
class FileSink(
    private val root: File,
    private val openFd: (File) -> Int = { -1 },
    private val onSaved: (File) -> Unit = {},
    private val claims: Claims = Claims(),
    override val where: String = root.path,
) : DownloadSink {
    override fun begin(transferID: String, manifestJSON: String): SinkTransfer {
        if (!root.isDirectory && !root.mkdirs()) throw IOException("cannot create the folder $where")
        return Tx(transferID)
    }

    override fun finished(transferId: String) = claims.release(transferId)

    private inner class Tx(private val id: String) : SinkTransfer {
        /** Folders this transfer created, parents first. */
        private val made = mutableListOf<File>()
        private val saved = mutableListOf<File>()
        private val open = mutableListOf<Out>()

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
            val (outs, files, dirs) = synchronized(this) { Triple(open.toList(), saved.toList(), made.toList()) }
            outs.forEach { it.abort() }
            files.forEach { runCatching { it.delete() } }
            // Children before parents; delete() leaves a folder that is not
            // empty, so nothing that was there before goes.
            dirs.asReversed().forEach { runCatching { it.delete() } }
        }

        override fun location(): String = goCallback("location", "") { where }

        private inner class Out(private val part: File, private val final: File) : SinkFile {
            private var stream: OutputStream? = null

            // No error result: a descriptor that cannot be opened means -1,
            // and the bytes come through write(), which can report it.
            override fun fd(): Long = goCallback("fd", -1L) { openFd(part).toLong() }

            override fun write(data: ByteArray) {
                val s = stream ?: part.outputStream().also { stream = it }
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

    override fun finished(transferId: String) = claims.release(transferId)

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
        private val uris = mutableListOf<Uri>()
        private val dirs = mutableListOf<File>()

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
            return Item(uri)
        }

        override fun abort() = goCallback("abort", Unit) {
            val (items, folders) = synchronized(this) { uris.toList() to dirs.toList() }
            items.forEach { runCatching { resolver.delete(it, null, null) } }
            folders.asReversed().forEach { runCatching { it.delete() } }
        }

        override fun location(): String = goCallback("location", "") { where }

        private inner class Item(private val uri: Uri) : SinkFile {
            private var stream: OutputStream? = null

            // The descriptor is detached for Go, which closes it; close()
            // on the emptied ParcelFileDescriptor is then a no-op. fd() has
            // no error result: when the pending item cannot be opened, -1
            // sends the bytes through write(), which reports the failure.
            override fun fd(): Long = goCallback("fd", -1L) {
                resolver.openFileDescriptor(uri, "w")?.use { it.detachFd().toLong() } ?: -1L
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
                return uri.toString()
            }

            override fun abort() = goCallback("abort", Unit) {
                runCatching { stream?.close() }
                stream = null
                runCatching { resolver.delete(uri, null, null) }
                synchronized(this@Tx) { uris -= uri }
            }
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
