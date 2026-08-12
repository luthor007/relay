package glass.relay.bridge.link

import java.io.EOFException
import java.io.IOException
import java.io.InputStream
import java.net.URI
import java.security.MessageDigest
import java.util.Base64
import java.util.Random

/**
 * RFC 6455, the parts this link uses, as pure functions over bytes.
 *
 * ## Why this is hand-written
 *
 * `:relay-bridge` carries no third-party dependencies — the same rule that made
 * `ConnectorClient` use `HttpURLConnection` — and the Android framework has no
 * WebSocket client of its own. `java.net.http.WebSocket` is JDK 11 and is not on
 * Android; OkHttp arrives only with the vendor AAR, which is `compileOnly` and
 * absent on most machines.
 *
 * The upside is that the interesting half — the handshake and the framing — is
 * arithmetic over byte arrays, which means `tools/verify-jvm-logic.sh` can check
 * it on a plain JVM against the vectors in RFC 6455 §1.3 and §5.7. A dependency
 * would have been unverifiable here; this is not.
 *
 * ## What is deliberately not implemented
 *
 * No extensions (`permessage-deflate`), no binary messages, no client-side
 * fragmentation of outbound frames. Control traffic is small JSON envelopes;
 * `SYSTEM.md` §6.1 says so and `APPS-SCOPE.md` §4.3 keeps bulk transfer on the
 * separate resumable HTTP path. An inbound frame with an RSV bit set is
 * therefore a protocol error rather than something to guess at: we negotiated
 * no extension, so those bits have no agreed meaning.
 */
object WebSocketProtocol {

    /** RFC 6455 §1.3. Not a secret; it makes an accidental upgrade impossible. */
    const val GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

    const val OPCODE_CONTINUATION = 0x0
    const val OPCODE_TEXT = 0x1
    const val OPCODE_BINARY = 0x2
    const val OPCODE_CLOSE = 0x8
    const val OPCODE_PING = 0x9
    const val OPCODE_PONG = 0xA

    /** RFC 6455 §5.5: a control frame carries at most 125 bytes and is never split. */
    const val MAX_CONTROL_PAYLOAD = 125

    class ProtocolException(message: String) : IOException(message)

    data class Frame(
        val fin: Boolean,
        val opcode: Int,
        val payload: ByteArray,
    ) {
        val isControl: Boolean get() = opcode and 0x8 != 0

        // ByteArray in a data class needs these, or equality is identity-based
        // and every assertion in a test silently compares references.
        override fun equals(other: Any?): Boolean =
            this === other || (
                other is Frame &&
                    fin == other.fin &&
                    opcode == other.opcode &&
                    payload.contentEquals(other.payload)
                )

        override fun hashCode(): Int =
            (if (fin) 1 else 0) * 31 * 31 + opcode * 31 + payload.contentHashCode()
    }

    // --- handshake ------------------------------------------------------------

    /** `Sec-WebSocket-Accept`: base64(SHA-1(key + GUID)). RFC 6455 §4.2.2. */
    fun acceptFor(key: String): String {
        val digest = MessageDigest.getInstance("SHA-1").digest((key + GUID).toByteArray(Charsets.US_ASCII))
        return Base64.getEncoder().encodeToString(digest)
    }

    /** 16 random bytes, base64. RFC 6455 §4.1 — nonce, not a credential. */
    fun newKey(random: Random): String {
        val bytes = ByteArray(16)
        random.nextBytes(bytes)
        return Base64.getEncoder().encodeToString(bytes)
    }

    data class Endpoint(val host: String, val port: Int, val path: String, val secure: Boolean) {
        /** What goes in `Host:`. The port is omitted when it is the default. */
        val hostHeader: String
            get() = if ((secure && port == 443) || (!secure && port == 80)) host else "$host:$port"
    }

    /**
     * Split a `ws://` or `wss://` URL.
     *
     * Refuses `http`/`https` rather than quietly accepting them: the two schemes
     * are interchangeable at the transport level, and accepting both is how a
     * config ends up with a URL nobody can search the codebase for.
     */
    fun parseEndpoint(url: String): Endpoint {
        val uri = try {
            URI(url)
        } catch (error: Exception) {
            throw ProtocolException("not a URL: $url")
        }
        val secure = when (uri.scheme?.lowercase()) {
            "wss" -> true
            "ws" -> false
            else -> throw ProtocolException("relayd URLs are ws:// or wss://, got ${uri.scheme}")
        }
        val host = uri.host ?: throw ProtocolException("no host in $url")
        val port = if (uri.port != -1) uri.port else if (secure) 443 else 80
        val path = buildString {
            append(uri.rawPath?.takeIf { it.isNotEmpty() } ?: "/")
            uri.rawQuery?.let { append('?').append(it) }
        }
        return Endpoint(host, port, path, secure)
    }

    /**
     * The upgrade request.
     *
     * The token goes in `Authorization`, never in the query string: query
     * strings end up in proxy logs and crash reports, and this one is a bearer
     * credential for everything on the box. `relayd` accepts `?token=` too, for
     * browsers that cannot set a header on an `EventSource`; a phone can, so it
     * does.
     */
    fun handshakeRequest(endpoint: Endpoint, auth: LinkAuth, key: String): String = buildString {
        append("GET ").append(endpoint.path).append(" HTTP/1.1\r\n")
        append("Host: ").append(endpoint.hostHeader).append("\r\n")
        append("Upgrade: websocket\r\n")
        append("Connection: Upgrade\r\n")
        append("Sec-WebSocket-Key: ").append(key).append("\r\n")
        append("Sec-WebSocket-Version: 13\r\n")
        append("Sec-WebSocket-Protocol: ").append(LINK_SUBPROTOCOL).append("\r\n")
        if (auth.token.isNotEmpty()) {
            append("Authorization: ").append(auth.authorizationHeader()).append("\r\n")
        }
        if (auth.deviceId.isNotEmpty()) {
            append("X-Relay-Device: ").append(auth.deviceId).append("\r\n")
        }
        append("\r\n")
    }

    data class HandshakeResponse(val status: Int, val headers: Map<String, String>)

    /**
     * Check the daemon's answer.
     *
     * A 401 is called out by name because it is the one a user can act on: the
     * token in the app does not match the one `relayd` printed on start, and
     * "connection failed" would send them looking at their WiFi.
     */
    fun verifyHandshake(response: HandshakeResponse, key: String) {
        if (response.status == 401 || response.status == 403) {
            throw ProtocolException(
                "relayd refused the token (HTTP ${response.status}); it prints one on start",
            )
        }
        if (response.status != 101) {
            throw ProtocolException("expected HTTP 101 Switching Protocols, got ${response.status}")
        }
        val upgrade = response.headers["upgrade"]?.lowercase()
        if (upgrade != "websocket") {
            throw ProtocolException("server did not upgrade: Upgrade: $upgrade")
        }
        val accept = response.headers["sec-websocket-accept"]
        val expected = acceptFor(key)
        if (accept != expected) {
            // Not a formality. This is what proves the thing that answered
            // understood a WebSocket handshake rather than being a proxy that
            // echoed a 101 at us.
            throw ProtocolException("Sec-WebSocket-Accept was $accept, expected $expected")
        }
    }

    /** Reads the status line and headers, stopping at the blank line. */
    fun readHandshakeResponse(input: InputStream): HandshakeResponse {
        val statusLine = readLine(input) ?: throw EOFException("no response to the upgrade request")
        val parts = statusLine.split(' ', limit = 3)
        val status = parts.getOrNull(1)?.toIntOrNull()
            ?: throw ProtocolException("unparseable status line: $statusLine")

        val headers = HashMap<String, String>()
        while (true) {
            val line = readLine(input) ?: throw EOFException("headers ended without a blank line")
            if (line.isEmpty()) break
            val colon = line.indexOf(':')
            if (colon <= 0) continue
            headers[line.substring(0, colon).trim().lowercase()] = line.substring(colon + 1).trim()
        }
        return HandshakeResponse(status, headers)
    }

    private fun readLine(input: InputStream): String? {
        val buffer = StringBuilder()
        while (true) {
            val byte = input.read()
            if (byte == -1) return if (buffer.isEmpty()) null else buffer.toString()
            if (byte == '\n'.code) {
                if (buffer.isNotEmpty() && buffer.last() == '\r') buffer.setLength(buffer.length - 1)
                return buffer.toString()
            }
            buffer.append(byte.toChar())
            // A header line this long is not a header line. Refusing bounds the
            // memory a hostile or broken endpoint can make us allocate before
            // we have authenticated anything.
            if (buffer.length > 8192) throw ProtocolException("header line is absurdly long")
        }
    }

    // --- framing --------------------------------------------------------------

    /**
     * Encode one frame, masked.
     *
     * RFC 6455 §5.3: a client **must** mask, and a server must close the
     * connection on an unmasked client frame. The mask is not security — the key
     * travels in the frame — it exists so that a hostile page cannot make a
     * browser emit bytes that a broken proxy would mistake for a cached HTTP
     * response.
     */
    fun encodeFrame(opcode: Int, payload: ByteArray, maskKey: ByteArray, fin: Boolean = true): ByteArray {
        require(maskKey.size == 4) { "a mask key is four bytes" }
        if (opcode and 0x8 != 0 && payload.size > MAX_CONTROL_PAYLOAD) {
            throw ProtocolException("control frame payload is ${payload.size} bytes, max $MAX_CONTROL_PAYLOAD")
        }

        val header = ArrayList<Byte>(14)
        header += (((if (fin) 0x80 else 0x00) or (opcode and 0x0F)).toByte())
        val length = payload.size
        when {
            length <= 125 -> header += ((0x80 or length).toByte())
            length <= 0xFFFF -> {
                header += ((0x80 or 126).toByte())
                header += ((length ushr 8) and 0xFF).toByte()
                header += (length and 0xFF).toByte()
            }
            else -> {
                header += ((0x80 or 127).toByte())
                // 64-bit big-endian. The top four bytes are always zero here:
                // an envelope that needed 4 GB would have been refused long
                // before it reached a socket.
                for (shift in 56 downTo 0 step 8) {
                    header += ((length.toLong() ushr shift) and 0xFF).toByte()
                }
            }
        }
        for (byte in maskKey) header += byte

        val out = ByteArray(header.size + length)
        for (index in header.indices) out[index] = header[index]
        for (index in 0 until length) {
            out[header.size + index] = (payload[index].toInt() xor maskKey[index % 4].toInt()).toByte()
        }
        return out
    }

    /**
     * Read one frame.
     *
     * Throws [ProtocolException] on anything the connection cannot recover from,
     * and [EOFException] when the peer simply went away — which the caller
     * treats as an abnormal close, because it is.
     */
    fun readFrame(input: InputStream, maxPayload: Long = MAX_PAYLOAD_BYTES): Frame {
        val first = input.read()
        if (first == -1) throw EOFException("socket closed mid-frame")
        val fin = first and 0x80 != 0
        if (first and 0x70 != 0) {
            throw ProtocolException("RSV bits are set but no extension was negotiated")
        }
        val opcode = first and 0x0F

        val second = input.read()
        if (second == -1) throw EOFException("socket closed after the first header byte")
        val masked = second and 0x80 != 0
        if (masked) {
            // RFC 6455 §5.1: the server must not mask. A masked server frame is
            // either a broken implementation or something pretending to be one.
            throw ProtocolException("server frames must not be masked")
        }

        var length = (second and 0x7F).toLong()
        when (length) {
            126L -> length = readBigEndian(input, 2)
            127L -> {
                length = readBigEndian(input, 8)
                if (length < 0) throw ProtocolException("frame length has the high bit set")
            }
        }
        if (length > maxPayload) {
            // Bounded because the alternative is letting whatever answered the
            // socket choose how much memory this process allocates.
            throw ProtocolException("frame of $length bytes exceeds the $maxPayload limit")
        }
        if (opcode and 0x8 != 0) {
            if (!fin) throw ProtocolException("control frames cannot be fragmented")
            if (length > MAX_CONTROL_PAYLOAD) {
                throw ProtocolException("control frame carries $length bytes")
            }
        }

        val payload = ByteArray(length.toInt())
        readFully(input, payload)
        return Frame(fin, opcode, payload)
    }

    /**
     * Reassemble a message from frames, answering control frames on the way.
     *
     * Control frames may arrive **between** the fragments of a message
     * (RFC 6455 §5.4), so this cannot simply loop until FIN — it has to hand
     * pings and closes to [onControl] and carry on with the partial message.
     *
     * Returns null when the peer sent a close.
     */
    fun readMessage(
        input: InputStream,
        maxMessage: Long = MAX_PAYLOAD_BYTES,
        onControl: (Frame) -> Unit,
    ): String? {
        val buffer = java.io.ByteArrayOutputStream()
        var started = false
        while (true) {
            val frame = readFrame(input, maxMessage)
            if (frame.isControl) {
                if (frame.opcode == OPCODE_CLOSE) {
                    onControl(frame)
                    return null
                }
                onControl(frame)
                continue
            }
            when (frame.opcode) {
                OPCODE_TEXT -> {
                    if (started) throw ProtocolException("a new message started before the last one finished")
                    started = true
                }
                OPCODE_BINARY -> throw ProtocolException("this link speaks JSON text, not binary frames")
                OPCODE_CONTINUATION -> {
                    if (!started) throw ProtocolException("continuation frame with nothing to continue")
                }
                else -> throw ProtocolException("unknown opcode 0x%x".format(frame.opcode))
            }
            buffer.write(frame.payload)
            if (buffer.size() > maxMessage) {
                throw ProtocolException("message exceeds the $maxMessage byte limit")
            }
            if (frame.fin) return buffer.toString(Charsets.UTF_8.name())
        }
    }

    /** The two-byte status code in a close frame, or 1005 when it carried none. */
    fun closeCode(frame: Frame): Int =
        if (frame.payload.size < 2) {
            1005
        } else {
            ((frame.payload[0].toInt() and 0xFF) shl 8) or (frame.payload[1].toInt() and 0xFF)
        }

    fun closeReason(frame: Frame): String =
        if (frame.payload.size <= 2) "" else String(frame.payload, 2, frame.payload.size - 2, Charsets.UTF_8)

    fun closePayload(code: Int, reason: String): ByteArray {
        val reasonBytes = reason.toByteArray(Charsets.UTF_8)
        // Truncated rather than refused: failing to close cleanly because the
        // *reason* was long is a worse outcome than a shortened reason.
        val kept = reasonBytes.copyOf(minOf(reasonBytes.size, MAX_CONTROL_PAYLOAD - 2))
        val out = ByteArray(2 + kept.size)
        out[0] = ((code ushr 8) and 0xFF).toByte()
        out[1] = (code and 0xFF).toByte()
        kept.copyInto(out, 2)
        return out
    }

    private fun readBigEndian(input: InputStream, bytes: Int): Long {
        var value = 0L
        repeat(bytes) {
            val byte = input.read()
            if (byte == -1) throw EOFException("socket closed inside an extended length")
            value = (value shl 8) or byte.toLong()
        }
        return value
    }

    private fun readFully(input: InputStream, into: ByteArray) {
        var read = 0
        while (read < into.size) {
            val count = input.read(into, read, into.size - read)
            if (count == -1) throw EOFException("socket closed with ${into.size - read} bytes of payload left")
            read += count
        }
    }

    /**
     * One megabyte. `SYSTEM.md` §6.1's traffic is "small, bidirectional" — a
     * session list of a few hundred rows — and bulk transfer has its own path.
     */
    const val MAX_PAYLOAD_BYTES: Long = 1L * 1024 * 1024
}
