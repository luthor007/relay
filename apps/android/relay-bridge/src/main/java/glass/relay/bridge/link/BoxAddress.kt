package glass.relay.bridge.link

/**
 * Where the box is, and in what order to try.
 *
 * `SYSTEM.md` §7: a machine in a house is not reachable from a phone on
 * cellular, and the answer is a relay both sides dial out to. Until this file
 * existed the phone had one address — whatever pairing wrote — so a phone that
 * left the house simply stopped reaching its box, forever, with the link
 * retrying a LAN hostname that does not resolve. The relay was built on the box
 * and on the console and never on the thing it was built for.
 *
 * This is only the decision. The socket is [RelaydLink]'s, and it is deliberately
 * split that way: an address list is pure and can be tested exhaustively, and a
 * reconnect loop is neither.
 *
 * ## The order is the design
 *
 * **Direct first, always.** §7's argument that the relay is affordable rests on
 * the day's audio never crossing it — *"bulk sync is phone ↔ glasses over WiFi,
 * then phone → server over the LAN they are usually both on at night"*. That is
 * only true if the phone prefers the LAN whenever it has one. A phone that
 * reached for the relay because it happened to answer first would move a
 * household's traffic onto our bill and out of their house, silently.
 *
 * **The relay is a fallback, not a replacement.** So the two alternate rather
 * than the relay winning once it works: a phone that failed over at the office
 * has to come home to the LAN by itself, and it does, because every other
 * attempt is the direct address again.
 *
 * **Neither configured is not an error.** A phone that has not paired has no box
 * and opens nothing — see `RelayCaptureService.openLink`, which already declines
 * to burn battery on a URL that does not exist.
 */

/** How the box is reached. Carried so a UI can say which, and it should. */
enum class BoxRoute {
    /** Straight to the box on the local network. */
    Direct,

    /** Through the rendezvous relay, because the phone is elsewhere. */
    Relay,
}

/** One address to dial, and what it means. */
data class BoxEndpoint(val url: String, val route: BoxRoute)

/**
 * What pairing wrote down.
 *
 * All three are nullable because all three are genuinely optional: a box on a
 * LAN with no relay configured has only [direct], a cloud box has only the relay
 * pair, and a home box with the relay turned on has all three.
 */
data class BoxAddress(
    /** The box's own URL, e.g. `ws://relay.local:8765`. */
    val direct: String? = null,
    /** The relay's base, e.g. `wss://rz.relay.glass`. */
    val relayUrl: String? = null,
    /** This box's durable name at the relay. Not a secret — see `config.Relay`. */
    val boxId: String? = null,
)

/**
 * endpoints turns an address into the list [RelaydLink] rotates through.
 *
 * Empty means there is nothing to dial, which the caller must treat as "do not
 * open a link" rather than as a failure.
 */
fun endpoints(address: BoxAddress): List<BoxEndpoint> {
    val out = mutableListOf<BoxEndpoint>()

    val direct = address.direct?.trim()?.trimEnd('/').orEmpty()
    if (direct.isNotEmpty() && isSocketUrl(direct)) {
        out += BoxEndpoint(direct, BoxRoute.Direct)
    }

    val relay = relayEndpoint(address)
    if (relay != null) out += relay

    return out
}

/**
 * relayEndpoint builds the relayed address, or returns null.
 *
 * The path is `/rz/v1/connect/{id}` with **no protocol label**, which is what
 * makes this the phone's protocol on the far side: `relaylink.serverFor("")`
 * returns the session server, and the console appends `?p=console.v1` to ask for
 * the other one. The absence is load-bearing, and it is what lets a box built
 * before the label existed still serve a phone.
 *
 * The route is spelled out in three languages — here, in
 * `console/src/cloud/box.ts`, and in `relayd/internal/rendezvous/ws.go` — and the
 * Go one is the contract. A change there is a change in all three.
 */
private fun relayEndpoint(address: BoxAddress): BoxEndpoint? {
    val base = address.relayUrl?.trim()?.trimEnd('/').orEmpty()
    val id = address.boxId?.trim().orEmpty()

    // Half-configured is refused rather than guessed at. A relay with no box id
    // would dial `/rz/v1/connect/` and be told there is no such machine, which
    // reads as the box being off.
    if (base.isEmpty() || id.isEmpty()) return null
    if (!isSocketUrl(base)) return null

    // Plaintext to a public host leaks who is talking to whom, which is the
    // metadata §7 is otherwise careful about — the same refusal
    // `config.Relay.validate` makes on the box, made again here because the
    // phone's copy of this URL arrived over pairing and was never validated by
    // anything else.
    if (base.startsWith("ws://") && !isLoopbackUrl(base)) return null

    return BoxEndpoint("$base/rz/v1/connect/${encodePathSegment(id)}", BoxRoute.Relay)
}

private fun isSocketUrl(url: String): Boolean =
    (url.startsWith("ws://") || url.startsWith("wss://")) && hostOf(url).isNotEmpty()

private fun hostOf(url: String): String {
    val afterScheme = url.substringAfter("://", "")
    return afterScheme.substringBefore('/').substringBefore('?')
}

private fun isLoopbackUrl(url: String): Boolean {
    val host = hostOf(url).substringBeforeLast(':', hostOf(url))
    return host == "localhost" || host == "127.0.0.1" || host == "[::1]" || host == "::1"
}

/**
 * encodePathSegment percent-encodes a box id.
 *
 * Box ids are `box-` plus lowercase base32, so nothing needs escaping today —
 * and that is exactly why this is here rather than omitted. `config.Relay.BoxID`
 * is configurable so a fleet can be named deliberately, so the day somebody
 * names one `office/spare`, the phone must dial one machine rather than a path
 * that means something else.
 */
private fun encodePathSegment(value: String): String {
    val out = StringBuilder(value.length)
    for (byte in value.toByteArray(Charsets.UTF_8)) {
        val c = byte.toInt().toChar()
        if (c.isLetterOrDigit() && c.code < 128 || c == '-' || c == '.' || c == '_' || c == '~') {
            out.append(c)
        } else {
            out.append('%').append("%02X".format(byte.toInt() and 0xFF))
        }
    }
    return out.toString()
}

/**
 * canOpenLink is the rule for whether there is a box to connect to at all.
 *
 * A token and *somewhere to send it*. It lives here rather than beside the
 * preferences that hold the values because it is the decision, and the decision
 * has a case that is easy to get wrong: a cloud box has **no LAN address**, so a
 * check written as "is there a direct URL" means a paying customer's phone never
 * opens a link, on a device where nothing reports why.
 */
fun canOpenLink(token: String, address: BoxAddress): Boolean =
    token.isNotBlank() && endpoints(address).isNotEmpty()
