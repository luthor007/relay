package glass.relay.bridge.consent

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * [ConsentPolicy] with a caller.
 *
 * The policy is a pure function; this is the thing that holds the signals it
 * needs, remembers what the user has answered, and produces the one boolean the
 * capture path is allowed to read. `RelayCaptureService` gates
 * `LocalRecordingController` and `VoiceSession` on [Verdict.capture] and on
 * nothing else, so there is exactly one place where "may we record" is decided.
 *
 * ## Why this class had to exist
 *
 * Capture used to be gated on a single stored boolean —
 * `CapturePreferences.consentGranted`, set once on the onboarding screen. That
 * is the app `ConsentPolicy`'s own header warns about: it "records a doctor's
 * appointment because someone said yes to a dialog in their kitchen three
 * months earlier". `ARCHITECTURE.md` §6 requires the opposite, in two clauses:
 *
 *  - **capture defaults to off in a new location or with new voices present,
 *    until confirmed**
 *  - bystander-visible recording indication, which is a *requirement* and
 *    therefore a precondition for recording, not a decoration on top of it
 *
 * Quebec — the user's home market — is a two-party consent jurisdiction, so
 * both clauses are legal exposure rather than polish.
 *
 * ## What the signals actually are today
 *
 * Honesty about the inputs is the whole design here:
 *
 *  - **Place.** The box decides whether a place is familiar; the phone cannot.
 *    Until it answers, [placeId] is null and the place reads as
 *    [ConsentPolicy.Familiarity.Unknown] — which asks. Nothing in production
 *    calls [enterPlace] yet, and the consequence of that is the *safe*
 *    direction: capture waits for a person rather than assuming.
 *  - **Voices.** Speaker diarisation happens on the box too. Same treatment.
 *  - **User-initiated.** This one the phone does know: tapping "Start capture"
 *    or triggering the voice loop is the wearer starting a conversation, and
 *    [startSession] is how that reaches the policy.
 *
 * A signal arriving later never *widens* what is already allowed without a
 * fresh answer: [enterPlace] clears [startSession]'s effect, because a session
 * the wearer began at home is not consent for the clinic they walked into.
 *
 * ## The indicator is a precondition
 *
 * [ConsentPolicy.indicatorRequired] is always true and cannot be configured
 * off. So a build that cannot show one must not record, and [indicatorAvailable]
 * is how the service says whether it can — on Android that is the ongoing
 * notification, which needs `POST_NOTIFICATIONS` on API 33+. Treating the
 * indicator as advisory is how a recording device ends up with no visible sign
 * that it is recording.
 *
 * Not thread-safe by itself; the service confines it to its own scope and
 * publishes through [verdict].
 */
class ConsentGate(
    initial: Stored = Stored(),

    /**
     * Whether a bystander-visible recording indicator can actually be shown.
     *
     * No default on purpose. [ConsentPolicy.indicatorRequired] exists so that
     * an exception has to be written down in a diff; a default here would let
     * a call site skip the question silently, which is the same hole one level
     * up.
     */
    indicatorAvailable: Boolean,

    /** True where all parties must consent. Quebec is one, and is the default. */
    private val twoPartyJurisdiction: Boolean = true,

    /**
     * Called whenever [Stored] changes, so the caller can persist it.
     *
     * Confirming a place has to outlive the process or the user is asked again
     * every morning, and a prompt people see daily is a prompt they dismiss
     * without reading.
     */
    private val persist: (Stored) -> Unit = {},
) {

    /** The part of consent that survives a restart. */
    data class Stored(
        val scope: ConsentPolicy.Scope = ConsentPolicy.Scope.None,
        /** Places the user has confirmed, by whatever id the box uses. */
        val confirmedPlaces: Set<String> = emptySet(),
    )

    data class Verdict(
        /** The only boolean the capture path may read. */
        val capture: Boolean,
        /** Always true. Carried so a caller cannot forget to ask. */
        val indicatorRequired: Boolean,
        val decision: ConsentPolicy.Decision,
        val signals: ConsentPolicy.Signals,
        val scope: ConsentPolicy.Scope,
        val placeId: String?,
        /** The question to put in front of the user, or null. */
        val question: String?,
        /** Why, in words a notification can show verbatim. */
        val why: String,
    ) {
        /** Capture is off and a person has to answer before it goes on. */
        val awaitingAnswer: Boolean get() = question != null
    }

    private var stored: Stored = initial
    private var indicator: Boolean = indicatorAvailable
    private var placeId: String? = null
    private var voices: ConsentPolicy.Familiarity = ConsentPolicy.Familiarity.Unknown
    private var userInitiated: Boolean = false

    /** Set by [answer] with `true` — covers the current place even if it has no id. */
    private var confirmedHere: Boolean = false

    /** Set by [answer] with `false`. Cleared only by a change of circumstance. */
    private var declined: Boolean = false

    private val _verdict = MutableStateFlow(evaluate())

    /** The decision, recomputed on every input. */
    val verdict: StateFlow<Verdict> = _verdict.asStateFlow()

    val storedState: Stored get() = stored

    // --- inputs ---------------------------------------------------------------

    /**
     * The user chose a scope — onboarding, or the settings screen.
     *
     * Widening the scope clears a previous refusal, because the refusal was an
     * answer to a question asked under the old one.
     */
    fun grant(scope: ConsentPolicy.Scope) {
        if (stored.scope == scope) return
        stored = stored.copy(scope = scope)
        declined = false
        persist(stored)
        publish()
    }

    /**
     * Consent withdrawn. Forgets the confirmed places too.
     *
     * Keeping them would mean that re-granting consent silently restores every
     * "yes" the user has ever given, which is not what withdrawing means.
     */
    fun revoke() {
        stored = Stored()
        placeId = null
        voices = ConsentPolicy.Familiarity.Unknown
        userInitiated = false
        confirmedHere = false
        declined = false
        persist(stored)
        publish()
    }

    /** The service can (or can no longer) show the recording indicator. */
    fun setIndicatorAvailable(available: Boolean) {
        if (indicator == available) return
        indicator = available
        publish()
    }

    /**
     * The box says where we are. Null means it has not answered.
     *
     * Everything transient resets: a new place is a new conversation, so the
     * wearer's "start capture" at the last one does not carry, and the voices
     * around them are unknown again until the box says otherwise. This is the
     * clause of `ARCHITECTURE.md` §6 that a stored boolean cannot express.
     */
    fun enterPlace(id: String?) {
        if (placeId == id) return
        placeId = id
        voices = ConsentPolicy.Familiarity.Unknown
        userInitiated = false
        confirmedHere = false
        declined = false
        publish()
    }

    /** The box's diarisation verdict on the voices it can hear. */
    fun observeVoices(familiarity: ConsentPolicy.Familiarity) {
        if (voices == familiarity) return
        voices = familiarity
        // A refusal is not cleared here. "No" meant no, and a voice leaving the
        // room is not the user changing their mind.
        publish()
    }

    /**
     * The wearer started this deliberately — tapped record, or triggered the
     * voice loop. [ConsentPolicy.Scope.Session] turns on exactly this.
     */
    fun startSession() {
        if (userInitiated && !declined) return
        userInitiated = true
        declined = false
        publish()
    }

    /** Capture stopped. Session consent lapses with it, by definition. */
    fun endSession() {
        if (!userInitiated && !confirmedHere) return
        userInitiated = false
        confirmedHere = false
        publish()
    }

    /**
     * The user answered [Verdict.question].
     *
     * A "yes" confirms this place, this conversation and the voices currently
     * in it — and nothing beyond them. It does not widen [Stored.scope]: saying
     * yes in a meeting must not turn on always-on capture.
     */
    fun answer(approve: Boolean) {
        if (!approve) {
            declined = true
            publish()
            return
        }
        declined = false
        userInitiated = true
        confirmedHere = true
        if (voices == ConsentPolicy.Familiarity.New) voices = ConsentPolicy.Familiarity.Known
        val id = placeId
        if (id != null && id !in stored.confirmedPlaces) {
            stored = stored.copy(confirmedPlaces = stored.confirmedPlaces + id)
            persist(stored)
        }
        publish()
    }

    // --- the decision ---------------------------------------------------------

    private fun placeFamiliarity(): ConsentPolicy.Familiarity = when {
        confirmedHere -> ConsentPolicy.Familiarity.Known
        placeId == null -> ConsentPolicy.Familiarity.Unknown
        placeId in stored.confirmedPlaces -> ConsentPolicy.Familiarity.Known
        else -> ConsentPolicy.Familiarity.New
    }

    private fun evaluate(): Verdict {
        val signals = ConsentPolicy.Signals(
            place = placeFamiliarity(),
            unfamiliarVoices = voices,
            userInitiated = userInitiated,
            twoPartyJurisdiction = twoPartyJurisdiction,
        )
        val decision = ConsentPolicy.decide(stored.scope, signals)
        val required = ConsentPolicy.indicatorRequired()

        // The indicator check runs last and can only ever subtract. Putting it
        // before the policy would let a missing indicator masquerade as a
        // consent question, and the user would answer the wrong problem.
        if (required && !indicator) {
            return Verdict(
                capture = false,
                indicatorRequired = true,
                decision = ConsentPolicy.Decision.Deny(INDICATOR_MISSING),
                signals = signals,
                scope = stored.scope,
                placeId = placeId,
                question = null,
                why = INDICATOR_MISSING,
            )
        }

        val effective = if (declined) {
            ConsentPolicy.Decision.Deny("the user said no to recording here")
        } else {
            decision
        }

        return Verdict(
            capture = effective is ConsentPolicy.Decision.Allow,
            indicatorRequired = required,
            decision = effective,
            signals = signals,
            scope = stored.scope,
            placeId = placeId,
            question = (effective as? ConsentPolicy.Decision.Confirm)?.question,
            why = when (effective) {
                is ConsentPolicy.Decision.Allow -> effective.why
                is ConsentPolicy.Decision.Confirm -> effective.why
                is ConsentPolicy.Decision.Deny -> effective.why
            },
        )
    }

    private fun publish() {
        _verdict.value = evaluate()
    }

    private companion object {
        const val INDICATOR_MISSING =
            "no recording indicator can be shown, and ARCHITECTURE.md §6 makes one a requirement"
    }
}
