package glass.relay.bridge.link

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Reaching the box from outside the house.
 *
 * `SYSTEM.md` §7's relay was built on the box (`internal/relaylink`) and on the
 * console (`console/src/cloud`) and never on the phone — so a phone that left
 * the house stopped reaching its box, forever, with the link retrying a LAN
 * hostname that does not resolve. These are the decision half of the fix; the
 * rotation half is in `RelaydLinkTest`.
 */
class BoxAddressTest {

    private val home = BoxAddress(
        direct = "ws://relay.local:8765",
        relayUrl = "wss://rz.relay.glass",
        boxId = "box-abc123",
    )

    @Test
    fun `the direct address is always tried first`() {
        // §7's argument that the relay is affordable rests on the day's audio
        // never crossing it, and that is only true if the phone prefers the LAN
        // whenever it has one. A phone that reached for the relay because it
        // answered first would move a household's traffic onto our bill and out
        // of their house, silently.
        val list = endpoints(home)
        assertEquals(2, list.size)
        assertEquals(BoxRoute.Direct, list[0].route)
        assertEquals("ws://relay.local:8765", list[0].url)
        assertEquals(BoxRoute.Relay, list[1].route)
    }

    @Test
    fun `the relayed address is the route the relay actually serves`() {
        val relayed = endpoints(home).first { it.route == BoxRoute.Relay }
        assertEquals("wss://rz.relay.glass/rz/v1/connect/box-abc123", relayed.url)

        // No protocol label, and the absence is load-bearing: an empty one is
        // the phone's protocol on the far side (`relaylink.serverFor("")`), and
        // it is what lets a box built before the label existed still answer.
        assertTrue("the phone asked for a protocol: ${relayed.url}", !relayed.url.contains("?p="))

        // And no credential in the URL. The socket carries none; pairing gave
        // the phone a derived credential it presents inside the connection.
        for (leak in listOf("token", "secret", "auth", "key=")) {
            assertTrue("the URL carries $leak", !relayed.url.lowercase().contains(leak))
        }
    }

    @Test
    fun `a trailing slash on the relay does not double up`() {
        val relayed = endpoints(home.copy(relayUrl = "wss://rz.relay.glass/")).last()
        assertEquals("wss://rz.relay.glass/rz/v1/connect/box-abc123", relayed.url)
    }

    @Test
    fun `a box id that means something in a path is escaped`() {
        // `config.Relay.BoxID` is configurable so a fleet can be named
        // deliberately. The day somebody names one `office/spare`, the phone has
        // to dial one machine rather than a path that means something else.
        val relayed = endpoints(home.copy(boxId = "office/spare")).last()
        assertEquals("wss://rz.relay.glass/rz/v1/connect/office%2Fspare", relayed.url)
    }

    @Test
    fun `a half-configured relay is refused rather than guessed at`() {
        // A relay with no box id dials `/rz/v1/connect/` and is told there is no
        // such machine, which reads as the box being off.
        assertEquals(
            listOf(BoxRoute.Direct),
            endpoints(home.copy(boxId = null)).map { it.route },
        )
        assertEquals(
            listOf(BoxRoute.Direct),
            endpoints(home.copy(relayUrl = "")).map { it.route },
        )
    }

    @Test
    fun `plaintext to a public relay is refused`() {
        // The same rule `config.Relay.validate` makes on the box, made again
        // here — the phone's copy of this URL arrived over pairing and was
        // never validated by anything else. An unencrypted hop to a public host
        // leaks who is talking to whom, which is the metadata §7 is otherwise
        // careful about.
        assertEquals(
            listOf(BoxRoute.Direct),
            endpoints(home.copy(relayUrl = "ws://rz.relay.glass")).map { it.route },
        )
        // Loopback is exempt, so a local relay works in development.
        assertEquals(
            listOf(BoxRoute.Direct, BoxRoute.Relay),
            endpoints(home.copy(relayUrl = "ws://127.0.0.1:8080")).map { it.route },
        )
    }

    @Test
    fun `a cloud box has only the relay, and a LAN box only the direct address`() {
        assertEquals(
            listOf(BoxRoute.Relay),
            endpoints(home.copy(direct = null)).map { it.route },
        )
        assertEquals(
            listOf(BoxRoute.Direct),
            endpoints(BoxAddress(direct = "ws://relay.local:8765")).map { it.route },
        )
    }

    @Test
    fun `nothing configured is an empty list, not an address to retry`() {
        // The caller must read this as "do not open a link". A phone
        // reconnecting to a URL that does not exist is a battery complaint with
        // no upside, which is why `RelayCaptureService.openLink` already
        // declines when there is no box.
        assertTrue(endpoints(BoxAddress()).isEmpty())
        assertTrue(endpoints(BoxAddress(direct = "  ")).isEmpty())
        assertTrue(endpoints(BoxAddress(direct = "http://relay.local")).isEmpty())
        assertTrue(endpoints(BoxAddress(direct = "ws://")).isEmpty())
    }
}

/**
 * Whether there is a box to connect to at all.
 *
 * Split out of the preferences that hold the values, because the decision has a
 * case that is easy to get wrong in a way nothing on a phone would report.
 */
class CanOpenLinkTest {

    @Test
    fun `a cloud box has no LAN address and must still open a link`() {
        // The bug this exists to prevent: a check written as "is there a direct
        // URL" means a paying customer's phone never connects, silently.
        assertTrue(
            canOpenLink(
                "t0ken",
                BoxAddress(relayUrl = "wss://rz.relay.glass", boxId = "box-abc"),
            ),
        )
    }

    @Test
    fun `a token with nowhere to send it is not a configured box`() {
        assertTrue(!canOpenLink("t0ken", BoxAddress()))
        assertTrue(!canOpenLink("t0ken", BoxAddress(relayUrl = "wss://rz.relay.glass")))
        assertTrue(!canOpenLink("", BoxAddress(direct = "ws://relay.local:8765")))
        assertTrue(!canOpenLink("  ", BoxAddress(direct = "ws://relay.local:8765")))
    }

    @Test
    fun `a LAN box with no relay is configured, which is the common case`() {
        assertTrue(canOpenLink("t0ken", BoxAddress(direct = "ws://relay.local:8765")))
    }
}
