package io.github.donaldsteele.rocket.ui

import io.github.donaldsteele.rocket.core.Transfer
import io.github.donaldsteele.rocket.core.TransferState
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class FormatTest {
    @Test
    fun sizesMatchDesktop() {
        assertEquals("0 B", formatSize(0))
        assertEquals("512 B", formatSize(512))
        assertEquals("1.5 KB", formatSize(1536))
        assertEquals("12.3 MB", formatSize((12.3 * 1024 * 1024).toLong()))
        assertEquals("2.0 TB", formatSize(2L shl 40))
        assertEquals("2048.0 TB", formatSize(2L shl 50))
    }

    @Test
    fun words() {
        assertEquals("photos (12 files)", countWhat("photos", 12))
        assertEquals("a.txt", countWhat("a.txt", 1))
        assertEquals(0, percent(1, 0))
        assertEquals(50, percent(50, 100))
        assertEquals(100, percent(150, 100))
        assertEquals("0123 · windows · via beacon + mdns", peerMeta("0123", "windows", listOf("beacon", "mdns")))
        assertEquals("0123", peerMeta("0123", "", emptyList()))
    }

    @Test
    fun addresses() {
        assertEquals("192.168.1.2:29005", parseAddress(" 192.168.1.2 "))
        assertEquals("192.168.1.2:29400", parseAddress("192.168.1.2:29400"))
        assertEquals("desk.local:29005", parseAddress("desk.local"))
        assertEquals("[fe80::1]:29005", parseAddress("fe80::1"))
        assertEquals("[fe80::1]:8080", parseAddress("[fe80::1]:8080"))
        assertNull(parseAddress(""))
        assertNull(parseAddress("host:0"))
        assertNull(parseAddress("host:70000"))
        assertNull(parseAddress("host:abc"))
        assertNull(parseAddress("http://host"))
        assertNull(parseAddress("two words"))
        assertNull(parseAddress("[fe80::1"))
    }

    @Test
    fun progressLines() {
        val t = Transfer(
            id = "t", peerName = "desk", name = "photos", sent = 1024, total = 4096, file = "photos/b.jpg",
            fileIndex = 2, files = 5, state = TransferState.Sending, error = "", cancellable = true,
        )
        assertEquals("1.0 KB of 4.0 KB · 2 of 5: photos/b.jpg", progressLine(t))
        assertEquals("1.0 KB of 4.0 KB", progressLine(t.copy(files = 1)))
        assertEquals("Waiting for desk to accept", progressLine(t.copy(state = TransferState.Waiting)))
        assertEquals("Failed", resultLook("error").second)
        assertEquals("No answer", resultLook("timeout").second)
    }
}
