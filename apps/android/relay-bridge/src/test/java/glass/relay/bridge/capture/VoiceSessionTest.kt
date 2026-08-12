package glass.relay.bridge.capture

import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Path A, and the one property that matters: the microphone is open only while
 * someone is talking to the assistant.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class VoiceSessionTest {

    /** Records what actually reached the device, in order. */
    private class FakeChannel(
        var failOnOpen: Boolean = false,
        var failOnClose: Boolean = false,
    ) : VoiceChannel {
        val log = mutableListOf<String>()
        var micOpen = false
            private set
        var maxSimultaneousOpens = 0
            private set

        override suspend fun openMicUplink() {
            if (failOnOpen) {
                log += "open:failed"
                throw IllegalStateException("device busy")
            }
            if (micOpen) maxSimultaneousOpens += 1
            micOpen = true
            log += "open"
        }

        override suspend fun closeMicUplink() {
            if (failOnClose) {
                log += "close:failed"
                throw IllegalStateException("device gone")
            }
            micOpen = false
            log += "close"
        }

        override suspend fun sendAudio(opus: ByteArray) {
            log += "send:${opus.size}"
        }

        override suspend fun setSpeakMode(mode: Int) {
            log += "mode:0x%02X".format(mode)
        }
    }

    private class RecordingSink : VoiceSession.AudioSink {
        val frames = mutableListOf<AudioFrame>()
        val closes = mutableListOf<Pair<Long, VoiceSession.CloseReason>>()
        override fun onFrame(frame: AudioFrame) { frames += frame }
        override fun onTurnClosed(turnId: Long, reason: VoiceSession.CloseReason) {
            closes += turnId to reason
        }
    }

    private fun frame(sequence: Int) =
        AudioFrame(ByteArray(60), sequence, deviceTimeMs = sequence * 20L)

    @Test
    fun `the mic closes when the utterance ends, not when the answer arrives`() = runTest {
        // The whole battery argument for Path A in one test. Closing at the end
        // of playback instead would hold both radios open through the model call.
        val channel = FakeChannel()
        val sink = RecordingSink()
        val session = VoiceSession(channel, this, sink)

        assertNull(session.trigger(VoiceSession.Trigger.Touch))
        assertTrue(channel.micOpen)

        session.endListening()
        assertFalse("mic must be closed before thinking", channel.micOpen)
        assertEquals(VoiceSession.State.Thinking, session.state.value)

        session.beginSpeaking()
        session.speak(ByteArray(3000))
        assertFalse("mic must still be closed while speaking", channel.micOpen)

        session.finish()
        assertEquals(VoiceSession.State.Idle, session.state.value)
    }

    @Test
    fun `audio offered outside a session is refused and counted`() = runTest {
        // The mechanical guarantee that nothing is captured outside a session
        // the user started.
        val channel = FakeChannel()
        val sink = RecordingSink()
        val session = VoiceSession(channel, this, sink)

        assertFalse(session.offerAudio(frame(1)))
        assertEquals(0, sink.frames.size)
        assertEquals(1, session.strayFrames)

        session.trigger(VoiceSession.Trigger.WakeWord)
        assertTrue(session.offerAudio(frame(2)))
        assertEquals(1, sink.frames.size)

        session.endListening()
        assertFalse("frames after the endpoint are stray too", session.offerAudio(frame(3)))
        assertEquals(2, session.strayFrames)
        assertEquals(1, session.acceptedFrames)
    }

    @Test
    fun `a stuck endpoint detector cannot hold the mic all day`() = runTest {
        val channel = FakeChannel()
        val session = VoiceSession(
            channel,
            this,
            RecordingSink(),
            VoiceSession.Config(maxListenMs = 30_000),
        )

        session.trigger(VoiceSession.Trigger.Touch)
        assertTrue(channel.micOpen)

        // Virtual time: nothing waits, but the watchdog fires exactly as it would.
        testScheduler.advanceTimeBy(29_000)
        testScheduler.runCurrent()
        assertTrue("still within the window", channel.micOpen)

        testScheduler.advanceTimeBy(2_000)
        testScheduler.runCurrent()
        assertFalse("the watchdog must close the uplink", channel.micOpen)
        assertEquals(VoiceSession.State.Thinking, session.state.value)
    }

    @Test
    fun `the watchdog does not fire after a normal endpoint`() = runTest {
        val channel = FakeChannel()
        val sink = RecordingSink()
        val session = VoiceSession(channel, this, sink, VoiceSession.Config(maxListenMs = 5_000))

        session.trigger(VoiceSession.Trigger.Touch)
        session.endListening()
        val closesAfterEndpoint = sink.closes.size

        testScheduler.advanceTimeBy(10_000)
        testScheduler.runCurrent()

        assertEquals("a cancelled watchdog must not close a second time", closesAfterEndpoint, sink.closes.size)
        session.cancel()
    }

    @Test
    fun `barge-in stops the reply and listens again`() = runTest {
        val channel = FakeChannel()
        val session = VoiceSession(channel, this, RecordingSink())

        session.trigger(VoiceSession.Trigger.Touch)
        session.endListening()
        session.beginSpeaking()
        assertEquals(VoiceSession.State.Speaking, session.state.value)

        // Interrupting is how people actually talk.
        assertNull(session.trigger(VoiceSession.Trigger.Touch))
        assertEquals(VoiceSession.State.Listening, session.state.value)
        assertTrue(channel.micOpen)
        session.cancel()
    }

    @Test
    fun `a second tap while already listening does not restart the sentence`() = runTest {
        val channel = FakeChannel()
        val session = VoiceSession(channel, this, RecordingSink())

        session.trigger(VoiceSession.Trigger.Touch)
        session.trigger(VoiceSession.Trigger.Touch)

        assertEquals(1, channel.log.count { it == "open" })
        assertEquals(0, channel.maxSimultaneousOpens)
        session.cancel()
    }

    @Test
    fun `a failed open leaves nothing hanging`() = runTest {
        val channel = FakeChannel(failOnOpen = true)
        val session = VoiceSession(channel, this, RecordingSink())

        val rejection = session.trigger(VoiceSession.Trigger.AppButton)

        assertNotNull("the caller has to know", rejection)
        assertEquals(VoiceSession.State.Idle, session.state.value)
        assertFalse(channel.micOpen)
        // We still tried to close: an open that throws halfway is not proof the
        // uplink stayed shut.
        assertTrue(channel.log.contains("close"))
    }

    @Test
    fun `a close that throws is reported, not swallowed`() = runTest {
        // The one failure whose consequence is a microphone we believe is shut
        // and is not. It must reach the caller.
        val channel = FakeChannel()
        val sink = RecordingSink()
        val session = VoiceSession(channel, this, sink)

        session.trigger(VoiceSession.Trigger.Touch)
        channel.failOnClose = true

        val error = runCatching { session.endListening() }.exceptionOrNull()
        assertNotNull("endListening must surface a failed close", error)
        assertTrue(sink.closes.any { it.second == VoiceSession.CloseReason.Failed })
    }

    @Test
    fun `no consent means the microphone is never opened`() = runTest {
        val channel = FakeChannel()
        val session = VoiceSession(
            channel,
            this,
            RecordingSink(),
            consentGranted = { false },
        )

        val rejection = session.trigger(VoiceSession.Trigger.WakeWord)

        assertNotNull(rejection)
        assertTrue(channel.log.isEmpty())
        assertEquals(VoiceSession.State.Idle, session.state.value)
    }

    @Test
    fun `the device is told we are working before the answer exists`() = runTest {
        // SYSTEM.md §7b: most of the perceived-latency budget is bought here.
        val channel = FakeChannel()
        val session = VoiceSession(channel, this, RecordingSink())

        session.trigger(VoiceSession.Trigger.Touch)

        assertTrue(
            "expected a thinking mode, got ${channel.log}",
            channel.log.contains("mode:0x04"),
        )
        session.cancel()
    }

    @Test
    fun `a long reply keeps the speak mode alive`() = runTest {
        // QGAISpeakMode.Hold. Dropping it mid-reply truncates the answer.
        val channel = FakeChannel()
        val session = VoiceSession(channel, this, RecordingSink())

        session.trigger(VoiceSession.Trigger.Touch)
        session.endListening()
        session.beginSpeaking()
        session.speak(ByteArray(3000))
        session.speak(ByteArray(3000))

        assertEquals(2, channel.log.count { it == "mode:0x02" })
        session.finish()
    }

    @Test
    fun `cancel closes the mic from any state`() = runTest {
        val channel = FakeChannel()
        val session = VoiceSession(channel, this, RecordingSink())

        session.trigger(VoiceSession.Trigger.Touch)
        session.cancel()

        assertFalse(channel.micOpen)
        assertEquals(VoiceSession.State.Idle, session.state.value)
    }
}
