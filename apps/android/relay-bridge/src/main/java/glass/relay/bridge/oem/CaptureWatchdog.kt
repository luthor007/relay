package glass.relay.bridge.oem

/**
 * Did the phone kill capture behind our back, and if so, when?
 *
 * Advice about OEM battery managers is worth very little on its own, because
 * the user has no reason to believe it applies to them until something has
 * already gone wrong — and when it does go wrong, the failure is silent. The
 * service is simply not running any more. Nothing crashes, nothing is logged,
 * and the first evidence is a hole in yesterday.
 *
 * So the service writes a heartbeat while it runs, and this reads it back:
 *
 *  - a gap longer than a few beats, while capture was supposed to be on, is a
 *    kill — the service does not stop quietly for any other reason
 *  - a gap that ends after a reboot with no beat immediately after it is the
 *    boot receiver failing to fire, which on MIUI and EMUI means Autostart was
 *    never granted
 *  - repeated gaps on a known-hostile manufacturer are worth escalating from a
 *    suggestion to a warning, because at that point it is not hypothetical
 *
 * Pure arithmetic over a list of timestamps, so the interesting cases — a kill
 * at 03:12, three kills in a week, a reboot that never restarted us — are all
 * reachable in a unit test instead of only reachable by leaving a phone on a
 * desk overnight.
 */
object CaptureWatchdog {

    /**
     * One beat. The service writes these while it holds the foreground.
     *
     * [captureIntended] records what the user asked for, not what happened, so
     * a gap can be told apart from a deliberate stop.
     */
    data class Beat(val atMs: Long, val captureIntended: Boolean = true)

    data class Gap(
        val fromMs: Long,
        val toMs: Long,
        /** True when the gap spans a reboot, per the boot timestamps supplied. */
        val spannedReboot: Boolean,
    ) {
        val durationMs: Long get() = toMs - fromMs
    }

    enum class Verdict {
        /** No gaps worth reporting. */
        Healthy,

        /** Capture was off; the gaps are the user's own doing. */
        NotRunning,

        /** Capture stopped without being asked to. Once. */
        Interrupted,

        /** It has happened more than once. Stop suggesting and start warning. */
        RepeatedlyKilled,

        /**
         * A reboot happened and capture did not come back.
         *
         * On the manufacturers in [OemPolicy] with `requiresAutostart`, this is
         * almost always a missing Autostart grant rather than a bug in the boot
         * receiver.
         */
        NotRestartedAfterReboot,
    }

    data class Report(
        val verdict: Verdict,
        val gaps: List<Gap>,
        val longestGapMs: Long,
        /** Copy the UI can show without rewording. */
        val message: String,
        /** Set when the manufacturer is a known offender and the diagnosis fits. */
        val advice: OemPolicy.Advice?,
    ) {
        val healthy: Boolean get() = verdict == Verdict.Healthy || verdict == Verdict.NotRunning
    }

    /**
     * @param beats in any order; sorted here.
     * @param expectedIntervalMs how often the service writes one.
     * @param rebootsAtMs boot timestamps observed since the first beat.
     * @param nowMs the current time, so an ongoing gap counts.
     */
    fun analyse(
        beats: List<Beat>,
        expectedIntervalMs: Long,
        nowMs: Long,
        rebootsAtMs: List<Long> = emptyList(),
        manufacturer: String? = null,
    ): Report {
        require(expectedIntervalMs > 0) { "heartbeat interval must be positive" }
        val advice = OemPolicy.adviceFor(manufacturer)

        val sorted = beats.sortedBy { it.atMs }
        if (sorted.isEmpty()) {
            return Report(Verdict.NotRunning, emptyList(), 0, "no capture recorded yet", advice)
        }

        // Three missed beats, not one. A single missed beat is a doze window or
        // a busy phone; three in a row is the process not existing.
        val threshold = expectedIntervalMs * 3

        val gaps = mutableListOf<Gap>()
        for (index in 1 until sorted.size) {
            val previous = sorted[index - 1]
            val current = sorted[index]
            if (!previous.captureIntended) continue
            val delta = current.atMs - previous.atMs
            if (delta > threshold) {
                gaps += Gap(previous.atMs, current.atMs, rebootedBetween(rebootsAtMs, previous.atMs, current.atMs))
            }
        }

        val last = sorted.last()
        val trailing = nowMs - last.atMs
        if (last.captureIntended && trailing > threshold) {
            gaps += Gap(last.atMs, nowMs, rebootedBetween(rebootsAtMs, last.atMs, nowMs))
        }

        if (gaps.isEmpty()) {
            val verdict = if (sorted.any { it.captureIntended }) Verdict.Healthy else Verdict.NotRunning
            return Report(
                verdict,
                emptyList(),
                0,
                if (verdict == Verdict.Healthy) "capture has been running without interruption" else "capture is off",
                advice,
            )
        }

        val longest = gaps.maxOf { it.durationMs }
        val rebootGap = gaps.lastOrNull { it.spannedReboot }

        val verdict = when {
            rebootGap != null -> Verdict.NotRestartedAfterReboot
            gaps.size > 1 -> Verdict.RepeatedlyKilled
            else -> Verdict.Interrupted
        }

        return Report(
            verdict = verdict,
            gaps = gaps,
            longestGapMs = longest,
            message = message(verdict, gaps, longest, advice),
            advice = advice,
        )
    }

    private fun rebootedBetween(reboots: List<Long>, fromMs: Long, toMs: Long): Boolean =
        reboots.any { it in (fromMs + 1) until toMs }

    private fun message(
        verdict: Verdict,
        gaps: List<Gap>,
        longestMs: Long,
        advice: OemPolicy.Advice?,
    ): String {
        val minutes = longestMs / 60_000
        val tail = advice?.let { " ${it.instruction}" }.orEmpty()
        return when (verdict) {
            Verdict.NotRestartedAfterReboot ->
                "Your phone restarted and Relay did not start capturing again.$tail"
            Verdict.RepeatedlyKilled ->
                "Your phone has stopped Relay ${gaps.size} times — the longest gap was $minutes minutes.$tail"
            Verdict.Interrupted ->
                "Capture stopped for $minutes minutes without being asked to.$tail"
            Verdict.Healthy -> "capture has been running without interruption"
            Verdict.NotRunning -> "capture is off"
        }
    }
}
