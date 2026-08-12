package glass.relay.bridge.link

import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.ByteArrayInputStream
import java.util.Random

/**
 * RFC 6455, pinned against the RFC's own examples.
 *
 * The handshake vector is §1.3's, verbatim. The frame vectors are §5.7's. They
 * are in the document precisely so that an implementation can be checked without
 * a peer, which is what makes this file possible on a machine with no Android
 * SDK — and hand-written framing that nobody checked against the spec is how a
 * link fails in a way that looks like a network problem.
 */
class WebSocketProtocolTest {

    private fun bytes(vararg values: Int) = ByteArray(values.size) { values[it].toByte() }

    // --- handshake ------------------------------------------------------------

    @Test
    fun `the accept value is RFC 6455 section 1 3's example`() {
        assertEquals("s3pPLMBiTxaQ9kYGzzhZRbK+xOo=", WebSocketProtocol.acceptFor("dGhlIHNhbXBsZSBub25jZQ=="))
    }

    @Test
    fun `a wrong accept value is refused, because it is what proves a real handshake`() {
        val response = WebSocketProtocol.HandshakeResponse(
            status = 101,
            headers = mapOf("upgrade" to "websocket", "sec-websocket-accept" to "not-it"),
        )

        val error = runCatching { WebSocketProtocol.verifyHandshake(response, "dGhlIHNhbXBsZSBub25jZQ==") }
            .exceptionOrNull()

        assertTrue(error is WebSocketProtocol.ProtocolException)
    }

    @Test
    fun `a 401 says the token is wrong rather than that the network is`() {
        val error = runCatching {
            WebSocketProtocol.verifyHandshake(
                WebSocketProtocol.HandshakeResponse(401, emptyMap()),
                "k",
            )
        }.exceptionOrNull()

        assertTrue(error!!.message!!.contains("token"))
    }

    @Test
    fun `ws and wss URLs split into host, port and path`() {
        val plain = WebSocketProtocol.parseEndpoint("ws://192.168.1.20:8787/v1/ws")
        assertEquals("192.168.1.20", plain.host)
        assertEquals(8787, plain.port)
        assertEquals("/v1/ws", plain.path)
        assertEquals(false, plain.secure)
        assertEquals("192.168.1.20:8787", plain.hostHeader)

        val secure = WebSocketProtocol.parseEndpoint("wss://relay.example/v1/ws")
        assertEquals(443, secure.port)
        assertEquals(true, secure.secure)
        assertEquals("relay.example", secure.hostHeader)
    }

    @Test
    fun `an http URL is refused rather than quietly accepted`() {
        val error = runCatching { WebSocketProtocol.parseEndpoint("https://relay.example/v1/ws") }
            .exceptionOrNull()
        assertTrue(error is WebSocketProtocol.ProtocolException)
    }

    @Test
    fun `the token travels in a header, never in the query string`() {
        // Query strings end up in proxy logs and crash reports, and this one is
        // a bearer credential for everything on the box.
        val endpoint = WebSocketProtocol.parseEndpoint("ws://127.0.0.1:8787/v1/ws")
        val request = WebSocketProtocol.handshakeRequest(endpoint, LinkAuth("s3cret"), "abc")

        assertTrue(request.contains("Authorization: Bearer s3cret\r\n"))
        assertTrue("no token in the request line", !request.lineSequence().first().contains("s3cret"))
        assertTrue(request.contains("Sec-WebSocket-Version: 13\r\n"))
        assertTrue(request.contains("Sec-WebSocket-Protocol: relay.v1\r\n"))
        assertTrue(request.endsWith("\r\n\r\n"))
    }

    @Test
    fun `a response's status and headers parse, case-insensitively`() {
        val raw = "HTTP/1.1 101 Switching Protocols\r\n" +
            "Upgrade: websocket\r\n" +
            "Connection: Upgrade\r\n" +
            "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n" +
            "\r\n"

        val response = WebSocketProtocol.readHandshakeResponse(ByteArrayInputStream(raw.toByteArray()))

        assertEquals(101, response.status)
        assertEquals("websocket", response.headers["upgrade"])
        WebSocketProtocol.verifyHandshake(response, "dGhlIHNhbXBsZSBub25jZQ==")
    }

    // --- framing --------------------------------------------------------------

    @Test
    fun `an unmasked server text frame decodes - RFC 6455 section 5 7`() {
        // 0x81 0x05 "Hello"
        val input = ByteArrayInputStream(bytes(0x81, 0x05, 0x48, 0x65, 0x6c, 0x6c, 0x6f))

        val frame = WebSocketProtocol.readFrame(input)

        assertEquals(true, frame.fin)
        assertEquals(WebSocketProtocol.OPCODE_TEXT, frame.opcode)
        assertEquals("Hello", String(frame.payload))
    }

    @Test
    fun `a masked client frame encodes to RFC 6455 section 5 7's bytes`() {
        // The RFC's example: key 0x37fa213d over "Hello".
        val expected = bytes(0x81, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58)

        val encoded = WebSocketProtocol.encodeFrame(
            WebSocketProtocol.OPCODE_TEXT,
            "Hello".toByteArray(),
            bytes(0x37, 0xfa, 0x21, 0x3d),
        )

        assertArrayEquals(expected, encoded)
    }

    @Test
    fun `a fragmented message reassembles - RFC 6455 section 5 7`() {
        // "Hel" with FIN=0, then "lo" as a continuation with FIN=1.
        val input = ByteArrayInputStream(
            bytes(0x01, 0x03, 0x48, 0x65, 0x6c) + bytes(0x80, 0x02, 0x6c, 0x6f),
        )

        val message = WebSocketProtocol.readMessage(input) {}

        assertEquals("Hello", message)
    }

    @Test
    fun `a control frame between fragments is handled without corrupting the message`() {
        // RFC 6455 §5.4 permits this and it is the case a naive "loop until
        // FIN" implementation splices a ping into the middle of the JSON.
        val ping = bytes(0x89, 0x02, 0x70, 0x69)
        val input = ByteArrayInputStream(
            bytes(0x01, 0x03, 0x48, 0x65, 0x6c) + ping + bytes(0x80, 0x02, 0x6c, 0x6f),
        )
        val control = mutableListOf<Int>()

        val message = WebSocketProtocol.readMessage(input) { control += it.opcode }

        assertEquals("Hello", message)
        assertEquals(listOf(WebSocketProtocol.OPCODE_PING), control)
    }

    @Test
    fun `a close frame ends the message and carries its code and reason`() {
        val payload = WebSocketProtocol.closePayload(1001, "going away")
        val input = ByteArrayInputStream(
            bytes(0x88, payload.size) + payload,
        )
        var closed: WebSocketProtocol.Frame? = null

        val message = WebSocketProtocol.readMessage(input) { closed = it }

        assertNull(message)
        assertEquals(1001, WebSocketProtocol.closeCode(closed!!))
        assertEquals("going away", WebSocketProtocol.closeReason(closed!!))
    }

    @Test
    fun `a close frame with no payload reports 1005 rather than inventing a code`() {
        assertEquals(1005, WebSocketProtocol.closeCode(WebSocketProtocol.Frame(true, 0x8, ByteArray(0))))
    }

    @Test
    fun `the two extended length encodings round-trip`() {
        val mask = bytes(0x01, 0x02, 0x03, 0x04)
        for (size in listOf(125, 126, 65_535, 65_536)) {
            val payload = ByteArray(size) { (it % 251).toByte() }
            val encoded = WebSocketProtocol.encodeFrame(WebSocketProtocol.OPCODE_TEXT, payload, mask)
            // Unmask it the way a server would, then feed it back as a server
            // frame so readFrame's decoding is checked against encodeFrame's.
            val unmasked = unmaskForServer(encoded)
            val frame = WebSocketProtocol.readFrame(ByteArrayInputStream(unmasked))
            assertArrayEquals("size $size", payload, frame.payload)
        }
    }

    @Test
    fun `a masked server frame is refused - RFC 6455 section 5 1`() {
        val masked = WebSocketProtocol.encodeFrame(
            WebSocketProtocol.OPCODE_TEXT, "Hello".toByteArray(), bytes(1, 2, 3, 4),
        )

        val error = runCatching { WebSocketProtocol.readFrame(ByteArrayInputStream(masked)) }
            .exceptionOrNull()

        assertTrue(error is WebSocketProtocol.ProtocolException)
        assertTrue(error!!.message!!.contains("must not be masked"))
    }

    @Test
    fun `an RSV bit is a protocol error, because no extension was negotiated`() {
        val error = runCatching {
            WebSocketProtocol.readFrame(ByteArrayInputStream(bytes(0xC1, 0x01, 0x41)))
        }.exceptionOrNull()

        assertTrue(error is WebSocketProtocol.ProtocolException)
    }

    @Test
    fun `a fragmented control frame is refused - RFC 6455 section 5 5`() {
        val error = runCatching {
            WebSocketProtocol.readFrame(ByteArrayInputStream(bytes(0x09, 0x02, 0x70, 0x69)))
        }.exceptionOrNull()

        assertTrue(error!!.message!!.contains("cannot be fragmented"))
    }

    @Test
    fun `an oversized frame is refused before it is allocated`() {
        // Whatever answered the socket does not get to choose how much memory
        // this process allocates.
        val header = bytes(0x81, 0x7F, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0xFF)

        val error = runCatching {
            WebSocketProtocol.readFrame(ByteArrayInputStream(header), maxPayload = 1024)
        }.exceptionOrNull()

        assertTrue(error!!.message!!.contains("exceeds"))
    }

    @Test
    fun `a truncated frame is an EOF rather than a short read`() {
        val error = runCatching {
            WebSocketProtocol.readFrame(ByteArrayInputStream(bytes(0x81, 0x05, 0x48, 0x65)))
        }.exceptionOrNull()

        assertTrue(error is java.io.EOFException)
    }

    @Test
    fun `a binary frame is refused, because this link speaks JSON text`() {
        val error = runCatching {
            WebSocketProtocol.readMessage(ByteArrayInputStream(bytes(0x82, 0x01, 0x00))) {}
        }.exceptionOrNull()

        assertTrue(error!!.message!!.contains("binary"))
    }

    @Test
    fun `a close reason too long for a control frame is truncated, not refused`() {
        // Failing to close cleanly because the *reason* was long is worse than
        // a shortened reason.
        val payload = WebSocketProtocol.closePayload(1000, "x".repeat(400))

        assertTrue(payload.size <= WebSocketProtocol.MAX_CONTROL_PAYLOAD)
        assertEquals(1000, WebSocketProtocol.closeCode(WebSocketProtocol.Frame(true, 0x8, payload)))
    }

    @Test
    fun `a key is sixteen random bytes, base64`() {
        val key = WebSocketProtocol.newKey(Random(9))
        assertEquals(24, key.length)
        assertEquals(16, java.util.Base64.getDecoder().decode(key).size)
    }

    /** Strip the mask bit and unmask the payload, as a server does on receipt. */
    private fun unmaskForServer(clientFrame: ByteArray): ByteArray {
        val lengthByte = clientFrame[1].toInt() and 0x7F
        val extra = when (lengthByte) {
            126 -> 2
            127 -> 8
            else -> 0
        }
        val maskAt = 2 + extra
        val mask = clientFrame.copyOfRange(maskAt, maskAt + 4)
        val body = clientFrame.copyOfRange(maskAt + 4, clientFrame.size)
        for (index in body.indices) {
            body[index] = (body[index].toInt() xor mask[index % 4].toInt()).toByte()
        }
        val header = clientFrame.copyOfRange(0, maskAt)
        header[1] = (header[1].toInt() and 0x7F).toByte()
        return header + body
    }
}
