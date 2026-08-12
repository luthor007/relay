package glass.relay.bridge.connector

import java.io.DataInputStream
import java.io.DataOutputStream
import java.io.File
import java.io.FileOutputStream
import java.io.IOException

/**
 * Durable backing for [StoreAndForwardQueue].
 *
 * `APPS-SCOPE.md` §4.5 — a crash or restart must not lose the day's capture —
 * which makes persistence part of the contract rather than an optimisation.
 * §4.2 names this and delivered-id memory as the two things the Kotlin queue
 * owed the TypeScript one; this is that debt paid.
 *
 * **[append] must not return until the bytes are durable.** A caller may delete
 * its source the moment it does — that is the whole point, because the source is
 * a file on the glasses and the glasses are where free space runs out. Returning
 * early turns a crash into silent data loss, which is the exact case this
 * exists to prevent.
 *
 * Blocking on purpose, and therefore to be called off the main thread. An
 * `fsync` that suspends is an `fsync` someone will forget to await.
 */
interface QueueStore {
    fun load(): Restored
    fun append(record: StoredRecord)
    fun remove(id: String)
    fun markDelivered(id: String)

    data class Restored(
        val pending: List<StoredRecord> = emptyList(),
        val delivered: List<String> = emptyList(),
    )
}

/**
 * A queued session on disk.
 *
 * [sequence] is monotonic and is what preserves order — not the position in a
 * list, which a directory listing does not have.
 */
data class StoredRecord(
    val manifest: ConnectorClient.SessionManifest,
    val body: ByteArray,
    val enqueuedAtMs: Long,
    val sequence: Long,
) {
    val id: String get() = manifest.sessionId

    override fun equals(other: Any?): Boolean =
        this === other || (
            other is StoredRecord &&
                manifest == other.manifest &&
                enqueuedAtMs == other.enqueuedAtMs &&
                sequence == other.sequence &&
                body.contentEquals(other.body)
            )

    override fun hashCode(): Int {
        var result = manifest.hashCode()
        result = 31 * result + enqueuedAtMs.hashCode()
        result = 31 * result + sequence.hashCode()
        result = 31 * result + body.contentHashCode()
        return result
    }
}

/** The test double, and the simulator's store. Loses everything, which is the point. */
class MemoryQueueStore(
    /** Simulate a full or failing disk. Enqueue must surface it, not swallow it. */
    var appendFails: Boolean = false,
) : QueueStore {

    private val pending = LinkedHashMap<String, StoredRecord>()
    private val delivered = mutableListOf<String>()

    override fun load() = QueueStore.Restored(
        pending = pending.values.sortedBy { it.sequence },
        delivered = delivered.toList(),
    )

    override fun append(record: StoredRecord) {
        if (appendFails) throw IOException("store: append failed")
        pending[record.id] = record.copy(body = record.body.copyOf())
    }

    override fun remove(id: String) {
        pending.remove(id)
    }

    override fun markDelivered(id: String) {
        delivered += id
    }

    val persistedIds: List<String> get() = pending.keys.toList()
}

/**
 * One file per record plus an append-only delivered log, under a directory.
 *
 * On Android that directory is `Context.filesDir/queue`; in a unit test it is a
 * temporary folder, which is why this class knows nothing about `Context`.
 *
 * Two details that matter more than the format:
 *
 *  - **Write to `.tmp`, fsync, then rename.** `File.renameTo` within one
 *    filesystem is atomic, so a crash leaves either the old state or the new
 *    one, never a half-written record that decodes into a truncated day.
 *  - **The delivered log is append-only and trimmed on load.** Rewriting it in
 *    place would put the one piece of replay-protection we have at risk during
 *    the write.
 *
 * The remaining honest gap: the *directory* entry is not fsynced, because Java
 * has no portable way to do it. A power cut in the millisecond after the rename
 * can therefore still lose the newest record. That is a smaller window than the
 * one this closes, and it is recorded here rather than assumed away.
 */
class FileQueueStore(private val directory: File) : QueueStore {

    private val deliveredLog = File(directory, DELIVERED_LOG)

    init {
        if (!directory.exists() && !directory.mkdirs() && !directory.isDirectory) {
            throw IOException("could not create queue directory: $directory")
        }
    }

    override fun load(): QueueStore.Restored {
        val records = directory.listFiles { file -> file.name.endsWith(RECORD_SUFFIX) }
            ?.mapNotNull { readRecord(it) }
            ?.sortedBy { it.sequence }
            ?: emptyList()

        val delivered = if (deliveredLog.exists()) {
            deliveredLog.readLines().filter { it.isNotBlank() }.map { decodeName(it) }
        } else {
            emptyList()
        }

        return QueueStore.Restored(records, delivered)
    }

    override fun append(record: StoredRecord) {
        val target = File(directory, encodeName(record.id) + RECORD_SUFFIX)
        val temporary = File(directory, encodeName(record.id) + ".tmp")

        FileOutputStream(temporary).use { raw ->
            // Deliberately not `use` on the wrapper: closing a DataOutputStream
            // closes the FileOutputStream under it, and `fd.sync()` on a closed
            // descriptor throws. Flush the wrapper, sync the descriptor, and let
            // the outer `use` do the closing.
            DataOutputStream(raw.buffered()).let { out ->
                out.writeInt(MAGIC)
                out.writeLong(record.sequence)
                out.writeLong(record.enqueuedAtMs)
                out.writeUTF(record.manifest.sessionId)
                out.writeUTF(record.manifest.kind)
                out.writeLong(record.manifest.startedAtMs)
                out.writeInt(record.manifest.durationS)
                out.writeLong(record.manifest.totalBytes)
                out.writeInt(record.manifest.chunkBytes)
                out.writeUTF(record.manifest.encoding)
                out.writeUTF(record.manifest.sourceName)
                out.writeInt(record.body.size)
                out.write(record.body)
                out.flush()
            }
            // Durability, not tidiness: without this the bytes are in the page
            // cache and a caller that deletes its source has just lost the day.
            raw.fd.sync()
        }

        if (!temporary.renameTo(target)) {
            temporary.delete()
            throw IOException("could not commit queue record ${record.id}")
        }
    }

    override fun remove(id: String) {
        File(directory, encodeName(id) + RECORD_SUFFIX).delete()
    }

    override fun markDelivered(id: String) {
        FileOutputStream(deliveredLog, true).use { raw ->
            raw.write((encodeName(id) + "\n").toByteArray(Charsets.UTF_8))
            raw.flush()
            raw.fd.sync()
        }
    }

    /** Drop everything older than the newest [keep] delivered ids. */
    fun trimDelivered(keep: Int) {
        if (!deliveredLog.exists()) return
        val lines = deliveredLog.readLines().filter { it.isNotBlank() }
        if (lines.size <= keep) return
        val trimmed = lines.takeLast(keep)
        val temporary = File(directory, "$DELIVERED_LOG.tmp")
        temporary.writeText(trimmed.joinToString("\n", postfix = "\n"))
        if (!temporary.renameTo(deliveredLog)) temporary.delete()
    }

    private fun readRecord(file: File): StoredRecord? = try {
        DataInputStream(file.inputStream().buffered()).use { input ->
            if (input.readInt() != MAGIC) {
                null
            } else {
                val sequence = input.readLong()
                val enqueuedAtMs = input.readLong()
                val manifest = ConnectorClient.SessionManifest(
                    sessionId = input.readUTF(),
                    kind = input.readUTF(),
                    startedAtMs = input.readLong(),
                    durationS = input.readInt(),
                    totalBytes = input.readLong(),
                    chunkBytes = input.readInt(),
                    encoding = input.readUTF(),
                    sourceName = input.readUTF(),
                )
                val declared = input.readInt()
                // A corrupt length must not become a multi-gigabyte allocation
                // on a phone. Anything larger than the file cannot be real.
                if (declared < 0 || declared > file.length()) throw IOException("bad record length")
                val body = ByteArray(declared)
                input.readFully(body)
                StoredRecord(manifest, body, enqueuedAtMs, sequence)
            }
        }
    } catch (_: IOException) {
        // A truncated record is one the rename never committed. Skipping it is
        // correct: the glasses still hold the source, so it will be pulled again.
        null
    }

    private companion object {
        /** "RLQ1". Bumping this is how a format change refuses to misread old files. */
        const val MAGIC = 0x524C5131
        const val RECORD_SUFFIX = ".rec"
        const val DELIVERED_LOG = "delivered.log"

        /**
         * Session ids come from device filenames and are not guaranteed to be
         * safe as path components. Hex is boring, reversible and cannot escape
         * the directory.
         */
        fun encodeName(id: String): String =
            id.toByteArray(Charsets.UTF_8).joinToString("") { "%02x".format(it) }

        fun decodeName(hex: String): String {
            val bytes = ByteArray(hex.length / 2) {
                ((Character.digit(hex[it * 2], 16) shl 4) or Character.digit(hex[it * 2 + 1], 16)).toByte()
            }
            return String(bytes, Charsets.UTF_8)
        }
    }
}
