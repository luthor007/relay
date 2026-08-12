package compaction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/facts"
	"github.com/luthor007/relay/relayd/internal/store"
)

// FactSource supplies MEMORY.md §5's durable facts as sentences a brief can
// carry. It is a one-method interface so a deployment with no fact tier yet
// hands in nil and gets a brief without the facts section rather than an error.
type FactSource interface {
	// Sentences returns the facts worth telling a fresh session, strongest
	// first. workspace is a hint, not a filter — most facts are about the
	// stack rather than about one repo.
	Sentences(ctx context.Context, workspace string, limit int) ([]string, error)
}

// FactStore adapts the facts tier to [FactSource].
type FactStore struct {
	Store *facts.Store
	// MinStrength drops facts that have decayed below a floor. Zero takes
	// facts.StaleBelow, which is the same floor routing uses.
	MinStrength float64
	Now         func() time.Time
}

// Sentences returns the strongest live facts, with anything that names this
// workspace pulled to the front.
func (f FactStore) Sentences(ctx context.Context, workspace string, limit int) ([]string, error) {
	if f.Store == nil {
		return nil, nil
	}
	at := time.Now()
	if f.Now != nil {
		at = f.Now()
	}
	min := f.MinStrength
	if min <= 0 {
		min = facts.StaleBelow
	}

	rows, err := f.Store.List(ctx, facts.Filter{MinStrength: min, At: at})
	if err != nil {
		return nil, fmt.Errorf("compaction: read facts: %w", err)
	}

	type scored struct {
		text  string
		score float64
	}
	base := strings.ToLower(baseName(workspace))
	out := make([]scored, 0, len(rows))
	for _, r := range rows {
		s := f.Store.Strength(r, at)
		if base != "" && strings.Contains(strings.ToLower(r.Text+" "+r.Object), base) {
			// A fact that names this repo is the more specific evidence, the
			// same way a scoped routing preference beats a global habit.
			s += 1
		}
		out = append(out, scored{text: r.Text, score: s})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })

	var texts []string
	for _, s := range out {
		if limit > 0 && len(texts) >= limit {
			break
		}
		if strings.TrimSpace(s.text) == "" {
			continue
		}
		texts = append(texts, s.text)
	}
	return texts, nil
}

// StoreBriefs builds handoff briefs out of what Relay already stores: the
// session summary from MEMORY.md §3's index, the recent turns and tool targets
// from the registry tier, and the facts from §5.
//
// This is the whole argument for the handoff living here. A runtime compacting
// has only its own transcript; every query below reads something no runtime can
// see, and the result is usually smaller and better aimed than the compaction
// it replaces.
type StoreBriefs struct {
	db    *store.DB
	build *BriefBuilder
	facts FactSource

	// Recent is how many recent turns to read, Files how many tool targets.
	Recent int
	Files  int
	// MaxFacts bounds the facts section before the budget gets to it.
	MaxFacts int
}

// NewStoreBriefs builds one over the main database.
func NewStoreBriefs(db *store.DB, b *BriefBuilder, f FactSource) (*StoreBriefs, error) {
	if db == nil {
		return nil, errors.New("compaction: briefs need the main database")
	}
	if db.Kind() != store.KindMain {
		return nil, fmt.Errorf("compaction: the index and the facts live in the main database, not the %s one", db.Kind())
	}
	if b == nil {
		return nil, ErrNoRedactor
	}
	return &StoreBriefs{db: db, build: b, facts: f, Recent: 6, Files: 12, MaxFacts: 6}, nil
}

// Brief implements [Briefs].
func (s *StoreBriefs) Brief(ctx context.Context, v SessionView) (Brief, error) {
	in := BriefInput{Session: v.ID, Runtime: v.Runtime, Workspace: v.Workspace}

	native := ""
	if row, err := s.db.GetSession(ctx, v.ID); err == nil {
		native = row.NativeID
		if in.Workspace == "" {
			in.Workspace = row.Workspace
		}
		if in.Runtime == "" {
			in.Runtime = adapter.Runtime(row.Runtime)
		}
		in.Summary = row.Subject
	} else if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
		return Brief{}, fmt.Errorf("compaction: read session %s: %w", v.ID, err)
	}

	// The summary table keys on the runtime's own session id, which is not
	// Relay's. Try both rather than silently finding nothing for every session
	// whose runtime named itself.
	summary, err := s.summary(ctx, string(in.Runtime), native, v.ID)
	if err != nil {
		return Brief{}, err
	}
	if summary != "" {
		in.Summary = summary
	}

	if in.Recent, err = s.recent(ctx, v.ID); err != nil {
		return Brief{}, err
	}
	if in.Files, err = s.files(ctx, v.ID); err != nil {
		return Brief{}, err
	}
	if s.facts != nil {
		if in.Facts, err = s.facts.Sentences(ctx, in.Workspace, s.MaxFacts); err != nil {
			return Brief{}, err
		}
	}
	return s.build.Build(in)
}

func (s *StoreBriefs) summary(ctx context.Context, runtime, native, relayID string) (string, error) {
	ids := make([]string, 0, 2)
	for _, id := range []string{native, relayID} {
		if id != "" && !contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		var text string
		err := s.db.SQL().QueryRowContext(ctx, `
			SELECT text FROM summary
			 WHERE kind = 'session' AND session_id = ? AND (? = '' OR runtime = ?)
			 ORDER BY created_at DESC LIMIT 1`, id, runtime, runtime).Scan(&text)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return "", fmt.Errorf("compaction: read summary: %w", err)
		}
		if strings.TrimSpace(text) != "" {
			return text, nil
		}
	}
	return "", nil
}

// recent reads the last few user turns, oldest first.
//
// User turns rather than agent ones, deliberately: what the person asked for is
// a much better statement of what the work is than what the agent said back,
// and it is shorter. The agent's prose is already summarised into the index,
// which is where the summary above comes from.
func (s *StoreBriefs) recent(ctx context.Context, sessionID string) ([]string, error) {
	n := s.Recent
	if n <= 0 {
		n = 6
	}
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT text FROM turn
		 WHERE session_id = ? AND role = 'user' AND text <> ''
		 ORDER BY at DESC LIMIT ?`, sessionID, n)
	if err != nil {
		return nil, fmt.Errorf("compaction: read turns: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("compaction: scan turn: %w", err)
		}
		out = append(out, text)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("compaction: read turns: %w", err)
	}
	// The query is newest first because that is what LIMIT wants; the brief
	// wants oldest first, so the newest is the one Build falls back to.
	reverse(out)
	return out, nil
}

func (s *StoreBriefs) files(ctx context.Context, sessionID string) ([]string, error) {
	n := s.Files
	if n <= 0 {
		n = 12
	}
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT target FROM tool_call
		 WHERE session_id = ? AND target <> ''
		 ORDER BY at DESC LIMIT ?`, sessionID, n)
	if err != nil {
		return nil, fmt.Errorf("compaction: read tool calls: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, fmt.Errorf("compaction: scan tool call: %w", err)
		}
		out = append(out, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("compaction: read tool calls: %w", err)
	}
	return out, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
