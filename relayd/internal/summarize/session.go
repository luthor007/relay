package summarize

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/index"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/logx"
	"github.com/luthor007/relay/relayd/internal/search"
	"github.com/luthor007/relay/relayd/internal/store"
)

// Errors from [New].
var ErrNoStore = errors.New("summarize: no store")

// Limits.
const (
	// MaxInputChars is how much source text one summary call sees. The middle
	// is dropped rather than the tail: a session states its purpose at the top
	// and its outcome at the bottom, and everything between is command output.
	MaxInputChars = 12000
	// MaxTitleChars matches what the runtimes' own titles look like.
	MaxTitleChars = 70
	// MaxSummaryChars bounds what gets indexed per summary. Long enough for
	// three sentences, short enough that the embedding is about one thing.
	MaxSummaryChars = 600
	// SummarizeTimeout bounds one model call. Backfill is an hour or two of
	// work and has to survive a provider hanging on one session.
	SummarizeTimeout = 45 * time.Second
)

// Options configures a Summarizer.
type Options struct {
	DB *store.DB
	// Model is the small model. Nil is a supported configuration and produces
	// deterministic metadata summaries — which is MEMORY.md §11's step 2b, "a
	// plain indexed list of every session with its title, repo and date", and
	// most of the value.
	Model llm.Provider
	// Embedder may be nil, which writes the lexical half of the index only.
	// Also step 2b: "before anything vectorised".
	Embedder search.Embedder
	// Redactor is required. Detection happens before indexing, never after, and
	// making it a required dependency is how that ordering stops being a
	// convention. Use [Detector] unless you have a reason not to.
	Redactor Redactor
	Log      *slog.Logger
	Now      func() time.Time
	// MaxInput overrides MaxInputChars.
	MaxInput int
	Timeout  time.Duration
}

// Summarizer writes the index tier: one summary per session, one per
// turn-cluster, each embedded and each pointing back at the transcript.
type Summarizer struct {
	db       *store.DB
	model    llm.Provider
	emb      search.Embedder
	redact   Redactor
	log      *slog.Logger
	now      func() time.Time
	maxInput int
	timeout  time.Duration
}

// New builds a Summarizer. It refuses to build one without a secret detector:
// an embedded key cannot be unembedded, so the detector is not an option a
// caller gets to leave nil.
func New(o Options) (*Summarizer, error) {
	if o.DB == nil {
		return nil, ErrNoStore
	}
	if o.Redactor == nil {
		return nil, ErrNoRedactor
	}
	if o.Embedder != nil && o.Embedder.Dims() != store.EmbeddingDims {
		return nil, fmt.Errorf("%w: embedder %s is %d, summary_vec is %d",
			search.ErrDims, o.Embedder.Model(), o.Embedder.Dims(), store.EmbeddingDims)
	}
	s := &Summarizer{
		db: o.DB, model: o.Model, emb: o.Embedder, redact: o.Redactor,
		log: o.Log, now: o.Now, maxInput: o.MaxInput, timeout: o.Timeout,
	}
	if s.log == nil {
		s.log = logx.Discard()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.maxInput <= 0 {
		s.maxInput = MaxInputChars
	}
	if s.timeout == 0 {
		s.timeout = SummarizeTimeout
	}
	return s, nil
}

// Model reports the summarising model's id, or "" when there is none.
func (s *Summarizer) Model() string {
	if s.model == nil {
		return ""
	}
	return s.model.Model()
}

// SessionInput is one session as a backfill reader found it.
type SessionInput struct {
	Runtime   string
	SessionID string
	// Path and ByteOffset point at the transcript on disk. The index stores a
	// pointer, never a copy.
	Path       string
	ByteOffset int64
	ByteLength int64

	// Title is the runtime's own title, when it has one: Claude Code's aiTitle,
	// Hermes's title. Empty for the runtimes that do not title their sessions.
	Title string

	Workspace   string
	GitBranch   string
	Model       string
	StartedAt   time.Time
	EndedAt     time.Time
	Messages    int64
	ToolCalls   int64
	CostUSD     *float64
	TokensTotal *int64
	SourceMTime time.Time
	SourceSize  int64

	// Excerpt is the material the reader chose to summarise from — user turns
	// and assistant conclusions, not command output.
	Excerpt  string
	Clusters []ClusterInput
}

// SessionResult is what one session's indexing produced.
type SessionResult struct {
	Title string
	// TitleStolen is true when the runtime had already titled the session and
	// we took it instead of paying a model to re-derive it.
	TitleStolen bool
	Summary     string
	SummaryID   int64
	ClusterIDs  []int64
	// Findings are the credentials detected and redacted before any of this
	// reached a model or the index.
	Findings []Finding
	Embedded bool
	// ModelCalls is how many completions this session cost. It is returned
	// because MEMORY.md §12.1 makes the total the load-bearing unknown for the
	// installer's longest step, and a count is the only way to turn the
	// estimate into arithmetic.
	ModelCalls int
}

// Title takes the session's title, preferring the one the runtime already
// wrote.
//
// MEMORY.md §4: Claude Code and Hermes both generate session titles, so the
// summariser's first job is already done for a large share of the corpus.
// Taking theirs is not a shortcut — re-deriving a title costs a model call per
// session across 3.6 GB of history, and the runtime's own title was written
// with the whole session in front of it.
func Title(in SessionInput) (string, bool) {
	if t := strings.TrimSpace(Clean(in.Title)); t != "" {
		fitted, _ := Fit(t, MaxTitleChars)
		return strings.TrimSuffix(fitted, "."), true
	}
	return "", false
}

// DerivedTitle builds a title from metadata alone, for a session with no title
// and no model available.
func DerivedTitle(in SessionInput) string {
	var parts []string
	if ws := baseName(in.Workspace); ws != "" {
		parts = append(parts, ws)
	}
	if in.GitBranch != "" && in.GitBranch != "main" && in.GitBranch != "master" {
		parts = append(parts, in.GitBranch)
	}
	head := strings.Join(parts, " · ")
	if head == "" {
		head = orDefault(in.Runtime, "session")
	}
	if !in.StartedAt.IsZero() {
		head += " — " + in.StartedAt.Format("2 Jan 2006")
	}
	fitted, _ := Fit(head, MaxTitleChars)
	return fitted
}

func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// SummarizeSession summarises a session and its turn-clusters, and writes them
// to the index.
//
// The order of operations is the part that matters, and it is fixed:
//
//  1. redact every piece of source text,
//  2. only then send anything to a model,
//  3. redact the model's output too,
//  4. only then embed and write.
//
// Steps 1 and 2 are one rule seen twice: a model call is network egress, so
// sending an unredacted transcript to a provider has already leaked the key
// whatever the index later holds.
func (s *Summarizer) SummarizeSession(ctx context.Context, in SessionInput) (SessionResult, error) {
	var res SessionResult
	if in.Runtime == "" || in.SessionID == "" {
		return res, errors.New("summarize: session needs a runtime and an id")
	}

	// 1. Redact, before anything else sees the text.
	var findings []Finding
	redact := func(text string) string {
		clean, f := s.redact.Redact(text)
		findings = append(findings, f...)
		return clean
	}
	in.Title = redact(in.Title)
	in.Excerpt = redact(in.Excerpt)
	clusters := make([]ClusterInput, len(in.Clusters))
	copy(clusters, in.Clusters)
	for i := range clusters {
		clusters[i].Excerpt = redact(clusters[i].Excerpt)
	}

	// 2. Title: the runtime's own, or derived, or the model's.
	title, stolen := Title(in)
	if !stolen {
		if s.model != nil && strings.TrimSpace(in.Excerpt) != "" {
			if t, ok := s.titleFromModel(ctx, in); ok {
				title = t
				res.ModelCalls++
			}
		}
		if title == "" {
			title = DerivedTitle(in)
		}
	}
	res.Title, res.TitleStolen = title, stolen

	// 3. The session summary.
	summary := ""
	if s.model != nil && strings.TrimSpace(in.Excerpt) != "" {
		if t, ok := s.complete(ctx, sessionSummaryPrompt, sessionBrief(in, title, s.maxInput)); ok {
			summary = t
			res.ModelCalls++
		}
	}
	if summary == "" {
		summary = metadataSummary(in, title)
	}
	summary = redact(summary)
	res.Summary = summary

	// 4. Cluster summaries.
	clusterTexts := make([]string, len(clusters))
	for i, c := range clusters {
		text := ""
		if s.model != nil && strings.TrimSpace(c.Excerpt) != "" {
			if t, ok := s.complete(ctx, clusterSummaryPrompt, clusterBrief(in, c, s.maxInput)); ok {
				text = t
				res.ModelCalls++
			}
		}
		if text == "" {
			text = metadataClusterSummary(in, c)
		}
		clusterTexts[i] = redact(text)
	}

	// 5. Embed everything in one call, then write.
	texts := append([]string{summary}, clusterTexts...)
	vectors, embedded := s.embed(ctx, texts)
	res.Embedded = embedded

	err := s.write(ctx, in, title, summary, clusters, clusterTexts, vectors, findings, &res)
	if err != nil {
		return res, err
	}
	res.Findings = findings
	return res, nil
}

func (s *Summarizer) write(ctx context.Context, in SessionInput, title, summary string,
	clusters []ClusterInput, clusterTexts []string, vectors [][]float32,
	findings []Finding, res *SessionResult) error {

	now := s.now()

	// Re-indexing the same transcript must replace its summaries, not add a
	// second set. Backfill is resumable and keyed on mtime, so this path runs
	// again whenever a session grows.
	if err := s.purge(ctx, in.Runtime, in.SessionID); err != nil {
		return err
	}

	if err := s.mergeSessionRow(ctx, in, title, now); err != nil {
		return err
	}

	var vec func(i int) []float32
	if len(vectors) == len(clusterTexts)+1 {
		vec = func(i int) []float32 { return vectors[i] }
	} else {
		vec = func(int) []float32 { return nil }
	}

	id, err := s.db.PutSummary(ctx, store.Summary{
		Kind:       store.SummarySession,
		Runtime:    in.Runtime,
		SessionID:  in.SessionID,
		Path:       in.Path,
		ByteOffset: in.ByteOffset,
		ByteLength: in.ByteLength,
		Text:       summary,
		Model:      s.Model(),
		CreatedAt:  now,
	}, vec(0))
	if err != nil {
		return fmt.Errorf("summarize: session summary: %w", err)
	}
	res.SummaryID = id

	for i, c := range clusters {
		cid, err := s.db.PutSummary(ctx, store.Summary{
			Kind:       store.SummaryCluster,
			Runtime:    in.Runtime,
			SessionID:  in.SessionID,
			Path:       in.Path,
			ByteOffset: c.ByteOffset,
			ByteLength: c.ByteLength,
			Text:       clusterTexts[i],
			Model:      s.Model(),
			CreatedAt:  now,
		}, vec(i+1))
		if err != nil {
			return fmt.Errorf("summarize: cluster summary: %w", err)
		}
		res.ClusterIDs = append(res.ClusterIDs, cid)
	}

	_ = findings
	return nil
}

// mergeSessionRow fills the index's session row without blanking anything
// already there.
//
// internal/index is the authoritative writer of this row during backfill —
// MEMORY.md §11 splits step 2b (readers and the session index) from step 2c
// (summaries and hybrid search), and the reader knows things this package never
// sees, including which of the four title provenances it had. So this merges:
// it supplies what the caller gave it, keeps what is already there, and the two
// steps can run in either order without one erasing the other.
func (s *Summarizer) mergeSessionRow(ctx context.Context, in SessionInput, title string, now time.Time) error {
	row, err := s.db.GetSessionIndex(ctx, in.Runtime, in.SessionID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("summarize: session index: %w", err)
		}
		row = store.SessionIndex{
			ID:        in.Runtime + "/" + in.SessionID,
			Runtime:   in.Runtime,
			SessionID: in.SessionID,
		}
	}
	setString(&row.Path, in.Path)
	setString(&row.Title, title)
	setString(&row.Workspace, in.Workspace)
	setString(&row.GitBranch, in.GitBranch)
	setString(&row.Model, in.Model)
	if row.ByteOffset == 0 {
		row.ByteOffset = in.ByteOffset
	}
	if row.StartedAt.IsZero() {
		row.StartedAt = in.StartedAt
	}
	if !in.EndedAt.IsZero() {
		row.EndedAt = in.EndedAt
	}
	if in.Messages > 0 {
		row.Messages = in.Messages
	}
	if in.ToolCalls > 0 {
		row.ToolCalls = in.ToolCalls
	}
	if in.CostUSD != nil {
		row.CostUSD = in.CostUSD
	}
	if in.TokensTotal != nil {
		row.TokensTotal = in.TokensTotal
	}
	if !in.SourceMTime.IsZero() {
		row.SourceMTime = in.SourceMTime
	}
	if in.SourceSize > 0 {
		row.SourceSize = in.SourceSize
	}
	row.IndexedAt = now
	if err := s.db.PutSessionIndex(ctx, row); err != nil {
		return fmt.Errorf("summarize: session index: %w", err)
	}
	return nil
}

func setString(dst *string, v string) {
	if *dst == "" && v != "" {
		*dst = v
	}
}

// writeTurnMarkers records what was redacted out of a live turn.
//
// Only the live path writes markers. During backfill internal/index has
// already read the same text off disk and written a marker for every finding,
// and two writers with two id schemes would put the same credential in the
// table twice.
func (s *Summarizer) writeTurnMarkers(ctx context.Context, d Digest, p Pointer, findings []Finding) error {
	now := s.now()
	for _, f := range findings {
		if err := s.db.PutSecretMarker(ctx, store.SecretMarker{
			ID:         MarkerID(d.Runtime, d.Session, d.Turn, f),
			Runtime:    d.Runtime,
			SessionID:  d.Session,
			Path:       p.Path,
			ByteOffset: p.ByteOffset,
			Detector:   f.Tier.String() + ":" + f.RuleID,
			Service:    f.Service,
			At:         now,
		}); err != nil {
			return fmt.Errorf("summarize: secret marker: %w", err)
		}
	}
	return nil
}

// purge removes a session's existing summaries, vectors included.
//
// The vector table has no delete trigger — summary_fts does, summary_vec
// cannot — so dropping a summary row without dropping its vec0 row leaves a
// rowid in the dense index pointing at nothing. That surfaces as a search hit
// with no text, which is why [search.Searcher] logs one rather than showing it.
func (s *Summarizer) purge(ctx context.Context, runtime, sessionID string) error {
	rows, err := s.db.SQL().QueryContext(ctx,
		`SELECT id FROM summary WHERE runtime = ? AND session_id = ?`, runtime, sessionID)
	if err != nil {
		return fmt.Errorf("summarize: purge: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `DELETE FROM summary_vec WHERE rowid = ?`, id); err != nil {
				return fmt.Errorf("summarize: purge vector %d: %w", id, err)
			}
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM summary WHERE runtime = ? AND session_id = ?`, runtime, sessionID)
		return err
	})
}

// embed vectorises a batch in one call, and reports whether it happened.
//
// One call for the whole session is the shape that matters: providers bill and
// rate-limit per request, and backfill runs this across thousands of sessions.
func (s *Summarizer) embed(ctx context.Context, texts []string) ([][]float32, bool) {
	if s.emb == nil || len(texts) == 0 {
		return nil, false
	}
	if err := search.SetEmbeddingModel(ctx, s.db, s.emb.Model()); err != nil {
		// The index already holds vectors from another model. Writing more
		// would mix two spaces silently; write the lexical half and say so.
		s.log.Warn("not embedding: the index belongs to another model", "err", err)
		return nil, false
	}
	vecs, err := s.emb.Embed(ctx, texts)
	if err != nil {
		s.log.Warn("embedding failed, indexing the lexical half only", "err", err)
		return nil, false
	}
	if len(vecs) != len(texts) {
		s.log.Warn("embedder returned the wrong count", "want", len(texts), "got", len(vecs))
		return nil, false
	}
	for i, v := range vecs {
		if len(v) != store.EmbeddingDims {
			s.log.Warn("embedder returned the wrong width", "index", i, "width", len(v))
			return nil, false
		}
	}
	return vecs, true
}

// ------------------------------------------------------------------ prompts --

const sessionSummaryPrompt = `You write short summaries of coding sessions for a search index.
At most three sentences. Say what the session was for, what it changed, and how it ended.
Name only repositories, files, tools and errors that appear in the material below.
Never invent a detail. If the material does not say what happened, say that it does not.
No preamble, no markdown, no bullet points, no code.`

const clusterSummaryPrompt = `You write one or two sentences describing a stretch of a coding session, for a search index.
Name only files, tools and errors that appear in the material below. Never invent a detail.
No preamble, no markdown, no code.`

const titlePrompt = `You write titles for coding sessions. At most 60 characters.
A noun phrase, not a sentence. No quotes, no trailing full stop, no preamble.
Use only words justified by the material below.`

func (s *Summarizer) titleFromModel(ctx context.Context, in SessionInput) (string, bool) {
	out, ok := s.complete(ctx, titlePrompt, sessionBrief(in, "", s.maxInput))
	if !ok {
		return "", false
	}
	out = strings.Trim(strings.TrimSpace(Clean(out)), `"'.`)
	if out == "" {
		return "", false
	}
	fitted, _ := Fit(out, MaxTitleChars)
	return strings.TrimSuffix(fitted, "."), fitted != ""
}

func (s *Summarizer) complete(ctx context.Context, system, user string) (string, bool) {
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	resp, err := s.model.Complete(cctx, llm.Request{
		System:    system,
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: user}},
		MaxTokens: MaxSummaryChars/3 + 32,
	})
	if err != nil {
		s.log.Warn("summary model call failed, falling back to metadata", "err", err)
		return "", false
	}
	out := strings.TrimSpace(Clean(resp.Text))
	if out == "" {
		return "", false
	}
	fitted, _ := Fit(out, MaxSummaryChars)
	return fitted, fitted != ""
}

// sessionBrief is what the model sees for a whole session.
func sessionBrief(in SessionInput, title string, maxInput int) string {
	var b strings.Builder
	w := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	w("runtime", in.Runtime)
	w("title", title)
	w("workspace", in.Workspace)
	w("branch", in.GitBranch)
	w("model", in.Model)
	if !in.StartedAt.IsZero() {
		w("started", in.StartedAt.Format(time.RFC3339))
	}
	if in.Messages > 0 {
		w("messages", itoa(int(in.Messages)))
	}
	if in.ToolCalls > 0 {
		w("tool calls", itoa(int(in.ToolCalls)))
	}
	b.WriteString("\n")
	b.WriteString(clampMiddle(in.Excerpt, maxInput))
	return b.String()
}

func clusterBrief(in SessionInput, c ClusterInput, maxInput int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "runtime: %s\n", in.Runtime)
	if in.Workspace != "" {
		fmt.Fprintf(&b, "workspace: %s\n", in.Workspace)
	}
	if len(c.Tools) > 0 {
		fmt.Fprintf(&b, "tools: %s\n", strings.Join(c.Tools, ", "))
	}
	if !c.StartedAt.IsZero() {
		fmt.Fprintf(&b, "started: %s\n", c.StartedAt.Format(time.RFC3339))
	}
	b.WriteString("\n")
	b.WriteString(clampMiddle(c.Excerpt, maxInput))
	return b.String()
}

// metadataSummary is the no-model summary. MEMORY.md §11 calls this step 2b and
// says it is most of the value: a plain indexed list of every session with its
// title, repo and date, which ships in days and makes the summariser's output
// verifiable against something.
func metadataSummary(in SessionInput, title string) string {
	var parts []string
	if title != "" {
		parts = append(parts, title)
	}
	loc := ""
	switch {
	case in.Workspace != "" && in.GitBranch != "":
		loc = in.Workspace + " on " + in.GitBranch
	case in.Workspace != "":
		loc = in.Workspace
	case in.GitBranch != "":
		loc = "branch " + in.GitBranch
	}
	head := orDefault(in.Runtime, "session")
	if loc != "" {
		head += " in " + loc
	}
	parts = append(parts, head)

	var counts []string
	if in.Messages > 0 {
		counts = append(counts, itoa(int(in.Messages))+" messages")
	}
	if in.ToolCalls > 0 {
		counts = append(counts, itoa(int(in.ToolCalls))+" tool calls")
	}
	if len(counts) > 0 {
		parts = append(parts, strings.Join(counts, ", "))
	}
	if !in.StartedAt.IsZero() {
		parts = append(parts, in.StartedAt.Format("2 Jan 2006"))
	}
	if ex := firstSentence(in.Excerpt); ex != "" {
		parts = append(parts, clip(ex, 200))
	}
	out, _ := Fit(strings.Join(parts, ". "), MaxSummaryChars)
	return out
}

func metadataClusterSummary(in SessionInput, c ClusterInput) string {
	var parts []string
	if len(c.Tools) > 0 {
		parts = append(parts, "tools: "+strings.Join(c.Tools, ", "))
	}
	if c.Turns > 0 {
		parts = append(parts, itoa(c.Turns)+" turns")
	}
	if !c.StartedAt.IsZero() {
		parts = append(parts, c.StartedAt.Format("2 Jan 2006 15:04"))
	}
	if ex := firstSentence(c.Excerpt); ex != "" {
		parts = append(parts, clip(ex, 240))
	}
	if in.Workspace != "" {
		parts = append(parts, baseName(in.Workspace))
	}
	out, _ := Fit(strings.Join(parts, ". "), MaxSummaryChars)
	return out
}

// clampMiddle keeps the head and the tail and drops the middle.
//
// A session says what it is for in its first turns and how it ended in its
// last; the middle is where the diffs and the command output live. Truncating
// the tail would throw away the outcome, which is the single most useful
// sentence in the whole transcript.
func clampMiddle(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	head := n * 2 / 3
	tail := n - head
	return string(r[:head]) + "\n…\n" + string(r[len(r)-tail:])
}

// FromIndexed builds a [SessionInput] from what step 2b produced.
//
// It exists to make the handoff between the two steps explicit rather than
// something each caller re-derives. `internal/index` reads a session off disk,
// writes the row and the secret markers, and hands back
// [index.Result.Redacted] — the text with every credential already replaced by
// its marker. That, never `index.Session.Text`, is what the summariser gets.
//
// The redactor still runs over it here. Not because this path is untrusted, but
// because [New] cannot tell which path a caller is on, and a defence that is
// skippable by passing a different argument is not a defence.
func FromIndexed(s index.Session, redacted string, clusters []ClusterInput) SessionInput {
	return SessionInput{
		Runtime:     string(s.Runtime),
		SessionID:   s.SessionID,
		Path:        s.Path,
		ByteOffset:  s.ByteOffset,
		ByteLength:  s.ByteLength,
		Title:       s.Title,
		Workspace:   s.Workspace,
		GitBranch:   s.GitBranch,
		Model:       s.Model,
		StartedAt:   s.StartedAt,
		EndedAt:     s.EndedAt,
		Messages:    s.Messages,
		ToolCalls:   s.ToolCalls,
		CostUSD:     s.CostUSD,
		TokensTotal: s.TokensTotal,
		SourceMTime: s.SourceMTime,
		SourceSize:  s.SourceSize,
		Excerpt:     redacted,
		Clusters:    clusters,
	}
}
