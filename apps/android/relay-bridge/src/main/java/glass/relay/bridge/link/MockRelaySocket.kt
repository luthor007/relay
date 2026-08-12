package glass.relay.bridge.link

import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.ThreadFactory
import java.util.concurrent.TimeUnit

/**
 * A socket that is not a socket.
 *
 * The same discipline as `MockGlassesTransport`: no real network, no real
 * timers, nothing opens until a test says so. It records every byte it was told
 * to send, so a test can assert ordering and redelivery on the exact text the
 * daemon would have seen rather than on the link's own bookkeeping.
 *
 * Lives in `main` rather than `test` on purpose — the same reason
 * `MockGlassesTransport` does. A debug build with no box to talk to still has to
 * be able to drive the whole product surface, and `APPS-SCOPE.md` §5 makes that
 * the highest-leverage thing in the module.
 */
class MockRelaySocket(val url: String, val auth: LinkAuth) : RelaySocket {

    val sent = mutableListOf<String>()

    private var events: RelaySocket.Events? = null
    private var opened = false
    private var closed = false

    /** Make [send] throw, the way a socket that is already dead does. */
    var sendFails: Boolean = false

    val isOpen: Boolean get() = opened

    override fun listen(events: RelaySocket.Events) {
        this.events = events
    }

    override fun send(text: String) {
        if (sendFails) throw IllegalStateException("mock socket: send failed")
        sent += text
    }

    override fun close(code: Int, reason: String) {
        if (closed) return
        closed = true
        opened = false
        events?.onClose(code, reason)
    }

    // --- test controls --------------------------------------------------------

    /** The daemon accepted the upgrade. */
    fun acceptOpen() {
        if (closed) return
        opened = true
        events?.onOpen()
    }

    /** Deliver an inbound frame exactly as the wire would. */
    fun deliver(text: String) {
        events?.onMessage(text)
    }

    fun deliverEnvelope(type: String, payload: Any? = null, id: String? = null, atMs: Long = 0) {
        deliver(
            RelayEnvelope(
                id = id ?: "srv-${sent.size}-$type",
                type = type,
                atMs = atMs,
                payload = payload,
            ).serialise(),
        )
    }

    /** `ack` as `relayd` sends it: one id, in `re`. */
    fun deliverAck(re: String) =
        deliverEnvelope(ServerFrame.ACK, org.json.JSONObject().put("re", re).put("ok", true))

    fun deliverError(re: String, code: String, message: String, milestone: String = "") =
        deliverEnvelope(
            ServerFrame.ERROR,
            org.json.JSONObject()
                .put("re", re)
                .put("code", code)
                .put("message", message)
                .apply { if (milestone.isNotEmpty()) put("milestone", milestone) },
        )

    /** The tunnel died: no close frame, no flush. The normal phone case. */
    fun dropAbruptly(reason: String = "network lost") {
        if (closed) return
        closed = true
        opened = false
        events?.onClose(1006, reason)
    }

    fun fail(message: String) {
        events?.onError(IllegalStateException(message))
    }
}

/** Hands back every socket it made, newest last. */
class MockRelaySocketFactory : RelaySocketFactory {
    val sockets = mutableListOf<MockRelaySocket>()

    /** Set to make [open] throw — a DNS failure, or no network at all. */
    var openFails: Boolean = false

    override fun open(url: String, auth: LinkAuth): RelaySocket {
        if (openFails) throw IllegalStateException("mock factory: cannot open")
        return MockRelaySocket(url, auth).also { sockets += it }
    }

    val latest: MockRelaySocket? get() = sockets.lastOrNull()
}

/**
 * Timers on a virtual clock.
 *
 * A test that asserts a thirty-second backoff must not take thirty seconds, and
 * one that takes even one is a test somebody will eventually delete. Also lives
 * in `main` so a mock build can run the link on a fake clock.
 */
class FakeLinkScheduler(private var nowMs: Long = 0) : LinkScheduler {

    private class Task(val dueAtMs: Long, val run: () -> Unit) {
        var cancelled = false
    }

    private val tasks = mutableListOf<Task>()

    override fun now(): Long = nowMs

    override fun schedule(delayMs: Long, task: () -> Unit): Cancellable {
        val entry = Task(nowMs + delayMs, task)
        tasks += entry
        return Cancellable { entry.cancelled = true }
    }

    /** How long until the next task would run, or null when there is none. */
    fun nextDelayMs(): Long? =
        tasks.filterNot { it.cancelled }.minOfOrNull { it.dueAtMs }?.let { it - nowMs }

    /** Move time forward and run everything that comes due, in order. */
    fun advance(byMs: Long) {
        val target = nowMs + byMs
        while (true) {
            val next = tasks.filterNot { it.cancelled }.minByOrNull { it.dueAtMs } ?: break
            if (next.dueAtMs > target) break
            nowMs = next.dueAtMs
            tasks.remove(next)
            next.run()
        }
        nowMs = target
    }

    /** Run whatever is pending, whenever it was due. For "just reconnect now". */
    fun runPending() {
        val due = tasks.filterNot { it.cancelled }.sortedBy { it.dueAtMs }
        tasks.removeAll(due)
        for (task in due) {
            nowMs = maxOf(nowMs, task.dueAtMs)
            task.run()
        }
    }
}

/**
 * The production scheduler: one thread, so timers cannot run concurrently with
 * each other.
 *
 * Daemon threads, because a link that keeps the JVM alive is a link that stops a
 * process from exiting — on Android that shows up as a service that will not die
 * and a user who force-stops the app.
 */
class SystemLinkScheduler(
    private val executor: ScheduledExecutorService = Executors.newSingleThreadScheduledExecutor(
        ThreadFactory { runnable ->
            Thread(runnable, "relay-link").apply { isDaemon = true }
        },
    ),
) : LinkScheduler {

    override fun now(): Long = System.currentTimeMillis()

    override fun schedule(delayMs: Long, task: () -> Unit): Cancellable {
        val future = executor.schedule(task, delayMs, TimeUnit.MILLISECONDS)
        return Cancellable { future.cancel(false) }
    }

    fun shutdown() = executor.shutdownNow()
}
