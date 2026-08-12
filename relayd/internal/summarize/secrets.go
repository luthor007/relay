package summarize

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/luthor007/relay/relayd/internal/index"
)

// ErrNoRedactor means a Summarizer was asked for without a secret detector.
// It is an error rather than a default because the alternative — a summariser
// that quietly indexes whatever it is given — looks exactly like a clean corpus
// right up until a key is in the vector table.
var ErrNoRedactor = errors.New("summarize: no secret detector, and indexing without one is not allowed")

// Finding is one detected credential. It is internal/index's type rather than a
// second one, because there is exactly one measured ruleset and MEMORY.md
// §12.2's recall figures belong to it.
type Finding = index.Finding

// Redactor replaces credentials with markers.
//
// It is an interface with one method rather than a concrete type so this
// package does not own a second copy of the ruleset — *index.Detector is the
// implementation, compiled from the patterns the 70.6% / 92.9% recall figures
// were measured against, and internal/index's own tests hold that measurement.
//
// Why it is required rather than optional: detection happens before indexing,
// never after, because an embedded key cannot be unembedded and a key posted to
// a model provider has already left the machine. Making the detector a
// constructor argument is how that ordering stops being a convention and starts
// being a thing the type system will not let you skip.
type Redactor interface {
	Redact(text string) (string, []Finding)
}

// Detector returns the measured detector, for a caller with no reason to build
// its own.
func Detector() Redactor { return index.MustDetector() }

// Proposable reports whether a finding may become a vault proposal.
//
// MEMORY.md §12.2 rule 1: tier 1 only. A tier-2 hit is redacted before indexing
// and must never auto-create a vault entry, because one in four of them is a
// checksum — a Twilio auth token and an MD5 digest are both exactly 32
// lowercase hex characters and there is no rule that separates them.
func Proposable(f Finding) bool { return f.Tier.ProposeToVault() }

// MarkerID is a stable id for a secret_marker row written from a live turn, so
// re-handling the same turn updates the marker rather than accumulating
// duplicates.
//
// Backfill does not use this: internal/index writes the markers for a session
// it read from disk, and one marker table with two id schemes writing the same
// credential twice is a bug waiting for a support ticket. This package writes
// markers only on the live path, where nothing else has seen the text.
func MarkerID(runtime, sessionID, turnID string, f Finding) string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		runtime, sessionID, turnID, f.RuleID, strconv.Itoa(f.Start),
	}, "\x00")))
	return hex.EncodeToString(h[:16])
}
