package io.github.donaldsteele.rocket.core

import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.ByteArrayInputStream
import java.io.InputStream

class SendItemsTest {
    @Test
    fun safeNameKeepsTheManifestValid() {
        assertEquals("report.pdf", safeName("report.pdf"))
        assertEquals("a_b_c_d", safeName("a/b\\c:d"))
        assertEquals("tab_here", safeName("tab\there"))
        assertEquals("file", safeName(null))
        assertEquals("file", safeName("   "))
        assertEquals("file", safeName(".."))
        assertEquals("folder", safeName(".", "folder"))
        assertEquals("ünïcødé 📷.jpg", safeName("ünïcødé 📷.jpg"))
    }

    @Test
    fun longNamesAreShortenedKeepingTheExtension() {
        val long = "x".repeat(300) + ".jpeg"
        val s = safeName(long)
        assertTrue(s.endsWith(".jpeg"))
        assertEquals(255, s.toByteArray().size)
        // Multi-byte characters are never cut in half.
        val emoji = "📷".repeat(100) + ".png"
        val e = safeName(emoji)
        assertTrue(e.toByteArray().size <= 255)
        assertTrue(e.endsWith(".png"))
        assertEquals(e, String(e.toByteArray(), Charsets.UTF_8))
    }

    @Test
    fun uniqueNamesIgnoreCase() {
        assertEquals(
            listOf("a.jpg", "A (1).JPG", "a (2).jpg", "b", "b (1)", "a (1)"),
            uniqueNames(listOf("a.jpg", "A.JPG", "a.jpg", "b", "b", "a (1)")),
        )
    }

    @Test
    fun summary() {
        val items = listOf(
            SendItem("photos", null, 0, true),
            SendItem("photos/a.jpg", "u1", 100, false),
            SendItem("photos/b.jpg", "u2", -1, false),
            SendItem("notes.txt", "u3", 5, false),
        )
        val s = summarize(items)
        assertEquals("photos and 1 more", s.label)
        assertEquals(3, s.files)
        assertEquals(105L, s.bytes)
        assertTrue(s.unknownSize)
        assertEquals("a.txt", summarize(listOf(SendItem("a.txt", "u", 1, false))).label)
    }

    /** Serves fixed bytes per ref and counts opens. */
    private class FakeOpener(private val files: Map<String, ByteArray>, private val fds: Map<String, Int> = emptyMap()) : ItemOpener {
        val opens = mutableListOf<String>()
        override fun openFd(ref: String): Int = fds[ref] ?: -1
        override fun openStream(ref: String): InputStream {
            opens += ref
            return ByteArrayInputStream(files.getValue(ref))
        }
    }

    @Test
    fun chunkReaderReadsForwardFromOneStream() {
        val data = ByteArray(1000) { (it % 251).toByte() }
        val opener = FakeOpener(mapOf("u" to data))
        val r = ChunkReader(opener)
        val got = mutableListOf<Byte>()
        var off = 0L
        while (true) {
            val b = r.read(0, "u", off, 300)
            if (b.isEmpty()) break
            got += b.toList()
            off += b.size
        }
        assertArrayEquals(data, got.toByteArray())
        assertEquals(listOf("u"), opener.opens)
    }

    @Test
    fun chunkReaderReopensForAnotherOffset() {
        val data = ByteArray(100) { it.toByte() }
        val opener = FakeOpener(mapOf("u" to data))
        val r = ChunkReader(opener)
        assertArrayEquals(data.copyOfRange(0, 10), r.read(0, "u", 0, 10))
        assertArrayEquals(data.copyOfRange(50, 60), r.read(0, "u", 50, 10))
        assertArrayEquals(data.copyOfRange(60, 70), r.read(0, "u", 60, 10))
        assertEquals(2, opener.opens.size)
        assertArrayEquals(ByteArray(0), r.read(0, "u", 100, 10))
        r.close()
    }

    @Test
    fun goSourceServesTheCore() {
        val items = listOf(
            SendItem("photos", null, 0, true),
            SendItem("photos/a.jpg", "fd", 3, false),
            SendItem("photos/b.bin", "stream", -1, false),
        )
        val opener = FakeOpener(mapOf("stream" to byteArrayOf(1, 2, 3, 4)), fds = mapOf("fd" to 42))
        val src = GoSource(items, opener)
        assertEquals(3L, src.count())
        assertEquals("photos/a.jpg", src.path(1))
        assertTrue(src.isDir(0))
        assertEquals(0L, src.size(0))
        assertEquals(3L, src.size(1))
        assertEquals(-1L, src.size(2))
        assertEquals(42L, src.openFd(1))
        assertEquals(-1L, src.openFd(2))
        assertArrayEquals(byteArrayOf(1, 2, 3, 4), src.readChunk(2, 0, 256 * 1024))
        assertArrayEquals(ByteArray(0), src.readChunk(2, 4, 256 * 1024))
        val folderOpen = runCatching { src.openFd(0) }
        assertTrue(folderOpen.isFailure)
        src.close()
    }
}
