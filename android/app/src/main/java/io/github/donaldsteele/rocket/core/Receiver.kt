package io.github.donaldsteele.rocket.core

import android.content.Context
import android.media.MediaScannerConnection
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import android.os.Build
import android.os.Environment
import android.os.Handler
import android.os.Looper
import android.os.ParcelFileDescriptor
import io.github.donaldsteele.rocket.core.rocketcore.Decider
import io.github.donaldsteele.rocket.core.rocketcore.EventListener
import io.github.donaldsteele.rocket.core.rocketcore.Node
import io.github.donaldsteele.rocket.core.rocketcore.Rocketcore
import io.github.donaldsteele.rocket.service.OfferNotifications
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import java.io.File
import java.util.concurrent.Executors

/** Whether this phone accepts offers. */
enum class ReceiverState { Off, Starting, On, Stopping }

/**
 * The receiving side: runs the Go core's receiver (GET /api/hello, POST
 * /api/offer and POST /api/recv only) while the person has receiving on,
 * advertises it over NSD as `<name>._rocket._tcp` (TXT fp, ver, os, proto),
 * and turns offers into the in-app sheet and a notification. Accepted files
 * go to Download/rocket; a receive whose connection is lost is kept there
 * and continued when the sender tries again. The receive service starts and stops it; starting
 * and stopping run on a background thread because stopping waits for
 * receives to wind down.
 */
class Receiver(
    context: Context,
    private val node: Node,
    private val transfers: Transfers,
) : Decider, EventListener {
    private val app = context.applicationContext
    private val main = Handler(Looper.getMainLooper())
    private val worker = Executors.newSingleThreadExecutor { r -> Thread(r, "rocket-receiver").apply { isDaemon = true } }
    private val nsd = app.getSystemService(NsdManager::class.java)
    private val claims = Claims()

    private val _state = MutableStateFlow(ReceiverState.Off)
    val state: StateFlow<ReceiverState> = _state.asStateFlow()

    private val _port = MutableStateFlow(0)

    /** The TCP port the receiver listens on, 0 while off. */
    val port: StateFlow<Int> = _port.asStateFlow()

    private val _offers = MutableStateFlow<List<IncomingOffer>>(emptyList())

    /** Offers waiting for a decision, oldest first. */
    val offers: StateFlow<List<IncomingOffer>> = _offers.asStateFlow()

    private val _problem = MutableStateFlow<String?>(null)

    /** Why receiving stopped or is only partly working, or null. */
    val problem: StateFlow<String?> = _problem.asStateFlow()

    private val _trustChanged = MutableStateFlow(0)

    /** Bumps when an answer pinned a sender, so the trusted list can reload. */
    val trustChanged: StateFlow<Int> = _trustChanged.asStateFlow()

    /** Offers seen recently, to describe their receives (by transfer ID). */
    private val seen = object : LinkedHashMap<String, IncomingOffer>() {
        override fun removeEldestEntry(eldest: MutableMap.MutableEntry<String, IncomingOffer>?) = size > 64
    }

    // NSD registration (main thread only).
    private var registration: NsdManager.RegistrationListener? = null

    /** The NSD service name actually registered (NSD may rename on a clash). */
    @Volatile
    var serviceName: String? = null
        private set

    private var sink: DownloadSink? = null

    /** What the person asked for last; the worker skips work it overtook. */
    @Volatile
    private var wanted = false

    /** Starts receiving; [onResult] runs on the main thread with null or what went wrong. */
    fun start(onResult: (String?) -> Unit = {}) {
        if (_state.value == ReceiverState.On || _state.value == ReceiverState.Starting) {
            onResult(null)
            return
        }
        wanted = true
        _state.value = ReceiverState.Starting
        _problem.value = null
        worker.execute {
            if (!wanted) return@execute
            val err = try {
                val s = sink ?: makeSink().also { sink = it }
                val port = node.startReceiver(Rocketcore.DefaultPort, this, s, this).toInt()
                _port.value = port
                // A stop asked for meanwhile is queued next and ends it.
                if (wanted) {
                    _state.value = ReceiverState.On
                    main.post { advertise(port) }
                }
                null
            } catch (e: Exception) {
                _state.value = ReceiverState.Off
                _port.value = 0
                (e.message ?: e.toString()).also { _problem.value = "Receiving could not start: $it" }
            }
            main.post { onResult(err) }
        }
    }

    /** Stops receiving: waiting offers are declined, receives in flight cancelled. */
    fun stop() {
        if (_state.value == ReceiverState.Off || _state.value == ReceiverState.Stopping) return
        wanted = false
        _state.value = ReceiverState.Stopping
        main.post { unadvertise() }
        worker.execute {
            node.stopReceiver()
            dropAllOffers()
            _port.value = 0
            // A start asked for meanwhile runs next and sets the state.
            if (!wanted) _state.value = ReceiverState.Off
        }
    }

    /**
     * Restarts a running receiver so it announces a new name, unless that
     * would cut off an offer or a receive. Reports whether it restarted.
     */
    fun restartIfIdle(): Boolean {
        if (_state.value != ReceiverState.On) return false
        if (_offers.value.isNotEmpty() || transfers.receiving()) return false
        _state.value = ReceiverState.Starting
        main.post { unadvertise() }
        worker.execute {
            node.stopReceiver()
            dropAllOffers()
            if (!wanted) return@execute
            try {
                val port = node.startReceiver(Rocketcore.DefaultPort, this, sink ?: makeSink().also { sink = it }, this).toInt()
                _port.value = port
                if (wanted) {
                    _state.value = ReceiverState.On
                    main.post { advertise(port) }
                }
            } catch (e: Exception) {
                _port.value = 0
                _state.value = ReceiverState.Off
                _problem.value = "Receiving could not restart: ${e.message ?: e}"
            }
        }
        return true
    }

    /**
     * Answers offer id: accept or decline, and with trust also pin the
     * sender's key. Reports false when the offer no longer waits.
     */
    fun answer(id: String, accept: Boolean, trust: Boolean): Boolean {
        val ok = node.answer(id, accept, accept && trust)
        removeOffer(id)
        if (ok && accept && trust) _trustChanged.update { it + 1 }
        return ok
    }

    /** Where received files go, for display. */
    val where: String get() = sink?.where ?: "${Environment.DIRECTORY_DOWNLOADS}/$DOWNLOAD_FOLDER"

    // --- Go callbacks (Go threads; they must return at once) ---

    // Neither callback has an error result, so nothing may be thrown back
    // into Go (goCallback). An offer whose notification fails still shows
    // in the app.
    override fun ask(offerJSON: String) = goCallback("ask", Unit) {
        val offer = parseOffer(offerJSON) ?: return@goCallback
        synchronized(seen) { seen[offer.transferId] = offer }
        _offers.update { l -> if (l.any { it.id == offer.id }) l else l + offer }
        goCallback("offer notification", Unit) { OfferNotifications.showOffer(app, offer) }
    }

    override fun onEvent(eventJSON: String) = goCallback("receive event", Unit) { handleEvent(eventJSON) }

    private fun handleEvent(eventJSON: String) {
        parseOfferGone(eventJSON)?.let { gone ->
            removeOffer(gone.transferId)
            return
        }
        if (isKeptEvent(eventJSON)) {
            // The attempt that completed a kept receive has its own card.
            keptCompleted(eventJSON)?.let { transfers.dropInterrupted(it) }
            transfers.reloadResumable()
            return
        }
        val ev = parseTransferEvent(eventJSON) ?: return
        val known = transfers.list.value.any { it.id == ev.transferId }
        val info = if (known) null else describe(ev.transferId)
        transfers.onIncoming(ev, info)
        if (ev !is TransferEvent.Progress) {
            sink?.finished(ev.transferId)
            if (ev is TransferEvent.Sent) {
                transfers.list.value.firstOrNull { it.id == ev.transferId }?.let { OfferNotifications.showReceived(app, it, where) }
            }
        }
    }

    /** What the core (or the offer) says about receive id. */
    private fun describe(id: String): TransferInfo? {
        parseTransferInfos(node.transfersJSON()).firstOrNull { it.id == id }?.let { return it }
        val o = synchronized(seen) { seen[id] } ?: return null
        val label = o.name.substringBefore('/')
        return TransferInfo(id = id, dir = "in", name = label, peerName = o.senderName, files = o.files, total = o.size, sent = 0)
    }

    private fun removeOffer(id: String) {
        _offers.update { l -> l.filterNot { it.id == id } }
        OfferNotifications.cancelOffer(app, id)
    }

    private fun dropAllOffers() {
        val gone = _offers.value
        _offers.value = emptyList()
        gone.forEach { OfferNotifications.cancelOffer(app, it.id) }
    }

    private fun makeSink(): DownloadSink =
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            MediaStoreSink(app.contentResolver, claims)
        } else {
            @Suppress("DEPRECATION")
            val downloads = Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DOWNLOADS)
            FileSink(
                root = File(downloads, DOWNLOAD_FOLDER),
                openFd = { f ->
                    ParcelFileDescriptor.open(f, ParcelFileDescriptor.MODE_WRITE_ONLY or ParcelFileDescriptor.MODE_TRUNCATE).use { it.detachFd() }
                },
                onSaved = { f -> MediaScannerConnection.scanFile(app, arrayOf(f.path), null, null) },
                claims = claims,
                where = "${Environment.DIRECTORY_DOWNLOADS}/$DOWNLOAD_FOLDER",
                openRw = { f -> ParcelFileDescriptor.open(f, ParcelFileDescriptor.MODE_READ_WRITE).use { it.detachFd() } },
            )
        }

    // --- NSD advertising (main thread) ---

    private fun advertise(port: Int) {
        val manager = nsd ?: return
        if (registration != null || !wanted || _state.value != ReceiverState.On) return
        val info = NsdServiceInfo().apply {
            serviceName = nsdName(node.name())
            serviceType = SERVICE_TYPE
            setPort(port)
            setAttribute("fp", node.fingerprint())
            setAttribute("ver", node.version())
            setAttribute("os", Rocketcore.OS)
            setAttribute("proto", Rocketcore.ProtoVersion.toString())
        }
        val l = object : NsdManager.RegistrationListener {
            override fun onServiceRegistered(si: NsdServiceInfo) {
                serviceName = si.serviceName
            }
            override fun onRegistrationFailed(si: NsdServiceInfo, errorCode: Int) {
                main.post {
                    if (registration === this) registration = null
                    serviceName = null
                    if (_state.value == ReceiverState.On) {
                        _problem.value = "mDNS advertising failed (error $errorCode). Others still find this phone by its beacon."
                    }
                }
            }
            override fun onServiceUnregistered(si: NsdServiceInfo) {}
            override fun onUnregistrationFailed(si: NsdServiceInfo, errorCode: Int) {}
        }
        registration = l
        serviceName = info.serviceName
        try {
            manager.registerService(info, NsdManager.PROTOCOL_DNS_SD, l)
        } catch (e: Exception) {
            registration = null
            serviceName = null
            _problem.value = "mDNS advertising failed (${e.message}). Others still find this phone by its beacon."
        }
    }

    private fun unadvertise() {
        val l = registration ?: return
        registration = null
        serviceName = null
        runCatching { nsd?.unregisterService(l) }
    }
}

/** A device name cut to fit one DNS label (63 bytes of UTF-8), whole characters only. */
fun nsdName(name: String): String {
    val out = StringBuilder()
    var bytes = 0
    var i = 0
    while (i < name.length) {
        val cp = name.codePointAt(i)
        val n = String(Character.toChars(cp)).toByteArray(Charsets.UTF_8).size
        if (bytes + n > 63) break
        out.appendCodePoint(cp)
        bytes += n
        i += Character.charCount(cp)
    }
    return out.toString().ifEmpty { "rocket" }
}
