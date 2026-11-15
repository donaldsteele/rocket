package io.github.donaldsteele.rocket.core

import android.content.ContentResolver
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.ParcelFileDescriptor
import android.provider.DocumentsContract
import android.provider.DocumentsContract.Document
import android.provider.OpenableColumns
import android.system.Os
import androidx.core.net.toUri
import java.io.File
import java.io.FileInputStream
import java.io.FileNotFoundException
import java.io.InputStream
import java.util.Locale

/**
 * Turns Storage Access Framework picks and share-sheet URIs into
 * [SendItem]s, and opens them for the Go core. Queries block: call off the
 * main thread.
 *
 * Only content URIs served by other apps are read ([readable]). Anything
 * else would be opened with rocket's own permissions: a file:// URI (or one
 * of rocket's own providers) in a share intent would let any app make rocket
 * send its private files, node.key among them. As a second guard, a
 * descriptor that turns out to point into rocket's private storage is
 * refused.
 */
class ContentItems(context: Context) : ItemOpener, GrantKeeper {
    private val app = context.applicationContext
    private val cr: ContentResolver = app.contentResolver
    private val ownPackage: String = app.packageName

    /** rocket's private storage (credential- and device-protected), as given and resolved. */
    private val privateRoots: List<String> by lazy {
        val dirs = listOfNotNull(
            app.dataDir,
            runCatching { app.createDeviceProtectedStorageContext().dataDir }.getOrNull(),
        )
        dirs.flatMap { d -> listOf(d.absolutePath, runCatching { d.canonicalPath }.getOrDefault(d.absolutePath)) }.distinct()
    }

    /**
     * Whether uri may be read on behalf of whoever handed it over: a content
     * URI whose provider belongs to another app.
     */
    fun readable(uri: Uri): Boolean {
        if (!isForeignContentUri(uri.scheme, uri.authority, ownPackage)) return false
        val authority = authorityWithoutUser(uri.authority ?: return false)
        val owner = runCatching { app.packageManager.resolveContentProvider(authority, 0)?.packageName }.getOrNull()
        return owner != ownPackage
    }

    private fun requireReadable(uri: Uri) {
        if (!readable(uri)) {
            throw SecurityException("rocket only sends files that other apps share as content, not ${uri.scheme ?: "plain"} links.")
        }
    }

    /** Refuses a descriptor that points into rocket's private storage. */
    private fun requireNotPrivate(fd: Int) {
        val path = runCatching { Os.readlink("/proc/self/fd/$fd") }.getOrNull() ?: return
        if (isUnderAny(path, privateRoots)) throw SecurityException("rocket does not send its own private files.")
    }

    /** Files picked one by one (or shared): each is a top-level item. */
    fun fromUris(uris: List<Uri>): List<SendItem> {
        if (uris.size > MAX_ITEMS) throw tooMany()
        uris.forEach { requireReadable(it) }
        val found = uris.map { uri ->
            var name: String? = null
            var size = -1L
            runCatching {
                cr.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE), null, null, null)
            }.getOrNull()?.use { c ->
                if (c.moveToFirst()) {
                    val ni = c.getColumnIndex(OpenableColumns.DISPLAY_NAME)
                    val si = c.getColumnIndex(OpenableColumns.SIZE)
                    if (ni >= 0 && !c.isNull(ni)) name = c.getString(ni)
                    if (si >= 0 && !c.isNull(si)) size = c.getLong(si)
                }
            }
            Triple(uri, safeName(name ?: uri.lastPathSegment?.substringAfterLast('/')), size)
        }
        val names = uniqueNames(found.map { it.second })
        return found.mapIndexed { i, (uri, _, size) -> SendItem(names[i], uri.toString(), size, isDir = false) }
    }

    /**
     * A folder picked with ACTION_OPEN_DOCUMENT_TREE: the folder itself,
     * then everything inside, each folder listed before its contents.
     */
    fun fromTree(tree: Uri): List<SendItem> {
        requireReadable(tree)
        val rootId = DocumentsContract.getTreeDocumentId(tree)
        val rootUri = DocumentsContract.buildDocumentUriUsingTree(tree, rootId)
        val rootName = queryName(rootUri) ?: rootId.substringAfterLast(':').substringAfterLast('/')
        val out = ArrayList<SendItem>()
        val top = safeName(rootName, "folder")
        out += SendItem(top, null, 0, isDir = true)
        walk(tree, rootId, top, out)
        return out
    }

    private fun walk(tree: Uri, docId: String, prefix: String, out: MutableList<SendItem>) {
        val children = DocumentsContract.buildChildDocumentsUriUsingTree(tree, docId)
        val cols = arrayOf(Document.COLUMN_DOCUMENT_ID, Document.COLUMN_DISPLAY_NAME, Document.COLUMN_MIME_TYPE, Document.COLUMN_SIZE)
        data class Child(val id: String, val name: String, val dir: Boolean, val size: Long)
        val list = ArrayList<Child>()
        cr.query(children, cols, null, null, null)?.use { c ->
            while (c.moveToNext()) {
                val id = c.getString(0) ?: continue
                val dir = c.getString(2) == Document.MIME_TYPE_DIR
                val size = if (c.isNull(3)) -1L else c.getLong(3)
                list += Child(id, safeName(c.getString(1)), dir, size)
            }
        } ?: throw FileNotFoundException("cannot list $prefix")
        val names = uniqueNames(list.map { it.name })
        list.forEachIndexed { i, ch ->
            if (out.size >= MAX_ITEMS) throw tooMany()
            val path = "$prefix/${names[i]}"
            if (ch.dir) {
                out += SendItem(path, null, 0, isDir = true)
                walk(tree, ch.id, path, out)
            } else {
                out += SendItem(path, DocumentsContract.buildDocumentUriUsingTree(tree, ch.id).toString(), ch.size, isDir = false)
            }
        }
    }

    private fun queryName(uri: Uri): String? = runCatching {
        cr.query(uri, arrayOf(Document.COLUMN_DISPLAY_NAME), null, null, null)?.use { c ->
            if (c.moveToFirst() && !c.isNull(0)) c.getString(0) else null
        }
    }.getOrNull()

    override fun openFd(ref: String): Int {
        val uri = ref.toUri()
        requireReadable(uri)
        // A provider that serves only part of a file (an asset) or no
        // descriptor at all fails here; the stream fallback copes.
        val pfd = try {
            cr.openFileDescriptor(uri, "r")
        } catch (_: FileNotFoundException) {
            null
        } catch (_: IllegalArgumentException) {
            null
        } catch (_: UnsupportedOperationException) {
            null
        } ?: return -1
        try {
            requireNotPrivate(pfd.fd)
        } catch (e: SecurityException) {
            pfd.closeQuietly()
            throw e
        }
        return pfd.detachFd()
    }

    override fun openStream(ref: String): InputStream {
        val uri = ref.toUri()
        requireReadable(uri)
        val s = cr.openInputStream(uri) ?: throw FileNotFoundException("cannot open $ref")
        try {
            (s as? FileInputStream)?.let { f -> ParcelFileDescriptor.dup(f.fd).use { requireNotPrivate(it.fd) } }
        } catch (e: SecurityException) {
            s.closeQuietly()
            throw e
        }
        return s
    }

    /**
     * Takes persistable read access to a picked URI (a document or a
     * folder tree from the system picker), so a send can be resumed after
     * the app was closed. Providers that do not offer it (most share-sheet
     * URIs) return false; Resume then asks for the files again.
     */
    override fun take(uri: String): Boolean = runCatching {
        cr.takePersistableUriPermission(uri.toUri(), Intent.FLAG_GRANT_READ_URI_PERMISSION)
        true
    }.getOrDefault(false)

    override fun release(uri: String) {
        runCatching { cr.releasePersistableUriPermission(uri.toUri(), Intent.FLAG_GRANT_READ_URI_PERMISSION) }
    }

    /**
     * Whether the files in items can still be opened (Resume without asking
     * again). Access is granted per pick, so the first few files tell. Blocks.
     */
    fun canRead(items: List<SendItem>): Boolean = items.asSequence().mapNotNull { it.ref }.take(16).all { ref ->
        runCatching {
            val uri = ref.toUri()
            requireReadable(uri)
            val pfd = runCatching { cr.openFileDescriptor(uri, "r") }.getOrNull()
            if (pfd != null) {
                pfd.close()
            } else {
                (cr.openInputStream(uri) ?: throw FileNotFoundException(ref)).close()
            }
            true
        }.getOrDefault(false)
    }

    private fun tooMany() = IllegalStateException("More than ${"%,d".format(MAX_ITEMS)} items: rocket sends at most that many at once.")
}

/**
 * Whether a URI with this scheme and authority is a content URI served by
 * some app other than ownPackage, judged by the authority (rocket's own
 * providers are named after its package). ContentResolver opens file://
 * URIs itself, with the caller's permissions, so they never qualify.
 */
fun isForeignContentUri(scheme: String?, authority: String?, ownPackage: String): Boolean {
    if (scheme != ContentResolver.SCHEME_CONTENT) return false
    val a = authorityWithoutUser(authority ?: return false).lowercase(Locale.ROOT)
    if (a.isEmpty()) return false
    val own = ownPackage.lowercase(Locale.ROOT)
    return a != own && !a.startsWith("$own.")
}

/** An authority without the "<user>@" prefix of a cross-user URI. */
internal fun authorityWithoutUser(authority: String): String = authority.substringAfterLast('@')

/** Whether path is one of roots or inside one ("/" separated, normalized). */
fun isUnderAny(path: String, roots: List<String>): Boolean {
    val p = File(path).normalize().invariantSeparatorsPath
    return roots.any { r ->
        val root = File(r).normalize().invariantSeparatorsPath.trimEnd('/')
        root.isNotEmpty() && (p == root || p.startsWith("$root/"))
    }
}
