package glass.relay.bridge

import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import kotlin.random.Random

/**
 * Reconnection timing is the kind of thing that regresses silently and only
 * shows up as "it stopped recording", so it is pinned here.
 */
class ConnectionSupervisorTest {

    @Test
    fun `backoff never exceeds the cap`() {
        for (attempt in 0..40) {
            val wait = ConnectionSupervisor.backoffMs(attempt)
            assertTrue("attempt $attempt gave ${wait}ms", wait in 1_000..30_000)
        }
    }

    @Test
    fun `backoff grows before it saturates`() {
        // Deterministic RNG so this asserts the ceiling, not the sample.
        val alwaysMax = object : Random() {
            override fun nextBits(bitCount: Int) = 0
            override fun nextLong(from: Long, until: Long) = until - 1
        }
        val early = ConnectionSupervisor.backoffMs(0, alwaysMax)
        val mid = ConnectionSupervisor.backoffMs(3, alwaysMax)
        val late = ConnectionSupervisor.backoffMs(20, alwaysMax)

        assertTrue("early=$early mid=$mid", mid > early)
        assertEquals("should saturate at the cap", 30_000L, late)
    }

    @Test
    fun `backoff is jittered, not fixed`() {
        // A shared trigger (Bluetooth toggled, phone woke) reconnects every app
        // at once; identical delays turn that into a thundering herd.
        val samples = (0..50).map { ConnectionSupervisor.backoffMs(5) }.toSet()
        assertTrue("expected varied delays, got $samples", samples.size > 1)
    }

    @OptIn(ExperimentalCoroutinesApi::class)
    @Test
    fun `wearing the glasses starts recording, removing them stops it`() = runTest {
        val transport = MockGlassesTransport(connectDelayMs = 0)
        val supervisor = ConnectionSupervisor(transport, this)

        supervisor.start()
        transport.connect()

        // The supervisor's collector is a coroutine that has not been dispatched
        // yet. Events are delivered with tryEmit onto a zero-replay SharedFlow,
        // so anything emitted before it subscribes is dropped on the floor —
        // asserting without this passes only by luck of scheduling.
        testScheduler.runCurrent()

        transport.simulateWear(true)
        testScheduler.runCurrent()
        assertTrue("wear should begin capture", supervisor.capture.value.worn)

        transport.simulateWear(false)
        testScheduler.runCurrent()
        assertEquals(false, supervisor.capture.value.worn)

        supervisor.stop()
    }

    @OptIn(ExperimentalCoroutinesApi::class)
    @Test
    fun `losing the link does not clear the recording flag`() = runTest {
        // The glasses keep writing to their own storage while the phone is out
        // of range. Showing "not recording" there would be a lie, and would push
        // people to re-start a recording that never stopped.
        val transport = MockGlassesTransport(connectDelayMs = 0)
        val supervisor = ConnectionSupervisor(transport, this)

        supervisor.start()
        transport.connect()
        testScheduler.runCurrent()

        transport.simulateWear(true)
        transport.startLocalRecording()
        testScheduler.runCurrent()
        assertTrue("precondition: should be recording", supervisor.capture.value.recording)

        transport.simulateDisconnect()
        testScheduler.runCurrent()

        assertTrue(
            "recording must survive a dropped link",
            supervisor.capture.value.recording,
        )
        assertEquals(ConnectionState.Reconnecting, supervisor.state.value)
        supervisor.stop()
    }

    @OptIn(ExperimentalCoroutinesApi::class)
    @Test
    fun `stop cancels the event collector rather than leaking it`() = runTest {
        // Every start/stop cycle used to leave a live collector behind, because
        // observeEvents launched into the outer scope instead of the supervisor's
        // own job. Two of them mutating the same state is a race that only shows
        // up as capture flapping on and off.
        val transport = MockGlassesTransport(connectDelayMs = 0)
        val supervisor = ConnectionSupervisor(transport, this)

        supervisor.start()
        testScheduler.runCurrent()
        supervisor.stop()
        testScheduler.runCurrent()

        // With the collector gone, a wear event cannot move capture state.
        transport.simulateWear(true)
        testScheduler.runCurrent()

        assertEquals(false, supervisor.capture.value.worn)
    }
}
