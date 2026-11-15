package io.github.donaldsteele.rocket.core

import io.github.donaldsteele.rocket.core.rocketcore.Node
import io.github.donaldsteele.rocket.core.rocketcore.ProgressListener
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicLong

/** Where a send goes: an address and the fingerprint expected there. */
data class SendTarget(
    /** host:port, or "" to let the core look [fp] up among discovered peers. */
    val addr: String,
    /** The peer's full fingerprint, or "" for an unverified host:port send. */
    val fp: String,
    /** What to call the peer on screen. */
    val name: String,
)

enum class TransferState(val active: Boolean) {
    /** Offered; the receiver has not decided yet. */
    Waiting(true),
    Sending(true),
    Done(false),
    Failed(false),
    Cancelled(false),
}

/** One send as the Transfers screen shows it. */
data class Transfer(
    val id: String,
    val peerName: String,
    val name: String,
    val sent: Long,
    val total: Long,
    val file: String,
    /** 1-based index of the file in flight. */
    val fileIndex: Int,
    val files: Int,
    val state: TransferState,
    val error: String,
    /** False for a send that never got a transfer ID (it failed to start). */
    val cancellable: Boolean,
    /** A receive (another device sends to this phone) rather than a send. */
    val incoming: Boolean = false,
    /** Where a finished receive was saved (a content URI or folder name). */
    val savedTo: String = "",
)

/** Applies one event to a transfer. */
fun Transfer.withEvent(ev: TransferEvent): Transfer {
    val n = ev.name.ifEmpty { name }
    return when (ev) {
        is TransferEvent.Progress -> copy(
            name = n,
            sent = ev.sent,
            total = ev.total,
            file = ev.file,
            fileIndex = ev.fileIndex,
            files = if (ev.files > 0) ev.files else files,
            state = if (state.active) TransferState.Sending else state,
        )
        is TransferEvent.Sent -> copy(
            name = n,
            sent = maxOf(sent, ev.bytes),
            total = maxOf(total, ev.bytes),
            state = TransferState.Done,
            savedTo = ev.path.ifEmpty { savedTo },
        )
        is TransferEvent.Failed -> copy(name = n, state = TransferState.Failed, error = ev.error)
        is TransferEvent.Cancelled -> copy(
            name = n,
            state = TransferState.Cancelled,
            error = when {
                ev.by != "peer" -> ""
                incoming -> "Cancelled by the sender"
                else -> "Cancelled by the receiver"
            },
        )
    }
}

/**
 * Applies an event of a receive to the transfer list. A receive not in the
 * list yet goes on top, described by [info] when the core still has it.
 */
fun applyIncoming(list: List<Transfer>, ev: TransferEvent, info: TransferInfo?): List<Transfer> {
    val i = list.indexOfFirst { it.id == ev.transferId }
    if (i >= 0) return list.toMutableList().also { it[i] = it[i].withEvent(ev) }
    val fresh = Transfer(
        id = ev.transferId,
        peerName = info?.peerName?.ifEmpty { null } ?: "another device",
        name = info?.name?.ifEmpty { null } ?: ev.name,
        sent = info?.sent ?: 0,
        total = info?.total ?: 0,
        file = "",
        fileIndex = 0,
        files = info?.files ?: 0,
        state = TransferState.Sending,
        error = "",
        cancellable = true,
        incoming = true,
    )
    return listOf(fresh.withEvent(ev)) + list
}

/**
 * The transfers of this app session: sends, and receives the
 * [Receiver] reports through [onIncoming]. A send is queued here ([enqueue]), started
 * by the transfer service ([start]) so it keeps running in the background,
 * and followed through the Go core's events.
 */
class Transfers(private val node: Node, private val opener: ItemOpener) {
    private class Job(val target: SendTarget, val items: List<SendItem>)

    private val jobs = ConcurrentHashMap<Long, Job>()
    private val nextJob = AtomicLong()
    private val nextLocal = AtomicLong()
    private val executor = Executors.newSingleThreadExecutor { r -> Thread(r, "rocket-send").apply { isDaemon = true } }

    private val _list = MutableStateFlow<List<Transfer>>(emptyList())

    /** Sends and receives of this session, newest first. */
    val list: StateFlow<List<Transfer>> = _list.asStateFlow()

    private val _starting = MutableStateFlow(0)

    /** Sends queued but not yet handed to the core. */
    val starting: StateFlow<Int> = _starting.asStateFlow()

    private val _messages = MutableSharedFlow<String>(extraBufferCapacity = 16)

    /** One-line problems worth telling the person (a send that could not start). */
    val messages: SharedFlow<String> = _messages.asSharedFlow()

    private val _ended = MutableStateFlow(0)

    /** Bumps whenever a transfer ends, so history can reload. */
    val ended: StateFlow<Int> = _ended.asStateFlow()

    /** Queues a send; pass the returned job ID to [start]. */
    fun enqueue(target: SendTarget, items: List<SendItem>): Long {
        val id = nextJob.incrementAndGet()
        jobs[id] = Job(target, items)
        _starting.update { it + 1 }
        return id
    }

    /**
     * Hands queued job [jobId] to the core. The core builds and checks the
     * manifest (which may read every file's size) before the transfer runs,
     * so this happens on a background thread.
     */
    fun start(jobId: Long) {
        val job = jobs.remove(jobId) ?: return
        executor.execute {
            val source = GoSource(job.items, opener)
            val listener = Listener(job.target, summarize(job.items), source)
            try {
                val tid = node.send(job.target.addr, job.target.fp, source, listener)
                listener.started(tid)
            } catch (e: Exception) {
                source.close()
                val why = e.message ?: e.toString()
                _messages.tryEmit("Could not send to ${job.target.name}: $why")
                val s = summarize(job.items)
                _list.update {
                    listOf(
                        Transfer(
                            id = "local-${nextLocal.incrementAndGet()}", peerName = job.target.name, name = s.label,
                            sent = 0, total = s.bytes, file = "", fileIndex = 0, files = s.files,
                            state = TransferState.Failed, error = why, cancellable = false,
                        ),
                    ) + it
                }
                _ended.update { it + 1 }
            } finally {
                _starting.update { it - 1 }
            }
        }
    }

    /** Drops a queued job that will never start (its service start failed). */
    fun forget(jobId: Long) {
        if (jobs.remove(jobId) != null) _starting.update { it - 1 }
    }

    fun cancel(id: String): Boolean = node.cancel(id)

    /** Cancels every send in flight (receives are left alone). */
    fun cancelAll() {
        _list.value.filter { it.state.active && it.cancellable && !it.incoming }.forEach { node.cancel(it.id) }
    }

    /** Records an event of a receive; info describes a receive seen for the first time. */
    fun onIncoming(ev: TransferEvent, info: TransferInfo?) {
        _list.update { applyIncoming(it, ev, info) }
        if (ev !is TransferEvent.Progress) _ended.update { it + 1 }
    }

    /** Whether a receive is in flight. */
    fun receiving(): Boolean = _list.value.any { it.incoming && it.state.active }

    /** Removes every send that has ended from the list. */
    fun clearFinished() {
        _list.update { l -> l.filter { it.state.active } }
    }

    private inner class Listener(
        private val target: SendTarget,
        private val summary: SendSummary,
        private val source: GoSource,
    ) : ProgressListener {
        private fun fresh(tid: String) = Transfer(
            id = tid, peerName = target.name, name = summary.label, sent = 0, total = summary.bytes,
            file = "", fileIndex = 0, files = summary.files, state = TransferState.Waiting, error = "",
            cancellable = true,
        )

        /** The core accepted the send; events may already have arrived. */
        fun started(tid: String) {
            _list.update { l -> if (l.any { it.id == tid }) l else listOf(fresh(tid)) + l }
        }

        override fun onEvent(eventJSON: String) {
            val ev = parseTransferEvent(eventJSON) ?: return
            val tid = ev.transferId
            _list.update { l ->
                val i = l.indexOfFirst { it.id == tid }
                if (i < 0) listOf(fresh(tid).withEvent(ev)) + l else l.toMutableList().also { it[i] = it[i].withEvent(ev) }
            }
            if (ev !is TransferEvent.Progress) {
                source.close()
                _ended.update { it + 1 }
            }
        }
    }
}
