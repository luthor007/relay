package glass.relay.bridge.capture

import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Path B — the all-day pipeline. Consent and wear gate it; nothing else does.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class LocalRecordingControllerTest {

    private class FakeDevice(var failNextStart: Boolean = false) : RecordingDevice {
        val log = mutableListOf<String>()
        var recording = false
            private set

        override suspend fun startLocalRecording() {
            if (failNextStart) {
                failNextStart = false
                log += "start:failed"
                throw IllegalStateException("storage full")
            }
            recording = true
            log += "start"
        }

        override suspend fun stopLocalRecording() {
            recording = false
            log += "stop"
        }
    }

    private class FakeClock(var nowMs: Long = 1_700_000_000_000) {
        operator fun invoke(): Long = nowMs
    }

    @Test
    fun `wearing the glasses starts capture, taking them off stops it`() = runTest {
        val device = FakeDevice()
        val controller = LocalRecordingController(device, this)

        controller.setConsent(true)
        assertFalse("consent alone is not enough", device.recording)

        controller.setWorn(true)
        assertTrue(device.recording)
        assertEquals(1, controller.segments.size)

        controller.setWorn(false)
        assertFalse(device.recording)
        assertEquals(LocalRecordingController.StopReason.Removed, controller.segments.last().stopReason)
    }

    @Test
    fun `withdrawing consent stops capture immediately`() = runTest {
        val device = FakeDevice()
        val controller = LocalRecordingController(device, this)

        controller.setConsent(true)
        controller.setWorn(true)
        assertTrue(device.recording)

        controller.setConsent(false)

        assertFalse(device.recording)
        assertEquals(
            LocalRecordingController.StopReason.ConsentWithdrawn,
            controller.segments.last().stopReason,
        )
        assertEquals("waiting for consent", controller.state.value.blockedBy)
    }

    @Test
    fun `the reason capture is not running is always available in words`() = runTest {
        // "Not recording" with no explanation is the state that makes people
        // restart a recording that never stopped.
        val controller = LocalRecordingController(FakeDevice(), this)

        controller.setConsent(false)
        assertEquals("waiting for consent", controller.state.value.blockedBy)

        controller.setConsent(true)
        assertEquals("glasses are not being worn", controller.state.value.blockedBy)

        controller.setWorn(true)
        assertEquals(null, controller.state.value.blockedBy)

        controller.pause()
        assertEquals("paused", controller.state.value.blockedBy)
    }

    @Test
    fun `pause survives a wear cycle until it is resumed`() = runTest {
        val device = FakeDevice()
        val controller = LocalRecordingController(device, this)

        controller.setConsent(true)
        controller.setWorn(true)
        controller.pause()
        assertFalse(device.recording)

        controller.setWorn(false)
        controller.setWorn(true)
        assertFalse("a wear cycle must not undo a deliberate pause", device.recording)

        controller.resume()
        assertTrue(device.recording)
        controller.stop()
    }

    @Test
    fun `the day is cut into segments so the box can transcribe as it goes`() = runTest {
        val clock = FakeClock()
        val device = FakeDevice()
        val controller = LocalRecordingController(
            device,
            this,
            LocalRecordingController.Config(segmentSeconds = 900),
            clock::invoke,
        )

        controller.setConsent(true)
        controller.setWorn(true)
        assertEquals(1, controller.segments.size)

        repeat(4) {
            clock.nowMs += 900_000
            testScheduler.advanceTimeBy(900_000)
            testScheduler.runCurrent()
        }

        assertEquals("an hour at fifteen minutes a segment", 5, controller.segments.size)
        assertTrue("still recording after the rolls", device.recording)

        val closed = controller.segments.dropLast(1)
        assertTrue(closed.all { it.stopReason == LocalRecordingController.StopReason.SegmentRolled })
        assertTrue(closed.all { it.durationSeconds == 900 })
        controller.stop()
    }

    @Test
    fun `wear detection can be turned off for firmware that does not report it`() = runTest {
        // 0x0301 says whether the device has it. Gating on a signal that never
        // arrives means never recording at all.
        val device = FakeDevice()
        val controller = LocalRecordingController(
            device,
            this,
            LocalRecordingController.Config(gateOnWear = false),
        )

        controller.setConsent(true)

        assertTrue(device.recording)
        controller.stop()
    }

    @Test
    fun `a device that refuses to start says so instead of pretending`() = runTest {
        val device = FakeDevice(failNextStart = true)
        val controller = LocalRecordingController(device, this)

        controller.setConsent(true)
        controller.setWorn(true)

        assertFalse(controller.state.value.recording)
        assertNotNull(controller.state.value.blockedBy)
        assertTrue(controller.state.value.blockedBy!!.contains("refused"))
    }

    @Test
    fun `stopping for good ends the open segment and stops rolling`() = runTest {
        val device = FakeDevice()
        val controller = LocalRecordingController(
            device,
            this,
            LocalRecordingController.Config(segmentSeconds = 60),
        )

        controller.setConsent(true)
        controller.setWorn(true)
        controller.stop()

        assertFalse(device.recording)
        assertEquals(LocalRecordingController.StopReason.Stopped, controller.segments.last().stopReason)

        val segmentsAtStop = controller.segments.size
        testScheduler.advanceTimeBy(600_000)
        testScheduler.runCurrent()
        assertEquals("the roller must be dead", segmentsAtStop, controller.segments.size)
    }

    @Test
    fun `every closed segment has a duration the uploader can use`() = runTest {
        val clock = FakeClock()
        val device = FakeDevice()
        val controller = LocalRecordingController(device, this, clock = clock::invoke)

        controller.setConsent(true)
        controller.setWorn(true)
        clock.nowMs += 42_000
        controller.setWorn(false)

        assertEquals(42, controller.segments.last().durationSeconds)
    }
}
