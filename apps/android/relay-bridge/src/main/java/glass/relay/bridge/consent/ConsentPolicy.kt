package glass.relay.bridge.consent

/**
 * When capture is allowed to run, and what has to be visible while it does.
 *
 * `ARCHITECTURE.md` §6 is not advisory. Quebec — the user's home market — is a
 * two-party consent jurisdiction, as are California, Illinois, Washington,
 * Pennsylvania, Massachusetts and Florida. Recording a conversation without all
 * parties' consent is a legal problem, not a design preference. The two
 * architectural requirements it states are:
 *
 *  - bystander-visible recording indication, ideally in hardware
 *  - **capture defaults to off in a new location or with new voices present,
 *    until confirmed**
 *
 * The second one is what this class is for. It is easy to write an app that
 * asks once at onboarding and never again; that app records a doctor's
 * appointment because someone said yes to a dialog in their kitchen three
 * months earlier.
 *
 * ## The honest caveat
 *
 * "New voices present" needs speaker diarisation, which happens on the box, not
 * on the phone. So [Signals.unfamiliarVoices] is an input this class consumes
 * rather than a thing it can determine, and its default is
 * [Familiarity.Unknown] — which asks. Defaulting an unknown to "allow" would
 * make the whole rule decorative.
 */
object ConsentPolicy {

    enum class Familiarity {
        /** Seen before and confirmed by the user. */
        Known,

        /** New. Ask. */
        New,

        /** No signal available — the box has not answered, or is unreachable. */
        Unknown,
    }

    /**
     * What the user has granted, and for how long.
     *
     * `Session` exists because "record this meeting" is a real request that
     * should not silently become "record everything from now on".
     */
    enum class Scope {
        /** Nothing. The default for a fresh install. */
        None,

        /** This conversation only; lapses when capture stops. */
        Session,

        /** Everywhere the user has already confirmed. */
        FamiliarPlaces,

        /** Everything, everywhere. Deliberate, and revocable in one tap. */
        Always,
    }

    data class Signals(
        val place: Familiarity = Familiarity.Unknown,
        val unfamiliarVoices: Familiarity = Familiarity.Unknown,
        /** The wearer explicitly started this session in the app or by voice. */
        val userInitiated: Boolean = false,
        /** True where all parties must consent. Quebec is one. */
        val twoPartyJurisdiction: Boolean = true,
    )

    sealed interface Decision {
        /** Capture may run. */
        data class Allow(val why: String) : Decision

        /**
         * Not until the user confirms. Capture stays **off** in the meantime —
         * this is not a banner over a running recording.
         */
        data class Confirm(val question: String, val why: String) : Decision

        data class Deny(val why: String) : Decision
    }

    /**
     * Whether a visible recording indicator must be shown.
     *
     * Always true, and a function rather than a constant so that the day
     * someone wants an exception, they have to add the branch and justify it in
     * a diff. There is no configuration that turns this off.
     */
    fun indicatorRequired(): Boolean = true

    /**
     * Whether call auto-recording (`0x0E09`) may default on.
     *
     * Off wherever all parties must consent, which is the shipping default.
     */
    fun callAutoRecordDefault(twoPartyJurisdiction: Boolean): Boolean = !twoPartyJurisdiction

    /**
     * Whether the audible recording prompt (`0x0E06`) should be on.
     *
     * On by default in two-party jurisdictions: it is the only bystander-facing
     * signal the hardware is known to have, and whether the M01 Pro has a
     * capture LED at all is still an open hardware question
     * (`ARCHITECTURE.md` §7).
     */
    fun recordingPromptDefault(twoPartyJurisdiction: Boolean): Boolean = twoPartyJurisdiction

    fun decide(scope: Scope, signals: Signals): Decision = when (scope) {
        Scope.None -> Decision.Deny("capture has not been turned on")

        Scope.Session ->
            if (signals.userInitiated) {
                Decision.Allow("the user started this session")
            } else {
                Decision.Confirm(
                    "Start recording?",
                    "session consent covers one conversation and this is a new one",
                )
            }

        Scope.FamiliarPlaces -> when {
            signals.place == Familiarity.New -> Decision.Confirm(
                "You are somewhere new. Record here?",
                "capture defaults to off in a new location — ARCHITECTURE.md §6",
            )
            signals.place == Familiarity.Unknown && !signals.userInitiated -> Decision.Confirm(
                "Record here?",
                "the box has not confirmed this place yet, and an unknown place is treated as new",
            )
            signals.unfamiliarVoices == Familiarity.New && signals.twoPartyJurisdiction -> Decision.Confirm(
                "Someone new is here. Keep recording?",
                "all parties must consent in this jurisdiction",
            )
            else -> Decision.Allow("a confirmed place with no new voices")
        }

        Scope.Always ->
            if (signals.unfamiliarVoices == Familiarity.New && signals.twoPartyJurisdiction) {
                // "Always" is the wearer's consent. It is not the other
                // person's, and it cannot be.
                Decision.Confirm(
                    "Someone new is here. Keep recording?",
                    "always-on covers you, not the people around you",
                )
            } else {
                Decision.Allow("always-on capture is enabled")
            }
    }
}
