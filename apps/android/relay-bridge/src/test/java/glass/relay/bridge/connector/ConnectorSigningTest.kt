package glass.relay.bridge.connector

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Cross-platform signing parity.
 *
 * The vectors below were produced by `connector/src/protocol.ts`, which is the
 * source of truth. Three independent implementations sign these requests — Node,
 * Kotlin and Swift — and a single character of disagreement in the payload
 * construction makes every request from that platform fail authentication with
 * an error that says nothing about why.
 *
 * Regenerate with:
 *
 *     cd connector && node --input-type=module -e '
 *       import { signBody } from "./src/protocol.ts";
 *       import { createHash } from "node:crypto";
 *       const bodyHash = createHash("sha256").update("chunk-zero-payload").digest("hex");
 *       console.log(signBody("relay-test-signing-key", "POST",
 *         "/v1/sessions/abc/chunks", bodyHash, 1700000000000));
 *     '
 */
class ConnectorSigningTest {

    @Test
    fun `sha256 of the body matches the reference implementation`() {
        assertEquals(BODY_HASH, ConnectorClient.sha256Hex(BODY.toByteArray()))
    }

    @Test
    fun `signature matches the reference implementation byte for byte`() {
        val signature = ConnectorClient.sign(
            signingKey = SIGNING_KEY,
            method = "POST",
            path = PATH,
            bodyHash = BODY_HASH,
            timestampMs = TIMESTAMP,
        )
        assertEquals(SIGNATURE, signature)
    }

    @Test
    fun `the method is part of the signature`() {
        val asGet = ConnectorClient.sign(SIGNING_KEY, "GET", PATH, BODY_HASH, TIMESTAMP)
        assertFalse(
            "signing must bind the method, or an upload can be replayed as a different verb",
            asGet == SIGNATURE,
        )
    }

    @Test
    fun `the path is part of the signature`() {
        val elsewhere = ConnectorClient.sign(
            SIGNING_KEY, "POST", "/v1/sessions/other/chunks", BODY_HASH, TIMESTAMP,
        )
        assertFalse(
            "signing must bind the path, or a chunk can be replayed into another session",
            elsewhere == SIGNATURE,
        )
    }

    @Test
    fun `the method is uppercased before signing`() {
        // The reference implementation uppercases; HttpURLConnection does not
        // guarantee the case it reports, so this pins our side of that contract.
        assertEquals(
            ConnectorClient.sign(SIGNING_KEY, "post", PATH, BODY_HASH, TIMESTAMP),
            SIGNATURE,
        )
    }

    @Test
    fun `hex encoding is lower case and zero padded`() {
        // A byte below 0x10 rendered as one nibble is the classic way two HMAC
        // implementations agree on the digest and disagree on the string.
        val hash = ConnectorClient.sha256Hex(ByteArray(0))
        assertEquals(64, hash.length)
        assertEquals(hash.lowercase(), hash)
        assertTrue(hash.all { it.isDigit() || it in 'a'..'f' })
    }

    private companion object {
        const val SIGNING_KEY = "relay-test-signing-key"
        const val BODY = "chunk-zero-payload"
        const val PATH = "/v1/sessions/abc/chunks"
        const val TIMESTAMP = 1_700_000_000_000L

        const val BODY_HASH = "21a542316a0d20d4264d3846a4d94a96d7e477f51407b30338f9d1e5f02b0cd9"
        const val SIGNATURE = "ccc92ba48607d71dd0ee2d2d0f1a542318e43a338126b3e3a6cde7e7085f674b"
    }
}
