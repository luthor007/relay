package glass.relay.bridge.link

import org.json.JSONObject
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.BufferedInputStream
import java.io.InputStream
import java.io.OutputStream
import java.net.InetAddress
import java.net.ServerSocket
import java.net.Socket
import java.util.Random
import java.util.concurrent.CountDownLatch
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit

/**
 * [JvmWebSocket] against a real socket.
 *
 * A loopback server rather than a mock, because the point of this test is the
 * part a mock would have replaced: the handshake going out on a stream, the
 * masked frames coming back through a kernel, and the abrupt close that a phone
 * losing signal actually produces. Everything binds `127.0.0.1` on an
 * ephemeral port and lives for the length of one test.
 *
 * Every wait is bounded and asserted, so a hang is a failed assertion with a
 * name rather than a build that sits there.
 */
class JvmWebSocketTest {

    private val servers = mutableListOf<TinyWebSocketServer>()
    private val sockets = mutableListOf<JvmWebSocket>()

    @After
    fun tearDown() {
        for (socket in sockets) runCatching { socket.close(1000, "test over") }
        for (server in servers) runCatching { server.close() }
    }

    private fun server(handler: TinyWebSocketServer.(Peer) -> Unit): TinyWebSocketServer =
        TinyWebSocketServer(handler).also { servers += it; it.start() }

    private fun socketTo(server: TinyWebSocketServer, token: String = "t0ken"): JvmWebSocket =
        JvmWebSocket(
            url = "ws://127.0.0.1:${server.port}/v1/ws",
            auth = LinkAuth(token, deviceId = "phone-1"),
            connectTimeoutMs = 3_000,
            readTimeoutMs = 3_000,
            random = Random(11),
        ).also { sockets += it }

    private class Recorder : RelaySocket.Events {
        val opened = CountDownLatch(1)
        val closed = CountDownLatch(1)
        val messages = LinkedBlockingQueue<String>()
        val errors = LinkedBlockingQueue<Throwable>()
        var closeCode: Int = 0

        override fun onOpen() = opened.countDown()
        override fun onMessage(text: String) { messages += text }
        override fun onError(error: Throwable) { errors += error }
        override fun onClose(code: Int, reason: String) {
            closeCode = code
            closed.countDown()
        }
    }

    @Test
    fun `it completes a real handshake and exchanges envelopes both ways`() {
        val server = server { peer ->
            peer.sendText(
                RelayEnvelope("srv-1", ServerFrame.SPEAK, 1, JSONObject().put("text", "hello")).serialise(),
            )
            peer.readText()?.let { peer.received += it }
        }
        val socket = socketTo(server)
        val events = Recorder()

        socket.listen(events)

        assertTrue("never opened", events.opened.await(5, TimeUnit.SECONDS))
        val inbound = events.messages.poll(5, TimeUnit.SECONDS)
        assertEquals(ServerFrame.SPEAK, parseEnvelope(inbound!!).type)

        socket.send(
            RelayEnvelope("phone-1", PhoneFrame.WEAR, 2, JSONObject().put("worn", true)).serialise(),
        )

        val fromPhone = server.received.poll(5, TimeUnit.SECONDS)
        assertEquals(PhoneFrame.WEAR, parseEnvelope(fromPhone!!).type)
        // The handshake headers relayd's auth middleware reads.
        assertEquals("Bearer t0ken", server.headers["authorization"])
        assertEquals("relay.v1", server.headers["sec-websocket-protocol"])
    }

    @Test
    fun `a ping is answered with a pong carrying the same payload`() {
        // A server that matches pong payloads concludes a link is dead when the
        // echo is wrong, and this link's idle timeout defaults to off precisely
        // because liveness is supposed to live down here.
        val server = server { peer ->
            peer.sendControl(WebSocketProtocol.OPCODE_PING, "beat".toByteArray())
            val frame = peer.readFrame()
            peer.control += frame
        }
        val socket = socketTo(server)
        val events = Recorder()

        socket.listen(events)
        assertTrue(events.opened.await(5, TimeUnit.SECONDS))

        val pong = server.control.poll(5, TimeUnit.SECONDS)
        assertEquals(WebSocketProtocol.OPCODE_PONG, pong!!.opcode)
        assertEquals("beat", String(pong.payload))
    }

    @Test
    fun `a server that hangs up without a close frame reports an abnormal close`() {
        // The normal phone case: the tunnel dies, nothing is flushed. 1006 is
        // what tells RelaydLink to put its in-flight envelopes back.
        val server = server { peer -> peer.hangUp() }
        val socket = socketTo(server)
        val events = Recorder()

        socket.listen(events)
        assertTrue(events.opened.await(5, TimeUnit.SECONDS))

        assertTrue("never reported a close", events.closed.await(5, TimeUnit.SECONDS))
        assertEquals(1006, events.closeCode)
    }

    @Test
    fun `a refused token is reported as a refused token, not a network failure`() {
        val server = TinyWebSocketServer({ }, rejectWith = 401).also { servers += it; it.start() }
        val socket = socketTo(server, token = "wrong")
        val events = Recorder()

        socket.listen(events)

        assertTrue(events.closed.await(5, TimeUnit.SECONDS))
        val error = events.errors.poll(5, TimeUnit.SECONDS)
        assertTrue(error!!.message!!.contains("token"))
    }

    @Test
    fun `nothing is listening is an error and a close, never a hang`() {
        val dead = ServerSocket(0, 0, InetAddress.getLoopbackAddress()).also { it.close() }
        val socket = JvmWebSocket(
            url = "ws://127.0.0.1:${dead.localPort}/v1/ws",
            auth = LinkAuth("t"),
            connectTimeoutMs = 1_000,
            readTimeoutMs = 1_000,
        ).also { sockets += it }
        val events = Recorder()

        socket.listen(events)

        assertTrue(events.closed.await(5, TimeUnit.SECONDS))
        assertEquals(1006, events.closeCode)
    }

    @Test
    fun `a link over a real socket queues, connects and delivers`() {
        // The whole stack: RelaydLink's outbox, JvmWebSocket's framing, and a
        // server on the other end of a file descriptor.
        val server = server { peer ->
            val first = peer.readText() ?: return@server
            peer.received += first
            peer.sendText(
                RelayEnvelope(
                    "srv-ack", ServerFrame.ACK, 1,
                    JSONObject().put("re", parseEnvelope(first).id).put("ok", true),
                ).serialise(),
            )
        }
        val scheduler = SystemLinkScheduler()
        val link = RelaydLink(
            url = "ws://127.0.0.1:${server.port}/v1/ws",
            auth = LinkAuth("t0ken"),
            socketFactory = JvmWebSocketFactory(connectTimeoutMs = 3_000, readTimeoutMs = 3_000),
            scheduler = scheduler,
            random = Random(5),
        )
        val acked = CountDownLatch(1)
        link.addListener(object : RelaydLink.Listener {
            override fun onFrame(envelope: RelayEnvelope) {
                if (envelope.type == ServerFrame.ACK) acked.countDown()
            }
        })

        try {
            link.send(PhoneFrame.UTTERANCE, JSONObject().put("text", "where was I"))
            link.connect()

            assertTrue("no ack came back", acked.await(5, TimeUnit.SECONDS))
            assertEquals("an acked envelope must not still be pending", 0, link.pending)
        } finally {
            link.close()
            scheduler.shutdown()
        }
    }

    // --- a server that is barely a server -------------------------------------

    /** One connection's streams, plus the frame helpers a test needs. */
    class Peer(val input: InputStream, val output: OutputStream, val socket: Socket) {
        val received = LinkedBlockingQueue<String>()
        val control = LinkedBlockingQueue<WebSocketProtocol.Frame>()

        /** Server frames are never masked. RFC 6455 §5.1. */
        fun sendText(text: String) = sendFrame(WebSocketProtocol.OPCODE_TEXT, text.toByteArray())

        fun sendControl(opcode: Int, payload: ByteArray) = sendFrame(opcode, payload)

        private fun sendFrame(opcode: Int, payload: ByteArray) {
            val header = if (payload.size <= 125) {
                byteArrayOf((0x80 or opcode).toByte(), payload.size.toByte())
            } else {
                byteArrayOf(
                    (0x80 or opcode).toByte(), 126,
                    ((payload.size ushr 8) and 0xFF).toByte(),
                    (payload.size and 0xFF).toByte(),
                )
            }
            output.write(header)
            output.write(payload)
            output.flush()
        }

        fun readFrame(): WebSocketProtocol.Frame = readClientFrame(input)

        fun readText(): String? {
            while (true) {
                val frame = readClientFrame(input)
                if (frame.opcode == WebSocketProtocol.OPCODE_CLOSE) return null
                if (frame.isControl) continue
                return String(frame.payload, Charsets.UTF_8)
            }
        }

        /** Vanish. No close frame, no FIN handshake — a tunnel going away. */
        fun hangUp() {
            socket.setSoLinger(true, 0)
            socket.close()
        }
    }

    class TinyWebSocketServer(
        private val handler: TinyWebSocketServer.(Peer) -> Unit,
        private val rejectWith: Int = 0,
    ) {
        private val server = ServerSocket(0, 1, InetAddress.getLoopbackAddress())
        private var thread: Thread? = null

        val port: Int get() = server.localPort
        val received = LinkedBlockingQueue<String>()
        val control = LinkedBlockingQueue<WebSocketProtocol.Frame>()
        @Volatile var headers: Map<String, String> = emptyMap()

        fun start() {
            thread = Thread({ runCatching { accept() } }, "tiny-ws").apply {
                isDaemon = true
                start()
            }
        }

        private fun accept() {
            server.accept().use { connection ->
                val input = BufferedInputStream(connection.getInputStream())
                val output = connection.getOutputStream()
                headers = readRequestHeaders(input)

                if (rejectWith != 0) {
                    output.write(
                        ("HTTP/1.1 $rejectWith Unauthorized\r\nContent-Length: 0\r\n\r\n")
                            .toByteArray(Charsets.US_ASCII),
                    )
                    output.flush()
                    return
                }

                val key = headers["sec-websocket-key"].orEmpty()
                output.write(
                    (
                        "HTTP/1.1 101 Switching Protocols\r\n" +
                            "Upgrade: websocket\r\n" +
                            "Connection: Upgrade\r\n" +
                            "Sec-WebSocket-Accept: ${WebSocketProtocol.acceptFor(key)}\r\n" +
                            "Sec-WebSocket-Protocol: $LINK_SUBPROTOCOL\r\n\r\n"
                        ).toByteArray(Charsets.US_ASCII),
                )
                output.flush()

                val peer = Peer(input, output, connection)
                handler(peer)
                // Drain whatever the peer collected into the server's queues so
                // the test can await them without knowing about the Peer.
                peer.received.drainTo(received)
                peer.control.drainTo(control)
            }
        }

        fun close() {
            runCatching { server.close() }
            thread?.interrupt()
        }
    }
}

/** The request line and headers, lower-cased keys. Enough for a test server. */
private fun readRequestHeaders(input: InputStream): Map<String, String> {
    fun line(): String {
        val buffer = StringBuilder()
        while (true) {
            val byte = input.read()
            if (byte == -1) break
            if (byte == '\n'.code) break
            if (byte != '\r'.code) buffer.append(byte.toChar())
        }
        return buffer.toString()
    }

    line() // the request line; the test server does not route
    val headers = HashMap<String, String>()
    while (true) {
        val header = line()
        if (header.isEmpty()) break
        val colon = header.indexOf(':')
        if (colon <= 0) continue
        headers[header.substring(0, colon).trim().lowercase()] = header.substring(colon + 1).trim()
    }
    return headers
}

/**
 * Read one **masked** frame, which is what a client sends.
 *
 * `WebSocketProtocol.readFrame` deliberately refuses masked frames, because it
 * only ever reads from a server. This is the mirror image, and having to write
 * it is a small proof that the refusal is real.
 */
private fun readClientFrame(input: InputStream): WebSocketProtocol.Frame {
    fun byte(): Int {
        val value = input.read()
        if (value == -1) throw java.io.EOFException("client went away mid-frame")
        return value
    }

    val first = byte()
    val fin = first and 0x80 != 0
    val opcode = first and 0x0F
    val second = byte()
    if (second and 0x80 == 0) throw IllegalStateException("a client frame must be masked")
    var length = (second and 0x7F).toLong()
    when (length) {
        126L -> length = ((byte().toLong()) shl 8) or byte().toLong()
        127L -> {
            length = 0
            repeat(8) { length = (length shl 8) or byte().toLong() }
        }
    }
    val mask = ByteArray(4) { byte().toByte() }
    val payload = ByteArray(length.toInt())
    var read = 0
    while (read < payload.size) {
        val count = input.read(payload, read, payload.size - read)
        if (count == -1) throw java.io.EOFException("client frame truncated")
        read += count
    }
    for (index in payload.indices) {
        payload[index] = (payload[index].toInt() xor mask[index % 4].toInt()).toByte()
    }
    return WebSocketProtocol.Frame(fin, opcode, payload)
}
