package io.github.donaldsteele.rocket.core

import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test
import java.io.File
import java.nio.file.Files

class OutgoingStoreTest {
    private val dir: File = Files.createTempDirectory("rocket-outgoing").toFile()

    private class FakeGrants : GrantKeeper {
        val released = mutableListOf<String>()
        override fun take(uri: String) = true
        override fun release(uri: String) {
            released += uri
        }
    }

    @After
    fun cleanUp() {
        dir.deleteRecursively()
    }

    private val a = "00112233445566778899aabbccddeeff"
    private val b = "ffeeddccbbaa99887766554433221100"
    private val items = listOf(
        SendItem("photos", null, 0, isDir = true),
        SendItem("photos/a.jpg", "content://docs/tree/1/document/2", 1234, isDir = false),
    )

    @Test
    fun savesLoadsAndReleasesUnusedGrants() {
        val grants = FakeGrants()
        val store = OutgoingStore(dir, grants)
        store.save(SavedSend(a, items, listOf("content://docs/tree/1")))
        store.save(SavedSend(b, items.drop(1), listOf("content://docs/tree/1", "content://docs/other")))
        assertEquals(SavedSend(a, items, listOf("content://docs/tree/1")), store.load(a))
        assertNull(store.load("short"))
        assertNull(store.load("0123456789abcdef0123456789abcdef"))

        // The tree grant is still used by b.
        store.remove(a)
        assertNull(store.load(a))
        assertEquals(emptyList<String>(), grants.released)

        // A resume takes b's grants over: nothing released.
        store.remove(b, release = false)
        assertEquals(emptyList<String>(), grants.released)

        store.save(SavedSend(b, items, listOf("content://docs/tree/1", "content://docs/other")))
        store.prune(setOf(a))
        assertNull(store.load(b))
        assertEquals(listOf("content://docs/tree/1", "content://docs/other"), grants.released)
    }

    @Test
    fun ignoresBadIdsAndFiles() {
        val store = OutgoingStore(dir, FakeGrants())
        store.save(SavedSend("../../evil-0123456789", items, emptyList()))
        assertEquals(0, dir.listFiles()?.size ?: 0)
        File(dir, "$a.json").writeText("{not json")
        assertNull(store.load(a))
    }
}
