package glass.relay.bridge.link

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The reconnect timing, pinned against `backoffMs` in
 * `glasses/bridge/src/relayd.ts`. The numbers are that function's, computed by
 * hand from the same formula — two ends of one link under one outage have to
 * behave the same way, and "roughly exponential" is not a specification.
 */
class BackoffTest {

    private val noJitter = BackoffOptions(jitter = 0.0)

    @Test
    fun `without jitter it doubles from the base to the ceiling`() {
        assertEquals(500L, backoffMs(0, noJitter))
        assertEquals(1_000L, backoffMs(1, noJitter))
        assertEquals(2_000L, backoffMs(2, noJitter))
        assertEquals(4_000L, backoffMs(3, noJitter))
        assertEquals(16_000L, backoffMs(5, noJitter))
        assertEquals(30_000L, backoffMs(6, noJitter))
    }

    @Test
    fun `the ceiling is a ceiling, however long the phone has been in a drawer`() {
        assertEquals(30_000L, backoffMs(60, noJitter))
        assertEquals(30_000L, backoffMs(Int.MAX_VALUE, noJitter))
    }

    @Test
    fun `jitter subtracts, so the ceiling stays a real ceiling`() {
        // roll = 1 is the full interval, roll = 0 is half of it at jitter 0.5.
        // Adding instead would make maxMs an average and let a fleet exceed it.
        val options = BackoffOptions(jitter = 0.5)
        assertEquals(30_000L, backoffMs(10, options, roll = 1.0))
        assertEquals(15_000L, backoffMs(10, options, roll = 0.0))
        assertEquals(22_500L, backoffMs(10, options, roll = 0.5))
    }

    @Test
    fun `a full-jitter interval can be anything up to the exponential`() {
        val options = BackoffOptions(jitter = 1.0)
        assertEquals(0L, backoffMs(2, options, roll = 0.0))
        assertEquals(2_000L, backoffMs(2, options, roll = 1.0))
    }

    @Test
    fun `a roll outside zero to one is clamped rather than trusted`() {
        val options = BackoffOptions(jitter = 0.5)
        assertEquals(backoffMs(3, options, roll = 1.0), backoffMs(3, options, roll = 4.2))
        assertEquals(backoffMs(3, options, roll = 0.0), backoffMs(3, options, roll = -1.0))
    }

    @Test
    fun `a negative attempt is the first attempt, not an error`() {
        assertEquals(backoffMs(0, noJitter), backoffMs(-3, noJitter))
    }

    @Test
    fun `the whole point - two phones on the same outage do not retry together`() {
        // An access point reboots and every phone in the building reconnects at
        // once. Without jitter they all hit the one box at 1 s, 2 s, 4 s.
        val options = BackoffOptions(jitter = 0.5)
        val spread = (0..10).map { backoffMs(5, options, roll = it / 10.0) }.toSet()

        assertTrue("expected a spread of delays, got $spread", spread.size > 5)
    }
}
