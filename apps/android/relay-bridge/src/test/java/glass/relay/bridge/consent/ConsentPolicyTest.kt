package glass.relay.bridge.consent

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * ARCHITECTURE.md §6, as assertions. Quebec is a two-party consent jurisdiction
 * and this is the user's home market, so these are legal requirements wearing
 * the clothes of unit tests.
 */
class ConsentPolicyTest {

    private fun assertConfirms(decision: ConsentPolicy.Decision) {
        assertTrue("expected a confirmation prompt, got $decision", decision is ConsentPolicy.Decision.Confirm)
    }

    private fun assertAllows(decision: ConsentPolicy.Decision) {
        assertTrue("expected Allow, got $decision", decision is ConsentPolicy.Decision.Allow)
    }

    @Test
    fun `a fresh install records nothing`() {
        val decision = ConsentPolicy.decide(ConsentPolicy.Scope.None, ConsentPolicy.Signals())
        assertTrue(decision is ConsentPolicy.Decision.Deny)
    }

    @Test
    fun `a new place asks, because capture defaults to off there`() {
        val decision = ConsentPolicy.decide(
            ConsentPolicy.Scope.FamiliarPlaces,
            ConsentPolicy.Signals(place = ConsentPolicy.Familiarity.New),
        )
        assertConfirms(decision)
    }

    @Test
    fun `an unknown place is treated as a new one`() {
        // The box answers this, and it may be unreachable. Defaulting an unknown
        // to "allow" would make the whole rule decorative.
        val decision = ConsentPolicy.decide(
            ConsentPolicy.Scope.FamiliarPlaces,
            ConsentPolicy.Signals(place = ConsentPolicy.Familiarity.Unknown),
        )
        assertConfirms(decision)
    }

    @Test
    fun `a confirmed place with no new voices runs`() {
        val decision = ConsentPolicy.decide(
            ConsentPolicy.Scope.FamiliarPlaces,
            ConsentPolicy.Signals(
                place = ConsentPolicy.Familiarity.Known,
                unfamiliarVoices = ConsentPolicy.Familiarity.Known,
            ),
        )
        assertAllows(decision)
    }

    @Test
    fun `a new voice in a familiar place still asks`() {
        val decision = ConsentPolicy.decide(
            ConsentPolicy.Scope.FamiliarPlaces,
            ConsentPolicy.Signals(
                place = ConsentPolicy.Familiarity.Known,
                unfamiliarVoices = ConsentPolicy.Familiarity.New,
            ),
        )
        assertConfirms(decision)
    }

    @Test
    fun `always-on is the wearer's consent, not the other person's`() {
        // The single most important line in this file. "Always" cannot grant
        // what the wearer does not have the standing to give.
        val decision = ConsentPolicy.decide(
            ConsentPolicy.Scope.Always,
            ConsentPolicy.Signals(unfamiliarVoices = ConsentPolicy.Familiarity.New),
        )
        assertConfirms(decision)
        assertEquals(
            "always-on covers you, not the people around you",
            (decision as ConsentPolicy.Decision.Confirm).why,
        )
    }

    @Test
    fun `outside a two-party jurisdiction always-on does not re-ask`() {
        val decision = ConsentPolicy.decide(
            ConsentPolicy.Scope.Always,
            ConsentPolicy.Signals(
                unfamiliarVoices = ConsentPolicy.Familiarity.New,
                twoPartyJurisdiction = false,
            ),
        )
        assertAllows(decision)
    }

    @Test
    fun `session consent covers one conversation`() {
        assertAllows(
            ConsentPolicy.decide(
                ConsentPolicy.Scope.Session,
                ConsentPolicy.Signals(userInitiated = true),
            ),
        )
        assertConfirms(
            ConsentPolicy.decide(
                ConsentPolicy.Scope.Session,
                ConsentPolicy.Signals(userInitiated = false),
            ),
        )
    }

    @Test
    fun `the recording indicator cannot be turned off`() {
        assertTrue(ConsentPolicy.indicatorRequired())
    }

    @Test
    fun `call auto-record is off and the audible prompt is on where all parties must consent`() {
        assertEquals(false, ConsentPolicy.callAutoRecordDefault(twoPartyJurisdiction = true))
        assertEquals(true, ConsentPolicy.recordingPromptDefault(twoPartyJurisdiction = true))
    }
}
