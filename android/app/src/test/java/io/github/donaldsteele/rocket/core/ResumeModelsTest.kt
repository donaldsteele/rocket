package io.github.donaldsteele.rocket.core

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** The 1.1 additions to the core models: protocol, encryption, retries and resume. */
class ResumeModelsTest {
    @Test
    fun peersCarryTheirProtocol() {
        val peers = parsePeers(
            """[{"name":"new","fp":"0123456789abcdef","proto":2},{"name":"old","fp":"fedcba9876543210"},
                {"name":"odd","fp":"1111111111111111","proto":0}]""",
        )
        assertEquals(listOf(2, 1, 1), peers.map { it.proto })
        assertTrue(peers[0].canSend(2))
        assertFalse(peers[1].canSend(2))
        assertFalse(Peer("n", "2222222222222222", "", "", "", emptyList(), proto = 3).canSend(2))
    }

    @Test
    fun historySaysEncrypted() {
        val h = parseHistory(
            """[{"id":"t1","dir":"out","result":"interrupted","encrypted":true},{"id":"t2","dir":"in","result":"ok"}]""",
        )
        assertTrue(h[0].encrypted)
        assertEquals("interrupted", h[0].result)
        assertFalse(h[1].encrypted)
    }

    @Test
    fun retryAndInterruptedEvents() {
        assertEquals(
            TransferEvent.Retrying("t1", "clip.mp4", 2, 5, 5000, "connection reset"),
            parseTransferEvent(
                """{"type":"retrying","data":{"transfer_id":"t1","name":"clip.mp4","dir":"out","attempt":2,"attempts":5,"delay_ms":5000,"error":"connection reset"}}""",
            ),
        )
        assertEquals(
            TransferEvent.Failed("t1", "clip.mp4", "transfer interrupted: gone", interrupted = true),
            parseTransferEvent(
                """{"type":"error","data":{"transfer_id":"t1","name":"clip.mp4","dir":"out","result":"interrupted","error":"transfer interrupted: gone"}}""",
            ),
        )
        assertTrue(isKeptEvent("""{"type":"kept","data":{"id":"k1"}}"""))
        assertTrue(isKeptEvent("""{"type":"kept_gone","data":{"id":"k1","reason":"expired"}}"""))
        assertFalse(isKeptEvent("""{"type":"progress","data":{}}"""))
        assertFalse(isKeptEvent("nope"))
        assertEquals("k1", keptCompleted("""{"type":"kept_gone","data":{"id":"k1","reason":"completed"}}"""))
        assertEquals(null, keptCompleted("""{"type":"kept_gone","data":{"id":"k1","reason":"expired"}}"""))
    }

    @Test
    fun resumableParses() {
        val r = parseResumable(
            """{"outgoing":[{"transfer_id":"00112233445566778899aabbccddeeff","name":"clip.mp4","peer_name":"desk",
                "peer_fp":"0123456789abcdef","addr":"10.0.2.2:29005","files":1,"bytes":4096,"error":"lost",
                "expires":"2026-11-16T10:00:00Z"},{"name":"no id"}],
               "incoming":[{"id":"ffeeddccbbaa99887766554433221100","name":"album","sender_name":"","sender_fp":"fedcba9876543210",
                "files":3,"total":300,"done":120,"location":"Download/rocket","kept_at":"2026-11-15T10:00:00Z",
                "expires":"2026-11-16T10:00:00Z"}]}""",
        )
        assertEquals(1, r.outgoing.size)
        val o = r.outgoing[0]
        assertEquals("clip.mp4", o.name)
        assertEquals("desk", o.peerName)
        assertEquals(4096L, o.bytes)
        assertEquals(1794823200000L, o.expires)
        val k = r.incoming.single()
        assertEquals("another device", k.senderName)
        assertEquals(120L, k.done)
        assertEquals(300L, k.total)
        assertFalse(r.isEmpty)
        assertTrue(parseResumable("""{"outgoing":[],"incoming":[]}""").isEmpty)
        assertTrue(parseResumable("bad").isEmpty)
    }

    @Test
    fun transferStatesForRetries() {
        val t = Transfer(
            id = "t1", peerName = "desk", name = "clip.mp4", sent = 40, total = 100, file = "", fileIndex = 1,
            files = 1, state = TransferState.Sending, error = "", cancellable = true,
        )
        val r = t.withEvent(TransferEvent.Retrying("t1", "", 1, 5, 2000, "reset"))
        assertEquals(TransferState.Reconnecting, r.state)
        assertTrue(r.state.active)
        assertEquals(1, r.attempt)
        assertEquals(5, r.attempts)
        assertEquals("reset", r.error)
        // Bytes moving again: sending once more.
        assertEquals(TransferState.Sending, r.withEvent(TransferEvent.Progress("t1", "", 50, 100, "", 1, 1)).state)
        val gaveUp = r.withEvent(TransferEvent.Failed("t1", "", "transfer interrupted", interrupted = true))
        assertEquals(TransferState.Interrupted, gaveUp.state)
        assertFalse(gaveUp.state.active)
        // A late retry event does not bring an ended send back.
        assertEquals(TransferState.Interrupted, gaveUp.withEvent(TransferEvent.Retrying("t1", "", 2, 5, 5000, "x")).state)
        assertEquals(TransferState.Failed, t.withEvent(TransferEvent.Failed("t1", "", "declined")).state)
    }
}
