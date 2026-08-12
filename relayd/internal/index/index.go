// Package index is MEMORY.md §11 step 2b: one row per session, for every
// session that has ever existed, across all five runtimes.
//
// §11 is emphatic that this comes before anything vectorised — "a plain indexed
// list of every session with its title, repo and date is most of the value,
// ships in days, and makes the summariser's output verifiable against
// something" — so nothing here embeds, summarises or calls a model. The
// summariser (2c) consumes [Result.Redacted] and adds rows of its own.
//
// Two invariants, and they are the whole package:
//
// **The row is a pointer, never a copy.** runtime, session id, path and byte
// offset locate the transcript where it already lives. MEMORY.md §3 keeps the
// measured 3.6 GB on disk, in place, unmoved: we are building an index, not a
// copy. Nothing here writes transcript text to the database.
//
// **Secrets are detected before indexing, never after.** [Indexer.Index] runs
// [Detector.Redact] over every string it is about to persist and over the
// transcript text it hands onward, writes a store.SecretMarker for each hit,
// and only then writes the session row. An embedded key cannot be unembedded,
// so the ordering is enforced by the code path rather than by a comment.
package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/store"
)

// TitleSource records where a session's title came from, because the answer
// changes what the summariser has to do.
//
// MEMORY.md §4: Claude Code and Hermes both title their own sessions, so for a
// large share of the corpus the summariser's first job is already done. A title
// we derived from the first user message is not the same artefact and must not
// be presented as though the runtime wrote it.
type TitleSource string

const (
	// TitleNone: the store had no title and no message to derive one from.
	TitleNone TitleSource = ""
	// TitleGenerated: the runtime generated it — Claude Code's aiTitle, Hermes's
	// title column. Free, and better than anything we would write.
	TitleGenerated TitleSource = "runtime"
	// TitleStored: a human-or-runtime-authored title field that is not
	// explicitly an AI title — OpenClaw's `title`, OpenCode's export title.
	TitleStored TitleSource = "stored"
	// TitleFirstMessage: derived by us from the opening user message. Codex has
	// no title anywhere in a rollout, so every Codex row is this.
	TitleFirstMessage TitleSource = "first-message"
)

// Generated reports whether the runtime titled this session itself.
func (t TitleSource) Generated() bool { return t == TitleGenerated }

// Session is one session as a reader found it: the row that will be written,
// plus the provenance and the transient text that never will be.
type Session struct {
	Runtime   adapter.Runtime
	SessionID string

	// Path and ByteOffset are the pointer into the original transcript.
	// ByteLength is 0 when the session is the whole file.
	Path       string
	ByteOffset int64
	ByteLength int64

	Title       string
	TitleSource TitleSource
	Workspace   string
	GitBranch   string
	Model       string

	StartedAt time.Time
	EndedAt   time.Time

	Messages  int64
	ToolCalls int64

	// CostUSD and TokensTotal are nil when the store does not report them —
	// never zero. Codex rollouts carry token_count and no currency at all;
	// Claude Code transcripts carry usage and, usually, no cost. A zero here
	// would claim an observation the store never made, which is the same class
	// of lie as an adapter emitting an event it cannot see.
	CostUSD     *float64
	TokensTotal *int64

	// SourceMTime and SourceSize are the resume key. For a one-session-per-file
	// store they are the file's; for a store that holds many sessions in one
	// file or one database they are the session's own last activity, because
	// the file's mtime moves for every session at once. MTimeFrom says which.
	SourceMTime time.Time
	SourceSize  int64
	MTimeFrom   string

	// Text is the extracted message text, for detection and, later, for
	// summarisation. It is transient: nothing in this package persists it, and
	// [Result.Redacted] is the only form of it anything downstream should see.
	Text string

	// TextTruncated is set when the reader stopped extracting at its limit, so
	// a caller can say that the tail was neither scanned nor summarised rather
	// than implying the whole session was.
	TextTruncated bool

	// Notes are everything the reader could not observe or had to derive, in
	// words. They are carried, not dropped, because "we guessed the workspace
	// from the directory slug" is the difference between a fact and a lie.
	Notes []string
}

// ID is the session index primary key: stable across re-runs, so backfill
// resuming after an interruption updates the row it wrote last time.
func (s Session) ID() string { return string(s.Runtime) + "/" + s.SessionID }

// Note appends a note.
func (s *Session) Note(format string, args ...any) {
	s.Notes = append(s.Notes, fmt.Sprintf(format, args...))
}

// Result is what one Index call did.
type Result struct {
	Runtime   adapter.Runtime
	SessionID string

	// Findings are every secret detected, tier 1 and tier 2, after overlap
	// resolution. Tier 2 findings are redacted and MUST NOT become vault
	// proposals: MEMORY.md §12.2 measured a 26% false-positive rate there, and
	// a Twilio auth token and an MD5 digest are the same 32 hex characters.
	Findings []Finding

	// Markers are the rows written to secret_marker. One per finding.
	Markers []store.SecretMarker

	// Redacted is the transcript text with every credential replaced by its
	// marker. This — never Session.Text — is what the summariser gets.
	Redacted string

	// Wrote is false when the session row was not written because the caller
	// asked for a dry run.
	Wrote bool
}

// VaultCandidates is the subset of findings that may be proposed to the vault:
// tier 1 only, and a proposal, never a silent capture (MEMORY.md §6).
func (r Result) VaultCandidates() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Tier.ProposeToVault() {
			out = append(out, f)
		}
	}
	return out
}

// MarkerSentences is the deduplicated set of "a Stripe secret key appeared in
// this session" lines. Search results, summaries and embeddings all see these
// instead of the credential.
func (r Result) MarkerSentences() []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range r.Findings {
		s := f.Sentence()
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Indexer writes session rows and secret markers. It holds no state beyond its
// dependencies and is safe to share.
type Indexer struct {
	db  *store.DB
	det *Detector

	// Now is injectable so a test can assert indexed_at.
	Now func() time.Time

	// DryRun runs detection and returns the result without writing anything.
	// The backfill probe MEMORY.md §12.1 asks for — count sessions and
	// characters with summarisation disabled — is this plus a counter.
	DryRun bool
}

// ErrNoDB is returned when an Indexer is used without a database.
var ErrNoDB = errors.New("index: no database")

// New builds an Indexer. A nil detector gets the measured one.
func New(db *store.DB, det *Detector) *Indexer {
	if det == nil {
		det = MustDetector()
	}
	return &Indexer{db: db, det: det, Now: time.Now}
}

// Detector is the ruleset this indexer redacts with.
func (ix *Indexer) Detector() *Detector { return ix.det }

func (ix *Indexer) now() time.Time {
	if ix.Now == nil {
		return time.Now()
	}
	return ix.Now()
}

// Index redacts, records markers, and writes exactly one session row.
//
// The order is the point. Detection runs over the transcript text and over
// every string that is about to be persisted; the markers are written; the row
// is written last, carrying a redacted title. Reversing any two of those steps
// puts a credential in the index, and §6 says that ordering is not negotiable.
func (ix *Indexer) Index(ctx context.Context, s Session) (Result, error) {
	res := Result{Runtime: s.Runtime, SessionID: s.SessionID}
	if s.Runtime == "" || s.SessionID == "" {
		return res, fmt.Errorf("index: session needs a runtime and an id, got %q/%q", s.Runtime, s.SessionID)
	}

	// 1. Detect. Transcript text first, then every persisted string, so a key
	//    that only ever appeared in a session title is caught too.
	redacted, findings := ix.det.Redact(s.Text)
	res.Redacted = redacted

	title, titleFindings := ix.det.Redact(s.Title)
	branch, branchFindings := ix.det.Redact(s.GitBranch)
	workspace, workspaceFindings := ix.det.Redact(s.Workspace)

	findings = append(findings, titleFindings...)
	findings = append(findings, branchFindings...)
	findings = append(findings, workspaceFindings...)
	res.Findings = findings

	// 2. Markers. The offset stored is the session's, not the match's: Text is
	//    extracted message text rather than a byte range of the file, so a
	//    per-match file offset would be a number we did not measure. The
	//    marker locates the session; re-reading it is what finds the line.
	at := ix.now()
	for i, f := range findings {
		res.Markers = append(res.Markers, store.SecretMarker{
			ID:         markerID(s.Runtime, s.SessionID, f.RuleID, i),
			Runtime:    string(s.Runtime),
			SessionID:  s.SessionID,
			Path:       s.Path,
			ByteOffset: s.ByteOffset,
			Detector:   f.RuleID + " (" + f.Tier.String() + ")",
			Service:    f.Service,
			At:         at,
		})
	}

	if ix.DryRun {
		return res, nil
	}
	if ix.db == nil {
		return res, ErrNoDB
	}

	// Marker ids are deterministic in (runtime, session, rule, ordinal), so
	// re-indexing an unchanged session rewrites the same rows rather than
	// duplicating them. Markers are never deleted on re-index: "a Stripe secret
	// key appeared in this session" stays true even after the key is edited out
	// of the transcript, and a marker that disappeared would quietly retract a
	// warning the user may have acted on.
	for _, m := range res.Markers {
		if err := ix.db.PutSecretMarker(ctx, m); err != nil {
			return res, fmt.Errorf("index: secret marker %s: %w", m.ID, err)
		}
	}

	// 3. The row. Redacted strings only.
	row := store.SessionIndex{
		ID:          s.ID(),
		Runtime:     string(s.Runtime),
		SessionID:   s.SessionID,
		Path:        s.Path,
		ByteOffset:  s.ByteOffset,
		Title:       clip(title, 300),
		Workspace:   workspace,
		GitBranch:   branch,
		Model:       s.Model,
		StartedAt:   s.StartedAt,
		EndedAt:     s.EndedAt,
		Messages:    s.Messages,
		ToolCalls:   s.ToolCalls,
		CostUSD:     s.CostUSD,
		TokensTotal: s.TokensTotal,
		SourceMTime: s.SourceMTime,
		SourceSize:  s.SourceSize,
		IndexedAt:   at,
	}
	if err := ix.db.PutSessionIndex(ctx, row); err != nil {
		return res, fmt.Errorf("index: session row %s: %w", row.ID, err)
	}
	res.Wrote = true
	return res, nil
}

// NeedsIndexing is the resume check, keyed on (runtime, session_id, mtime) per
// MEMORY.md §4. Unseen sessions and sessions whose mtime or size moved come
// back true.
func (ix *Indexer) NeedsIndexing(ctx context.Context, runtime adapter.Runtime, sessionID string, mtime time.Time, size int64) (bool, error) {
	if ix.db == nil {
		return true, nil
	}
	return ix.db.NeedsIndexing(ctx, string(runtime), sessionID, mtime, size)
}

func markerID(rt adapter.Runtime, sessionID, ruleID string, ordinal int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", rt, sessionID, ruleID, ordinal)))
	return hex.EncodeToString(sum[:12])
}

// clip trims a string to n bytes, dropping any partial rune at the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
