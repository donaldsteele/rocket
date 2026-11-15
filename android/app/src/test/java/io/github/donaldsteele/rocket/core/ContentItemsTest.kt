package io.github.donaldsteele.rocket.core

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class ContentItemsTest {
    private val own = "io.github.donaldsteele.rocket"

    @Test
    fun onlyOtherAppsContentUrisAreReadable() {
        assertTrue(isForeignContentUri("content", "com.android.providers.media.documents", own))
        assertTrue(isForeignContentUri("content", "media", own))
        assertTrue(isForeignContentUri("content", "10@com.example.fileprovider", own))
        // A share intent with a file:// URI would be opened with rocket's
        // own access (its node.key, for one).
        assertFalse(isForeignContentUri("file", "", own))
        assertFalse(isForeignContentUri("file", null, own))
        assertFalse(isForeignContentUri("FILE", null, own))
        assertFalse(isForeignContentUri("android.resource", own, own))
        assertFalse(isForeignContentUri(null, "media", own))
        // ContentResolver matches the scheme exactly.
        assertFalse(isForeignContentUri("Content", "media", own))
        assertFalse(isForeignContentUri("content", null, own))
        assertFalse(isForeignContentUri("content", "", own))
        // rocket's own providers (androidx-startup and the like).
        assertFalse(isForeignContentUri("content", own, own))
        assertFalse(isForeignContentUri("content", "$own.androidx-startup", own))
        assertFalse(isForeignContentUri("content", "0@$own.androidx-startup", own))
        assertFalse(isForeignContentUri("content", "IO.GitHub.DonaldSteele.Rocket.provider", own))
        // A different package that only starts with the same letters.
        assertTrue(isForeignContentUri("content", "${own}x.provider", own))
    }

    @Test
    fun authorityLosesItsUser() {
        assertEquals("media", authorityWithoutUser("10@media"))
        assertEquals("media", authorityWithoutUser("media"))
    }

    @Test
    fun privateStorageIsRecognised() {
        val roots = listOf("/data/user/0/$own", "/data/data/$own", "/data/user_de/0/$own/")
        assertTrue(isUnderAny("/data/data/$own/files/rocket/node.key", roots))
        assertTrue(isUnderAny("/data/user/0/$own/shared_prefs/rocket.xml", roots))
        assertTrue(isUnderAny("/data/user_de/0/$own", roots))
        assertTrue(isUnderAny("/data/data/other/../$own/files/rocket/trusted.json", roots))
        assertFalse(isUnderAny("/data/data/${own}x/files/a", roots))
        assertFalse(isUnderAny("/storage/emulated/0/Download/a.jpg", roots))
        assertFalse(isUnderAny("pipe:[12345]", roots))
        assertFalse(isUnderAny("/anything", listOf("/", "")))
    }
}
