package io.github.donaldsteele.rocket.core

import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import java.io.File
import java.nio.file.Files

class DownloadSinkTest {
    private val root: File = Files.createTempDirectory("rocket-sink").toFile()

    @After
    fun cleanUp() {
        root.deleteRecursively()
    }

    private fun tree(): List<String> =
        root.walkTopDown().filter { it != root }.map { it.relativeTo(root).invariantSeparatorsPath }.sorted().toList()

    @Test
    fun numbersLikeDesktop() {
        assertEquals("photos", numberedName("photos", 0))
        assertEquals("photos (1)", numberedName("photos", 1))
        assertEquals("a (2).txt", numberedName("a.txt", 2))
        assertEquals("archive.tar (1).gz", numberedName("archive.tar.gz", 1))
        // As Go's filepath.Ext: a leading dot is an extension too.
        assertEquals(" (1).bashrc", numberedName(".bashrc", 1))
    }

    @Test
    fun likeEscapes() {
        assertEquals("100\\% a\\_b", likeEscape("100% a_b"))
    }

    @Test
    fun singleFileThroughWrite() {
        val sink = FileSink(root)
        val tx = sink.begin("t1", "{}")
        val f = tx.create("a.txt", 5)
        assertEquals(-1L, f.fd())
        f.write("he".toByteArray())
        f.write("llo".toByteArray())
        // Still a part file until committed.
        assertEquals(listOf("a.txt.rocket-part"), tree())
        val loc = f.commit()
        assertEquals(File(root, "a.txt").path, loc)
        assertEquals("hello", File(root, "a.txt").readText())
        assertEquals(listOf("a.txt"), tree())
    }

    @Test
    fun takenNamesGetNumbers() {
        File(root, "a.txt").writeText("old")
        File(root, "photos").mkdirs()
        File(root, "photos/keep.txt").writeText("keep")
        val sink = FileSink(root)
        val tx = sink.begin("t1", "{}")
        tx.create("a.txt", 3).apply { write("new".toByteArray()) }.commit()
        val top = tx.mkdir("photos")
        assertEquals("photos (1)", top)
        tx.mkdir("$top/sub")
        tx.create("$top/sub/b.jpg", 1).apply { write(byteArrayOf(1)) }.commit()
        assertEquals("old", File(root, "a.txt").readText())
        assertEquals("new", File(root, "a (1).txt").readText())
        assertEquals("keep", File(root, "photos/keep.txt").readText())
        assertTrue(File(root, "photos (1)/sub/b.jpg").isFile)
    }

    @Test
    fun concurrentTransfersNeverShareAFolder() {
        val claims = Claims()
        val sink = FileSink(root, claims = claims)
        val a = sink.begin("t1", "{}")
        val b = sink.begin("t2", "{}")
        assertEquals("photos", a.mkdir("photos"))
        assertEquals("photos (1)", b.mkdir("photos"))
        // A finished transfer's folder exists, so the name stays taken.
        sink.finished("t1")
        assertEquals("photos (2)", sink.begin("t3", "{}").mkdir("photos"))
    }

    @Test
    fun abortRemovesEverythingItWrote() {
        File(root, "existing.txt").writeText("x")
        val sink = FileSink(root)
        val tx = sink.begin("t1", "{}")
        val top = tx.mkdir("photos")
        tx.mkdir("$top/empty")
        tx.create("$top/a.jpg", 1).apply { write(byteArrayOf(1)) }.commit()
        val half = tx.create("$top/deep/b.jpg", 10)
        half.write(byteArrayOf(1, 2))
        tx.abort()
        assertEquals(listOf("existing.txt"), tree())
    }

    @Test
    fun abortedFileLeavesNothing() {
        val tx = FileSink(root).begin("t1", "{}")
        val f = tx.create("a.bin", 4)
        f.write(byteArrayOf(1, 2))
        f.abort()
        assertEquals(emptyList<String>(), tree())
    }

    @Test
    fun descriptorIsUsedWhenGiven() {
        val opened = mutableListOf<String>()
        val saved = mutableListOf<String>()
        val sink = FileSink(root, openFd = { opened += it.name; 42 }, onSaved = { saved += it.name })
        val f = sink.begin("t1", "{}").create("a.txt", 0)
        assertEquals(42L, f.fd())
        f.commit()
        assertEquals(listOf("a.txt.rocket-part"), opened)
        assertEquals(listOf("a.txt"), saved)
    }

    @Test
    fun missingRootIsMade() {
        val nested = File(root, "Download/rocket")
        val sink = FileSink(nested, where = "Download/rocket")
        sink.begin("t1", "{}").create("x", 0).commit()
        assertTrue(File(nested, "x").isFile)
        assertEquals("Download/rocket", sink.begin("t2", "{}").location())
    }

    @Test
    fun unusableRootFails() {
        val file = File(root, "plain").apply { writeText("") }
        try {
            FileSink(File(file, "sub")).begin("t1", "{}")
            fail("expected an error")
        } catch (_: Exception) {
        }
        assertFalse(File(file, "sub").exists())
    }
}
