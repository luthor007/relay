package glass.relay.bridge.oem

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Knowing which phone this is, and noticing when it has killed us.
 */
class OemPolicyTest {

    // --- who kills what -----------------------------------------------------

    @Test
    fun `Build MANUFACTURER is not a controlled vocabulary`() {
        // Xiaomi ships both spellings; OnePlus has shipped "OnePlus" and
        // "Oneplus"; Redmi and POCO devices run MIUI under their own names.
        for (reported in listOf("Xiaomi", "xiaomi", "XIAOMI", "Redmi", "POCO")) {
            assertNotNull("$reported must be recognised", OemPolicy.adviceFor(reported))
        }
        assertNotNull(OemPolicy.adviceFor("OnePlus"))
        assertNotNull(OemPolicy.adviceFor("Oneplus"))
    }

    @Test
    fun `a stock device gets no extra instructions`() {
        assertNull(OemPolicy.adviceFor("Google"))
        assertNull(OemPolicy.adviceFor("motorola"))
        assertNull(OemPolicy.adviceFor(null))
        assertNull(OemPolicy.adviceFor(""))
    }

    @Test
    fun `the four manufacturers the brief names are all covered`() {
        for (name in listOf("Xiaomi", "Huawei", "Samsung", "OnePlus")) {
            val advice = OemPolicy.adviceFor(name)
            assertNotNull("$name must have advice", advice)
            assertTrue("$name needs a usable instruction", advice!!.instruction.length > 20)
            assertTrue("$name needs somewhere to send the user", advice.components.isNotEmpty())
        }
    }

    @Test
    fun `the ones that need Autostart are flagged, because a boot receiver without it is a decoration`() {
        assertTrue(OemPolicy.adviceFor("Xiaomi")!!.requiresAutostart)
        assertTrue(OemPolicy.adviceFor("Huawei")!!.requiresAutostart)
        assertTrue(OemPolicy.adviceFor("vivo")!!.requiresAutostart)
        // Samsung sleeps apps rather than blocking restart.
        assertEquals(false, OemPolicy.adviceFor("Samsung")!!.requiresAutostart)
    }

    @Test
    fun `severity separates kills from restricts`() {
        assertEquals(OemPolicy.Severity.Kills, OemPolicy.adviceFor("Xiaomi")!!.severity)
        assertEquals(OemPolicy.Severity.Restricts, OemPolicy.adviceFor("Samsung")!!.severity)
    }

    @Test
    fun `every known manufacturer resolves to itself`() {
        for (key in OemPolicy.known) {
            assertNotNull(key, OemPolicy.adviceFor(key))
        }
    }

    // --- did it happen ------------------------------------------------------

    private val minute = 60_000L

    private fun beats(count: Int, startMs: Long = 0, everyMs: Long = minute) =
        (0 until count).map { CaptureWatchdog.Beat(startMs + it * everyMs) }

    @Test
    fun `an uninterrupted run is healthy`() {
        val heartbeats = beats(60)
        val report = CaptureWatchdog.analyse(
            beats = heartbeats,
            expectedIntervalMs = minute,
            nowMs = heartbeats.last().atMs + 30_000,
        )
        assertEquals(CaptureWatchdog.Verdict.Healthy, report.verdict)
        assertTrue(report.healthy)
    }

    @Test
    fun `one missed beat is a doze window, not a kill`() {
        val heartbeats = listOf(
            CaptureWatchdog.Beat(0),
            CaptureWatchdog.Beat(minute),
            CaptureWatchdog.Beat(minute * 2 + 20_000),
            CaptureWatchdog.Beat(minute * 3 + 20_000),
        )
        val report = CaptureWatchdog.analyse(heartbeats, minute, minute * 3 + 40_000)
        assertEquals(CaptureWatchdog.Verdict.Healthy, report.verdict)
    }

    @Test
    fun `a long silence while capture was on is a kill`() {
        val heartbeats = beats(10) + beats(10, startMs = 90 * minute)
        val report = CaptureWatchdog.analyse(
            heartbeats,
            minute,
            nowMs = 100 * minute,
            manufacturer = "Xiaomi",
        )

        assertEquals(CaptureWatchdog.Verdict.Interrupted, report.verdict)
        assertEquals(1, report.gaps.size)
        assertTrue(report.longestGapMs > 60 * minute)
        assertTrue("the message must be showable as-is", report.message.contains("without being asked"))
        assertTrue("and must carry the OEM fix", report.message.contains("Autostart"))
    }

    @Test
    fun `two kills stop being a suggestion and become a warning`() {
        val heartbeats = beats(5) + beats(5, startMs = 60 * minute) + beats(5, startMs = 200 * minute)
        val report = CaptureWatchdog.analyse(heartbeats, minute, nowMs = 205 * minute)

        assertEquals(CaptureWatchdog.Verdict.RepeatedlyKilled, report.verdict)
        assertEquals(2, report.gaps.size)
        assertTrue(report.message.contains("2 times"))
    }

    @Test
    fun `a reboot with no restart is diagnosed as exactly that`() {
        // On MIUI and EMUI this is almost always a missing Autostart grant
        // rather than a bug in the boot receiver, and saying so saves a support
        // round trip.
        val heartbeats = beats(5) + beats(5, startMs = 120 * minute)
        val report = CaptureWatchdog.analyse(
            heartbeats,
            minute,
            nowMs = 125 * minute,
            rebootsAtMs = listOf(30 * minute),
            manufacturer = "Huawei",
        )

        assertEquals(CaptureWatchdog.Verdict.NotRestartedAfterReboot, report.verdict)
        assertTrue(report.message.contains("restarted"))
        assertTrue(report.gaps.single().spannedReboot)
    }

    @Test
    fun `an ongoing silence counts, not just gaps between beats`() {
        // The most important case: the service is not running *now*.
        val report = CaptureWatchdog.analyse(beats(5), minute, nowMs = 300 * minute)
        assertEquals(CaptureWatchdog.Verdict.Interrupted, report.verdict)
        assertTrue(report.longestGapMs > 290 * minute)
    }

    @Test
    fun `a deliberate stop is not a kill`() {
        val heartbeats = listOf(
            CaptureWatchdog.Beat(0, captureIntended = true),
            CaptureWatchdog.Beat(minute, captureIntended = false),
        )
        val report = CaptureWatchdog.analyse(heartbeats, minute, nowMs = 600 * minute)
        assertTrue("the user turning capture off is not a fault", report.healthy)
    }

    @Test
    fun `no history yet is not a fault`() {
        val report = CaptureWatchdog.analyse(emptyList(), minute, nowMs = 0)
        assertEquals(CaptureWatchdog.Verdict.NotRunning, report.verdict)
        assertTrue(report.healthy)
    }

    @Test
    fun `a stock phone gets the diagnosis without OEM boilerplate`() {
        val heartbeats = beats(5) + beats(5, startMs = 60 * minute)
        val report = CaptureWatchdog.analyse(heartbeats, minute, nowMs = 65 * minute, manufacturer = "Google")
        assertNull(report.advice)
        assertTrue(report.message.contains("without being asked"))
    }
}
