package glass.relay.bridge.connector

/**
 * Holds sessions while the box is unreachable — the subway and aeroplane case
 * from `docs/APPS-SCOPE.md` §4.2.
 *
 * Mirrors `StoreAndForwardQueue` in connector/src/client.ts and
 * `glasses/bridge/src/queue.ts`. The four semantics all three share are
 * idempotent enqueue, refuse-newest, FIFO, and stop-at-first-failure; the
 * TypeScript suite ports this file's tests verbatim so the implementations
 * cannot drift silently. Two queues that disagree is a bug that only shows up
 * on a bad network, which is the worst possible place to find one.
 *
 * ## The eviction policy, stated
 *
 * | Situation | What happens | Why |
 * |---|---|---|
 * | Queue full | the **newest** record is refused, with a reason | dropping the oldest silently loses last Tuesday; a refusal is a state the UI can honestly report |
 * | Record larger than the whole capacity | refused as [Refusal.TooLarge] at once | it will never fit, and retrying it forever blocks everything behind it |
 * | More records than [capacityItems] | refused as [Refusal.ItemLimit] | bytes are not the only bound: an index, a directory and a restore pass all cost per *record*, so a million one-byte records is a different failure from one large one |
 * | Same id twice | accepted, not duplicated | a retry after a failed flush must not double-upload |
 * | Id already delivered | accepted as a no-op | replaying a sync must not re-upload the day |
 * | Flush hits an error | stops there, order intact | skipping past a stuck record reorders someone's day, and the box segments episodes by time |
 * | The disk says no | refused as [Refusal.StoreFailed] | the caller still owns its bytes and must not delete its source |
 *
 * Nothing is ever dropped silently.
 *
 * ## Durability
 *
 * Added here to pay the debt `APPS-SCOPE.md` §4.2 records. [enqueue] does not
 * return until [QueueStore.append] has made the bytes durable, so a caller may
 * delete its source the moment it does; [restore] reloads what a previous run
 * left behind, in its original order. That makes the app being killed mid-day
 * cost nothing but the record in flight, which is re-pulled because the glasses
 * still have it.
 *
 * Blocking, and therefore to be driven from `Dispatchers.IO`. The default store
 * is in-memory, so the existing callers and tests behave exactly as before.
 */
class StoreAndForwardQueue(
    /**
     * Hard cap on what the phone will hold for an unreachable box.
     *
     * Unbounded is not an option: the phone fills up, the OS starts evicting,
     * and what gets evicted is the only copy of a conversation. Evicting the
     * *oldest* would be worse than useless for a memory product — it silently
     * loses last Tuesday — so a full queue refuses new work instead, which is a
     * state the UI can honestly report.
     */
    private val capacityBytes: Long = 2L * 1024 * 1024 * 1024,

    /**
     * Hard cap on the *number* of pending records, separate from the byte cap.
     *
     * Parity with `glasses/bridge/src/queue.ts` and `apps/ios/RelayKit/Queue.swift`,
     * which both have it. The default is effectively unbounded, so this changes
     * no existing caller's behaviour — but the bound has to be *expressible*,
     * because the two limits fail differently: bytes bound what the disk holds,
     * items bound what the index, the directory listing and the restore pass
     * cost. A queue that accepts a million one-byte records is within its byte
     * budget and still takes minutes to reload after a crash.
     */
    private val capacityItems: Int = Int.MAX_VALUE,

    private val store: QueueStore = MemoryQueueStore(),

    /**
     * Ids remembered after delivery, for replay dedupe.
     *
     * ~40 files a day, so 1024 is weeks of cover. 0 disables it.
     */
    private val deliveredMemory: Int = 1024,

    private val clock: () -> Long = System::currentTimeMillis,
) {

    data class Queued(
        val manifest: ConnectorClient.SessionManifest,
        val data: ByteArray,
    ) {
        // ByteArray in a data class needs these, or equality is identity-based
        // and the dedupe below silently stops working.
        override fun equals(other: Any?): Boolean =
            this === other ||
                (other is Queued && manifest == other.manifest && data.contentEquals(other.data))

        override fun hashCode(): Int = 31 * manifest.hashCode() + data.contentHashCode()
    }

    data class FlushResult(val sent: Int, val remaining: Int, val error: Throwable? = null)

    /**
     * Why a record was not taken.
     *
     * The same four names, spelled the same way, as `QueueRefusal` in
     * `glasses/bridge/src/queue.ts` and `QueueRefusal` in
     * `apps/ios/RelayKit/Queue.swift`. A refusal a UI cannot render identically
     * on two platforms is a refusal that gets rendered as "something went
     * wrong".
     */
    enum class Refusal {
        /** Capacity reached. The newest record is the one refused. */
        Full,

        /** More records than [capacityItems]. */
        ItemLimit,

        /** Bigger than the entire capacity — it can never fit, so say so now. */
        TooLarge,

        /** The disk said no. Never swallowed. */
        StoreFailed,
    }

    data class EnqueueResult(
        val accepted: Boolean,
        /** Already pending under this id — accepted, not duplicated. */
        val duplicate: Boolean = false,
        /** Delivered in a previous run — accepted as a no-op. */
        val alreadyDelivered: Boolean = false,
        val reason: Refusal? = null,
        val message: String? = null,
    )

    private val pending = ArrayDeque<StoredRecord>()
    private val delivered = ArrayDeque<String>()
    private val deliveredSet = mutableSetOf<String>()
    private var sequence = 0L

    val size: Int get() = pending.size

    val usedBytes: Long get() = pending.sumOf { it.body.size.toLong() }

    val freeBytes: Long get() = maxOf(0, capacityBytes - usedBytes)

    val sessionIds: List<String> get() = pending.map { it.id }

    val deliveredIds: List<String> get() = delivered.toList()

    fun has(sessionId: String): Boolean = pending.any { it.id == sessionId }

    /**
     * Reload whatever the last run left behind.
     *
     * A record that is both pending and delivered is the crash window inside
     * [flush]; it is resolved in favour of delivered, because the alternative
     * re-uploads a day every time the app restarts.
     */
    fun restore() {
        val restored = store.load()
        delivered.clear()
        deliveredSet.clear()
        restored.delivered.takeLast(maxOf(deliveredMemory, 0)).forEach {
            delivered.addLast(it)
            deliveredSet += it
        }

        pending.clear()
        restored.pending
            .filter { it.id !in deliveredSet }
            .sortedBy { it.sequence }
            .forEach { pending.addLast(it) }

        sequence = restored.pending.maxOfOrNull { it.sequence + 1 } ?: 0
    }

    /** Idempotent: re-queuing after a failed flush must not duplicate. */
    fun enqueue(session: Queued): Boolean = offer(session).accepted

    /** [enqueue] with the reason attached, for a UI that has to explain itself. */
    fun offer(session: Queued): EnqueueResult {
        val id = session.manifest.sessionId
        if (id in deliveredSet) return EnqueueResult(accepted = true, alreadyDelivered = true)
        if (has(id)) return EnqueueResult(accepted = true, duplicate = true)

        val size = session.data.size.toLong()
        if (size > capacityBytes) {
            return EnqueueResult(
                accepted = false,
                reason = Refusal.TooLarge,
                message = "record is $size bytes and the queue holds $capacityBytes",
            )
        }
        if (usedBytes + size > capacityBytes) {
            return EnqueueResult(
                accepted = false,
                reason = Refusal.Full,
                message = "queue full: $usedBytes + $size exceeds $capacityBytes",
            )
        }
        // After the byte checks and before the store, exactly as the TypeScript
        // and Swift order them. The precedence is part of the contract: a record
        // that is both over the byte cap and over the item cap must report the
        // same reason on all three platforms, or the three UIs disagree about
        // what the user should do next.
        if (pending.size + 1 > capacityItems) {
            return EnqueueResult(
                accepted = false,
                reason = Refusal.ItemLimit,
                message = "queue full: ${pending.size} records is the limit",
            )
        }

        val record = StoredRecord(
            manifest = session.manifest,
            body = session.data,
            enqueuedAtMs = clock(),
            sequence = sequence++,
        )

        try {
            store.append(record)
        } catch (error: Exception) {
            // The caller still owns its bytes. Saying "accepted" here is how a
            // day of audio gets deleted off the glasses and never arrives.
            return EnqueueResult(
                accepted = false,
                reason = Refusal.StoreFailed,
                message = error.message ?: error.toString(),
            )
        }

        pending.addLast(record)
        return EnqueueResult(accepted = true)
    }

    /**
     * Try to send everything, oldest first, stopping at the first failure.
     *
     * Skipping past a stuck session would quietly reorder someone's day, and the
     * box segments episodes by time — so order is not cosmetic here.
     */
    suspend fun flush(
        client: ConnectorClient,
        onProgress: (ConnectorClient.UploadProgress) -> Unit = {},
    ): FlushResult = flushWith { record ->
        client.upload(record.manifest, record.body, onProgress)
    }

    /**
     * The same loop against any sender.
     *
     * A separate name rather than an overload: two `flush`es that differ only in
     * whether the last parameter is a lambda is exactly the kind of resolution
     * puzzle that gets solved wrongly at a call site.
     */
    suspend fun flushWith(send: suspend (StoredRecord) -> Unit): FlushResult {
        var sent = 0
        while (pending.isNotEmpty()) {
            val next = pending.first()
            try {
                send(next)
            } catch (e: Exception) {
                return FlushResult(sent, pending.size, e)
            }
            // Mark delivered *before* removing. A crash between the two leaves a
            // record that is both pending and delivered, which restore() resolves
            // in favour of delivered; the other order loses that and re-uploads.
            remember(next.id)
            store.remove(next.id)
            pending.removeFirst()
            sent += 1
        }
        return FlushResult(sent, 0)
    }

    private fun remember(id: String) {
        if (deliveredMemory <= 0) return
        delivered.addLast(id)
        deliveredSet += id
        while (delivered.size > deliveredMemory) {
            val dropped = delivered.removeFirst()
            deliveredSet -= dropped
        }
        store.markDelivered(id)
    }
}
