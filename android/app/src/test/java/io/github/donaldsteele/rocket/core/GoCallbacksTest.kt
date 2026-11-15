package io.github.donaldsteele.rocket.core

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Test
import java.io.InputStream

class GoCallbacksTest {
    @Test
    fun aThrowingBodyGivesTheFallback() {
        assertEquals(7, goCallback("x", 0) { 7 })
        assertEquals(-1L, goCallback("x", -1L) { throw IllegalStateException("boom") })
        assertEquals("", goCallback<String>("x", "") { throw OutOfMemoryError("big") })
    }

    // count, path, size and isDir have no error result in the Go binding:
    // they must not throw into Go, even for an index the list lacks.
    @Test
    fun sourceCallbacksNeverThrow() {
        val opener = object : ItemOpener {
            override fun openFd(ref: String): Int = -1
            override fun openStream(ref: String): InputStream = throw UnsupportedOperationException()
        }
        val src = GoSource(listOf(SendItem("a.txt", "content://x/1", 3, isDir = false)), opener)
        assertEquals(1L, src.count())
        assertEquals("a.txt", src.path(0))
        assertEquals("", src.path(5))
        assertEquals(-1L, src.size(5))
        assertFalse(src.isDir(-1))
    }
}
