package facts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/logx"
	"github.com/luthor007/relay/relayd/internal/store"
)

// Scope is one session to extract from. MEMORY.md §4's live paragraph re-runs
// extraction "against that session only" — the whole corpus is an hour or two
// of work and re-running it on every turn boundary would make the machine
// unusable.
type Scope struct {
	Runtime   string
	SessionID string
}

func (s Scope) valid() error {
	if s.Runtime == "" || s.SessionID == "" {
		return errors.New("facts: a scope needs a runtime and a session")
	}
	return nil
}

// EpisodeRuntime is the runtime name evidence carries when a fact came from a
// spoken episode rather than an agent session. The evidence columns are
// (runtime, session_id) for both, so an episode is addressable by the same
// pointer the review screen already knows how to render.
const EpisodeRuntime = "episode"

// Batch is one extraction run.
type Batch struct {
	Observations []Observation
	ModelCalls   int
	// Sources is how many numbered lines the model was shown. Zero with no
	// error means there was nothing to read, which is not a failure.
	Sources int
	// Skipped explains an empty batch that was not "nothing to find". An
	// extractor that returns nothing because the model timed out and one that
	// returns nothing because the user said nothing durable must not look the
	// same to the caller.
	Skipped string
}

// Extractor re-derives durable facts for one session.
type Extractor interface {
	Extract(ctx context.Context, sc Scope) (Batch, error)
}

// None is the extractor for a build with no model configured. Explicit rather
// than a nil check, so "facts are off" appears in the wiring instead of being
// inferred from a nil pointer.
type None struct{}

// Extract always returns an empty batch that says why.
func (None) Extract(context.Context, Scope) (Batch, error) {
	return Batch{Skipped: "no fact extractor configured"}, nil
}

var _ Extractor = None{}

// MaxPerRun bounds one extraction. A session that appears to yield thirty
// durable preferences has produced thirty guesses.
const MaxPerRun = 8

// MaxSources caps how many summaries or episodes one prompt carries.
const MaxSources = 40

// MaxInputChars caps the prompt. The small model is the one being used here,
// per ORCHESTRATOR.md §3b, and its context is the tighter of the two.
const MaxInputChars = 12000

// LLM extracts facts through the small model, from the index and the episodes.
//
// Two things it does that a naive extractor does not, and both exist to serve
// §5's first rule:
//
//   - **It numbers its sources and requires the model to cite them.** A fact
//     with no citation is dropped at the boundary, before it can reach a store
//     that would have to reject it anyway. Attaching every source to every fact
//     — the obvious shortcut — produces evidence that is technically present
//     and useless on the review screen, which is worse than none because it
//     looks like it works.
//   - **It redacts what it sends.** Session summaries arrive redacted from
//     internal/summarize, but episode transcripts are raw text off the day's
//     audio and have never been through a detector. A key posted to a model
//     provider has already left the machine, so the detector runs on the way
//     out as well as on the way in.
type LLM struct {
	DB    *store.DB
	Model llm.Provider
	// Redact is required, for the reason above.
	Redact Redactor
	Log    *slog.Logger
	Now    func() time.Time
}

var _ Extractor = (*LLM)(nil)

const prompt = `You extract durable facts about a developer from numbered notes about their work.
A durable fact is a lasting preference, service, tool or language choice: "prefers Supabase over Firebase", "deploys on Vercel", "writes Go for daemons".
Not a fact: anything about one task, one bug, one file, or one day's work.

Return a JSON array and nothing else. Each element:
{"predicate":"prefers|uses|deploys_on|writes","object":"...","text":"...","confidence":0.0-1.0,"sources":[1,2],"replaces":["..."]}

"sources" lists the numbers of the notes that support the fact. A fact with no sources is discarded, so cite or do not claim.
"replaces" names anything this fact contradicts — the old service they moved off. Leave it empty unless a note actually says so.
Return [] if the notes support none. Returning nothing is the correct answer far more often than not.
Never state a fact the notes do not support.`

// Extract reads one session's summaries and asks the model what is durable.
func (e *LLM) Extract(ctx context.Context, sc Scope) (Batch, error) {
	if err := sc.valid(); err != nil {
		return Batch{}, err
	}
	sources, err := e.sessionSources(ctx, sc)
	if err != nil {
		return Batch{}, err
	}
	if len(sources) == 0 {
		return Batch{Skipped: "no summaries for this session yet"}, nil
	}
	return e.ask(ctx, sources)
}

// ExtractEpisodes reads the day's episodes — SYSTEM.md §5's entity, not an
// agent session — and asks the same question of them.
//
// It is a separate call rather than part of [LLM.Extract] because the two have
// different cadences: sessions re-extract on every completed turn, episodes at
// most once a segment. Folding them together would re-ask the model about a
// week of conversation every few seconds.
func (e *LLM) ExtractEpisodes(ctx context.Context, since time.Time, limit int) (Batch, error) {
	sources, err := e.episodeSources(ctx, since, limit)
	if err != nil {
		return Batch{}, err
	}
	if len(sources) == 0 {
		return Batch{Skipped: "no episodes in that window"}, nil
	}
	return e.ask(ctx, sources)
}

// source is one numbered note the model sees, with the pointer that will become
// evidence if it is cited.
type source struct {
	Text string
	Ev   Evidence
}

func (e *LLM) sessionSources(ctx context.Context, sc Scope) ([]source, error) {
	rows, err := e.DB.SQL().QueryContext(ctx, `
		SELECT text, path, byte_offset, created_at FROM summary
		WHERE runtime = ? AND session_id = ?
		ORDER BY kind = 'session' DESC, byte_offset
		LIMIT ?`, sc.Runtime, sc.SessionID, MaxSources)
	if err != nil {
		return nil, fmt.Errorf("facts: read summaries: %w", err)
	}
	defer rows.Close()

	var out []source
	for rows.Next() {
		var text, path string
		var offset, created int64
		if err := rows.Scan(&text, &path, &offset, &created); err != nil {
			return nil, err
		}
		at := time.UnixMilli(created).UTC()
		out = append(out, source{
			Text: text,
			Ev: Evidence{
				Runtime: sc.Runtime, SessionID: sc.SessionID,
				Path: path, ByteOffset: offset, Quote: clip(text, 200), At: at,
			},
		})
	}
	return out, rows.Err()
}

func (e *LLM) episodeSources(ctx context.Context, since time.Time, limit int) ([]source, error) {
	if limit <= 0 || limit > MaxSources {
		limit = MaxSources
	}
	rows, err := e.DB.SQL().QueryContext(ctx, `
		SELECT id, started_at, transcript FROM episode
		WHERE started_at >= ? AND transcript != ''
		ORDER BY started_at DESC LIMIT ?`, since.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("facts: read episodes: %w", err)
	}
	defer rows.Close()

	var out []source
	for rows.Next() {
		var id, transcript string
		var started int64
		if err := rows.Scan(&id, &started, &transcript); err != nil {
			return nil, err
		}
		at := time.UnixMilli(started).UTC()
		out = append(out, source{
			Text: transcript,
			Ev: Evidence{
				Runtime: EpisodeRuntime, SessionID: id,
				Quote: clip(transcript, 200), At: at,
			},
		})
	}
	return out, rows.Err()
}

// ask builds the prompt, calls the model and turns citations into evidence.
func (e *LLM) ask(ctx context.Context, sources []source) (Batch, error) {
	res := Batch{Sources: len(sources)}
	if e.DB == nil {
		return res, errors.New("facts: extractor has no store")
	}
	if e.Redact == nil {
		return res, ErrNoRedactor
	}
	log := e.Log
	if log == nil {
		log = logx.Discard()
	}
	if e.Model == nil {
		res.Skipped = "no model configured"
		return res, nil
	}

	var b strings.Builder
	for i, s := range sources {
		clean, found := e.Redact.Redact(s.Text)
		if len(found) > 0 {
			log.Info("facts: redacted a credential before the model saw it",
				"detector", found[0].RuleID, "count", len(found))
		}
		sources[i].Text = clean
		sources[i].Ev.Quote, _ = e.Redact.Redact(sources[i].Ev.Quote)
		fmt.Fprintf(&b, "[%d] %s\n", i+1, clip(strings.TrimSpace(clean), 1200))
	}

	resp, err := e.Model.Complete(ctx, llm.Request{
		System:    prompt,
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: clip(b.String(), MaxInputChars)}},
		MaxTokens: 700,
	})
	if err != nil {
		// A failed model call is a reported skip, not an error that loses the
		// turn summary this ran behind.
		log.Warn("facts: extraction failed", "err", err)
		res.Skipped = "model call failed: " + err.Error()
		return res, nil
	}
	res.ModelCalls = 1

	raw, perr := parse(resp.Text)
	if perr != nil {
		log.Info("facts: unparseable model output", "err", perr)
		res.Skipped = "unparseable model output"
		return res, nil
	}

	for _, r := range raw {
		if len(res.Observations) >= MaxPerRun {
			break
		}
		o, ok := r.observation(sources)
		if !ok {
			continue
		}
		res.Observations = append(res.Observations, o)
	}
	return res, nil
}

// rawFact is the model's own JSON shape.
type rawFact struct {
	Predicate  string   `json:"predicate"`
	Object     string   `json:"object"`
	Text       string   `json:"text"`
	Confidence float64  `json:"confidence"`
	Sources    []int    `json:"sources"`
	Replaces   []string `json:"replaces"`
}

// observation resolves the model's citations into evidence, and refuses a fact
// whose citations resolve to nothing.
func (r rawFact) observation(sources []source) (Observation, bool) {
	object := strings.TrimSpace(r.Object)
	text := strings.TrimSpace(r.Text)
	if object == "" || text == "" {
		return Observation{}, false
	}
	p, ok := ParsePredicate(r.Predicate)
	if !ok {
		return Observation{}, false
	}

	seen := map[int]bool{}
	var ev []Evidence
	for _, n := range r.Sources {
		i := n - 1
		if i < 0 || i >= len(sources) || seen[i] {
			continue
		}
		seen[i] = true
		ev = append(ev, sources[i].Ev)
	}
	// A model that cited nothing, or cited notes that do not exist, has
	// produced a claim with no provenance. It is dropped here rather than
	// stored at low confidence — §5's first rule, applied at the boundary
	// where the citation is still checkable.
	if len(ev) == 0 {
		return Observation{}, false
	}

	conf := r.Confidence
	if conf <= 0 || conf > 1 {
		conf = 0.5
	}
	var replaces []string
	for _, v := range r.Replaces {
		if v = strings.TrimSpace(v); v != "" {
			replaces = append(replaces, v)
		}
	}
	return Observation{
		Subject: DefaultSubject, Predicate: p, Object: object, Text: text,
		Confidence: conf, Evidence: ev, Replaces: replaces,
	}, true
}

// parse reads the model's JSON, tolerating the fenced-code wrapper models add
// regardless of instruction.
func parse(out string) ([]rawFact, error) {
	s := strings.TrimSpace(out)
	if i := strings.Index(s, "["); i >= 0 {
		if j := strings.LastIndex(s, "]"); j > i {
			s = s[i : j+1]
		}
	}
	var raw []rawFact
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
