package io.github.donaldsteele.rocket.ui

import io.github.donaldsteele.rocket.core.InterruptedSend
import io.github.donaldsteele.rocket.core.KeptReceive
import io.github.donaldsteele.rocket.core.Peer
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
    fun navLabelsStopGrowingAtTheCap() {
        assertEquals(1f, navLabelFactor(1f), 0f)
        assertEquals(1f, navLabelFactor(0.85f), 0f)
        assertEquals(1f, navLabelFactor(NAV_LABEL_MAX_SCALE), 0f)
        assertEquals(NAV_LABEL_MAX_SCALE, 2f * navLabelFactor(2f), 1e-6f)
        assertEquals(NAV_LABEL_MAX_SCALE, 1.5f * navLabelFactor(1.5f), 1e-6f)
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

    @Test
    fun wordsForProtocolAndRetries() {
        val old = Peer("Den", "0123456789abcdef", "", "windows", "1.0.0", emptyList(), proto = 1)
        assertEquals("Runs rocket 1.0.0 (protocol 1). Update it to rocket 1.1 or later to send to it.", peerUpdateText(old, 2))
        assertEquals(
            "Runs an older rocket (protocol 1). Update it to rocket 1.1 or later to send to it.",
            peerUpdateText(old.copy(version = ""), 2),
        )
        assertEquals(
            "Runs a newer rocket (protocol 3). Update rocket on this phone to send to it.",
            peerUpdateText(old.copy(proto = 3), 2),
        )
        assertEquals("Reconnecting 2/5", reconnectingWord(2, 5))
        assertEquals("Reconnecting, attempt 2 of 5", reconnectingSpoken(2, 5))
        assertEquals("Reconnecting", reconnectingWord(0, 0))
        assertEquals(
            "Connection lost: the other device does not answer. rocket tries again by itself and continues where it stopped.",
            reconnectingLine("Get \"http://10.0.2.2:29610/api/hello\": dial tcp 10.0.2.2:29610: connect: connection refused"),
        )
        assertEquals("the connection was cut", networkWhy("write tcp: wsasend: An existing connection was forcibly closed by the remote host."))
        assertEquals("nothing moved for a while", networkWhy("i/o timeout"))
        assertEquals("", networkWhy("something odd"))
        assertEquals("Connection lost. rocket tries again by itself and continues where it stopped.", reconnectingLine(""))
        assertEquals("Interrupted", resultLook("interrupted").second)
        assertEquals(false, showSecurity("declined"))
        assertEquals(true, showSecurity("interrupted"))
        val k = KeptReceive("k", "album", "desk", "", 3, 1000, 420, "", 0)
        assertEquals("42% · 420 B of 1000 B", keptMeta(k))
        val s = InterruptedSend("t", "clip.mp4", "desk", "", 1, 2048, "", 0)
        assertEquals("2.0 KB", interruptedMeta(s))
    }
}
