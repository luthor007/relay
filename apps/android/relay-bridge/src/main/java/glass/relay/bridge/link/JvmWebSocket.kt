package glass.relay.bridge.link

import java.io.BufferedInputStream
import java.io.EOFException
import java.io.OutputStream
import java.net.InetSocketAddress
import java.net.Socket
import java.util.Random
import java.util.concurrent.atomic.AtomicBoolean
import javax.net.ssl.SSLSocket
import javax.net.ssl.SSLSocketFactory

/**
 * A [RelaySocket] over `java.net.Socket`, speaking [WebSocketProtocol].
 *
 * Plain JVM rather than platform APIs, so `tools/verify-jvm-logic.sh` compiles
 * it and `JvmWebSocketTest` drives it against a loopback server. That matters
 * more than it sounds — this is the one file in the link that touches a real
 * file descriptor, and "the socket half is unverifiable" was the excuse that
 * would otherwise have shipped four hundred lines of untested I/O.
 *
 * (The verifier's rule is mechanical — a file naming a platform symbol
 * anywhere, prose included, is excluded from the checked set. Hence the
 * circumlocution above. A doc comment is not worth losing the coverage over.)
 *
 * ## One reader thread, one writer lock
 *
 * The reader thread does the handshake and then blocks on frames, delivering
 * each message to [RelaySocket.Events]. Writes happen on whatever thread called
 * [send], serialised by [writeLock], because two frames interleaved on one
 * stream is a corrupt connection rather than a slow one.
 *
 * `RelaydLink` holds its own lock around every callback, so the reader thread
 * and a `send` from the capture service cannot interleave *in the link* either.
 *
 * ## Pings are answered here
 *
 * RFC 6455 §5.5.2 liveness lives at this layer, which is why `RelaydLink`'s idle
 * timeout defaults to off. A pong goes back with the ping's own payload, as the
 * RFC requires — echoing something else makes a server that matches payloads
 * conclude the connection is dead.
 */
class JvmWebSocket(
    private val url: String,
    private val auth: LinkAuth,
    private val connectTimeoutMs: Int = 10_000,
    /**
     * How long a read may block before the socket is considered dead.
     *
     * Not a heartbeat: `relayd` sends a session list on connect and then only
     * when something changes, so a quiet link is normal. This is the backstop
     * for a connection a carrier NAT has silently dropped, where the read never
     * returns and never fails.
     */
    private val readTimeoutMs: Int = 5 * 60_000,
    private val random: Random = Random(),
    private val socketFactory: () -> Socket = { Socket() },
    private val sslFactory: SSLSocketFactory =
        SSLSocketFactory.getDefault() as SSLSocketFactory,
    private val threadStarter: (Runnable) -> Unit = { runnable ->
        Thread(runnable, "relay-ws").apply { isDaemon = true }.start()
    },
) : RelaySocket {

    private val writeLock = Any()
    private val closed = AtomicBoolean(false)

    @Volatile private var socket: Socket? = null
    @Volatile private var output: OutputStream? = null
    @Volatile private var events: RelaySocket.Events? = null

    override fun listen(events: RelaySocket.Events) {
        this.events = events
        threadStarter(Runnable { run() })
    }

    override fun send(text: String) {
        val stream = output ?: throw IllegalStateException("socket is not open")
        val payload = text.toByteArray(Charsets.UTF_8)
        val mask = ByteArray(4).also { random.nextBytes(it) }
        val frame = WebSocketProtocol.encodeFrame(WebSocketProtocol.OPCODE_TEXT, payload, mask)
        synchronized(writeLock) {
            stream.write(frame)
            stream.flush()
        }
    }

    override fun close(code: Int, reason: String) {
        if (!closed.compareAndSet(false, true)) return
        // Best effort: a close frame is courtesy, and the peer may already be
        // gone. What must happen is the file descriptor going away — an
        // un-closed socket on a phone is a wake lock nobody can find.
        runCatching { writeControl(WebSocketProtocol.OPCODE_CLOSE, WebSocketProtocol.closePayload(code, reason)) }
        runCatching { socket?.close() }
        deliverClose(code, reason)
    }

    // --- the reader thread ----------------------------------------------------

    private fun run() {
        val handler = events ?: return
        val endpoint = try {
            WebSocketProtocol.parseEndpoint(url)
        } catch (error: Exception) {
            handler.onError(error)
            deliverClose(1006, error.message ?: "bad url")
            return
        }

        val raw = try {
            connect(endpoint)
        } catch (error: Exception) {
            handler.onError(error)
            deliverClose(1006, error.message ?: "connect failed")
            return
        }
        socket = raw

        try {
            val input = BufferedInputStream(raw.getInputStream())
            val stream = raw.getOutputStream()
            output = stream

            val key = WebSocketProtocol.newKey(random)
            synchronized(writeLock) {
                stream.write(
                    WebSocketProtocol.handshakeRequest(endpoint, auth, key)
                        .toByteArray(Charsets.US_ASCII),
                )
                stream.flush()
            }
            WebSocketProtocol.verifyHandshake(WebSocketProtocol.readHandshakeResponse(input), key)

            handler.onOpen()

            while (!closed.get()) {
                val message = WebSocketProtocol.readMessage(input, onControl = ::onControlFrame)
                    ?: break // the peer sent a close
                handler.onMessage(message)
            }
            close(1000, "peer closed")
        } catch (error: EOFException) {
            // The tunnel died with no close frame. The normal phone case, and
            // the reason everything in flight goes back to the head of the
            // outbox rather than being assumed delivered.
            if (!closed.get()) handler.onError(error)
            failAndClose(error.message ?: "connection lost")
        } catch (error: Exception) {
            if (!closed.get()) handler.onError(error)
            failAndClose(error.message ?: error.toString())
        }
    }

    /**
     * Open the transport.
     *
     * A `ws://` box on the LAN works even though the platform's default network
     * security policy forbids cleartext from API 28 on: that policy is enforced
     * by the HTTP stacks, and a raw socket is not one of them. Worth stating
     * because it looks like an oversight — and because the day someone "fixes"
     * it by adding a cleartext exception to the manifest, they will have widened
     * the policy for `ConnectorClient` as well, which does go through
     * `HttpURLConnection` and should keep being told no.
     */
    private fun connect(endpoint: WebSocketProtocol.Endpoint): Socket {
        val plain = socketFactory()
        plain.tcpNoDelay = true
        plain.connect(InetSocketAddress(endpoint.host, endpoint.port), connectTimeoutMs)
        plain.soTimeout = readTimeoutMs
        if (!endpoint.secure) return plain
        val secure = sslFactory.createSocket(plain, endpoint.host, endpoint.port, true) as SSLSocket
        // Without this the handshake succeeds against any certificate that
        // chains to a trusted root, for any hostname. That is the difference
        // between TLS and the appearance of TLS.
        secure.sslParameters = secure.sslParameters.apply { endpointIdentificationAlgorithm = "HTTPS" }
        secure.startHandshake()
        secure.soTimeout = readTimeoutMs
        return secure
    }

    private fun onControlFrame(frame: WebSocketProtocol.Frame) {
        when (frame.opcode) {
            WebSocketProtocol.OPCODE_PING ->
                runCatching { writeControl(WebSocketProtocol.OPCODE_PONG, frame.payload) }
            WebSocketProtocol.OPCODE_CLOSE -> {
                val code = WebSocketProtocol.closeCode(frame)
                val reason = WebSocketProtocol.closeReason(frame)
                if (closed.compareAndSet(false, true)) {
                    runCatching {
                        writeControl(
                            WebSocketProtocol.OPCODE_CLOSE,
                            WebSocketProtocol.closePayload(1000, ""),
                        )
                    }
                    runCatching { socket?.close() }
                    deliverClose(code, reason)
                }
            }
            // A pong we did not ask for is legal (RFC 6455 §5.5.3) and means
            // nothing to us; ignoring it is the specified behaviour.
            WebSocketProtocol.OPCODE_PONG -> Unit
        }
    }

    private fun writeControl(opcode: Int, payload: ByteArray) {
        val stream = output ?: return
        val mask = ByteArray(4).also { random.nextBytes(it) }
        val frame = WebSocketProtocol.encodeFrame(opcode, payload, mask)
        synchronized(writeLock) {
            stream.write(frame)
            stream.flush()
        }
    }

    private fun failAndClose(reason: String) {
        if (!closed.compareAndSet(false, true)) return
        runCatching { socket?.close() }
        deliverClose(1006, reason)
    }

    private val closeDelivered = AtomicBoolean(false)

    private fun deliverClose(code: Int, reason: String) {
        if (!closeDelivered.compareAndSet(false, true)) return
        events?.onClose(code, reason)
    }
}

/**
 * The production factory. One socket per connect attempt, never reused.
 *
 * A reconnect gets a fresh [JvmWebSocket] because a socket that has closed
 * cannot be reopened, and pretending otherwise is how a link ends up delivering
 * a dead connection's close event to its replacement.
 */
class JvmWebSocketFactory(
    private val random: Random = Random(),
    private val connectTimeoutMs: Int = 10_000,
    private val readTimeoutMs: Int = 5 * 60_000,
) : RelaySocketFactory {
    override fun open(url: String, auth: LinkAuth): RelaySocket =
        JvmWebSocket(
            url = url,
            auth = auth,
            connectTimeoutMs = connectTimeoutMs,
            readTimeoutMs = readTimeoutMs,
            random = random,
        )
}
