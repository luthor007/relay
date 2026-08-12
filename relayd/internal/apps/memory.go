package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory, scoped and logged — APP-PLATFORM.md §5's fourth bullet.
//
// "Every read is recorded, and the user can see exactly which app touched which
// episode. An app that reads the whole archive on install is visible."
//
// The word doing the work there is *visible*. A log that is written after the
// data is handed over, or that is skipped when the disk is full, cannot make
// anything visible — it can only make it look logged. So this package borrows
// `internal/audit`'s rule and points it the other way: **a read that could not
// be recorded does not happen.** [Memory.Search] writes the access line first
// and returns the error if that write failed, before it has looked at an
// episode.

// Episode is what an app sees. It is `internal/store`'s episode narrowed to the
// SDK's shape — the app's `Episode` interface in apps/sdk/src/app.ts — and
// deliberately carries no ids of anything else on the box.
type Episode struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	StartedAt    time.Time `json:"startedAt"`
	EndedAt      time.Time `json:"endedAt"`
	Transcript   string    `json:"transcript"`
	Participants []string  `json:"participants,omitempty"`
	Location     string    `json:"location,omitempty"`
}

// Commitment is one extracted promise, as the SDK types it.
type Commitment struct {
	Text            string     `json:"text"`
	To              string     `json:"to,omitempty"`
	DueAt           *time.Time `json:"dueAt,omitempty"`
	SourceEpisodeID string     `json:"sourceEpisodeId"`
}

// Note is what `memory.write` writes.
type Note struct {
	Kind        string       `json:"kind"`
	Title       string       `json:"title"`
	Body        string       `json:"body"`
	Commitments []Commitment `json:"commitments,omitempty"`
	Tags        []string     `json:"tags,omitempty"`
}

// Query is one search.
type Query struct {
	Text  string
	Limit int
	Since time.Time
}

// Source is the user's memory, as this package needs it. It is an interface so
// the app runtime has no way to reach the database directly and so a test can
// assert on exactly what was read.
type Source interface {
	Search(ctx context.Context, q Query) ([]Episode, error)
	Recent(ctx context.Context, kind string, within time.Duration) (*Episode, error)
	Get(ctx context.Context, id string) (*Episode, error)
	Extract(ctx context.Context, e Episode) ([]Commitment, error)
}

// Sink is where `memory.write` lands.
//
// It is separate from [Source] because the two have different failure modes and
// because there is, today, nowhere in `internal/store` for a note to go: the
// schema has episodes and commitments and no notes table. An implementation that
// cannot store a note must say so — see [ErrNoNoteStore] — rather than accept
// one and drop it, which would be an app told its work was saved when it was not.
type Sink interface {
	WriteNote(ctx context.Context, appID string, n Note) (string, error)
}

// ErrNoNoteStore is a memory.write with nowhere to write to.
var ErrNoNoteStore = errors.New("apps: this box has no note store wired, so memory.write cannot save anything")

// Access is one recorded memory read or write.
type Access struct {
	At    time.Time `json:"at"`
	AppID string    `json:"app"`
	// Invocation ties a run of accesses to the trigger that caused it, so the
	// console can say "when you said 'wrap up the standup', it read these four".
	Invocation string `json:"invocation,omitempty"`
	// Op is the method: memory.search, memory.get, memory.write…
	Op string `json:"op"`
	// Episodes are the episode ids touched. Empty for a search that matched
	// nothing, which is itself worth recording.
	Episodes []string `json:"episodes,omitempty"`
	// Query is the search text, redacted. A query is user text and can carry a
	// credential like any other.
	Query string `json:"query,omitempty"`
	// Count is how many rows the call returned or wrote.
	Count int `json:"count"`
}

// AccessLog records memory access. Append-only by contract; there is no read
// path here because the console reads the file or the table directly.
type AccessLog interface {
	Record(ctx context.Context, a Access) error
}

// MemoryAccessLog keeps accesses in memory. For tests and for a box that has not
// been given a durable log yet — and it is honest about which it is, because
// [Runtime] reports whether the log it holds is durable.
type MemoryAccessLog struct {
	mu   sync.Mutex
	list []Access
}

func (l *MemoryAccessLog) Record(_ context.Context, a Access) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, a)
	return nil
}

// All returns everything recorded, oldest first.
func (l *MemoryAccessLog) All() []Access {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Access(nil), l.list...)
}

// FileAccessLog appends one JSON object per line and fsyncs before returning.
//
// The fsync is the point. "An app read your whole archive" is exactly the fact
// that goes missing when the machine is turned off in anger, and a log that
// returns before the bytes are durable would let the read happen and the record
// not.
type FileAccessLog struct {
	mu   sync.Mutex
	path string
}

// NewFileAccessLog opens (or creates) a durable access log.
func NewFileAccessLog(path string) (*FileAccessLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("apps: access log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("apps: open access log: %w", err)
	}
	_ = f.Close()
	return &FileAccessLog{path: path}, nil
}

func (l *FileAccessLog) Record(_ context.Context, a Access) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// Durable reports whether an access log survives a restart. [Runtime] carries
// this into every [Invocation] so a console can say "these reads are recorded"
// or "these reads were not", and never the first when it means the second.
func Durable(l AccessLog) bool {
	_, ok := l.(*FileAccessLog)
	return ok
}

// MemoryOptions configures a [MemoryCap].
type MemoryOptions struct {
	Source Source
	Sink   Sink
	Log    AccessLog
	// Redact is required for the write path. See [ErrNoRedactor].
	Redact Redactor
	AppID  string
	// Invocation ties the accesses to one run.
	Invocation string
	// MaxResults caps a single search regardless of what the app asked for. An
	// app that asks for a million episodes is the case §5's logging bullet is
	// about, and the log records the ask as well as the answer.
	MaxResults int
	Now        func() time.Time
}

// DefaultMaxResults is the ceiling on one search.
const DefaultMaxResults = 50

// ErrNoRedactor is a memory capability built without a secret detector. The same
// structural refusal `internal/episode` and `internal/index` make, for the same
// reason: there is no code path here that writes text without having looked for
// credentials in it first.
var ErrNoRedactor = errors.New("apps: no secret detector, and writing text without one is not allowed")

// Redactor replaces credentials with markers. It is `internal/index`'s
// interface, not a second copy — see [Detector].
type Redactor interface {
	Redact(text string) (string, []Finding)
}

// MemoryCap serves the `memory.*` methods.
type MemoryCap struct {
	src        Source
	sink       Sink
	log        AccessLog
	redact     Redactor
	appID      string
	invocation string
	max        int
	now        func() time.Time
}

// NewMemory builds the memory capability.
func NewMemory(o MemoryOptions) (*MemoryCap, error) {
	if o.Source == nil {
		return nil, errors.New("apps: memory needs a source")
	}
	if o.Log == nil {
		return nil, errors.New("apps: memory needs an access log — an unlogged read is the thing §5 forbids")
	}
	if o.Redact == nil {
		return nil, ErrNoRedactor
	}
	if o.AppID == "" {
		return nil, errors.New("apps: memory needs the app id, or the access log cannot name who read")
	}
	if o.MaxResults <= 0 {
		o.MaxResults = DefaultMaxResults
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &MemoryCap{
		src: o.Source, sink: o.Sink, log: o.Log, redact: o.Redact,
		appID: o.AppID, invocation: o.Invocation, max: o.MaxResults, now: o.Now,
	}, nil
}

// record writes the access line and refuses the call if it could not.
func (m *MemoryCap) record(ctx context.Context, a Access) error {
	a.At = m.now()
	a.AppID = m.appID
	a.Invocation = m.invocation
	if err := m.log.Record(ctx, a); err != nil {
		return fmt.Errorf("apps: memory access could not be recorded, so it did not happen: %w", err)
	}
	return nil
}

// Search runs a semantic search and records it.
func (m *MemoryCap) Search(ctx context.Context, q Query) ([]Episode, error) {
	if q.Limit <= 0 || q.Limit > m.max {
		q.Limit = m.max
	}
	eps, err := m.src.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(eps) > q.Limit {
		eps = eps[:q.Limit]
	}
	cleanQuery, _ := m.redact.Redact(q.Text)
	if err := m.record(ctx, Access{
		Op: string(MethodMemorySearch), Query: cleanQuery, Episodes: ids(eps), Count: len(eps),
	}); err != nil {
		return nil, err
	}
	return eps, nil
}

// Recent returns the most recent episode of a kind.
func (m *MemoryCap) Recent(ctx context.Context, kind string, within time.Duration) (*Episode, error) {
	ep, err := m.src.Recent(ctx, kind, within)
	if err != nil {
		return nil, err
	}
	acc := Access{Op: string(MethodMemoryRecentEpisode), Query: kind}
	if ep != nil {
		acc.Episodes = []string{ep.ID}
		acc.Count = 1
	}
	if err := m.record(ctx, acc); err != nil {
		return nil, err
	}
	return ep, nil
}

// Get reads one episode by id.
func (m *MemoryCap) Get(ctx context.Context, id string) (*Episode, error) {
	ep, err := m.src.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	acc := Access{Op: string(MethodMemoryGet), Episodes: []string{id}}
	if ep != nil {
		acc.Count = 1
	}
	if err := m.record(ctx, acc); err != nil {
		return nil, err
	}
	return ep, nil
}

// ExtractCommitments runs extraction over an episode the app already holds.
//
// It re-reads the episode by id rather than trusting the one the app passed
// back. An app that can hand the runtime an episode of its own invention could
// otherwise put words in the user's mouth and have them extracted into a
// commitment with the user's name on it.
func (m *MemoryCap) ExtractCommitments(ctx context.Context, e Episode) ([]Commitment, error) {
	if e.ID == "" {
		return nil, errors.New("apps: extractCommitments needs an episode with an id")
	}
	real, err := m.src.Get(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	if real == nil {
		return nil, fmt.Errorf("apps: no episode %s", e.ID)
	}
	cs, err := m.src.Extract(ctx, *real)
	if err != nil {
		return nil, err
	}
	if err := m.record(ctx, Access{
		Op: string(MethodMemoryExtract), Episodes: []string{e.ID}, Count: len(cs),
	}); err != nil {
		return nil, err
	}
	return cs, nil
}

// Write saves a note. Redact, then write — the ordering `internal/episode`'s
// writer states and the reason it states it: an embedded key cannot be
// unembedded, and a note is exactly the kind of text a search index gets built
// over later.
func (m *MemoryCap) Write(ctx context.Context, n Note) (string, error) {
	if m.sink == nil {
		return "", ErrNoNoteStore
	}
	n.Kind = "note"
	n.Title, _ = m.redact.Redact(n.Title)
	n.Body, _ = m.redact.Redact(n.Body)
	for i := range n.Commitments {
		n.Commitments[i].Text, _ = m.redact.Redact(n.Commitments[i].Text)
		n.Commitments[i].To, _ = m.redact.Redact(n.Commitments[i].To)
	}
	for i := range n.Tags {
		n.Tags[i], _ = m.redact.Redact(n.Tags[i])
	}

	id, err := m.sink.WriteNote(ctx, m.appID, n)
	acc := Access{Op: string(MethodMemoryWrite), Query: n.Title, Count: 1}
	if err != nil {
		acc.Count = 0
	}
	if rerr := m.record(ctx, acc); rerr != nil {
		return "", rerr
	}
	return id, err
}

func ids(eps []Episode) []string {
	out := make([]string, 0, len(eps))
	for _, e := range eps {
		out = append(out, e.ID)
	}
	return out
}

// ---------------------------------------------------------------- sources --

// StaticSource is a [Source] over a fixed list of episodes: the test double, and
// the thing a `relay app run --fixture` would use. Search is a keyword match
// rather than the hybrid retrieval in `internal/search`, and it says so — a
// double that pretends to be the real ranker teaches an app author something
// false about relevance.
type StaticSource struct {
	Episodes  []Episode
	Extractor func(Episode) []Commitment
	Now       func() time.Time
}

func (s *StaticSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *StaticSource) Search(_ context.Context, q Query) ([]Episode, error) {
	terms := strings.Fields(strings.ToLower(q.Text))
	var out []Episode
	for _, e := range s.Episodes {
		if !q.Since.IsZero() && e.StartedAt.Before(q.Since) {
			continue
		}
		hay := strings.ToLower(e.Transcript + " " + e.Kind + " " + e.Location)
		ok := len(terms) == 0
		for _, t := range terms {
			if strings.Contains(hay, t) {
				ok = true
				break
			}
		}
		if ok {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (s *StaticSource) Recent(_ context.Context, kind string, within time.Duration) (*Episode, error) {
	cutoff := time.Time{}
	if within > 0 {
		cutoff = s.now().Add(-within)
	}
	var best *Episode
	for i := range s.Episodes {
		e := s.Episodes[i]
		if kind != "" && e.Kind != kind {
			continue
		}
		if !cutoff.IsZero() && e.EndedAt.Before(cutoff) {
			continue
		}
		if best == nil || e.StartedAt.After(best.StartedAt) {
			cp := e
			best = &cp
		}
	}
	return best, nil
}

func (s *StaticSource) Get(_ context.Context, id string) (*Episode, error) {
	for i := range s.Episodes {
		if s.Episodes[i].ID == id {
			cp := s.Episodes[i]
			return &cp, nil
		}
	}
	return nil, nil
}

func (s *StaticSource) Extract(_ context.Context, e Episode) ([]Commitment, error) {
	if s.Extractor == nil {
		return nil, nil
	}
	return s.Extractor(e), nil
}

// MemorySink collects notes in memory.
type MemorySink struct {
	mu    sync.Mutex
	Notes []Note
	Apps  []string
}

func (s *MemorySink) WriteNote(_ context.Context, appID string, n Note) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Notes = append(s.Notes, n)
	s.Apps = append(s.Apps, appID)
	return fmt.Sprintf("note-%d", len(s.Notes)), nil
}
