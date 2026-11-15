package io.github.donaldsteele.rocket.core

import android.content.Context
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import android.net.wifi.WifiManager
import android.os.Build
import android.os.Handler
import android.os.Looper
import io.github.donaldsteele.rocket.core.rocketcore.Node
import io.github.donaldsteele.rocket.core.rocketcore.PeerListener
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.net.Inet4Address
import java.net.InetAddress

/** The mDNS service type rocket hosts advertise (docs/protocol.md). */
const val SERVICE_TYPE = "_rocket._tcp"

/**
 * Finds other rocket hosts: the Go core listens for UDP beacons, NsdManager
 * browses mDNS, and the core merges both by fingerprint into one peer list.
 * Runs while at least one screen holds it ([acquire]/[release], main thread),
 * with a Wi-Fi multicast lock held meanwhile.
 */
class Discovery(context: Context, private val node: Node) {
    private val app = context.applicationContext
    private val main = Handler(Looper.getMainLooper())
    private val nsd = app.getSystemService(NsdManager::class.java)
    private val lock = app.getSystemService(WifiManager::class.java)
        ?.createMulticastLock("rocket-discovery")
        ?.apply { setReferenceCounted(false) }

    private val _peers = MutableStateFlow<List<Peer>>(emptyList())

    /** Live peers, freshest first. */
    val peers: StateFlow<List<Peer>> = _peers.asStateFlow()

    private val _problem = MutableStateFlow<String?>(null)

    /** Why discovery is only partly working, or null. */
    val problem: StateFlow<String?> = _problem.asStateFlow()

    private var users = 0
    private var running = false
    private var browse: NsdManager.DiscoveryListener? = null
    private val queue = ArrayDeque<NsdServiceInfo>()
    private var resolving = false

    /** Resolved mDNS sightings by service name, re-reported until lost. */
    private val sightings = HashMap<String, Sighting>()

    private data class Sighting(val name: String, val fp: String, val host: String, val port: Int, val ver: String, val os: String)

    private val peerListener = object : PeerListener {
        override fun onPeers(peersJSON: String) {
            _peers.value = parsePeers(peersJSON)
        }
    }

    // NsdManager reports a service once; the core forgets an mDNS sighting
    // after 75 s, so live ones are re-reported well inside that.
    private val refresh = object : Runnable {
        override fun run() {
            if (!running) return
            sightings.values.forEach { observe(it) }
            main.postDelayed(this, REFRESH_MS)
        }
    }

    fun acquire() {
        if (users++ == 0) start()
    }

    fun release() {
        if (users > 0 && --users == 0) stop()
    }

    private fun start() {
        running = true
        _problem.value = null
        runCatching { lock?.acquire() }
        try {
            node.startDiscovery(peerListener)
        } catch (e: Exception) {
            _problem.value = "Beacon discovery is off (${e.message}). Hosts found over mDNS still show."
        }
        startBrowse()
        main.postDelayed(refresh, REFRESH_MS)
        _peers.value = parsePeers(node.peersJSON())
    }

    private fun stop() {
        running = false
        main.removeCallbacks(refresh)
        browse?.let { l -> runCatching { nsd?.stopServiceDiscovery(l) } }
        browse = null
        queue.clear()
        // The next browse reports live services again; stale ones simply
        // expire in the core.
        sightings.clear()
        node.stopDiscovery()
        runCatching { lock?.release() }
    }

    private fun startBrowse() {
        val manager = nsd ?: return
        val l = object : NsdManager.DiscoveryListener {
            override fun onDiscoveryStarted(serviceType: String) {}
            override fun onDiscoveryStopped(serviceType: String) {}
            override fun onStartDiscoveryFailed(serviceType: String, errorCode: Int) {
                main.post {
                    if (browse === this) browse = null
                    if (running) _problem.value = "mDNS discovery could not start (error $errorCode). Beacons still work."
                }
            }
            override fun onStopDiscoveryFailed(serviceType: String, errorCode: Int) {}
            override fun onServiceFound(info: NsdServiceInfo) {
                main.post {
                    if (running && queue.none { it.serviceName == info.serviceName }) {
                        queue.addLast(info)
                        resolveNext()
                    }
                }
            }
            override fun onServiceLost(info: NsdServiceInfo) {
                main.post {
                    queue.removeAll { it.serviceName == info.serviceName }
                    sightings.remove(info.serviceName)?.let { node.forgetMDNS(it.fp) }
                }
            }
        }
        browse = l
        try {
            manager.discoverServices(SERVICE_TYPE, NsdManager.PROTOCOL_DNS_SD, l)
        } catch (e: Exception) {
            browse = null
            _problem.value = "mDNS discovery could not start (${e.message}). Beacons still work."
        }
    }

    // Before Android 14 NsdManager resolves one service at a time, so
    // resolves are queued.
    private fun resolveNext() {
        val manager = nsd ?: return
        if (resolving || !running) return
        val info = queue.removeFirstOrNull() ?: return
        resolving = true
        val l = object : NsdManager.ResolveListener {
            override fun onResolveFailed(serviceInfo: NsdServiceInfo, errorCode: Int) {
                main.post { resolving = false; resolveNext() }
            }
            override fun onServiceResolved(serviceInfo: NsdServiceInfo) {
                main.post {
                    resolving = false
                    if (running) record(serviceInfo)
                    resolveNext()
                }
            }
        }
        try {
            @Suppress("DEPRECATION")
            manager.resolveService(info, l)
        } catch (_: Exception) {
            resolving = false
            main.post { resolveNext() }
        }
    }

    private fun record(si: NsdServiceInfo) {
        val txt = si.attributes.orEmpty()
        fun attr(k: String) = txt[k]?.toString(Charsets.UTF_8).orEmpty()
        val host = preferredHost(addresses(si)) ?: return
        val s = Sighting(si.serviceName, attr("fp"), host, si.port, attr("ver"), attr("os"))
        sightings[s.name] = s
        observe(s)
    }

    private fun observe(s: Sighting) {
        node.observeMDNS(s.name, s.fp, s.host, s.port.toLong(), s.ver, s.os)
    }

    private fun addresses(si: NsdServiceInfo): List<InetAddress> =
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            si.hostAddresses
        } else {
            @Suppress("DEPRECATION")
            listOfNotNull(si.host)
        }

    private companion object {
        const val REFRESH_MS = 30_000L
    }
}

/**
 * The address to reach a host at: IPv4 first, then IPv6, link-local
 * addresses (169.254.x, fe80::) last, as docs/protocol.md asks.
 */
fun preferredHost(addrs: List<InetAddress>): String? = addrs
    .filter { !it.isLoopbackAddress && !it.isAnyLocalAddress && !it.isMulticastAddress }
    .sortedBy {
        when {
            it is Inet4Address && !it.isLinkLocalAddress -> 0
            !it.isLinkLocalAddress -> 1
            it is Inet4Address -> 2
            else -> 3
        }
    }
    .firstOrNull()
    ?.hostAddress
