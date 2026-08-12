package glass.relay.bridge.link

import org.json.JSONArray
import org.json.JSONObject
import java.util.Random

/**
 * The phone ↔ `relayd` envelope. `docs/SYSTEM.md` §6.1, exactly:
 *
 *     { v: 1, id: "<uuid>", type: "<name>", at: <unix_ms>, payload: {...} }
 *
 * This is a port of `glasses/bridge/src/relayd.ts`, which is the reference
 * implementation, checked against `relayd/internal/api/wire.go`, which is what
 * actually answers. Where the two disagreed, the Go won — see [ServerFrame.ACK].
 *
 * `org.json` rather than a JSON library, for the same reason `ConnectorClient`
 * uses `HttpURLConnection`: this module carries no third-party dependencies, and
 * the parser ships inside the platform.
 */
const val LINK_VERSION: Int = 1

/**
 * The subprotocol both ends agree on. Bump it with the envelope version.
 *
 * `relayd` does not currently require it (`internal/api/server.go` accepts the
 * upgrade and authenticates with a bearer token), but sending it is what makes
 * a future version bump a clean refusal instead of a phone and a daemon
 * silently disagreeing about what `v: 2` means.
 */
const val LINK_SUBPROTOCOL: String = "relay.v1"

/** Phone → server. `docs/SYSTEM.md` §6.1's list, complete. */
object PhoneFrame {
    const val UTTERANCE = "utterance"
    const val TOUCH = "touch"
    const val WEAR = "wear"
    const val AUDIO_CHUNK = "audio.chunk"
    const val PHOTO = "photo"
    const val SESSION_COMMAND = "session.command"
    const val CONSENT_DECISION = "consent.decision"
    const val SYNC_OFFER = "sync.offer"

    val ALL: Set<String> = setOf(
        UTTERANCE, TOUCH, WEAR, AUDIO_CHUNK, PHOTO,
        SESSION_COMMAND, CONSENT_DECISION, SYNC_OFFER,
    )
}

/**
 * Server → phone.
 *
 * The six product frames of `SYSTEM.md` §6.1 plus the four the transport turns
 * out to need, all ten of which `relayd/internal/api/wire.go` implements today.
 * The four are not an invention of this file: §6.1 documents each of them and
 * why leaving it out made a documented behaviour unimplementable.
 */
object ServerFrame {
    const val SPEAK = "speak"
    const val UI_RENDER = "ui.render"
    const val SESSION_LIST = "session.list"
    const val CONFIRM_REQUEST = "confirm.request"
    const val CONNECTOR_PROPOSAL = "connector.proposal"
    const val DIGEST = "digest"

    /**
     * "Your frame landed."
     *
     * **`ack`, not `link.ack`.** The TypeScript invented `link.ack` with a
     * payload of `{ ids: [...] }` before `relayd` existed; the daemon
     * acknowledges one frame at a time with `{ re, ok }` (`wire.go`'s `Ack`).
     * A phone that prunes its outbox on a message the server never sends holds
     * every envelope it has ever sent until the socket drops, and then sends
     * them all again.
     */
    const val ACK = "ack"

    /**
     * "Your frame did not land, and here is why."
     *
     * Carries `re`, a `code`, and — for anything unbuilt — the milestone that
     * will build it. A phone told `not_implemented, M4` keeps the audio on the
     * device instead of deleting it.
     */
    const val ERROR = "error"

    /** A notification that arrives without speech. `ADAPTERS.md` §7, quiet hours. */
    const val NOTIFY = "notify"

    /** Retracts a [CONFIRM_REQUEST] whose question is already answered. */
    const val CONFIRM_RESOLVED = "confirm.resolved"

    val ALL: Set<String> = setOf(
        SPEAK, UI_RENDER, SESSION_LIST, CONFIRM_REQUEST, CONNECTOR_PROPOSAL, DIGEST,
        ACK, ERROR, NOTIFY, CONFIRM_RESOLVED,
    )
}

/** Error codes `relayd` puts in an [ServerFrame.ERROR] payload. */
object LinkErrorCodes {
    const val BAD_ENVELOPE = "bad_envelope"
    const val UNSUPPORTED_VERSION = "unsupported_version"
    const val UNKNOWN_TYPE = "unknown_type"
    const val BAD_PAYLOAD = "bad_payload"
    const val NOT_IMPLEMENTED = "not_implemented"
    const val NO_SUCH_SESSION = "no_such_session"
    const val UNSUPPORTED = "unsupported"
    const val FAILED = "failed"
}

/**
 * One envelope, in either direction.
 *
 * [payload] is whatever `org.json` produced — a [JSONObject], a [JSONArray], a
 * boxed primitive, or null when the frame carried none. Deliberately not a
 * typed union: `SYSTEM.md` §6.1 fixes the envelope, and the payload vocabulary
 * grows on the server's schedule. A phone that refuses a frame because its
 * payload has a field it has not heard of is a phone that breaks on every
 * daemon release.
 */
data class RelayEnvelope(
    val id: String,
    val type: String,
    val atMs: Long,
    val payload: Any? = null,
    val v: Int = LINK_VERSION,
) {
    /** The payload as an object, or null when it was absent or not one. */
    fun payloadObject(): JSONObject? = payload as? JSONObject

    fun toJson(): JSONObject {
        val json = JSONObject()
            .put("v", v)
            .put("id", id)
            .put("type", type)
            .put("at", atMs)
        // `put(key, null)` removes the key, which is what we want: the Go side
        // declares `payload,omitempty` and an explicit JSON null is a different
        // thing from an absent payload.
        if (payload != null) json.put("payload", payload)
        return json
    }

    fun serialise(): String = toJson().toString()
}

/** Why an envelope, an outbox or a socket said no. Never thrown at the caller. */
class LinkException(
    val code: Code,
    message: String,
    cause: Throwable? = null,
) : Exception(message, cause) {

    enum class Code {
        /** Inbound text was not a valid envelope. Reported, never fatal. */
        Malformed,

        /** The daemon speaks a different envelope version. */
        VersionMismatch,

        /** The outbox is full. The caller still owns the data. */
        OutboxFull,

        SocketFailed,

        /** The server refused a frame we sent, by name. */
        Refused,
    }
}

/**
 * Strict on purpose.
 *
 * An envelope that half-parses produces a UI that acts on a field it invented,
 * and §6.1 is a contract three languages have to implement identically. The
 * version check comes first so that a daemon from the future is one clear
 * refusal rather than a series of confusing ones.
 */
fun parseEnvelope(text: String): RelayEnvelope {
    val json = try {
        JSONObject(text)
    } catch (error: Exception) {
        throw LinkException(LinkException.Code.Malformed, "envelope is not a JSON object", error)
    }

    val version = if (json.has("v")) json.optInt("v", Int.MIN_VALUE) else Int.MIN_VALUE
    if (version != LINK_VERSION) {
        throw LinkException(
            LinkException.Code.VersionMismatch,
            "envelope v=${json.opt("v")}, this link speaks v=$LINK_VERSION",
        )
    }

    val id = json.optString("id", "")
    if (id.isEmpty() || json.opt("id") !is String) {
        throw LinkException(LinkException.Code.Malformed, "envelope.id must be a non-empty string")
    }
    val type = json.optString("type", "")
    if (type.isEmpty() || json.opt("type") !is String) {
        throw LinkException(LinkException.Code.Malformed, "envelope.type must be a non-empty string")
    }
    val at = json.opt("at")
    if (at !is Number) {
        throw LinkException(LinkException.Code.Malformed, "envelope.at must be a number")
    }

    val payload = json.opt("payload")
    return RelayEnvelope(
        id = id,
        type = type,
        atMs = at.toLong(),
        payload = if (payload == JSONObject.NULL) null else payload,
    )
}

/**
 * RFC 4122 v4 from an injected source of randomness.
 *
 * Not `java.util.UUID.randomUUID()`: that reads `SecureRandom` on every call,
 * and this is called once per utterance, per touch, per audio chunk. The bytes
 * come from wherever the caller says, so a test gets stable ids for free — and
 * the id is the server's dedupe key, so tests that assert on redelivery need to
 * be able to predict it.
 */
fun newEnvelopeId(random: Random): String {
    val bytes = ByteArray(16)
    random.nextBytes(bytes)
    bytes[6] = ((bytes[6].toInt() and 0x0f) or 0x40).toByte()
    bytes[8] = ((bytes[8].toInt() and 0x3f) or 0x80).toByte()
    val hex = StringBuilder(32)
    for (byte in bytes) hex.append("%02x".format(byte))
    return buildString {
        append(hex, 0, 8).append('-')
        append(hex, 8, 12).append('-')
        append(hex, 12, 16).append('-')
        append(hex, 16, 20).append('-')
        append(hex, 20, 32)
    }
}
