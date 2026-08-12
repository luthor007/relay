package episode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/index"
	"github.com/luthor007/relay/relayd/internal/store"
)

// Finding and Redactor are `internal/index`'s, not a second copy. MEMORY.md
// §12.2's recall figures were measured against that one ruleset and belong to
// it.
type Finding = index.Finding

// Redactor replaces credentials with markers.
type Redactor interface {
	Redact(text string) (string, []Finding)
}

// Detector returns the measured detector.
func Detector() Redactor { return index.MustDetector() }

// ErrNoRedactor is a writer built without a secret detector.
var ErrNoRedactor = errors.New("episode: no secret detector, and writing episodes without one is not allowed")

// Store is the half of `*store.DB` this package writes to. An interface so the
// dependency is visible and a test can assert on exactly what was persisted.
type Store interface {
	PutEpisode(ctx context.Context, v store.Episode) error
	PutCommitment(ctx context.Context, v store.Commitment) error
	PutSecretMarker(ctx context.Context, v store.SecretMarker) error
}

// MarkerRuntime is the `runtime` column value for a marker that came out of
// capture rather than out of an agent session.
//
// MEMORY.md §4's rule is "secret markers have one writer per path", and this is
// a third path: nothing else has seen this text, so this package writes the
// markers for it and the id scheme below is its own.
const MarkerRuntime = "capture"

// WriterOptions configures a [Writer].
type WriterOptions struct {
	Store Store
	// Redact is required. See [ErrNoRedactor].
	Redact Redactor
	Now    func() time.Time
}

// Writer persists episodes and their commitments.
type Writer struct {
	store  Store
	redact Redactor
	now    func() time.Time
}

// NewWriter builds a writer. It refuses without a redactor — the same
// structural rule `internal/index` and `internal/summarize` use, and for the
// same reason: there is no code path here that writes text without having
// looked for credentials in it first.
func NewWriter(o WriterOptions) (*Writer, error) {
	if o.Store == nil {
		return nil, errors.New("episode: no store")
	}
	if o.Redact == nil {
		return nil, ErrNoRedactor
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Writer{store: o.Store, redact: o.Redact, now: o.Now}, nil
}

// WriteResult is what one episode's write did.
type WriteResult struct {
	EpisodeID   string
	Commitments int
	// Redactions is how many credentials were replaced with markers across the
	// transcript, the commitments, the decisions and the notes.
	Redactions int
	// Findings carry the matched values so MEMORY.md §6's vault proposal flow
	// can offer them. In memory only — nothing here persists them, and the
	// marker rows hold no credential.
	Findings []Finding
}

// Write persists an episode and its extraction.
//
// The order is the contract: **redact, then write.** Every string that is about
// to be stored goes through the detector first, a marker row is written for each
// finding, and only then does the episode row exist. An embedded key cannot be
// unembedded, and an episode's transcript is exactly the kind of text a search
// index will later be built over.
func (w *Writer) Write(ctx context.Context, e Episode, ex Extraction) (WriteResult, error) {
	clean, found := Redact(e, w.redact)
	return w.write(ctx, clean, ex, found)
}

// write persists an already-redacted episode, carrying the findings that
// redaction produced so the marker rows can be written for them.
//
// It is separate from [Writer.Write] because [Writer.WriteDay] redacts *before*
// extracting — see [Redact] — and a second pass over already-marked text finds
// nothing, which would leave the credential recorded nowhere at all. Passing the
// findings forward is what keeps the audit trail attached to the redaction that
// actually happened.
func (w *Writer) write(ctx context.Context, e Episode, ex Extraction, found []Finding) (WriteResult, error) {
	res := WriteResult{EpisodeID: e.ID}
	if e.ID == "" {
		return res, errors.New("episode: cannot write an episode with no id")
	}

	// One credential, one finding, whichever string it was found in. The seen
	// set is shared between the redaction that already happened and the scrub
	// below, so the same key in the transcript and in the commitment derived
	// from it is counted once.
	seen := map[string]bool{}
	record := func(fs []Finding) {
		for _, f := range fs {
			k := f.RuleID + "\x00" + f.Value
			if seen[k] {
				continue
			}
			seen[k] = true
			res.Findings = append(res.Findings, f)
			res.Redactions++
		}
	}
	record(found)

	// Belt and braces on anything derived: an extraction built from unredacted
	// text by a caller using Write directly still gets cleaned here.
	scrub := func(s string) string {
		cleaned, f := w.redact.Redact(s)
		record(f)
		return cleaned
	}

	commitments := make([]store.Commitment, 0, len(ex.Commitments))
	for _, c := range ex.Commitments {
		c.Text = scrub(c.Text)
		c.Evidence = scrub(c.Evidence)
		commitments = append(commitments, store.Commitment{
			ID:        CommitmentID(e.ID, c),
			EpisodeID: e.ID,
			Text:      c.Text,
			OwedTo:    c.OwedTo,
			DueAt:     c.DueAt,
			CreatedAt: nonZero(c.At, w.now()),
		})
	}
	for i := range ex.Decisions {
		ex.Decisions[i].Text = scrub(ex.Decisions[i].Text)
	}
	for i := range ex.Notes {
		ex.Notes[i] = scrub(ex.Notes[i])
	}

	// Markers first. If the process dies between the markers and the episode,
	// the marker table has an entry for a credential that is not indexed
	// anywhere — which is noise. The other order leaves a transcript in the
	// database with nothing recording that a credential was in it, which is a
	// hole in the audit trail.
	for _, f := range res.Findings {
		if err := w.store.PutSecretMarker(ctx, store.SecretMarker{
			ID:        markerID(e.ID, f),
			Runtime:   MarkerRuntime,
			SessionID: e.ID,
			Detector:  f.RuleID,
			Service:   f.Service,
			At:        w.now(),
		}); err != nil {
			return res, fmt.Errorf("episode: write secret marker: %w", err)
		}
	}

	if err := w.store.PutEpisode(ctx, store.Episode{
		ID:           e.ID,
		StartedAt:    e.StartedAt,
		EndedAt:      e.EndedAt,
		Kind:         string(e.Kind),
		Transcript:   e.Transcript,
		Participants: e.Participants,
		Location:     e.Location,
	}); err != nil {
		return res, fmt.Errorf("episode: write episode: %w", err)
	}

	for _, c := range commitments {
		if err := w.store.PutCommitment(ctx, c); err != nil {
			return res, fmt.Errorf("episode: write commitment: %w", err)
		}
		res.Commitments++
	}
	return res, nil
}

// Redact returns a copy of an episode with every credential in its transcript
// and its utterances replaced by a marker.
//
// It exists as its own step because redacting only what is *written* is not
// enough. The digest is derived from the same episode and goes to the phone;
// so would a summary, an embedding, or anything else built on it later. The
// only ordering that holds is to redact the episode itself and derive
// everything from the redacted copy — which is what [Writer.WriteDay] does.
func Redact(e Episode, r Redactor) (Episode, []Finding) {
	var found []Finding
	scrub := func(s string) string {
		if s == "" {
			return s
		}
		clean, f := r.Redact(s)
		found = append(found, f...)
		return clean
	}

	out := e
	out.Transcript = scrub(e.Transcript)
	out.Utterances = make([]Utterance, len(e.Utterances))
	copy(out.Utterances, e.Utterances)
	for i := range out.Utterances {
		out.Utterances[i].Text = scrub(out.Utterances[i].Text)
	}
	return out, dedupe(found)
}

// dedupe collapses the same credential found in two places into one finding.
// The transcript and the utterance it was rendered from both contain it, and
// MEMORY.md §4's rule is one credential, one marker.
func dedupe(in []Finding) []Finding {
	seen := map[string]bool{}
	var out []Finding
	for _, f := range in {
		k := f.RuleID + "\x00" + f.Value
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	return out
}

// WriteDay segments, redacts, extracts, persists and digests one day in one
// call. It is the nightly job, and it is the only entry point that guarantees
// the ordering in [Redact].
func (w *Writer) WriteDay(ctx context.Context, day time.Time, us []Utterance, o Options, limits DigestLimits) (Digest, []WriteResult, error) {
	raw := Segment(us, o)
	eps := make([]Episode, 0, len(raw))
	results := make([]WriteResult, 0, len(raw))

	for _, e := range raw {
		// Redact BEFORE extracting, so the commitments, decisions and notes
		// that reach the digest are derived from redacted text rather than
		// cleaned up afterwards. A credential inside a commitment on somebody's
		// phone is still a credential that left the database.
		clean, found := Redact(e, w.redact)
		res, err := w.write(ctx, clean, Extract(clean, o), found)
		if err != nil {
			return Digest{}, results, err
		}
		results = append(results, res)
		eps = append(eps, clean)
	}
	return Day(day, eps, o, limits), results, nil
}

// Digest builds a day's digest from episodes, redacting them first.
//
// Prefer this to the package-level [Day] for anything that will be shown to a
// person: [Day] is a pure function over the text it is given, and this is the
// one that guarantees the text has been through the detector.
func (w *Writer) Digest(day time.Time, eps []Episode, o Options, limits DigestLimits) Digest {
	clean := make([]Episode, 0, len(eps))
	for _, e := range eps {
		c, _ := Redact(e, w.redact)
		clean = append(clean, c)
	}
	return Day(day, clean, o, limits)
}

// Proposable reports whether a finding may become a vault proposal. MEMORY.md
// §12.2 rule 1: tier 1 only, because one tier-2 hit in four is a checksum.
func Proposable(f Finding) bool { return f.Tier.ProposeToVault() }

// markerID is stable per (episode, rule, credential), which is MEMORY.md §4's
// rule stated as an id scheme: **one credential, one marker.**
//
// It is keyed on a hash of the value rather than on a byte offset, and that is
// the fix for a real duplicate: the same key appears once in the episode's
// transcript and once in the utterance it came from, at two different offsets,
// and an offset-keyed id would file the same credential twice. The value is
// hashed and never stored — the marker's whole point is that the credential is
// not in the database.
func markerID(episodeID string, f Finding) string {
	v := sha256.Sum256([]byte(f.Value))
	h := sha256.Sum256([]byte(strings.Join([]string{
		MarkerRuntime, episodeID, f.RuleID, hex.EncodeToString(v[:]),
	}, "\x00")))
	return hex.EncodeToString(h[:16])
}

func nonZero(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}
