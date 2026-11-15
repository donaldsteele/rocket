package io.github.donaldsteele.rocket.core

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ModelsTest {
    @Test
    fun peersParseDesktopShape() {
        val json = """[{"name":"desk","fp":"0123456789abcdef","addr":"192.168.1.5:29005","os":"windows","version":"1.0.0","via":["beacon","mdns"]},
            {"name":"","fp":"fedcba9876543210","addr":"10.0.0.2:29005","os":"android","version":"1.0.0","via":null},
            {"name":"nofp","fp":""}]"""
        val peers = parsePeers(json)
        assertEquals(2, peers.size)
        assertEquals(Peer("desk", "0123456789abcdef", "192.168.1.5:29005", "windows", "1.0.0", listOf("beacon", "mdns")), peers[0])
        // A nameless peer falls back to its fingerprint; a null via is empty.
        assertEquals("fedcba9876543210", peers[1].name)
        assertEquals(emptyList<String>(), peers[1].via)
    }

    @Test
    fun badJsonIsEmpty() {
        assertEquals(emptyList<Peer>(), parsePeers("null"))
        assertEquals(emptyList<Peer>(), parsePeers("{"))
        assertEquals(emptyList<HistoryEntry>(), parseHistory(""))
        assertNull(parseTransferEvent("nope"))
    }

    @Test
    fun historyParsesGoTimes() {
        val json = """[{"id":"t1","dir":"out","name":"photos","peer_name":"desk","peer_fp":"0123456789abcdef",
            "files":12,"bytes":4096,"started":"2026-11-14T10:00:00.123456789Z","ended":"2026-11-14T11:00:00+01:00",
            "result":"ok"},
            {"id":"t2","dir":"in","name":"a.txt","peer_name":"x","files":1,"bytes":1,
            "started":"0001-01-01T00:00:00Z","ended":"bad","result":"declined","error":"declined"}]"""
        val h = parseHistory(json)
        assertEquals(2, h.size)
        assertEquals("photos", h[0].name)
        assertEquals(12, h[0].files)
        assertEquals(4096L, h[0].bytes)
        assertEquals(1794650400123L, h[0].started)
        assertEquals(1794650400000L, h[0].ended)
        assertEquals("0123456789abcdef", h[0].peerFp)
        assertEquals(0L, h[1].started)
        assertEquals(0L, h[1].ended)
        assertEquals("declined", h[1].result)
        assertEquals("", h[1].peerFp)
    }

    @Test
    fun eventsParse() {
        val p = parseTransferEvent(
            """{"type":"progress","data":{"transfer_id":"t1","name":"photos","sent":10,"total":100,"dir":"out","file":"photos/a.jpg","file_index":2,"files":5}}""",
        )
        assertEquals(TransferEvent.Progress("t1", "photos", 10, 100, "photos/a.jpg", 2, 5), p)
        assertEquals(
            TransferEvent.Sent("t1", "photos", 5, 100),
            parseTransferEvent("""{"type":"sent","data":{"transfer_id":"t1","name":"photos","to":"h:1","files":5,"bytes":100}}"""),
        )
        assertEquals(
            TransferEvent.Failed("t1", "photos", "declined"),
            parseTransferEvent("""{"type":"error","data":{"transfer_id":"t1","name":"photos","dir":"out","result":"declined","error":"declined"}}"""),
        )
        assertEquals(
            TransferEvent.Cancelled("t1", "", "peer", "cancelled by the receiver"),
            parseTransferEvent("""{"type":"cancelled","data":{"transfer_id":"t1","dir":"out","by":"peer","error":"cancelled by the receiver"}}"""),
        )
        // Offers and events without a transfer are not send events.
        assertNull(parseTransferEvent("""{"type":"offer","data":{"transfer_id":"t1"}}"""))
        assertNull(parseTransferEvent("""{"type":"progress","data":{"sent":1}}"""))
        assertTrue(parseTransferEvent("""{"type":"error","data":{"transfer_id":"t9"}}""") is TransferEvent.Failed)
    }
}
