package summarize

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/logx"
	"github.com/luthor007/relay/relayd/internal/store"
)

// Pointer is where a live session's transcript lives on disk, so the summaries
// written from its events carry the same pointer every other index row does.
//
// It is registered rather than derived because the normalized event model has
// no path in it — deliberately, since three of the five runtimes write their
// transcripts somewhere Relay does not control. A session with no pointer still
// gets summarised; the row simply says it cannot be reopened yet, and backfill
// fills it in later.
type Pointer struct {
	Path       string
	ByteOffset int64
}

// LiveOptions configures a Live.
type LiveOptions struct {
	Summarizer *Summarizer
	// Narrator produces the spoken line for each completed turn. Nil means no
	// speech is produced, which is a valid headless configuration.
	Narrator *Narrator
	// Facts re-runs fact extraction after each turn, scoped to that session.
	// Nil becomes NoFacts.
	Facts FactExtractor
	Log   *slog.Logger
	Now   func() time.Time
}

// Live is MEMORY.md §4's live ingestion path.
//
// Every TurnCompleted writes a turn summary, updates the session row, and
// re-runs fact extraction against that session only. It also produces the
// ~160-character spoken line for the completion ping, from the same digest,
// because building the summary and the speech from two different sources is how
// they end up disagreeing.
type Live struct {
	sum *Summarizer
	nar *Narrator
	fx  FactExtractor
	log *slog.Logger
	now func() time.Time

	mu       sync.Mutex
	digests  map[string]*Digester
	pointers map[string]Pointer
	done     map[string]bool
}

// NewLive builds the live handler.
func NewLive(o LiveOptions) (*Live, error) {
	if o.Summarizer == nil {
		return nil, errors.New("summarize: live needs a summarizer")
	}
	l := &Live{
		sum: o.Summarizer, nar: o.Narrator, fx: o.Facts, log: o.Log, now: o.Now,
		digests:  map[string]*Digester{},
		pointers: map[string]Pointer{},
		done:     map[string]bool{},
	}
	if l.fx == nil {
		l.fx = NoFacts{}
	}
	if l.log == nil {
		l.log = logx.Discard()
	}
	if l.now == nil {
		l.now = time.Now
	}
	return l, nil
}

// Bind records where a live session's transcript lives.
func (l *Live) Bind(runtime, session string, p Pointer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pointers[runtime+"/"+session] = p
}

// Outcome is what one completed turn produced.
type Outcome struct {
	Digest Digest
	// Speech is the completion ping's line, ~160 characters, outcome first.
	// Zero when no narrator is configured.
	Speech Speech
	// SummaryID is the turn summary's row, 0 when nothing was written.
	SummaryID int64
	// Text is the summary that was indexed — the REDACTED one, after
	// Summarizer.redact has run over it, which is why it is safe to hand to
	// anything downstream. It is not Digest.Text: that is raw assistant prose.
	//
	// It exists so a consumer that wants what the turn was about does not have
	// to re-read the row it was just told about. ORCHESTRATOR.md §4b's proposer
	// is the first such consumer.
	Text     string
	Findings []Finding
	Facts    FactResult
	// Skipped explains a turn that produced no index write.
	Skipped string
}

// Written reports whether this turn reached the index.
func (o Outcome) Written() bool { return o.SummaryID != 0 }

// Handle folds one event in and, on TurnCompleted, does the work.
//
// It returns a nil Outcome for every event that is not a turn boundary, so a
// caller can hand it the whole stream.
func (l *Live) Handle(ctx context.Context, ev event.Event) (*Outcome, error) {
	if ev == nil {
		return nil, nil
	}
	m := ev.Envelope()
	key := m.Runtime + "/" + m.Session + "/" + m.Turn

	l.mu.Lock()
	g, ok := l.digests[key]
	if !ok {
		g = NewDigester()
		l.digests[key] = g
	}
	g.Add(ev)
	l.mu.Unlock()

	if ev.Kind() != event.KindTurnCompleted {
		return nil, nil
	}

	l.mu.Lock()
	d := g.Digest()
	delete(l.digests, key)
	already := l.done[key]
	if !already {
		l.done[key] = true
	}
	ptr := l.pointers[m.Runtime+"/"+m.Session]
	l.mu.Unlock()

	out := &Outcome{Digest: d}

	// A replayed turn is not news and it is not new data either. ACP's
	// session/load replays a whole conversation before it resolves, and
	// PutSummary inserts rather than upserts, so indexing replays would write a
	// second copy of history that backfill already owns from the transcript on
	// disk.
	if d.Replay {
		out.Skipped = "replayed turn: history belongs to backfill"
		return out, nil
	}
	if already {
		out.Skipped = "turn already summarised"
		return out, nil
	}

	res, err := l.sum.SummarizeTurn(ctx, d, ptr)
	if err != nil {
		return out, err
	}
	out.SummaryID = res.SummaryID
	out.Text = res.Text
	out.Findings = res.Findings

	if l.nar != nil {
		out.Speech = l.nar.Completed(ctx, d)
	}

	facts, err := l.fx.Extract(ctx, FactScope{Runtime: d.Runtime, SessionID: d.Session})
	if err != nil {
		// Fact extraction is the least load-bearing thing here and the most
		// likely to fail; it must not lose the summary that already landed.
		l.log.Warn("fact extraction failed after a turn",
			"runtime", d.Runtime, "session", d.Session, "err", err)
	}
	out.Facts = facts
	return out, nil
}

// Forget drops any in-flight digest for a session, for when a session closes
// mid-turn.
func (l *Live) Forget(runtime, session string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	prefix := runtime + "/" + session + "/"
	for k := range l.digests {
		if strings.HasPrefix(k, prefix) {
			delete(l.digests, k)
		}
	}
	for k := range l.done {
		if strings.HasPrefix(k, prefix) {
			delete(l.done, k)
		}
	}
	delete(l.pointers, runtime+"/"+session)
}

// ------------------------------------------------------- the turn summariser --

// TurnResult is one turn summary written to the index.
type TurnResult struct {
	SummaryID int64
	Text      string
	Findings  []Finding
	Embedded  bool
	ModelCall bool
}

const turnSummaryPrompt = `You write one or two sentences describing what an agent just did, for a search index.
You are given structured events, not a transcript. Use only what they say.
Name the tools, targets and errors that appear; never invent one.
State the outcome. No preamble, no markdown, no code, no command output.`

// SummarizeTurn writes a turn's summary to the index and updates the session
// row.
//
// It summarises the *events* rather than the transcript, which is ADAPTERS.md
// §6's first rule applied to the index as well as to speech. The digest holds
// no tool output at all, so a turn that printed forty thousand lines of test
// output contributes the same handful of facts as one that printed none — which
// is exactly the property that makes summaries worth embedding and transcripts
// not.
func (s *Summarizer) SummarizeTurn(ctx context.Context, d Digest, p Pointer) (TurnResult, error) {
	var res TurnResult
	if d.Runtime == "" || d.Session == "" {
		return res, errors.New("summarize: turn needs a runtime and a session")
	}

	brief := Brief(MomentCompleted, d)
	// The brief is built from events, but a tool target or an assistant
	// sentence can still carry a key. Redact before the model, not after.
	brief, briefFindings := s.redact.Redact(brief)

	text := ""
	if s.model != nil {
		if t, ok := s.complete(ctx, turnSummaryPrompt, brief); ok {
			text = t
			res.ModelCall = true
		}
	}
	if text == "" {
		text = turnTemplate(d)
	}
	text, outFindings := s.redact.Redact(text)
	res.Text = text
	res.Findings = append(append([]Finding{}, briefFindings...), outFindings...)

	vectors, embedded := s.embed(ctx, []string{text})
	res.Embedded = embedded
	var vec []float32
	if embedded {
		vec = vectors[0]
	}

	now := s.now()
	id, err := s.db.PutSummary(ctx, store.Summary{
		Kind:       store.SummaryCluster,
		Runtime:    d.Runtime,
		SessionID:  d.Session,
		Path:       p.Path,
		ByteOffset: p.ByteOffset,
		Text:       text,
		Model:      s.Model(),
		CreatedAt:  now,
	}, vec)
	if err != nil {
		return res, fmt.Errorf("summarize: turn summary: %w", err)
	}
	res.SummaryID = id

	if err := s.writeTurnMarkers(ctx, d, p, res.Findings); err != nil {
		return res, err
	}
	if err := s.touchSession(ctx, d, p, text); err != nil {
		return res, err
	}
	return res, nil
}

// touchSession updates the index's row for a live session: counts, times, and
// whatever usage the runtime was able to report.
//
// This is the *index* row, not the registry's. The registry tier holds what is
// running now and is owned by internal/registry; this tier holds one row per
// session ever seen. Conflating them is what MEMORY.md §2 keeps apart, and the
// two have different lifetimes — minutes against forever.
func (s *Summarizer) touchSession(ctx context.Context, d Digest, p Pointer, summary string) error {
	row, err := s.db.GetSessionIndex(ctx, d.Runtime, d.Session)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		row = store.SessionIndex{
			ID:        d.Runtime + "/" + d.Session,
			Runtime:   d.Runtime,
			SessionID: d.Session,
			StartedAt: d.StartedAt,
		}
	}
	if row.Path == "" {
		row.Path = p.Path
		row.ByteOffset = p.ByteOffset
	}
	if row.Title == "" {
		// A live session has no runtime-supplied title yet — Claude Code writes
		// aiTitle into the transcript, not into the event stream — so the first
		// turn's summary stands in until backfill reads the real one.
		title, _ := Fit(firstSentence(summary), MaxTitleChars)
		row.Title = strings.TrimSuffix(title, ".")
	}
	if row.StartedAt.IsZero() {
		row.StartedAt = d.StartedAt
	}
	if !d.EndedAt.IsZero() {
		row.EndedAt = d.EndedAt
	}
	row.Messages++
	row.ToolCalls += int64(len(d.Tools))
	row.IndexedAt = s.now()

	// Nil rather than zero where the runtime cannot report it: ACP carries no
	// usage object at all, so the console shows a gap instead of a free turn.
	if d.Usage != nil {
		if d.Usage.TotalTokens != nil {
			row.TokensTotal = d.Usage.TotalTokens
		}
		if d.Usage.CostUSD != nil {
			// Claude Code's total_cost_usd is session-cumulative, and the other
			// two runtimes report no money at all, so this is the session total
			// rather than a running sum of per-turn costs.
			row.CostUSD = d.Usage.CostUSD
		}
	}
	return s.db.PutSessionIndex(ctx, row)
}

// turnTemplate is the no-model turn summary. Unlike the spoken line it is not
// budgeted by seconds — nobody hears it — so it says more.
func turnTemplate(d Digest) string {
	var parts []string
	if d.Completed {
		if d.OK {
			parts = append(parts, "Turn completed")
		} else {
			parts = append(parts, "Turn failed")
		}
		if d.StopReason != "" {
			parts[len(parts)-1] += " (" + string(d.StopReason) + ")"
		}
	} else {
		parts = append(parts, "Turn still running")
	}
	if names := d.ToolNames(); len(names) > 0 {
		parts = append(parts, "tools: "+strings.Join(names, ", "))
	}
	for _, t := range d.Tools {
		if t.Target != "" {
			parts = append(parts, "on "+clip(Clean(t.Target), 80))
			break
		}
	}
	if d.PlanObserved && len(d.Plan) > 0 {
		done, total := d.PlanProgress()
		parts = append(parts, fmt.Sprintf("plan %d/%d", done, total))
	}
	for _, e := range d.Errors {
		parts = append(parts, "error: "+clip(Clean(e), 120))
	}
	if t := firstSentence(d.Text); t != "" {
		parts = append(parts, clip(t, 200))
	}
	out, _ := Fit(strings.Join(parts, ". "), MaxSummaryChars)
	if out == "" {
		out = "Turn produced no observable events."
	}
	return out
}
