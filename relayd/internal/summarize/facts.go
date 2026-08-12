package summarize

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

// FactScope is which session to extract from. MEMORY.md §4's live path re-runs
// extraction "against that session only" — the whole corpus is an hour of work
// and re-running it on every turn boundary would make the machine unusable.
type FactScope struct {
	Runtime   string
	SessionID string
}

// Evidence is where a fact came from. MEMORY.md §5: a fact that cannot point at
// where it came from is deleted, not kept at low confidence.
type Evidence struct {
	Runtime    string
	SessionID  string
	Path       string
	ByteOffset int64
	Quote      string
	At         time.Time
}

// Fact is a proposal, not a stored fact.
//
// This package extracts and evidences; it does not write to the fact table.
// MEMORY.md §11 puts fact extraction *and the review screen* together at step
// 5b, and §5 explains why they cannot be separated: an unexamined inference
// store poisons every routing decision downstream, silently, forever. Writing
// facts before there is a screen to correct them on would build exactly that.
type Fact struct {
	Predicate  string // prefers | uses | deploys_on | writes
	Object     string
	Text       string // the sentence a human reads
	Confidence float64
	Evidence   []Evidence
}

// FactResult is one extraction run.
type FactResult struct {
	Facts      []Fact
	ModelCalls int
	// Skipped explains an empty result that was not "nothing to find".
	Skipped string
}

// FactExtractor re-derives durable facts for one session.
type FactExtractor interface {
	Extract(ctx context.Context, scope FactScope) (FactResult, error)
}

// NoFacts is the extractor that does nothing, for a build with no model
// configured. It is explicit rather than a nil check so that "facts are off"
// appears in the wiring instead of being inferred from a nil pointer.
type NoFacts struct{}

func (NoFacts) Extract(context.Context, FactScope) (FactResult, error) {
	return FactResult{Skipped: "no fact extractor configured"}, nil
}

// MaxFactsPerSession bounds one extraction. A session that appears to yield
// thirty durable preferences has produced thirty guesses.
const MaxFactsPerSession = 8

// LLMFacts extracts facts from a session's own summaries.
//
// Reading the summaries rather than the transcript is not a shortcut: the
// summaries are already redacted, already short enough to fit one prompt, and
// already the distillation the index was built to hold. MEMORY.md §5's last
// rule — nothing in this tier is a secret — is therefore true by construction
// rather than by a second detection pass.
type LLMFacts struct {
	DB    *store.DB
	Model llm.Provider
	Log   *slog.Logger
	Now   func() time.Time
}

var _ FactExtractor = (*LLMFacts)(nil)

const factPrompt = `You extract durable facts about a developer from summaries of their coding sessions.
A durable fact is a lasting preference, tool, service or language choice: "prefers Supabase over Firebase", "deploys on Vercel", "writes Go for daemons".
Not a fact: anything about one task, one bug, one file, or one day's work.
Return a JSON array and nothing else. Each element: {"predicate":"prefers|uses|deploys_on|writes","object":"...","text":"...","confidence":0.0-1.0}.
Return [] if the material supports none. Returning nothing is the correct answer far more often than not.
Never state a fact the material does not support.`

func (e *LLMFacts) Extract(ctx context.Context, scope FactScope) (FactResult, error) {
	var res FactResult
	if e.DB == nil {
		return res, errors.New("summarize: fact extractor has no store")
	}
	log := e.Log
	if log == nil {
		log = logx.Discard()
	}
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	if e.Model == nil {
		res.Skipped = "no model configured"
		return res, nil
	}

	rows, err := e.DB.SQL().QueryContext(ctx, `
		SELECT text, path, byte_offset, created_at FROM summary
		WHERE runtime = ? AND session_id = ?
		ORDER BY kind = 'session' DESC, byte_offset
		LIMIT 40`, scope.Runtime, scope.SessionID)
	if err != nil {
		return res, fmt.Errorf("summarize: read summaries: %w", err)
	}
	defer rows.Close()

	var b strings.Builder
	var ev []Evidence
	for rows.Next() {
		var text, path string
		var offset, created int64
		if err := rows.Scan(&text, &path, &offset, &created); err != nil {
			return res, err
		}
		b.WriteString("- ")
		b.WriteString(text)
		b.WriteString("\n")
		ev = append(ev, Evidence{
			Runtime: scope.Runtime, SessionID: scope.SessionID,
			Path: path, ByteOffset: offset, Quote: clip(text, 200),
			At: time.UnixMilli(created).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	if b.Len() == 0 {
		res.Skipped = "no summaries for this session yet"
		return res, nil
	}

	resp, err := e.Model.Complete(ctx, llm.Request{
		System:    factPrompt,
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: clampMiddle(b.String(), MaxInputChars)}},
		MaxTokens: 500,
	})
	if err != nil {
		log.Warn("fact extraction failed", "runtime", scope.Runtime, "session", scope.SessionID, "err", err)
		res.Skipped = "model call failed: " + err.Error()
		return res, nil
	}
	res.ModelCalls = 1

	facts, perr := parseFacts(resp.Text)
	if perr != nil {
		log.Info("fact extraction returned unparseable output", "err", perr)
		res.Skipped = "unparseable model output"
		return res, nil
	}
	if len(facts) > MaxFactsPerSession {
		facts = facts[:MaxFactsPerSession]
	}
	stamp := now()
	for i := range facts {
		facts[i].Evidence = ev
		for j := range facts[i].Evidence {
			if facts[i].Evidence[j].At.IsZero() {
				facts[i].Evidence[j].At = stamp
			}
		}
	}
	res.Facts = facts
	return res, nil
}

// parseFacts reads the model's JSON, tolerating the fenced-code wrapper models
// add regardless of instruction. It drops any fact with no object or no text
// rather than storing a shape with nothing in it.
func parseFacts(out string) ([]Fact, error) {
	s := strings.TrimSpace(out)
	if i := strings.Index(s, "["); i >= 0 {
		if j := strings.LastIndex(s, "]"); j > i {
			s = s[i : j+1]
		}
	}
	var raw []struct {
		Predicate  string  `json:"predicate"`
		Object     string  `json:"object"`
		Text       string  `json:"text"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, err
	}
	var facts []Fact
	for _, r := range raw {
		object := strings.TrimSpace(r.Object)
		text := strings.TrimSpace(Clean(r.Text))
		if object == "" || text == "" {
			continue
		}
		conf := r.Confidence
		if conf <= 0 || conf > 1 {
			conf = 0.5
		}
		facts = append(facts, Fact{
			Predicate:  strings.TrimSpace(r.Predicate),
			Object:     object,
			Text:       text,
			Confidence: conf,
		})
	}
	return facts, nil
}
