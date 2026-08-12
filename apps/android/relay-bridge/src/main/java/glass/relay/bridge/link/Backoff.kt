package glass.relay.bridge.link

import kotlin.math.max
import kotlin.math.min

/**
 * Reconnect timing for a phone that keeps vanishing.
 *
 * Ported from `backoffMs` in `glasses/bridge/src/relayd.ts`, arithmetic
 * included, because the two ends of this link have to behave the same way under
 * the same outage and "roughly exponential" is not a specification.
 */
data class BackoffOptions(
    val baseMs: Long = 500,
    val maxMs: Long = 30_000,
    /** Fraction of the interval that is random. 0 = none, 0.5 = half. */
    val jitter: Double = 0.5,
)

/**
 * Exponential with jitter; [roll] in `[0, 1]` supplies the randomness.
 *
 * The jitter **subtracts** rather than adds, so [BackoffOptions.maxMs] is a real
 * ceiling rather than an average. It is not decoration either: every phone in a
 * building loses WiFi at the same moment when an access point reboots, and a
 * fleet that all retries at exactly 1 s, 2 s, 4 s is a self-inflicted
 * thundering herd on the one box in the user's house.
 *
 * The exponential is clamped *before* the jitter is applied, matching the
 * TypeScript. Clamping afterwards would make the last few attempts land on
 * exactly `maxMs` and reintroduce the synchronisation the jitter exists to break.
 */
fun backoffMs(attempt: Int, options: BackoffOptions = BackoffOptions(), roll: Double = 0.0): Long {
    val jitter = min(1.0, max(0.0, options.jitter))
    val clampedRoll = min(1.0, max(0.0, roll))
    val steps = max(0, attempt)
    // Doubling in a loop rather than pow(): attempt is unbounded — a phone left
    // in a drawer retries for days — and `base * 2^60` overflows a Double's
    // exact integer range long before the loop would matter.
    var exponential = options.baseMs.toDouble()
    var step = 0
    while (step < steps && exponential < options.maxMs) {
        exponential *= 2
        step += 1
    }
    exponential = min(options.maxMs.toDouble(), exponential)
    return Math.round(exponential * (1 - jitter + jitter * clampedRoll))
}
