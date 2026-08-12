package glass.relay.bridge.link

import java.util.Random
import java.util.concurrent.locks.ReentrantLock
import kotlin.concurrent.withLock

/**
 * The phone ↔ `relayd` link, in Kotlin.
 *
 * `docs/SYSTEM.md` §6.1: one authenticated WebSocket, JSON envelopes, both
 * directions. Ported from `glasses/bridge/src/relayd.ts` — read that first, it
 * is the reference implementation — and checked frame by frame against
 * `relayd/internal/api/wire.go`, which is the thing that actually answers.
 *
 * Until this existed, Android had **no channel to the box at all**: the only
 * network code in the module was `ConnectorClient`, which is bulk HTTP upload.
 * That made the whole of §6.1 unimplemented on this platform — no `speak`, no
 * `ui.render`, no `session.list`, no `confirm.request` — which is
 * `ORCHESTRATOR.md` §5's jobs 1 and 3, i.e. most of the product. iOS had
 * `RelaydLink.swift`; Android had nothing.
 *
 * ## Sleep/wake is the normal path
 *
 * §6.1's reason for choosing a socket — "a WebSocket survives a phone that
 * sleeps and wakes far more gracefully than a stream that has to be
 * re-established with state" — is the single most load-bearing sentence for this
 * file. A phone in a pocket loses this link dozens of times a day: screen off,
 * cell handover, WiFi to LTE, Doze, and the phone deliberately joining the
 * glasses' access point for a bulk sync, which costs it the uplink outright
 * (`APPS-SCOPE.md` §3.1).
 *
 * So three properties, all about the gap:
 *
 *   reconnect   exponential backoff with jitter, and an immediate retry when the
 *               OS says the network is back rather than waiting out the timer
 *   resume      queued work survives the gap, in order
 *   never drop  an envelope handed to [send] during a reconnect goes out later,
 *               and one that cannot be accepted is refused out loud
 *
 * ## At-least-once, without inventing a message type
 *
 * Anything sent on a connection that then closed goes back to the *head* of the
 * outbox and is sent again — see [Outbox]. That makes delivery at-least-once and
 * duplicates possible, which is what the envelope `id` is for and why `relayd`
 * dedupes on it.
 *
 * `ack` prunes what the link is holding; `error` prunes too, because the daemon
 * received the frame and decided about it. Unknown inbound types are surfaced
 * and never fatal: this link has to tolerate a server newer than itself.
 *
 * ## Threading
 *
 * All state sits behind one reentrant lock, so a socket callback on a reader
 * thread and a [send] from the capture service cannot interleave. Listener
 * callbacks run **while the lock is held** and the lock is reentrant, so a
 * listener may call [send] — the router does exactly that when it answers a
 * `confirm.request`. A listener that blocks on another thread which then calls
 * into the link will deadlock; do not do that.
 *
 * ## What this deliberately does not do
 *
 * `relayd.ts` also seals every envelope with `SealedChannel` when the route is
 * the rendezvous relay (`SYSTEM.md` §7). There is no Kotlin port of `pairing.ts`
 * yet, so there is nothing here to seal with, and inventing one would be
 * hand-rolled cryptography in the one place `APPS-SCOPE.md` §4.3 explicitly says
 * not to hand-roll it. The seam for it is [RelaySocketFactory]: sealing wraps
 * the socket, not the state machine.
 */
class RelaydLink(
    /**
     * Every address this box can be reached at, in the order to try them.
     *
     * Built by [endpoints]. More than one is the normal case for a box with the
     * relay turned on: the LAN address first, the relayed one second, and the
     * two alternate on every reconnect — see [open] for why alternating rather
     * than sticking is the correct behaviour.
     */
    private val addresses: List<BoxEndpoint>,
    private val auth: LinkAuth,
    private val socketFactory: RelaySocketFactory,
    private val scheduler: LinkScheduler,
    private val random: Random = Random(),
    private val backoff: BackoffOptions = BackoffOptions(),
    outboxLimit: Int = 1_000,
    /**
     * Close and reconnect if nothing arrives for this long. Default 0 (off).
     *
     * Liveness belongs in WebSocket ping/pong (RFC 6455 §5.5.2) and
     * [JvmWebSocket] answers pings beneath this layer. The knob exists for the
     * case that layer cannot see: a TCP connection that a carrier NAT has
     * silently forgotten, where writes succeed into a void.
     */
    private val idleTimeoutMs: Long = 0,
) {

    init {
        // A link with nowhere to dial would retry forever against an empty list
        // and divide by zero doing it. The caller's job is to not build one —
        // `RelayCaptureService.openLink` already declines when there is no box —
        // so this is the assertion of that contract rather than a fallback.
        require(addresses.isNotEmpty()) { "a link needs at least one address" }
    }

    /**
     * One address, which is what a box on a LAN with no relay has.
     *
     * Kept because it is the honest signature for that case and because every
     * test that predates the relay is written against it. It is not a
     * convenience for production: a box with the relay configured must be built
     * from [endpoints], or the phone silently loses the ability to reach it from
     * outside the house — which is the whole defect this constructor's absence
     * would have hidden.
     */
    constructor(
        url: String,
        auth: LinkAuth,
        socketFactory: RelaySocketFactory,
        scheduler: LinkScheduler,
        random: Random = Random(),
        backoff: BackoffOptions = BackoffOptions(),
        outboxLimit: Int = 1_000,
        idleTimeoutMs: Long = 0,
    ) : this(
        listOf(BoxEndpoint(url, BoxRoute.Direct)),
        auth,
        socketFactory,
        scheduler,
        random,
        backoff,
        outboxLimit,
        idleTimeoutMs,
    )

    enum class State {
        Idle,
        Connecting,
        Open,

        /** Waiting out a backoff. Work still queues. */
        Reconnecting,

        /** Deliberately shut. Only [connect] reopens it. */
        Closed,
    }

    /**
     * Everything the link tells the outside world.
     *
     * Defaults on every method so a caller implements only what it uses, and so
     * adding one later does not break the two implementations that exist.
     */
    interface Listener {
        fun onState(state: State) {}

        /** Every inbound envelope, including [ServerFrame.ACK] and errors. */
        fun onFrame(envelope: RelayEnvelope) {}

        /** A type this build does not know. Forward compatibility, not an error. */
        fun onUnknownFrame(envelope: RelayEnvelope) {}

        /** Bad inbound frame, refused outbox, dead socket. Never thrown. */
        fun onError(error: LinkException) {}

        /** Envelopes put back after a close, and therefore sent twice. */
        fun onRedelivered(ids: List<String>) {}

        fun onSent(envelope: RelayEnvelope) {}
    }

    private val lock = ReentrantLock()
    private val outbox = Outbox(outboxLimit)
    private val listeners = mutableListOf<Listener>()

    private var state: State = State.Idle
    private var socket: RelaySocket? = null
    private var attempt = 0

    /**
     * Which address the current or most recent attempt used.
     *
     * Read by the UI, and it should be: "connected through the relay" and
     * "connected on your network" are different facts about latency, about whose
     * bandwidth is being spent, and about whether the box is actually at home.
     */
    private var route: BoxRoute = BoxRoute.Direct

    /**
     * Which address the next attempt will use.
     *
     * Separate from [attempt], which counts backoff. Conflating them is off by
     * one and subtly so: `scheduleRetry` increments the backoff counter *before*
     * the retry runs, so an address chosen from it would skip the front of the
     * list on the first reconnect after a successful connection — which is
     * exactly the moment a phone that has come home should try the LAN again.
     */
    private var addressIndex = 0
    private var retryTimer: Cancellable? = null
    private var idleTimer: Cancellable? = null

    val currentState: State get() = lock.withLock { state }

    /** Accepted and not yet acknowledged, including anything in flight. */
    val pending: Int get() = lock.withLock { outbox.size }

    val pendingIds: List<String> get() = lock.withLock { outbox.ids }

    /** How many reconnects have been attempted since the last open socket. */
    val attempts: Int get() = lock.withLock { attempt }

    /** How the box is being reached, for anything that shows the user. */
    val currentRoute: BoxRoute get() = lock.withLock { route }

    fun addListener(listener: Listener) = lock.withLock { listeners.add(listener) }

    fun removeListener(listener: Listener) = lock.withLock { listeners.remove(listener) }

    // --- lifecycle ------------------------------------------------------------

    fun connect() = lock.withLock {
        if (state == State.Connecting || state == State.Open) return@withLock
        open()
    }

    /**
     * Deliberate shutdown.
     *
     * Anything queued stays queued: the app being closed is not a reason to
     * throw away an utterance the user already said. Even a clean close can lose
     * what is still in the socket's buffer, so in-flight work goes back to the
     * outbox and waits for the next [connect].
     */
    fun close() = lock.withLock {
        cancelTimers()
        val doomed = socket
        socket = null
        requeueInFlight()
        setState(State.Closed)
        doomed?.close(1000, "client closed")
    }

    /**
     * The OS says the network is back, or the app came to the foreground.
     *
     * Collapses the backoff instead of waiting it out. Without this a phone that
     * wakes into good WiFi sits idle for up to `maxMs` for no reason, which is
     * the whole sleep/wake case behaving badly. A deliberately [close]d link
     * stays closed — waking is not a way to reopen something the user shut.
     */
    fun wake() = lock.withLock {
        if (state == State.Open || state == State.Connecting || state == State.Closed) {
            return@withLock
        }
        retryTimer?.cancel()
        retryTimer = null
        attempt = 0
        open()
    }

    // --- sending --------------------------------------------------------------

    /**
     * Queue an envelope.
     *
     * Returns its id, which is also the server's dedupe key, or null when the
     * outbox refused it. Never throws for a closed or reconnecting link — that
     * is the *normal* state of a phone, and a caller that has to check first is
     * a caller that will forget.
     */
    fun send(type: String, payload: Any?): String? = lock.withLock {
        val envelope = RelayEnvelope(
            id = newEnvelopeId(random),
            type = type,
            atMs = scheduler.now(),
            payload = payload,
        )
        if (outbox.isFull) {
            // Refuse the newest and say so, exactly as `StoreAndForwardQueue`
            // does. Silently dropping the oldest here would discard the
            // utterance the user is waiting on an answer to.
            emitError(
                LinkException(
                    LinkException.Code.OutboxFull,
                    "link outbox is full at ${outbox.size} envelopes; \"$type\" refused",
                ),
            )
            return@withLock null
        }
        outbox.offer(Outbox.Entry(envelope, envelope.serialise()))
        flush()
        envelope.id
    }

    // --- internals ------------------------------------------------------------

    private fun open() {
        setState(State.Connecting)
        // Rotate on every attempt rather than exhausting one address first.
        //
        // `attempt` resets to zero the moment a socket opens, so a working link
        // always comes back to the front of the list — which is the direct
        // address. That is the property SYSTEM.md §7's bandwidth argument needs:
        // a phone that failed over to the relay at the office returns to the LAN
        // by itself when it gets home, without anything having to notice that it
        // did. Sticking to whichever address answered would leave a household's
        // traffic on the relay until the app was restarted.
        val endpoint = addresses[addressIndex % addresses.size]
        addressIndex += 1
        route = endpoint.route
        val opened: RelaySocket = try {
            socketFactory.open(endpoint.url, auth)
        } catch (error: Exception) {
            emitError(
                LinkException(LinkException.Code.SocketFailed, "could not open socket", error),
            )
            scheduleRetry()
            return
        }
        socket = opened
        opened.listen(SocketEvents(opened))
    }

    /**
     * One socket's callbacks, tagged with the socket they belong to.
     *
     * The identity check in every method is not paranoia: a socket that is being
     * replaced can still deliver a close or a frame after the link has moved on,
     * and acting on it would cancel the retry for a connection that no longer
     * exists.
     */
    private inner class SocketEvents(private val owner: RelaySocket) : RelaySocket.Events {

        override fun onOpen() = lock.withLock {
            if (socket !== owner) return@withLock
            attempt = 0
            // Back to the front of the list. This is what lets a phone that
            // failed over to the relay at the office return to the LAN by itself
            // when it gets home, without anything having to notice that it did.
            addressIndex = 0
            setState(State.Open)
            armIdleTimer()
            flush()
        }

        override fun onMessage(text: String) = lock.withLock {
            if (socket !== owner) return@withLock
            armIdleTimer()
            receive(text)
        }

        override fun onError(error: Throwable) = lock.withLock {
            if (socket !== owner) return@withLock
            emitError(
                LinkException(
                    LinkException.Code.SocketFailed,
                    error.message ?: error.toString(),
                    error,
                ),
            )
        }

        override fun onClose(code: Int, reason: String) = lock.withLock {
            if (socket !== owner) return@withLock
            socket = null
            idleTimer?.cancel()
            idleTimer = null
            // A clean close still loses whatever was in the socket's buffer, so
            // everything in flight goes back either way.
            requeueInFlight()
            if (state != State.Closed) scheduleRetry()
        }
    }

    private fun receive(text: String) {
        val envelope = try {
            parseEnvelope(text)
        } catch (error: LinkException) {
            emitError(error)
            return
        }

        when (envelope.type) {
            ServerFrame.ACK -> acknowledge(envelope, refused = false)
            ServerFrame.ERROR -> acknowledge(envelope, refused = true)
        }

        for (listener in listeners.toList()) listener.onFrame(envelope)
        if (envelope.type !in ServerFrame.ALL) {
            for (listener in listeners.toList()) listener.onUnknownFrame(envelope)
        }
    }

    /**
     * `ack` and `error` both name the frame they answer, in `re`.
     *
     * An error still prunes: the daemon has the frame and has decided. What it
     * does *not* do is disappear — the listener sees it, so a caller told
     * `not_implemented, M4` can keep the audio on the device rather than
     * deleting it.
     */
    private fun acknowledge(envelope: RelayEnvelope, refused: Boolean) {
        val re = envelope.payloadObject()?.optString("re", "").orEmpty()
        if (re.isEmpty()) {
            // relayd sends an unaddressed error for a frame it could not even
            // parse an id out of. Nothing to prune; the listener still sees it.
            if (refused) emitRefusal(envelope, null)
            return
        }
        val known = outbox.acknowledge(re)
        if (refused) emitRefusal(envelope, re.takeIf { known })
    }

    private fun emitRefusal(envelope: RelayEnvelope, re: String?) {
        val payload = envelope.payloadObject()
        val code = payload?.optString("code", "").orEmpty().ifEmpty { "error" }
        val message = payload?.optString("message", "").orEmpty()
        val milestone = payload?.optString("milestone", "").orEmpty()
        val about = if (re != null) " for $re" else ""
        val schedule = if (milestone.isNotEmpty()) " (arrives in $milestone)" else ""
        emitError(
            LinkException(LinkException.Code.Refused, "relayd refused$about: $code — $message$schedule"),
        )
    }

    private fun flush() {
        val open = socket
        if (open == null || state != State.Open) return
        while (true) {
            val entry = outbox.head() ?: return
            try {
                open.send(entry.text)
            } catch (error: Exception) {
                // The socket refused it. Leave it at the head — the close that
                // follows triggers a reconnect and it goes out then, first.
                emitError(
                    LinkException(LinkException.Code.SocketFailed, "send failed", error),
                )
                return
            }
            outbox.markSent()
            for (listener in listeners.toList()) listener.onSent(entry.envelope)
        }
    }

    private fun requeueInFlight() {
        val returned = outbox.requeueInFlight()
        if (returned.isEmpty()) return
        val ids = returned.map { it.envelope.id }
        for (listener in listeners.toList()) listener.onRedelivered(ids)
    }

    private fun scheduleRetry() {
        if (state == State.Closed) return
        setState(State.Reconnecting)
        val delay = backoffMs(attempt, backoff, random.nextDouble())
        attempt += 1
        retryTimer?.cancel()
        retryTimer = scheduler.schedule(delay) {
            lock.withLock {
                retryTimer = null
                if (state == State.Closed) return@withLock
                open()
            }
        }
    }

    private fun armIdleTimer() {
        idleTimer?.cancel()
        idleTimer = null
        if (idleTimeoutMs <= 0) return
        idleTimer = scheduler.schedule(idleTimeoutMs) {
            lock.withLock {
                idleTimer = null
                val open = socket ?: return@withLock
                emitError(
                    LinkException(
                        LinkException.Code.SocketFailed,
                        "nothing inbound for ${idleTimeoutMs}ms; assuming the link is half-open",
                    ),
                )
                // Close it rather than fake a close event: a half-open socket
                // that is never closed is a file descriptor and a wake lock
                // held forever.
                open.close(4000, "idle timeout")
            }
        }
    }

    private fun cancelTimers() {
        retryTimer?.cancel()
        retryTimer = null
        idleTimer?.cancel()
        idleTimer = null
    }

    private fun setState(next: State) {
        if (state == next) return
        state = next
        for (listener in listeners.toList()) listener.onState(next)
    }

    private fun emitError(error: LinkException) {
        for (listener in listeners.toList()) listener.onError(error)
    }
}

/**
 * How the link authenticates.
 *
 * `SYSTEM.md` §6.1: "One authenticated socket means one token, printed on start
 * like the pairing code". `relayd/internal/api/server.go` checks exactly that —
 * a bearer token, constant-time compared — so that is what this carries.
 *
 * `relayd.ts` additionally builds an HMAC proof and presents it as a WebSocket
 * subprotocol. `relayd` does not check one today, so it is **not** ported here:
 * a credential the server ignores reads as security to whoever inspects the
 * tree, which is the same failure as an unwired consent policy.
 */
data class LinkAuth(val token: String, val deviceId: String = "") {
    fun authorizationHeader(): String = "Bearer $token"
}

/** Cancels a scheduled task. Idempotent. */
fun interface Cancellable {
    fun cancel()
}

/**
 * Time and timers, injected.
 *
 * The link never touches `System.currentTimeMillis` or a real thread pool, so
 * every timing rule in it — backoff, jitter, the idle timeout — is testable
 * without a test that sleeps. A test that sleeps is a test that is flaky on a
 * loaded CI box and passes on a laptop.
 */
interface LinkScheduler {
    fun now(): Long
    fun schedule(delayMs: Long, task: () -> Unit): Cancellable
}

/** The socket seam. `MockRelaySocket` is the test double; `JvmWebSocket` is real. */
interface RelaySocket {

    interface Events {
        fun onOpen()
        fun onMessage(text: String)
        fun onError(error: Throwable)
        fun onClose(code: Int, reason: String)
    }

    /**
     * Start delivering events. Called once, immediately after construction.
     *
     * Separate from the factory so the link can install its handlers before the
     * socket can possibly report anything — a connection that opens and closes
     * before `listen` would otherwise be lost.
     */
    fun listen(events: Events)

    fun send(text: String)

    fun close(code: Int, reason: String)
}

fun interface RelaySocketFactory {
    fun open(url: String, auth: LinkAuth): RelaySocket
}
