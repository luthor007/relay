// Package episode is the third stage of SYSTEM.md §10 step 5: a day of
// transcript becomes the entity §5 actually names.
//
//	Episode  id, started_at, ended_at, kind(meeting|focus|conversation|ambient),
//	         transcript, participants[], location?
//	Commitment  id, episode_id, text, to?, due_at?, done_at?
//
// # Why episodes rather than one long log
//
// ARCHITECTURE.md §4: "Episodes, not one endless log. Retrieval quality depends
// on segmentation." A day is sixteen hours; a question is "what did I decide
// about the CRC". Answering the second from the first is a search problem that
// segmentation mostly solves in advance.
//
// # Commitments are the output that earns the product
//
// Also §4: "Commitments are the killer output. 'You told Marc you'd send the
// BOM by Friday' is worth more than a searchable transcript, and it is what
// makes the memory feel alive rather than archival."
//
// So [Extract] is deliberately **rule-based and deterministic**, not a model
// call. Three reasons, in the order they matter:
//
//   - A commitment carries a person's name and a date. A model that invents
//     either produces a confident, wrong reminder — which is worse than no
//     reminder, because the user stops trusting all of them. Rules can only be
//     wrong in ways a test can pin down.
//   - It runs on a day of transcript, nightly, on the user's own machine. Free
//     and instant beats good-and-metered for a job that has no latency budget
//     and no user watching.
//   - [Extractor] is an interface, so a model-backed pass can be added *on top*
//     later — proposing, with the rule output as the floor. That is the shape
//     MEMORY.md §5 uses for facts: everything carries evidence, and a claim
//     that cannot point at where it came from is deleted rather than kept at
//     low confidence.
//
// # Nothing is claimed that was not observed
//
// The rule that runs through all three packages of this milestone shows up here
// as speaker attribution. A recogniser that does not diarise produces
// unlabelled speech, and unlabelled speech is **not** attributed to the wearer:
// [Options.AttributeUnlabelledToWearer] exists, defaults to off, and an episode
// that had to fall back says so in [Episode.Notes]. Filing a conversation as
// "you said" when nobody knows who was talking is the same class of error as an
// adapter emitting an event it did not see.
//
// # Secrets, once more, before anything is written
//
// [Writer] cannot be built without `internal/index`'s detector, and it redacts
// every string it is about to persist — transcript, commitment, decision, note
// — before the first insert. The transcript arriving from `internal/transcript`
// has already been through the same detector; running it again is deliberate,
// because this package also accepts episodes built from other sources and the
// marker text is idempotent under a second pass.
package episode
