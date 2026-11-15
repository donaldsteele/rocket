package io.github.donaldsteele.rocket.core

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.net.InetAddress

class TransfersTest {
    private val fresh = Transfer(
        id = "t1", peerName = "desk", name = "photos", sent = 0, total = 100, file = "", fileIndex = 0,
        files = 3, state = TransferState.Waiting, error = "", cancellable = true,
    )

    @Test
    fun progressThenSent() {
        val p = fresh.withEvent(TransferEvent.Progress("t1", "photos", 40, 100, "photos/a.jpg", 1, 3))
        assertEquals(TransferState.Sending, p.state)
        assertEquals(40L, p.sent)
        assertEquals("photos/a.jpg", p.file)
        assertTrue(p.state.active)
        val done = p.withEvent(TransferEvent.Sent("t1", "photos", 3, 100))
        assertEquals(TransferState.Done, done.state)
        assertEquals(100L, done.sent)
        assertFalse(done.state.active)
        // A late progress event does not bring a finished send back.
        assertEquals(TransferState.Done, done.withEvent(TransferEvent.Progress("t1", "", 90, 100, "", 3, 3)).state)
    }

    @Test
    fun failedAndCancelled() {
        val f = fresh.withEvent(TransferEvent.Failed("t1", "", "declined"))
        assertEquals(TransferState.Failed, f.state)
        assertEquals("declined", f.error)
        assertEquals("photos", f.name)
        val byPeer = fresh.withEvent(TransferEvent.Cancelled("t1", "photos", "peer", "x"))
        assertEquals(TransferState.Cancelled, byPeer.state)
        assertEquals("Cancelled by the receiver", byPeer.error)
        assertEquals("", fresh.withEvent(TransferEvent.Cancelled("t1", "photos", "local", "x")).error)
    }

    @Test
    fun hostPreference() {
        fun a(s: String) = InetAddress.getByName(s)
        assertEquals("192.168.1.9", preferredHost(listOf(a("fe80::1"), a("169.254.3.4"), a("2001:db8::5"), a("192.168.1.9"))))
        assertEquals("2001:db8:0:0:0:0:0:5", preferredHost(listOf(a("fe80::1"), a("169.254.3.4"), a("2001:db8::5"))))
        assertEquals("169.254.3.4", preferredHost(listOf(a("fe80::1"), a("169.254.3.4"))))
        assertEquals(null, preferredHost(listOf(a("127.0.0.1"))))
        assertEquals(null, preferredHost(emptyList()))
    }
}
