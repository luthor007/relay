package facts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/index"
	"github.com/luthor007/relay/relayd/internal/logx"
	"github.com/luthor007/relay/relayd/internal/store"
)

// Redactor is internal/index's measured detector, behind the one method this
// package needs.
//
// It is a constructor argument rather than an option because MEMORY.md's rule
// is "detect before writing, never after", and a required argument is how that
// stops being a convention. *index.Detector is the implementation — there is
// exactly one ruleset and §12.2's recall figures belong to it.
type Redactor interface {
	Redact(text string) (string, []index.Finding)
}

// Detector returns the measured detector, for a caller with no reason to build
// its own.
func Detector() Redactor { return index.MustDetector() }

// Options configures a [Store].
type Options struct {
	// Redactor is required. See [Redactor].
	Redactor Redactor
	// HalfLife defaults to [DefaultHalfLife].
	HalfLife time.Duration
	// Now defaults to time.Now. It stamps supersessions, edits and deletes —
	// never last_seen, which comes from the evidence.
	Now func() time.Time
	Log *slog.Logger
}

// Store is the fact tier over the main database.
//
// There is deliberately no Put: [Store.Reconcile] is the only way in, and it
// takes [Observation], which cannot be built without evidence. That is what
// makes "every fact carries evidence" structurally true rather than a rule
// somebody remembers.
type Store struct {
	db       *store.DB
	redact   Redactor
	halfLife time.Duration
	now      func() time.Time
	log      *slog.Logger
}

// Open builds a fact store over an already-open main database.
func Open(db *store.DB, o Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("facts: no store")
	}
	if db.Kind() != store.KindMain {
		return nil, fmt.Errorf("facts: %s is the %s database; facts live in the main one", db.Path(), db.Kind())
	}
	if o.Redactor == nil {
		return nil, ErrNoRedactor
	}
	s := &Store{db: db, redact: o.Redactor, halfLife: o.HalfLife, now: o.Now, log: o.Log}
	if s.halfLife <= 0 {
		s.halfLife = DefaultHalfLife
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = logx.Discard()
	}
	return s, nil
}

// HalfLife is the decay constant this store was built with.
func (s *Store) HalfLife() time.Duration { return s.halfLife }

// Strength is [Fact.Strength] under this store's half-life.
func (s *Store) Strength(f Fact, at time.Time) float64 {
	return Decay(f.Confidence, f.LastSeen, at, s.halfLife)
}

// ------------------------------------------------------------- reconcile --

// Skip is one observation that did not become a fact, and why. Every rejection
// is reported rather than dropped: an extractor that is silently losing half
// its output looks identical to a quiet user.
type Skip struct {
	Text   string
	Reason string
}

// Result is what one reconciliation did.
type Result struct {
	Created    []string
	Updated    []string
	Superseded []string
	Revived    []string
	// Suppressed are observations that matched a fact the user deleted, or one
	// already superseded and not named as revived. They are not errors.
	Suppressed []Skip
	// Rejected are observations this tier refused: no evidence, an unknown
	// predicate, an empty sentence, or a credential in the text.
	Rejected []Skip
	// NewEvidence counts evidence rows that did not already exist. Confidence
	// only moves when this does.
	NewEvidence int
}

// Wrote reports whether anything reached the tier.
func (r Result) Wrote() bool { return len(r.Created)+len(r.Updated)+len(r.Revived) > 0 }

// Reconcile folds observations into the tier. It is idempotent: the same
// observations twice produce the same rows, the same dates and the same
// confidence, which is what makes MEMORY.md §4's re-run on every TurnCompleted
// safe to do every few seconds.
func (s *Store) Reconcile(ctx context.Context, obs []Observation) (Result, error) {
	var res Result
	now := s.now()

	for _, o := range obs {
		norm, skip := s.prepare(o)
		if skip != nil {
			res.Rejected = append(res.Rejected, *skip)
			continue
		}
		if err := s.reconcileOne(ctx, norm, now, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

// prepare validates and cleans one observation, or says why it cannot become a
// fact.
func (s *Store) prepare(o Observation) (Observation, *Skip) {
	label := strings.TrimSpace(o.Text)
	if label == "" {
		label = strings.TrimSpace(o.Object)
	}

	p, ok := ParsePredicate(string(o.Predicate))
	if !ok {
		return o, &Skip{Text: label, Reason: "unknown predicate " + string(o.Predicate)}
	}
	o.Predicate = p
	o.Subject = normSubject(o.Subject)
	o.Object = strings.TrimSpace(o.Object)
	o.Text = strings.TrimSpace(o.Text)
	if o.Object == "" || o.Text == "" {
		return o, &Skip{Text: label, Reason: "a fact needs an object and a sentence"}
	}

	// Nothing in this tier is a secret, and the check runs before the write
	// rather than after it. A sentence that contains a credential is refused
	// outright instead of stored with a marker in it: "uses [relay:redacted
	// Stripe secret key]" is not a preference, it is a leak with a bandage on.
	for _, field := range []string{o.Text, o.Object} {
		if _, found := s.redact.Redact(field); len(found) > 0 {
			return o, &Skip{Text: "[redacted]", Reason: "the sentence contained a " + found[0].Label + "; facts never hold credentials"}
		}
	}

	// Every fact carries evidence, and evidence carries a date.
	var ev []Evidence
	for _, e := range o.Evidence {
		if err := e.Valid(); err != nil {
			continue
		}
		e.Quote, _ = s.redact.Redact(e.Quote)
		ev = append(ev, e)
	}
	if len(ev) == 0 {
		return o, &Skip{Text: label, Reason: ErrNoEvidence.Error()}
	}
	o.Evidence = ev

	if o.Confidence <= 0 || o.Confidence > 1 {
		o.Confidence = 0.5
	}
	return o, nil
}

func (s *Store) reconcileOne(ctx context.Context, o Observation, now time.Time, res *Result) error {
	id := FactID(o.Subject, o.Predicate, o.Object)

	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		cur, found, err := readFactTx(ctx, tx, id)
		if err != nil {
			return err
		}

		switch {
		case found && cur.Deleted():
			// A fact the user deleted is a decision, not a gap. Re-deriving it
			// on the next turn would make "why did this come back" the most
			// common support question on the facts screen.
			res.Suppressed = append(res.Suppressed, Skip{Text: o.Text, Reason: "the user deleted this fact"})
			return nil
		case found && cur.Superseded():
			// Contradictions replace in both directions, but only when named.
			// Moving back to Firebase revives the Firebase fact and supersedes
			// the Supabase one; drifting back silently does not, because
			// silence is decay's job.
			// The test is specifically whether the new evidence names the fact
			// that *replaced* this one. Asking whether it names this one is no
			// test at all: a sentence about Firebase always says "Firebase".
			if !namesSupersessor(ctx, tx, o, cur) {
				res.Suppressed = append(res.Suppressed,
					Skip{Text: o.Text, Reason: "superseded by " + cur.SupersededBy + "; new evidence did not name it"})
				return nil
			}
			res.Revived = append(res.Revived, id)
		}

		// The survey runs before the fact row is written because the row has to
		// exist before its evidence can reference it, and the confidence that
		// goes into the row depends on how much of that evidence is new.
		newEvidence, first, last, err := surveyEvidenceTx(ctx, tx, id, o.Evidence)
		if err != nil {
			return err
		}
		res.NewEvidence += newEvidence

		f := Fact{
			ID: id, Subject: o.Subject, Predicate: o.Predicate,
			Object: o.Object, Text: o.Text, Confidence: o.Confidence,
			FirstSeen: first, LastSeen: last,
		}
		if found {
			f.FirstSeen = earliest(cur.FirstSeen, first)
			f.LastSeen = latest(cur.LastSeen, last)
			f.EditedAt = cur.EditedAt
			// A human correction is not re-derived over the top. The extractor
			// may refresh when a fact was last seen and what it points at; the
			// words on the screen stay the user's.
			if cur.Edited() {
				f.Text, f.Object, f.Predicate = cur.Text, cur.Object, cur.Predicate
			}
			// Confidence only moves on evidence the fact did not already have.
			f.Confidence = cur.Confidence
			if newEvidence > 0 {
				f.Confidence = combine(cur.Confidence, o.Confidence)
			}
		}

		if err := writeFactTx(ctx, tx, f); err != nil {
			return err
		}
		if err := insertEvidenceTx(ctx, tx, id, o.Evidence); err != nil {
			return err
		}
		if found {
			res.Updated = append(res.Updated, id)
		} else {
			res.Created = append(res.Created, id)
		}

		superseded, err := supersedeTx(ctx, tx, o, f, now)
		if err != nil {
			return err
		}
		res.Superseded = append(res.Superseded, superseded...)
		return nil
	})
}

// supersedeTx retires the facts this observation contradicts.
//
// Only ones it *names*. Two objects sharing a subject and a predicate is not
// evidence that they are alternatives — plenty of people deploy on two hosts —
// so guessing here would delete true facts silently, which is precisely the
// failure §5 says poisons every routing decision downstream.
func supersedeTx(ctx context.Context, tx *sql.Tx, o Observation, f Fact, now time.Time) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT `+factCols+` FROM fact
		WHERE subject = ? AND predicate = ? AND id != ?
		  AND deleted_at IS NULL AND superseded_at IS NULL`,
		f.Subject, string(f.Predicate), f.ID)
	if err != nil {
		return nil, err
	}
	var others []Fact
	for rows.Next() {
		old, err := scanFact(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		others = append(others, old)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []string
	for _, old := range others {
		if !namesFact(o, old) {
			continue
		}
		// A fact a human wrote or corrected is never buried by the extractor.
		// It stays on the screen where the user put it; two facts the user can
		// see and reconcile beat one the machine chose for them.
		if old.Edited() {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE fact SET superseded_by = ?, superseded_at = ? WHERE id = ?`,
			f.ID, now.UnixMilli(), old.ID); err != nil {
			return nil, err
		}
		out = append(out, old.ID)
	}
	return out, nil
}

// namesFact reports whether an observation points at an older fact — either by
// listing its object in Replaces or by saying its name in the new sentence
// ("moved from Firebase to Supabase").
func namesFact(o Observation, old Fact) bool {
	for _, r := range o.Replaces {
		if normObject(r) == normObject(old.Object) {
			return true
		}
	}
	return mentions(o.Text, old.Object)
}

// namesSupersessor reports whether an observation names the fact that retired
// this one — "back on Firebase, Supabase was a mistake".
func namesSupersessor(ctx context.Context, tx *sql.Tx, o Observation, cur Fact) bool {
	if cur.SupersededBy == "" {
		return false
	}
	var object string
	err := tx.QueryRowContext(ctx, `SELECT object FROM fact WHERE id = ?`, cur.SupersededBy).Scan(&object)
	if err != nil {
		return false
	}
	for _, r := range o.Replaces {
		if normObject(r) == normObject(object) {
			return true
		}
	}
	return mentions(o.Text, object)
}

// ------------------------------------------------------------------ reads --

// Filter narrows a listing. Its defaults are the review screen's default view:
// live facts, newest observation first.
type Filter struct {
	Subject   string
	Predicate Predicate
	// IncludeSuperseded adds the history half — DASHBOARD.md §3.3's toggle.
	IncludeSuperseded bool
	// IncludeDeleted is for the console's own audit, not for routing.
	IncludeDeleted bool
	// MinStrength drops facts that have decayed below a floor. At is the moment
	// to decay to, and defaults to now.
	MinStrength float64
	At          time.Time
	Limit       int
}

// List returns facts with their evidence attached.
func (s *Store) List(ctx context.Context, f Filter) ([]Fact, error) {
	q := `SELECT ` + factCols + ` FROM fact WHERE 1 = 1`
	var args []any
	if f.Subject != "" {
		q += ` AND subject = ?`
		args = append(args, normSubject(f.Subject))
	}
	if f.Predicate != "" {
		q += ` AND predicate = ?`
		args = append(args, string(f.Predicate))
	}
	if !f.IncludeDeleted {
		q += ` AND deleted_at IS NULL`
	}
	if !f.IncludeSuperseded {
		q += ` AND superseded_at IS NULL`
	}
	q += ` ORDER BY superseded_at IS NOT NULL, last_seen DESC, id`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := s.db.SQL().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	at := f.At
	if at.IsZero() {
		at = s.now()
	}
	var out []Fact
	for rows.Next() {
		v, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		if f.MinStrength > 0 && s.Strength(v, at) < f.MinStrength {
			continue
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.attachEvidence(ctx, out)
}

// Get returns one fact with its evidence.
func (s *Store) Get(ctx context.Context, id string) (Fact, error) {
	row := s.db.SQL().QueryRowContext(ctx, `SELECT `+factCols+` FROM fact WHERE id = ?`, id)
	f, err := scanFact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Fact{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Fact{}, err
	}
	list := []Fact{f}
	if err := s.attachEvidence(ctx, list); err != nil {
		return Fact{}, err
	}
	return list[0], nil
}

func (s *Store) attachEvidence(ctx context.Context, facts []Fact) error {
	if len(facts) == 0 {
		return nil
	}
	byID := make(map[string]int, len(facts))
	for i, f := range facts {
		byID[f.ID] = i
	}
	rows, err := s.db.SQL().QueryContext(ctx,
		`SELECT fact_id, runtime, session_id, path, byte_offset, quote, at
		 FROM fact_evidence ORDER BY at DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var factID string
		var e Evidence
		var at int64
		if err := rows.Scan(&factID, &e.Runtime, &e.SessionID, &e.Path, &e.ByteOffset, &e.Quote, &at); err != nil {
			return err
		}
		e.At = time.UnixMilli(at).UTC()
		if i, ok := byID[factID]; ok {
			facts[i].Evidence = append(facts[i].Evidence, e)
		}
	}
	return rows.Err()
}

// ----------------------------------------------------------------- writes --

// Edit is a human correction from DASHBOARD.md §3.3. Every field is a pointer
// so "leave this alone" and "set it to empty" are different requests.
type Edit struct {
	Text       *string
	Object     *string
	Predicate  *Predicate
	Confidence *float64
}

// Edit applies a correction and stamps edited_at, which is what stops the
// extractor rewriting it on the next turn.
//
// Editing the object changes what the fact is *about*, and the identity of a
// fact is its subject, predicate and object — so the row keeps its id and the
// extractor's next sighting of the original object lands back on it. That is
// deliberate: a corrected fact absorbs the correction rather than spawning a
// second row that says the thing the user just fixed.
func (s *Store) Edit(ctx context.Context, id string, e Edit) (Fact, error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return Fact{}, err
	}
	if cur.Deleted() {
		return Fact{}, fmt.Errorf("%w: %s was deleted", ErrNotFound, id)
	}

	next := cur
	if e.Text != nil {
		t := strings.TrimSpace(*e.Text)
		if t == "" {
			return Fact{}, errors.New("facts: a fact with no sentence is not a correction; delete it instead")
		}
		next.Text = t
	}
	if e.Object != nil {
		o := strings.TrimSpace(*e.Object)
		if o == "" {
			return Fact{}, errors.New("facts: a fact needs an object")
		}
		next.Object = o
	}
	if e.Predicate != nil {
		p, ok := ParsePredicate(string(*e.Predicate))
		if !ok {
			return Fact{}, fmt.Errorf("facts: unknown predicate %q", *e.Predicate)
		}
		next.Predicate = p
	}
	if e.Confidence != nil {
		if *e.Confidence < 0 || *e.Confidence > 1 {
			return Fact{}, errors.New("facts: confidence is between 0 and 1")
		}
		next.Confidence = *e.Confidence
	}

	// A human can no more write a secret into this tier than the extractor can.
	for _, field := range []string{next.Text, next.Object} {
		if _, found := s.redact.Redact(field); len(found) > 0 {
			return Fact{}, fmt.Errorf("facts: that text contains a %s; facts never hold credentials", found[0].Label)
		}
	}

	next.EditedAt = s.now()
	if err := s.db.Tx(ctx, func(tx *sql.Tx) error { return writeFactTx(ctx, tx, next) }); err != nil {
		return Fact{}, err
	}
	return s.Get(ctx, id)
}

// Delete removes a fact from the tier without losing that it existed.
//
// Soft, for two reasons: the evidence rows point at transcripts that are still
// on disk, and a deleted fact is a signal the extractor must not immediately
// re-derive. A hard delete makes "why did this come back" unanswerable.
func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.SQL().ExecContext(ctx,
		`UPDATE fact SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		s.now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

// SweepResult is what a sweep removed.
type SweepResult struct {
	// Unevidenced is the ids of facts that could not point at where they came
	// from. MEMORY.md §5: deleted, not kept at low confidence.
	Unevidenced []string
}

// Sweep enforces the evidence rule against the table itself.
//
// [Store.Reconcile] cannot create an unevidenced fact, but a transcript can be
// removed, a session can be re-read, and a future writer can be wrong. This is
// the periodic check that the rule still holds, and it *deletes* — hard, rows
// and all — rather than lowering a confidence, because §5 is explicit that a
// fact which cannot point at where it came from is not a weak fact.
func (s *Store) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT f.id FROM fact f
		WHERE NOT EXISTS (SELECT 1 FROM fact_evidence e WHERE e.fact_id = f.id)`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return res, err
		}
		res.Unevidenced = append(res.Unevidenced, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, id := range res.Unevidenced {
		// superseded_by is ON DELETE SET NULL, so the history pointer degrades
		// to "superseded, by something no longer here" instead of cascading a
		// second deletion.
		if _, err := s.db.SQL().ExecContext(ctx, `DELETE FROM fact WHERE id = ?`, id); err != nil {
			return res, err
		}
		s.log.Info("facts: deleted a fact with no evidence", "fact", id)
	}
	return res, nil
}

// ------------------------------------------------------------------- rows --

const factCols = `id, subject, predicate, object, text, confidence, first_seen,
	last_seen, superseded_by, superseded_at, edited_at, deleted_at`

func scanFact(sc interface{ Scan(...any) error }) (Fact, error) {
	var f Fact
	var predicate string
	var supersededBy sql.NullString
	var first, last int64
	var supersededAt, editedAt, deletedAt *int64
	err := sc.Scan(&f.ID, &f.Subject, &predicate, &f.Object, &f.Text, &f.Confidence,
		&first, &last, &supersededBy, &supersededAt, &editedAt, &deletedAt)
	if err != nil {
		return Fact{}, err
	}
	f.Predicate = Predicate(predicate)
	f.SupersededBy = supersededBy.String
	f.FirstSeen = time.UnixMilli(first).UTC()
	f.LastSeen = time.UnixMilli(last).UTC()
	f.SupersededAt = fromPtr(supersededAt)
	f.EditedAt = fromPtr(editedAt)
	f.DeletedAt = fromPtr(deletedAt)
	return f, nil
}

func readFactTx(ctx context.Context, tx *sql.Tx, id string) (Fact, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+factCols+` FROM fact WHERE id = ?`, id)
	f, err := scanFact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Fact{}, false, nil
	}
	if err != nil {
		return Fact{}, false, err
	}
	return f, true, nil
}

// writeFactTx upserts the row. Reviving a superseded fact clears the pointer,
// which is why superseded_by and superseded_at are written rather than left.
func writeFactTx(ctx context.Context, tx *sql.Tx, f Fact) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO fact (id, subject, predicate, object, text, confidence,
			first_seen, last_seen, superseded_by, superseded_at, edited_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			subject = excluded.subject, predicate = excluded.predicate,
			object = excluded.object, text = excluded.text,
			confidence = excluded.confidence, first_seen = excluded.first_seen,
			last_seen = excluded.last_seen, superseded_by = excluded.superseded_by,
			superseded_at = excluded.superseded_at, edited_at = excluded.edited_at,
			deleted_at = excluded.deleted_at`,
		f.ID, f.Subject, string(f.Predicate), f.Object, f.Text, clamp(f.Confidence),
		f.FirstSeen.UnixMilli(), f.LastSeen.UnixMilli(),
		nullString(f.SupersededBy), nullTime(f.SupersededAt),
		nullTime(f.EditedAt), nullTime(f.DeletedAt))
	return err
}

// surveyEvidenceTx reports how many of these citations the tier does not
// already have, and the span of dates they cover.
//
// It is a read, run before the fact row is written, for two reasons. The row
// must exist before its evidence can point at it, and the confidence written
// into that row depends on whether any of this is new: SQLite's RowsAffected
// says 1 for an insert and 1 for a DO UPDATE alike, so counting after the fact
// would count every re-run as a fresh sighting and let the live path talk
// itself into certainty from one session.
func surveyEvidenceTx(ctx context.Context, tx *sql.Tx, factID string, ev []Evidence) (int, time.Time, time.Time, error) {
	var newRows int
	var first, last time.Time
	seen := map[string]bool{}

	for _, e := range ev {
		first, last = earliest(first, e.At), latest(last, e.At)
		id := e.ID(factID)
		if seen[id] {
			continue
		}
		seen[id] = true

		var existed int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM fact_evidence WHERE id = ?`, id).Scan(&existed); err != nil {
			return newRows, first, last, err
		}
		if existed == 0 {
			newRows++
		}
	}
	return newRows, first, last, nil
}

// insertEvidenceTx writes the citations, leaving ones already there alone apart
// from a refreshed pointer. It runs after the fact row exists.
func insertEvidenceTx(ctx context.Context, tx *sql.Tx, factID string, ev []Evidence) error {
	sorted := append([]Evidence(nil), ev...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })

	for _, e := range sorted {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO fact_evidence (id, fact_id, runtime, session_id, path, byte_offset, quote, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				path = excluded.path, quote = excluded.quote,
				at = min(fact_evidence.at, excluded.at)`,
			e.ID(factID), factID, e.Runtime, e.SessionID, e.Path, e.ByteOffset,
			e.Quote, e.At.UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func fromPtr(v *int64) time.Time {
	if v == nil {
		return time.Time{}
	}
	return time.UnixMilli(*v).UTC()
}

func earliest(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() || a.Before(b) {
		return a
	}
	return b
}

func latest(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() || a.After(b) {
		return a
	}
	return b
}
