package glass.relay.bridge.consent

import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Test
import java.io.File

/**
 * A drift guard for the thing that made [ConsentPolicy] worthless: no callers.
 *
 * The policy was a well-tested state machine that no production code read. What
 * gated capture was a plain boolean in `SharedPreferences`, so
 * `ARCHITECTURE.md` §6 — "capture defaults to off in a new location or with new
 * voices present, until confirmed" — was enforced nowhere, while a reviewer
 * reading the tree would have concluded it was. That is worse than having no
 * policy at all, because it reads as compliance.
 *
 * `RelayCaptureService` uses platform APIs, so `verify-jvm-logic.sh` cannot
 * compile it and no ordinary test can reach it. This reads the source instead —
 * the same trick `CommandCatalogTest` uses to re-parse the Python command table,
 * and for the same reason: an invariant that nothing checks is an invariant that
 * gets deleted on a quiet afternoon.
 *
 * (A file naming the platform package anywhere, prose included, is dropped from
 * the verified set by that script's own rule. Hence the circumlocution.)
 *
 * It asserts the *shape* of the wiring, not its behaviour; `ConsentGateTest`
 * covers the behaviour. If this file starts failing, the question to ask is
 * "what is gating capture now, and does it still ask in a new place?"
 */
class ConsentWiringTest {

    private fun service(): String? = repoFile(SERVICE_PATH)?.readText()

    @Test
    fun `the capture service reads the consent gate`() {
        val source = service() ?: return skip()

        assertTrue(
            "RelayCaptureService must import ConsentGate — a policy with no callers is not a policy",
            source.contains("import glass.relay.bridge.consent.ConsentGate"),
        )
        assertTrue(
            "the service must build a ConsentGate",
            source.contains("ConsentGate("),
        )
    }

    @Test
    fun `the recording controller is gated on the gate's verdict, not on a stored boolean`() {
        val source = service() ?: return skip()

        assertTrue(
            "LocalRecordingController's consent must come from the gate",
            Regex("""recording\.setConsent\(\s*(verdict\.capture|consent\.verdict\.value\.capture)""")
                .containsMatchIn(source),
        )
        assertTrue(
            "nothing may gate capture on preferences.consentGranted again — it cannot say *where* " +
                "consent was given, which is the whole of ARCHITECTURE.md §6",
            !source.contains("setConsent(preferences.consentGranted)"),
        )
    }

    @Test
    fun `the voice loop is gated on the same verdict`() {
        val source = service() ?: return skip()

        assertTrue(
            "VoiceSession must read the gate, checked at the moment of the trigger",
            source.contains("consentGranted = { consent.verdict.value.capture }"),
        )
    }

    @Test
    fun `the service refuses to record when it cannot show a recording indicator`() {
        val source = service() ?: return skip()

        assertTrue(
            "ConsentPolicy.indicatorRequired() has no off switch, so the gate has to be told " +
                "whether an indicator can actually be shown",
            source.contains("indicatorAvailable = canShowIndicator("),
        )
    }

    @Test
    fun `a boot restart does not count as the wearer starting a conversation`() {
        val boot = repoFile(BOOT_PATH)?.readText() ?: return skip()

        // BootReceiver calls RelayCaptureService.start(context) with the
        // user-initiated flag left at its default. Passing true there would
        // make Scope.Session mean "always", which is the failure the scope
        // exists to prevent.
        assertTrue(
            "BootReceiver must not claim a boot was user-initiated",
            !Regex("""start\([^)]*userInitiated\s*=\s*true""").containsMatchIn(boot),
        )
    }

    private fun skip() = assumeTrue(
        "repository sources not present; run this from a full checkout to check for drift",
        false,
    )

    private companion object {
        const val SERVICE_PATH =
            "apps/android/relay-bridge/src/main/java/glass/relay/bridge/RelayCaptureService.kt"
        const val BOOT_PATH =
            "apps/android/relay-bridge/src/main/java/glass/relay/bridge/BootReceiver.kt"

        /**
         * Walk up from the working directory to find a repository file.
         *
         * Gradle runs unit tests with the module directory as the working
         * directory and `tools/verify-jvm-logic.sh` runs them from
         * `apps/android`, so neither can be hard-coded.
         */
        fun repoFile(relative: String): File? {
            var directory: File? = File(System.getProperty("user.dir")).absoluteFile
            repeat(8) {
                val candidate = File(directory, relative)
                if (candidate.isFile) return candidate
                directory = directory?.parentFile ?: return null
            }
            return null
        }
    }
}
