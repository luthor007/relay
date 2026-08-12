package glass.relay.bridge.capture

import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class RecognitionTest {

    private fun frame(seq: Int, format: String = AudioFrame.OPUS) =
        AudioFrame(data = byteArrayOf(1, 2, 3), sequence = seq, deviceTimeMs = seq * 20L, format = format)

    // ---------------------------------------------------------------- policy

    @Test
    fun `a turn where nobody spoke is not a failure`() {
        // The two codes that are not errors, and the reason this function
        // exists: a tap on the glasses followed by thinking better of it is a
        // normal end to a turn, and apologising for it every time would make
        // the assistant exhausting.
        for (code in listOf(RecognitionPolicy.ERROR_NO_MATCH, RecognitionPolicy.ERROR_SPEECH_TIMEOUT)) {
            val outcome = RecognitionPolicy.fromAndroidError(code)
            assertTrue("code $code should be silence, not failure", outcome is Outcome.Heard)
            assertEquals("", (outcome as Outcome.Heard).text)
        }
    }

    @Test
    fun `a broken recogniser is reported in words a person can act on`() {
        val cases = mapOf(
            RecognitionPolicy.ERROR_INSUFFICIENT_PERMISSIONS to "permission",
            RecognitionPolicy.ERROR_NETWORK to "network",
            RecognitionPolicy.ERROR_RECOGNIZER_BUSY to "using speech recognition",
            RecognitionPolicy.ERROR_AUDIO to "could not read the audio",
        )
        for ((code, fragment) in cases) {
            val outcome = RecognitionPolicy.fromAndroidError(code)
            assertTrue("code $code should be unavailable", outcome is Outcome.Unavailable)
            val reason = (outcome as Outcome.Unavailable).reason
            assertTrue("reason for $code was \"$reason\"", reason.contains(fragment))
        }
    }

    @Test
    fun `an unknown code still says something`() {
        val outcome = RecognitionPolicy.fromAndroidError(9999)
        assertTrue(outcome is Outcome.Unavailable)
        assertTrue((outcome as Outcome.Unavailable).reason.contains("9999"))
    }

    @Test
    fun `the last non-empty partial wins, including a shorter one`() {
        // A recogniser that shortens has usually dropped a misheard word.
        assertEquals(
            "set a timer for ten",
            RecognitionPolicy.best(listOf("set the", "set the timer", "set a timer for ten")),
        )
        assertEquals("go", RecognitionPolicy.best(listOf("go now please", "go")))
    }

    @Test
    fun `an empty revision never beats real text`() {
        // A recogniser resetting mid-turn must not erase what it already heard.
        assertEquals("deploy staging", RecognitionPolicy.best(listOf("deploy staging", "", "   ")))
        assertEquals("", RecognitionPolicy.best(emptyList()))
    }

    @Test
    fun `partials are not sent twice or one character at a time`() {
        assertTrue(RecognitionPolicy.shouldEmit("", "check staging"))
        assertFalse("the same text twice", RecognitionPolicy.shouldEmit("check staging", "check staging"))
        assertFalse("whitespace-only change", RecognitionPolicy.shouldEmit("check staging", " check staging "))
        assertFalse("a single character", RecognitionPolicy.shouldEmit("", "a"))
        assertFalse("nothing", RecognitionPolicy.shouldEmit("", "   "))
    }

    // ---------------------------------------------------------------- decode

    @Test
    fun `the passthrough decoder refuses opus rather than guessing`() {
        val d = PassthroughPcm()
        // The whole reason this refuses: a decoder that returned Opus bytes
        // unchanged gives a recogniser that hears static and reports silence,
        // and that is indistinguishable from a quiet room.
        assertNull(d.toPcm16(frame(1, AudioFrame.OPUS)))
        assertNull(d.toPcm16(frame(2, "aac")))
        assertEquals(3, d.toPcm16(frame(3, AudioFrame.PCM16))?.size)
    }

    @Test
    fun `the decoder describes the pipe it fills`() {
        // These two values are what the recogniser tells the platform the audio
        // is. If they ever disagree with what the decoder emits, the transcript
        // is garbage — so they come from one place.
        val d = PassthroughPcm(sampleRateHz = 16_000, channelCount = 1)
        assertEquals(16_000, d.sampleRateHz)
        assertEquals(1, d.channelCount)
    }

    // ----------------------------------------------------------------- mock

    @Test
    fun `the mock recogniser runs a turn`() = runBlocking {
        val r = MockRecognizer(listOf("check staging", "and deploy it"))
        assertFalse(r.isRunning)

        r.start()
        assertTrue(r.isRunning)
        r.append(frame(1))
        r.append(frame(2))
        assertEquals(2, r.framesSeen)
        assertEquals("check staging", r.finish())
        assertFalse(r.isRunning)

        r.start()
        assertEquals("and deploy it", r.finish())
    }

    @Test
    fun `a mock recogniser accepts no audio outside a turn`() {
        // The same rule VoiceSession enforces on the other side of the seam:
        // no audio is captured outside a session the user started, and "was it
        // listening?" is the entire trust surface.
        val r = MockRecognizer()
        r.append(frame(1))
        assertEquals(0, r.framesSeen)

        r.start()
        r.append(frame(2))
        r.cancel()
        r.append(frame(3))
        assertEquals(1, r.framesSeen)
    }

    @Test
    fun `finishing without starting is empty rather than an error`() = runBlocking {
        assertEquals("", MockRecognizer(listOf("never said")).finish())
    }

    @Test
    fun `running out of scripted transcripts is silence`() = runBlocking {
        val r = MockRecognizer(listOf("one"))
        r.start(); assertEquals("one", r.finish())
        r.start(); assertEquals("", r.finish())
    }
}
