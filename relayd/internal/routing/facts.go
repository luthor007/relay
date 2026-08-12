package routing

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/store"
)

// MinPreferenceConfidence is how sure a learned preference has to be before it
// outranks the entitlement table.
//
// It is high, and it is high because of what step 2 of MEMORY.md §8 costs when
// it is wrong: a weakly-evidenced "you always use Codex for this" sends work
// past a subscription the user already pays for and onto a metered key. A
// preference that cannot clear this stays out of the way and entitlement
// decides, which is the cheaper mistake.
const MinPreferenceConfidence = 0.6

// PreferenceHalfLife is how fast a learned preference decays on last
// observation. MEMORY.md §5 decays facts on last sighting rather than on
// creation, so a long-held habit that still shows up stays strong and one that
// stopped six months ago quietly stops steering anything.
const PreferenceHalfLife = 90 * 24 * time.Hour

// FactPreferences reads MEMORY.md §5's facts tier and answers question 2's
// step 2 with it.
//
// It reads and never writes. The facts tier is owned elsewhere; routing is a
// consumer of it, and a router that edited the evidence it routes on would be
// unfalsifiable.
type FactPreferences struct {
	db  *store.DB
	now func() time.Time
}

// NewFactPreferences builds one over the main database.
func NewFactPreferences(db *store.DB) (*FactPreferences, error) {
	if db == nil {
		return nil, fmt.Errorf("routing: fact preferences need the main database")
	}
	if db.Kind() != store.KindMain {
		// The vault is a separate file with no facts in it, and asking it for
		// preferences would silently answer "none" forever.
		return nil, fmt.Errorf("routing: facts live in the main database, not the %s one", db.Kind())
	}
	return &FactPreferences{db: db, now: time.Now}, nil
}

// SetClock is for tests.
func (p *FactPreferences) SetClock(now func() time.Time) { p.now = now }

// Preference is one learned routing fact.
type Preference struct {
	Runtime adapter.Runtime
	// Scope are the words that gate it: "rust", "the api repo". Empty means it
	// applies to everything.
	Scope []string
	// Text is the sentence a human reads, which is also what gets spoken when
	// someone asks why.
	Text string
	// Confidence is the stored confidence after decay.
	Confidence float64
	LastSeen   time.Time
}

// Preferred implements [Preferences].
func (p *FactPreferences) Preferred(ctx context.Context, req RuntimeRequest) (adapter.Runtime, string, bool) {
	prefs, err := p.List(ctx)
	if err != nil {
		return "", "", false
	}
	want := append(tokens(req.Text), tokens(baseName(req.Workspace))...)

	var best Preference
	var bestScore float64
	for _, pref := range prefs {
		if pref.Confidence < MinPreferenceConfidence {
			continue
		}
		score := pref.Confidence
		if len(pref.Scope) > 0 {
			hit := overlap(pref.Scope, want)
			if hit == 0 {
				// A scoped preference that does not match this request is not a
				// preference for this request. Applying it anyway is how "uses
				// Codex for Rust" becomes "uses Codex for everything".
				continue
			}
			score *= 0.5 + 0.5*hit
			// A matching scope beats a global habit, because it is the more
			// specific evidence.
			score += 0.25
		}
		if score > bestScore {
			best, bestScore = pref, score
		}
	}
	if best.Runtime == "" {
		return "", "", false
	}
	return best.Runtime, best.Text, true
}

// List reads the live preference facts.
func (p *FactPreferences) List(ctx context.Context) ([]Preference, error) {
	rows, err := p.db.SQL().QueryContext(ctx, `
        SELECT predicate, object, text, confidence, last_seen
          FROM fact
         WHERE deleted_at IS NULL
           AND superseded_at IS NULL
           AND predicate IN ('prefers', 'uses')
      ORDER BY confidence DESC, last_seen DESC`)
	if err != nil {
		return nil, fmt.Errorf("routing: read facts: %w", err)
	}
	defer rows.Close()

	now := p.now()
	var out []Preference
	for rows.Next() {
		var predicate, object, text string
		var confidence float64
		var lastSeen int64
		if err := rows.Scan(&predicate, &object, &text, &confidence, &lastSeen); err != nil {
			return nil, fmt.Errorf("routing: scan fact: %w", err)
		}
		pref, ok := preferenceOf(object, text, confidence, time.UnixMilli(lastSeen), now)
		if !ok {
			continue
		}
		out = append(out, pref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("routing: read facts: %w", err)
	}
	return out, nil
}

// preferenceOf turns one fact row into a routing preference, or reports that it
// is not one.
//
// Only a fact whose object names one of the five runtimes counts. "prefers
// TypeScript" is a true and useful fact that says nothing about which agent
// should run the work, and a router that read it as one would be inventing a
// preference out of an unrelated observation.
func preferenceOf(object, text string, confidence float64, lastSeen, now time.Time) (Preference, bool) {
	rt := ParseRuntime(object)
	if rt == "" {
		rt = runtimeIn(text)
	}
	if rt == "" {
		return Preference{}, false
	}

	pref := Preference{
		Runtime:    rt,
		Text:       text,
		LastSeen:   lastSeen,
		Confidence: clamp(confidence * decay(now.Sub(lastSeen), PreferenceHalfLife)),
		Scope:      scopeOf(text, rt),
	}
	return pref, true
}

// runtimeIn finds a runtime name inside a sentence, checking the two-word names
// first so "claude code" is not read as "claude".
func runtimeIn(text string) adapter.Runtime {
	t := normalize(text)
	for _, name := range []string{"claude code", "open claw", "open code"} {
		if strings.Contains(t, name) {
			return ParseRuntime(name)
		}
	}
	for _, f := range strings.Fields(t) {
		if rt := ParseRuntime(f); rt != "" {
			return rt
		}
	}
	return ""
}

// scopeOf is what is left of the sentence once the runtime name and the habit
// verbs are removed: "always uses Codex for Rust" scopes to "rust".
func scopeOf(text string, rt adapter.Runtime) []string {
	drop := map[string]bool{
		"always": true, "usually": true, "prefers": true, "prefer": true,
		"uses": true, "use": true, "using": true, "user": true, "work": true,
		"works": true, "runs": true, "run": true, "does": true, "do": true,
		"everything": true, "all": true,
	}
	for _, t := range tokens(RuntimeLabel(rt)) {
		drop[t] = true
	}
	for _, t := range tokens(string(rt)) {
		drop[t] = true
	}

	var out []string
	for _, t := range tokens(text) {
		if drop[t] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// PutPreferenceFact writes one preference into the facts tier.
//
// It exists so a stated preference — "always use Codex for Rust", said out loud
// once — becomes a durable fact rather than a setting nobody can find, and so
// this package's tests exercise the same rows the extractor writes. Every fact
// written here carries its evidence, which MEMORY.md §5 requires: a fact that
// cannot point at where it came from is deleted rather than kept at low
// confidence.
func PutPreferenceFact(ctx context.Context, db *store.DB, f PreferenceFact) error {
	if f.ID == "" {
		return fmt.Errorf("routing: a fact needs an id")
	}
	if f.Runtime == "" {
		return fmt.Errorf("routing: a routing preference needs a runtime")
	}
	if f.Text == "" {
		return fmt.Errorf("routing: a fact needs the sentence a human reads")
	}
	if f.Confidence <= 0 {
		f.Confidence = 0.7
	}
	if f.At.IsZero() {
		f.At = time.Now()
	}
	at := f.At.UnixMilli()

	return db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO fact (id, subject, predicate, object, text, confidence, first_seen, last_seen)
            VALUES (?, 'user', 'prefers', ?, ?, ?, ?, ?)
            ON CONFLICT (id) DO UPDATE SET
                object = excluded.object,
                text = excluded.text,
                confidence = excluded.confidence,
                last_seen = excluded.last_seen`,
			f.ID, string(f.Runtime), f.Text, f.Confidence, at, at); err != nil {
			return err
		}
		if f.Session == "" {
			// MEMORY.md §5: no evidence, no fact.
			return fmt.Errorf("routing: a fact needs evidence — a session it came from")
		}
		_, err := tx.ExecContext(ctx, `
            INSERT INTO fact_evidence (id, fact_id, runtime, session_id, quote, at)
            VALUES (?, ?, ?, ?, ?, ?)
            ON CONFLICT (id) DO UPDATE SET quote = excluded.quote, at = excluded.at`,
			f.ID+":ev", f.ID, string(f.Runtime), f.Session, f.Quote, at)
		return err
	})
}

// PreferenceFact is one stated or observed routing preference.
type PreferenceFact struct {
	ID      string
	Runtime adapter.Runtime
	// Text is the sentence: "always uses Codex for Rust".
	Text       string
	Confidence float64
	// Session and Quote are the evidence.
	Session string
	Quote   string
	At      time.Time
}

// clamp keeps a decayed confidence in range without a branch at each call site.
func clamp(v float64) float64 { return math.Max(0, math.Min(1, v)) }
