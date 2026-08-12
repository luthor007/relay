package glass.relay.bridge.link

/**
 * What the link is holding, and what it has handed to a socket.
 *
 * Split out of [RelaydLink] so the part with all the ordering rules can be
 * tested without a socket, a clock or a state machine. Every property here is a
 * decision `glasses/bridge/src/relayd.ts` argues for in prose; this is that
 * prose as a data structure.
 *
 * ## The two queues
 *
 * `pending` is waiting to go out. `inFlight` is what a socket accepted and
 * nothing has acknowledged yet — which is **not** the same as delivered. A
 * `send()` that a socket accepted may still have died in a kernel buffer when
 * the link dropped, and neither TCP nor the WebSocket layer will tell us.
 *
 * ## Replay to the head, not the tail
 *
 * [requeueInFlight] puts unacknowledged envelopes back at the *front* of
 * `pending`, oldest first. `relayd` segments episodes by time (`SYSTEM.md` §5),
 * so an utterance that comes back after the three that followed it lands in the
 * wrong conversation. Duplicates are the price of ordering, and the envelope
 * `id` is what pays it: the server dedupes on it.
 *
 * ## Refuse the newest
 *
 * At [limit] the *new* envelope is refused and the caller is told. The same rule
 * as `StoreAndForwardQueue`, for the same reason: dropping the oldest silently
 * discards the utterance the user is waiting on an answer to, and a refusal is a
 * state the UI can honestly report.
 *
 * Not thread-safe on its own; [RelaydLink] holds a lock around it.
 */
class Outbox(
    /**
     * Bounded, because an unbounded outbox is a memory leak with a good excuse.
     *
     * Counts pending *and* in-flight: a phone whose socket keeps accepting
     * frames that are never acknowledged is exactly as full as one whose socket
     * accepts nothing.
     */
    private val limit: Int = 1_000,
) {

    data class Entry(val envelope: RelayEnvelope, val text: String)

    private val pending = ArrayDeque<Entry>()
    private val inFlight = ArrayDeque<Entry>()

    val pendingCount: Int get() = pending.size

    val inFlightCount: Int get() = inFlight.size

    /** Accepted and not yet known to have landed, in the order they will go. */
    val size: Int get() = pending.size + inFlight.size

    val ids: List<String> get() = (inFlight + pending).map { it.envelope.id }

    val isFull: Boolean get() = size >= limit

    /** Returns false when the outbox is full. The caller still owns the data. */
    fun offer(entry: Entry): Boolean {
        if (isFull) return false
        pending.addLast(entry)
        return true
    }

    fun head(): Entry? = pending.firstOrNull()

    /**
     * The socket accepted the head. Move it to in-flight.
     *
     * Two steps rather than one so that a socket which *throws* leaves the entry
     * exactly where it was — at the head — and the reconnect sends it first.
     */
    fun markSent(): Entry? {
        val entry = pending.removeFirstOrNull() ?: return null
        inFlight.addLast(entry)
        return entry
    }

    /**
     * The server acknowledged one id, by `ack.re` or `error.re`.
     *
     * An `error` prunes as surely as an `ack` does: the daemon received the
     * frame, decided about it, and said so. Redelivering it would produce the
     * same refusal forever — `audio.chunk` today answers `not_implemented, M4`,
     * and a link that retried it would spend the user's battery on a loop.
     */
    fun acknowledge(id: String): Boolean = inFlight.removeAll { it.envelope.id == id }

    /**
     * Everything unacknowledged goes back to the head, oldest first.
     *
     * Returns what moved, so the caller can report how many envelopes are about
     * to be sent twice. Silence here would make duplicates look like a server
     * bug the next time somebody reads a log.
     */
    fun requeueInFlight(): List<Entry> {
        if (inFlight.isEmpty()) return emptyList()
        val returned = inFlight.toList()
        inFlight.clear()
        for (entry in returned.asReversed()) pending.addFirst(entry)
        return returned
    }
}
