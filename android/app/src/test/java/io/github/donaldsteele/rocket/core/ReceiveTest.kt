package io.github.donaldsteele.rocket.core

import io.github.donaldsteele.rocket.ui.doneLine
import io.github.donaldsteele.rocket.ui.namesLine
import io.github.donaldsteele.rocket.ui.offerWhat
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ReceiveTest {
    private val offerJSON = """{"id":"t7","transfer_id":"t7","kind":"multi","name":"photos","size":2048,"files":3,
        |"names":["photos","photos/a.jpg","photos/b.jpg"],"sender_name":"desk","sender_fp":"0123456789abcdef",
        |"from_browser":false,"addr":"192.168.1.20"}""".trimMargin()

    @Test
    fun offerParses() {
        val o = parseOffer(offerJSON)!!
        assertEquals("t7", o.id)
        assertEquals("multi", o.kind)
        assertEquals(2048L, o.size)
        assertEquals(3, o.files)
        assertEquals(listOf("photos", "photos/a.jpg", "photos/b.jpg"), o.names)
        assertEquals("desk", o.senderName)
        assertEquals("0123456789abcdef", o.senderFp)
        assertEquals("192.168.1.20", o.addr)
        assertEquals("3 files", offerWhat(o))
        assertEquals("photos, photos/a.jpg, photos/b.jpg", namesLine(o))

        val one = parseOffer("""{"id":"t8","kind":"file","name":"a.pdf","size":5,"files":1,"names":["a.pdf"],"sender_name":"","addr":"10.0.0.2"}""")!!
        assertEquals("t8", one.transferId)
        assertEquals("a.pdf", offerWhat(one))
        assertEquals("Unknown device", one.senderName)
        assertEquals("", one.senderFp)

        assertNull(parseOffer("""{"kind":"file"}"""))
        assertNull(parseOffer("not json"))
        assertEquals(listOf("t7"), parseOffers("[$offerJSON]").map { it.id })
        assertTrue(parseOffers("null").isEmpty())
    }

    @Test
    fun namesLineMarksMore() {
        val o = parseOffer("""{"id":"x","files":9,"names":["a","b","c","d","e"]}""")!!
        assertEquals("a, b, c, d, e, …", namesLine(o))
    }

    @Test
    fun offerGoneParses() {
        assertEquals(
            OfferGone("t7", "timed out"),
            parseOfferGone("""{"type":"offer_gone","data":{"transfer_id":"t7","reason":"timed out"}}"""),
        )
        assertNull(parseOfferGone("""{"type":"progress","data":{"transfer_id":"t7"}}"""))
        assertNull(parseOfferGone("""{"type":"offer_gone","data":{}}"""))
        // offer_gone is not a transfer event.
        assertNull(parseTransferEvent("""{"type":"offer_gone","data":{"transfer_id":"t7","reason":"accepted"}}"""))
    }

    @Test
    fun doneCarriesPath() {
        val ev = parseTransferEvent(
            """{"type":"done","data":{"transfer_id":"t7","name":"photos","path":"photos (1)","paths":["photos (1)"],"files":3,"bytes":2048,"from":"desk"}}""",
        )
        assertEquals(TransferEvent.Sent("t7", "photos", 3, 2048, "photos (1)"), ev)
    }

    @Test
    fun trustedAndTransfersParse() {
        val t = parseTrusted(
            """[{"fingerprint":"0123456789abcdef","name":"desk","pub":"AAAA","approved_at":"2026-11-01T10:00:00Z"},{"name":"bad"}]""",
        )
        assertEquals(1, t.size)
        assertEquals("desk", t[0].name)
        assertEquals(1793527200000L, t[0].approvedAt)

        val infos = parseTransferInfos(
            """[{"transfer_id":"t7","dir":"in","name":"photos","peer_name":"desk","files":3,"total":2048,"sent":10,"file_index":1,"state":"running","started":"2026-11-01T10:00:00Z"}]""",
        )
        assertEquals(TransferInfo("t7", "in", "photos", "desk", 3, 2048, 10), infos.single())
    }

    @Test
    fun receivesJoinTheList() {
        val info = TransferInfo("t7", "in", "photos", "desk", 3, 2048, 0)
        var list = applyIncoming(emptyList(), TransferEvent.Progress("t7", "photos", 100, 2048, "photos/a.jpg", 1, 3), info)
        val t = list.single()
        assertTrue(t.incoming)
        assertEquals("desk", t.peerName)
        assertEquals(TransferState.Sending, t.state)
        assertEquals(100L, t.sent)

        list = applyIncoming(list, TransferEvent.Sent("t7", "photos", 3, 2048, "photos"), null)
        val done = list.single()
        assertEquals(TransferState.Done, done.state)
        assertEquals("photos", done.savedTo)
        assertFalse(done.state.active)
        assertEquals("2.0 KB received, saved in Download/rocket", doneLine(done))

        // A receive the core no longer knows still shows, from its event.
        val failed = applyIncoming(list, TransferEvent.Failed("t9", "notes.txt", "disk full"), null).first()
        assertEquals("t9", failed.id)
        assertEquals("notes.txt", failed.name)
        assertEquals("another device", failed.peerName)
        assertEquals(TransferState.Failed, failed.state)

        val cancelled = applyIncoming(emptyList(), TransferEvent.Cancelled("t3", "x", "peer", "gone"), info).single()
        assertEquals("Cancelled by the sender", cancelled.error)
    }

    @Test
    fun nsdNameFitsOneLabel() {
        assertEquals("desk", nsdName("desk"))
        assertEquals(63, nsdName("a".repeat(80)).length)
        // 21 three-byte characters fit, the 22nd would make 66 bytes.
        assertEquals("€".repeat(21), nsdName("€".repeat(30)))
        // A surrogate pair is never split.
        val emoji = "🚀"
        val cut = nsdName("a".repeat(61) + emoji)
        assertEquals("a".repeat(61), cut)
        assertEquals("rocket", nsdName(""))
    }
}
