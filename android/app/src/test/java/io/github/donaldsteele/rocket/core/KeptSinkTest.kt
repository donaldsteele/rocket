package io.github.donaldsteele.rocket.core

import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import java.io.File
import java.nio.file.Files

/** Keeping and continuing a broken receive in [FileSink] (Android 8 and 9, and the shape MediaStoreSink follows). */
class KeptSinkTest {
    private val root: File = Files.createTempDirectory("rocket-kept").toFile()

    @After
    fun cleanUp() {
        root.deleteRecursively()
    }

    private fun tree(): List<String> =
        root.walkTopDown().filter { it != root }.map { it.relativeTo(root).invariantSeparatorsPath }.sorted().toList()

    @Test
    fun keepLeavesEverythingAndResumeContinues() {
        val sink = FileSink(root)
        val tx = sink.begin("t1", "{}")
        val top = tx.mkdir("album")
        tx.create("$top/a.jpg", 2).apply { write("ab".toByteArray()) }.commit()
        val half = tx.create("$top/b.jpg", 6)
        half.write("012".toByteArray())
        val ref = half.ref()
        assertTrue(ref.endsWith("b.jpg.rocket-part"))
        val state = tx.keep()
        assertEquals(listOf("album", "album/a.jpg", "album/b.jpg.rocket-part"), tree())
        sink.check(state)

        // The next attempt reopens the part file and finishes it.
        val tx2 = sink.resume("t2", "{}", state)
        val f = tx2.reopen(ref)
        f.write("345".toByteArray())
        f.commit()
        assertEquals(listOf("album", "album/a.jpg", "album/b.jpg"), tree())
        assertEquals("012345", File(root, "album/b.jpg").readText())
        try {
            tx2.reopen(File(root, "album/a.jpg").path)
            fail("reopened a file that is not a kept part")
        } catch (_: Exception) {
        }
    }

    @Test
    fun resumedAbortAndDiscardRemoveWhatWasKept() {
        File(root, "mine.txt").writeText("x")
        val sink = FileSink(root)
        val tx = sink.begin("t1", "{}")
        tx.create("clip.mp4", 10).apply { write("01234".toByteArray()) }
        val state = tx.keep()
        assertEquals(listOf("clip.mp4.rocket-part", "mine.txt"), tree())
        sink.resume("t2", "{}", state).abort()
        assertEquals(listOf("mine.txt"), tree())

        val tx3 = sink.begin("t3", "{}")
        val top = tx3.mkdir("album")
        tx3.create("$top/a.jpg", 4).apply { write("ab".toByteArray()) }
        val kept = tx3.keep()
        File(root, "album/a.jpg.rocket-part").delete()
        try {
            sink.check(kept)
            fail("check passed with the part file gone")
        } catch (_: Exception) {
        }
        sink.discard(kept)
        assertEquals(listOf("mine.txt"), tree())
    }

    @Test
    fun keptPathsMustBeInTheFolder() {
        val sink = FileSink(root)
        val outside = KeptFiles(dirs = emptyList(), files = listOf(File(root.parentFile, "elsewhere.txt").path), parts = emptyList()).json()
        try {
            sink.check(outside)
            fail("accepted a kept path outside the folder")
        } catch (_: Exception) {
        }
        try {
            sink.resume("t1", "{}", outside)
            fail("resumed a kept path outside the folder")
        } catch (_: Exception) {
        }
        // discard has no error result: it must not throw, and must not touch the file.
        val victim = File(root.parentFile, "elsewhere-${System.nanoTime()}.txt").apply { writeText("keep") }
        try {
            sink.discard(KeptFiles(emptyList(), listOf(victim.path), emptyList()).json())
            assertTrue(victim.exists())
        } finally {
            victim.delete()
        }
    }
}
