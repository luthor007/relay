// Package connector is the connector half of SYSTEM.md §10 step 6: grants,
// evidence-grounded proposals, the normalized envelope, and the first real
// connector.
//
// It is not the TypeScript `connector/` at the repository root — that is
// APPS-SCOPE.md §4.3's authenticated channel between the phone and the box, a
// different thing that happens to share a word. This package is
// ORCHESTRATOR.md §4b's sense of the term: a service the agent can reach, that
// the user granted, one half at a time.
//
// Everything here exists to hold one sentence from §4b:
//
//	Asking for every scope up front is the single best way to lose the install,
//	and it is the pattern that gets software flagged as malware-shaped. A consent
//	sheet with fourteen items is not read; it is abandoned or blind-accepted, and
//	neither outcome is one you want to defend later.
//
// So there is no install-time consent sheet in this package and no function
// that could build one. There is:
//
//   - [Envelope], SYSTEM.md §3.4's fixed shape, so the orchestrator never
//     learns a vendor's. Text on the way in goes through the same secret
//     detector internal/index uses, before anything is stored — MEMORY.md
//     §12.2's ordering, which cannot be fixed after the fact.
//   - [Grants], the only path from ungranted to granted, and it refuses a
//     request that does not carry an explicit human decision. Read and write
//     are separate grants and separate calls, because reading a calendar is not
//     sending invitations.
//   - [Proposer], which turns evidence from the capture pipeline into "you have
//     mentioned your Prusa four times this week — want me to connect it?" and
//     has no reference to a grant store at all. A proposal cannot grant. That
//     is enforced by the type, not by review.
//   - [Prusa], the first connector: a read half that is genuinely useful on its
//     own, and a write half whose tools stop and confirm at the glasses because
//     printing is one of the four examples §4b names.
package connector
