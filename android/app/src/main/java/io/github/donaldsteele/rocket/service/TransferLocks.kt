package io.github.donaldsteele.rocket.service

import android.content.Context
import android.net.wifi.WifiManager
import android.os.PowerManager

/**
 * Keeps the CPU awake (a partial wake lock) and Wi-Fi out of power save (a
 * Wi-Fi lock) while transfers run. A foreground service alone does neither:
 * with the screen off the phone would suspend between packets and a long
 * transfer would crawl or time out. Each service owns one and holds it only
 * while it has transfers in flight. Main thread only.
 */
class TransferLocks(context: Context, tag: String) {
    private val app = context.applicationContext

    private val wake: PowerManager.WakeLock? = runCatching {
        app.getSystemService(PowerManager::class.java)
            ?.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "rocket:$tag")
            ?.apply { setReferenceCounted(false) }
    }.getOrNull()

    // HIGH_PERF is replaced by the low-latency lock on newer Android, which
    // is the best on offer there.
    @Suppress("DEPRECATION")
    private val wifi: WifiManager.WifiLock? = runCatching {
        app.getSystemService(WifiManager::class.java)
            ?.createWifiLock(WifiManager.WIFI_MODE_FULL_HIGH_PERF, "rocket:$tag")
            ?.apply { setReferenceCounted(false) }
    }.getOrNull()

    /**
     * Holds the locks while busy, releases them otherwise. Call again on
     * each progress update: the wake lock lapses after [TIMEOUT_MS] without
     * one, so a stuck transfer cannot keep the phone awake for ever.
     */
    fun hold(busy: Boolean) {
        if (busy) {
            runCatching { wake?.acquire(TIMEOUT_MS) }
            runCatching { wifi?.let { if (!it.isHeld) it.acquire() } }
        } else {
            release()
        }
    }

    fun release() {
        runCatching { wake?.let { if (it.isHeld) it.release() } }
        runCatching { wifi?.let { if (it.isHeld) it.release() } }
    }

    private companion object {
        const val TIMEOUT_MS = 10 * 60 * 1000L
    }
}
