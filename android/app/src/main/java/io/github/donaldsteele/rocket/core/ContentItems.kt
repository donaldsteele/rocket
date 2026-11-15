package io.github.donaldsteele.rocket.core

import android.content.ContentResolver
import android.net.Uri
import android.provider.DocumentsContract
import android.provider.DocumentsContract.Document
import android.provider.OpenableColumns
import androidx.core.net.toUri
import java.io.FileNotFoundException
import java.io.InputStream

/**
 * Turns Storage Access Framework picks and share-sheet URIs into
 * [SendItem]s, and opens them for the Go core. Queries block: call off the
 * main thread.
 */
class ContentItems(private val cr: ContentResolver) : ItemOpener {

    /** Files picked one by one (or shared): each is a top-level item. */
    fun fromUris(uris: List<Uri>): List<SendItem> {
        if (uris.size > MAX_ITEMS) throw tooMany()
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
        // A provider that serves only part of a file (an asset) or no
        // descriptor at all fails here; the stream fallback copes.
        val pfd = try {
            cr.openFileDescriptor(ref.toUri(), "r")
        } catch (_: FileNotFoundException) {
            null
        } catch (_: IllegalArgumentException) {
            null
        } catch (_: UnsupportedOperationException) {
            null
        }
        return pfd?.detachFd() ?: -1
    }

    override fun openStream(ref: String): InputStream =
        cr.openInputStream(ref.toUri()) ?: throw FileNotFoundException("cannot open $ref")

    private fun tooMany() = IllegalStateException("More than ${"%,d".format(MAX_ITEMS)} items: rocket sends at most that many at once.")
}
