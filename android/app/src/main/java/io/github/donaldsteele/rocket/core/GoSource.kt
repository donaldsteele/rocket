package io.github.donaldsteele.rocket.core

import io.github.donaldsteele.rocket.core.rocketcore.Source

/**
 * The Go core's view of what to send. File content crosses as detached file
 * descriptors the core takes over; when a provider has none, the core reads
 * through [readChunk] instead. Called on Go threads.
 */
class GoSource(
    private val items: List<SendItem>,
    private val opener: ItemOpener,
) : Source {
    private val chunks = ChunkReader(opener)

    override fun count(): Long = items.size.toLong()

    override fun path(i: Long): String = item(i).path

    override fun size(i: Long): Long = if (item(i).isDir) 0 else item(i).size

    override fun isDir(i: Long): Boolean = item(i).isDir

    override fun openFd(i: Long): Long {
        val ref = item(i).ref ?: throw IllegalArgumentException("${item(i).path} is a folder")
        return opener.openFd(ref).toLong()
    }

    override fun readChunk(i: Long, offset: Long, max: Long): ByteArray {
        val ref = item(i).ref ?: throw IllegalArgumentException("${item(i).path} is a folder")
        return chunks.read(i.toInt(), ref, offset, max.coerceAtMost(Int.MAX_VALUE.toLong()).toInt())
    }

    /** Releases the fallback streams once the transfer has ended. */
    fun close() = chunks.close()

    private fun item(i: Long): SendItem = items[i.toInt()]
}
