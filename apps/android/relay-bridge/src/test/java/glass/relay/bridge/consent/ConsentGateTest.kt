package glass.relay.bridge.consent

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The half of `ARCHITECTURE.md` §6 that `ConsentPolicyTest` cannot reach.
 *
 * `ConsentPolicy` is a pure function and was already tested; what was untested —
 * because it did not exist — is anything holding it. The whole failure this
 * class guards against is a stored "yes" from three months ago still being the
 * only thing between the microphone and a room the user has never been in.
 */
class ConsentGateTest {

    private fun gate(
        scope: ConsentPolicy.Scope = ConsentPolicy.Scope.FamiliarPlaces,
        places: Set<String> = emptySet(),
        indicator: Boolean = true,
        twoParty: Boolean = true,
        persist: (ConsentGate.Stored) -> Unit = {},
    ) = ConsentGate(
        initial = ConsentGate.Stored(scope = scope, confirmedPlaces = places),
        indicatorAvailable = indicator,
        twoPartyJurisdiction = twoParty,
        persist = persist,
    )

    @Test
    fun `a fresh install captures nothing`() {
        val gate = gate(scope = ConsentPolicy.Scope.None)

        assertFalse(gate.verdict.value.capture)
        assertTrue(gate.verdict.value.decision is ConsentPolicy.Decision.Deny)
    }

    @Test
    fun `a new location with the same stored consent still refuses to record`() {
        // THE test. The stored grant does not change, the user does not answer
        // anything, and capture stops the moment the box says this is somewhere
        // else. A boolean in SharedPreferences cannot express this, which is
        // why it was the wrong thing to gate on.
        val gate = gate(places = setOf("home"))
        gate.enterPlace("home")
        gate.startSession()
        assertTrue("a confirmed place should record", gate.verdict.value.capture)

        gate.enterPlace("clinic")

        val verdict = gate.verdict.value
        assertFalse("a new place must not record silently", verdict.capture)
        assertTrue("and it must ask rather than just stopping", verdict.awaitingAnswer)
        assertEquals(
            "the stored grant is untouched — this is not a revocation",
            ConsentPolicy.Scope.FamiliarPlaces,
            gate.storedState.scope,
        )
    }

    @Test
    fun `the place the user has not confirmed asks, and answering yes confirms only that place`() {
        var saved: ConsentGate.Stored? = null
        val gate = gate(places = setOf("home"), persist = { saved = it })

        gate.enterPlace("clinic")
        assertFalse(gate.verdict.value.capture)

        gate.answer(approve = true)

        assertTrue(gate.verdict.value.capture)
        assertEquals(setOf("home", "clinic"), saved?.confirmedPlaces)
        assertEquals(
            "saying yes in a room does not turn on always-on capture",
            ConsentPolicy.Scope.FamiliarPlaces,
            gate.storedState.scope,
        )
    }

    @Test
    fun `a confirmed place survives a restart, because a daily prompt is a dismissed prompt`() {
        var saved = ConsentGate.Stored(scope = ConsentPolicy.Scope.FamiliarPlaces)
        val first = ConsentGate(
            initial = saved,
            indicatorAvailable = true,
            persist = { saved = it },
        )
        first.enterPlace("studio")
        first.answer(approve = true)

        val afterRestart = ConsentGate(initial = saved, indicatorAvailable = true)
        afterRestart.enterPlace("studio")

        assertTrue(afterRestart.verdict.value.capture)
    }

    @Test
    fun `saying no keeps capture off until the circumstances change`() {
        val gate = gate(places = setOf("home"))
        gate.enterPlace("clinic")
        gate.answer(approve = false)

        assertFalse(gate.verdict.value.capture)
        assertNull("a refusal is not a question to re-ask", gate.verdict.value.question)

        // A voice leaving the room is not the user changing their mind.
        gate.observeVoices(ConsentPolicy.Familiarity.Known)
        assertFalse(gate.verdict.value.capture)

        // Walking somewhere else is a new question.
        gate.enterPlace("home")
        gate.startSession()
        assertTrue(gate.verdict.value.capture)
    }

    @Test
    fun `an unfamiliar voice stops always-on capture and asks`() {
        // Always-on is the wearer's consent. It is not the other person's.
        val gate = gate(scope = ConsentPolicy.Scope.Always)
        assertTrue(gate.verdict.value.capture)

        gate.observeVoices(ConsentPolicy.Familiarity.New)

        assertFalse(gate.verdict.value.capture)
        assertEquals("Someone new is here. Keep recording?", gate.verdict.value.question)
    }

    @Test
    fun `answering yes to a new voice covers that voice and not the next one`() {
        val gate = gate(scope = ConsentPolicy.Scope.Always)
        gate.observeVoices(ConsentPolicy.Familiarity.New)
        gate.answer(approve = true)
        assertTrue(gate.verdict.value.capture)

        gate.observeVoices(ConsentPolicy.Familiarity.New)

        assertFalse("someone else walking in is a fresh question", gate.verdict.value.capture)
    }

    @Test
    fun `no signal from the box is treated as unknown, and unknown asks`() {
        // The box may be unreachable, and it is the only thing that knows
        // whether a place is familiar. Defaulting an unknown to allow would
        // make the whole rule decorative.
        val gate = gate()

        val verdict = gate.verdict.value

        assertFalse(verdict.capture)
        assertTrue(verdict.awaitingAnswer)
        assertEquals(ConsentPolicy.Familiarity.Unknown, verdict.signals.place)
    }

    @Test
    fun `the wearer starting capture is consent for this conversation`() {
        val gate = gate()
        gate.startSession()

        assertTrue(gate.verdict.value.capture)
    }

    @Test
    fun `starting capture here is not consent for the next place`() {
        val gate = gate()
        gate.startSession()
        assertTrue(gate.verdict.value.capture)

        gate.enterPlace("somewhere else")

        assertFalse(gate.verdict.value.capture)
    }

    @Test
    fun `session scope lapses when capture stops`() {
        val gate = gate(scope = ConsentPolicy.Scope.Session)
        gate.startSession()
        assertTrue(gate.verdict.value.capture)

        gate.endSession()

        assertFalse("session consent covers one conversation", gate.verdict.value.capture)
        assertTrue(gate.verdict.value.awaitingAnswer)
    }

    @Test
    fun `capture is refused outright when no recording indicator can be shown`() {
        // ARCHITECTURE.md §6 lists bystander-visible indication as a
        // requirement, so it is a precondition for recording rather than a
        // decoration on top of one. On Android that is the ongoing
        // notification, which needs POST_NOTIFICATIONS.
        val gate = gate(scope = ConsentPolicy.Scope.Always, indicator = false)

        val verdict = gate.verdict.value

        assertFalse(verdict.capture)
        assertTrue(verdict.indicatorRequired)
        assertTrue(verdict.decision is ConsentPolicy.Decision.Deny)
        assertNull("this is not a question the user can answer away", verdict.question)

        gate.setIndicatorAvailable(true)
        assertTrue(gate.verdict.value.capture)
    }

    @Test
    fun `a missing indicator outranks a question, so the user answers the real problem`() {
        val gate = gate(indicator = false)

        assertNull(gate.verdict.value.question)
        assertTrue(gate.verdict.value.why.contains("indicator"))
    }

    @Test
    fun `revoking forgets every place that was ever confirmed`() {
        var saved: ConsentGate.Stored? = null
        val gate = gate(places = setOf("home", "office"), persist = { saved = it })

        gate.revoke()

        assertEquals(ConsentPolicy.Scope.None, gate.storedState.scope)
        assertEquals(emptySet<String>(), gate.storedState.confirmedPlaces)
        assertEquals(emptySet<String>(), saved?.confirmedPlaces)
        assertFalse(gate.verdict.value.capture)
    }

    @Test
    fun `outside a two-party jurisdiction a new voice does not stop always-on`() {
        val gate = gate(scope = ConsentPolicy.Scope.Always, twoParty = false)
        gate.observeVoices(ConsentPolicy.Familiarity.New)

        assertTrue(gate.verdict.value.capture)
    }

    @Test
    fun `the verdict carries the reason so the notification never says nothing`() {
        val gate = gate()

        val verdict = gate.verdict.value

        assertNotNull(verdict.why)
        assertTrue(verdict.why.isNotBlank())
    }
}
