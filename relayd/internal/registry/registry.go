// Package registry is the session registry: every agent session across every
// runtime in one table.
//
// SYSTEM.md §10 step 2 — "one list where there were five". None of the five
// runtimes expose this, and none of them know about each other, so the
// orchestrator maintains it by driving their protocols and normalising what
// comes back (ADAPTERS.md §5).
//
// Three properties are load-bearing and each one is a design decision rather
// than an implementation detail:
//
//   - **It survives a restart.** The table lives in SQLite (internal/store), not
//     in memory. relayd restarting must not lose the list, and it must not
//     silently claim a session is still running when the process that was
//     driving it is gone: [Registry.Recover] detaches those rows and records an
//     incident for each.
//   - **It is reconcilable against the runtimes' own stores**, not only
//     rebuildable from what we watched. Hermes keeps SQLite, Claude Code writes
//     one JSONL per session, Codex keeps session_index.jsonl. [Reconciler] is
//     that seam; the backfill readers implement it.
//   - **Failure is visible.** SYSTEM.md §6.2 calls the subprocess seam the
//     weakest in the system. A session that dies, a restart that cannot carry
//     history, a runtime that no longer has a session we think it has — each is
//     an [Incident] with a name, not a log line nobody reads.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/bus"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/store"
)

// Errors this package returns.
var (
	// ErrNoSuchSession is a command for a session the registry is not driving.
	// It may still exist in the table — closed, or from before a restart — and
	// the caller resumes it rather than failing.
	ErrNoSuchSession = errors.New("registry: no live session with that id")

	// ErrNoAdapter is a runtime nothing is registered for: not installed, not
	// enabled in config, or not built yet.
	ErrNoAdapter = errors.New("registry: no adapter registered for that runtime")

	// ErrNoOpenQuestion is an answer for a session that is not blocked.
	ErrNoOpenQuestion = errors.New("registry: that session has no open question")

	// ErrClosed is the registry after Shutdown.
	ErrClosed = errors.New("registry: closed")
)

// MaxTurnText caps how much assistant text the registry keeps per turn.
//
// The registry tier is live state, not the archive: MEMORY.md §3 keeps the
// transcripts on disk in place and stores a pointer, and nothing written here is
// ever embedded. A cap exists anyway because a long coding turn is thousands of
// tokens of diff and holding all of it per live session buys nothing the console
// can use.
const MaxTurnText = 4096

// Options configures a registry.
type Options struct {
	DB  *store.DB
	Bus *bus.Bus
	Log *slog.Logger

	// Restart is what happens when a session ends without being asked to.
	Restart RestartPolicy

	// FlushInterval is how often last_active is written back for sessions that
	// are merely busy rather than changing state. Default 5s: an event stream
	// runs at hundreds of events a turn and a write per event is pointless I/O.
	FlushInterval time.Duration

	// IncidentBuffer is how many recent incidents to keep for the health
	// endpoint. Default 200.
	IncidentBuffer int

	Now   func() time.Time
	NewID func() string
}

// Registry is every session, everywhere.
type Registry struct {
	db  *store.DB
	bus *bus.Bus
	log *slog.Logger

	now     func() time.Time
	newID   func() string
	restart RestartPolicy
	flushIv time.Duration

	changes   *bus.Topic[Change]
	incidents *bus.Topic[Incident]

	mu          sync.RWMutex
	adapters    map[adapter.Runtime]adapter.Adapter
	entries     map[string]*Entry
	reconcilers map[adapter.Runtime]Reconciler
	recent      []Incident
	recentCap   int
	closed      bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New builds a registry. The store is required; everything else defaults.
func New(o Options) (*Registry, error) {
	if o.DB == nil {
		return nil, errors.New("registry: a store is required")
	}
	if o.Bus == nil {
		o.Bus = bus.New(bus.Options{Log: o.Log})
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewID == nil {
		o.NewID = uuid.NewString
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 5 * time.Second
	}
	if o.IncidentBuffer <= 0 {
		o.IncidentBuffer = 200
	}
	o.Restart = o.Restart.withDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	r := &Registry{
		db:          o.DB,
		bus:         o.Bus,
		log:         o.Log,
		now:         o.Now,
		newID:       o.NewID,
		restart:     o.Restart,
		flushIv:     o.FlushInterval,
		changes:     bus.NewTopic(bus.TopicOptions[Change]{Buffer: 128, Log: o.Log}),
		incidents:   bus.NewTopic(bus.TopicOptions[Incident]{Buffer: 128, Log: o.Log}),
		adapters:    map[adapter.Runtime]adapter.Adapter{},
		entries:     map[string]*Entry{},
		reconcilers: map[adapter.Runtime]Reconciler{},
		recentCap:   o.IncidentBuffer,
		ctx:         ctx,
		cancel:      cancel,
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.flushLoop()
	}()
	return r, nil
}

// Bus is the event bus every adapter's stream is fanned into.
func (r *Registry) Bus() *bus.Bus { return r.bus }

// AddAdapter registers the adapter for one runtime. Starting a session for a
// runtime with no adapter fails with ErrNoAdapter rather than silently doing
// nothing.
func (r *Registry) AddAdapter(a adapter.Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.Runtime()] = a
}

// Adapter returns the adapter for a runtime.
func (r *Registry) Adapter(rt adapter.Runtime) (adapter.Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[rt]
	return a, ok
}

// Runtimes lists the runtimes an adapter is registered for.
func (r *Registry) Runtimes() []adapter.Runtime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []adapter.Runtime
	for _, rt := range adapter.Runtimes() {
		if _, ok := r.adapters[rt]; ok {
			out = append(out, rt)
		}
	}
	return out
}

// StartOptions is a new session, in registry terms.
type StartOptions struct {
	Runtime adapter.Runtime

	// ID is Relay's session id. Empty generates one. Claude Code takes it
	// directly as --session-id, so the orchestrator names sessions rather than
	// discovering their names afterwards.
	ID string

	// Subject is what this session is for, in the user's words — "the payments
	// refactor". It is what the ping says out loud, so an empty one produces
	// "session 3f2a…" rather than an invented topic.
	Subject   string
	Workspace string
	Model     string
	Entities  []string

	PermissionMode string
	MCPServers     []adapter.MCPServer
	Env            []string
	Extra          map[string]string
}

func (o StartOptions) sessionOptions(id string) adapter.SessionOptions {
	return adapter.SessionOptions{
		ID:             id,
		Workspace:      o.Workspace,
		Model:          o.Model,
		MCPServers:     o.MCPServers,
		PermissionMode: o.PermissionMode,
		Env:            o.Env,
		Extra:          o.Extra,
	}
}

// Start opens a session on a runtime and registers it.
func (r *Registry) Start(ctx context.Context, o StartOptions) (*Entry, error) {
	r.mu.RLock()
	closed := r.closed
	a, ok := r.adapters[o.Runtime]
	r.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoAdapter, o.Runtime)
	}

	id := o.ID
	if id == "" {
		id = r.newID()
	}
	sess, err := a.Start(ctx, o.sessionOptions(id))
	if err != nil {
		r.record(Incident{
			Runtime: string(o.Runtime), Session: id, Kind: IncidentStartFailed,
			Message: err.Error(), Fatal: true,
		})
		return nil, fmt.Errorf("registry: start %s: %w", o.Runtime, err)
	}
	return r.adopt(ctx, sess, o)
}

// Adopt registers a session that was opened elsewhere — a resume, a test, or an
// adapter that discovered a session already running.
func (r *Registry) Adopt(ctx context.Context, sess adapter.Session, o StartOptions) (*Entry, error) {
	if o.Runtime == "" {
		o.Runtime = sess.Runtime()
	}
	return r.adopt(ctx, sess, o)
}

func (r *Registry) adopt(ctx context.Context, sess adapter.Session, o StartOptions) (*Entry, error) {
	now := r.now()
	row := store.Session{
		ID:         sess.ID(),
		Runtime:    string(sess.Runtime()),
		NativeID:   sess.Native(),
		Agent:      o.Model,
		Subject:    o.Subject,
		Workspace:  o.Workspace,
		Entities:   o.Entities,
		CreatedAt:  now,
		LastActive: now,
		State:      store.SessionIdle,
	}

	e := &Entry{
		reg:  r,
		sess: sess,
		opts: o,
		row:  row,
		caps: sess.Capabilities(),
		done: make(chan struct{}),
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	if prev, ok := r.entries[row.ID]; ok && prev != e {
		r.mu.Unlock()
		return nil, fmt.Errorf("registry: session %s is already registered", row.ID)
	}
	r.entries[row.ID] = e
	r.mu.Unlock()

	if err := r.db.PutSession(ctx, row); err != nil {
		r.mu.Lock()
		delete(r.entries, row.ID)
		r.mu.Unlock()
		return nil, fmt.Errorf("registry: persist session %s: %w", row.ID, err)
	}
	r.changes.Publish(Change{Kind: ChangeAdded, Session: row, At: now})

	// A capability the runtime cannot observe is a visible gap, not a silent
	// one: a session with no needs-input path will never ask for approval, and
	// the user should be told that before something destructive runs unattended.
	if e.caps.Get(adapter.CapNeedsInput) != adapter.SupportYes {
		r.record(Incident{
			Runtime: row.Runtime, Session: row.ID, Kind: IncidentDegraded,
			Message: fmt.Sprintf("this session cannot report a blocked question (%s): %s",
				e.caps.Get(adapter.CapNeedsInput), e.caps.Note(adapter.CapNeedsInput)),
		})
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		e.pump()
	}()
	return e, nil
}

// Get returns a live session by Relay id.
func (r *Registry) Get(id string) (*Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	return e, ok
}

// Live returns every session the registry is currently driving.
func (r *Registry) Live() []*Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Entry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	return out
}

// List reads the table. This is the whole list — live sessions and every
// session ever reconciled out of a runtime's own store.
func (r *Registry) List(ctx context.Context, f store.SessionFilter) ([]store.Session, error) {
	return r.db.ListSessions(ctx, f)
}

// Session reads one row.
func (r *Registry) Session(ctx context.Context, id string) (store.Session, error) {
	return r.db.GetSession(ctx, id)
}

// Detail is a session plus what the console shows next to it.
type Detail struct {
	Session store.Session
	Turns   []store.Turn
	Tools   []store.ToolCall

	// Live is true when the registry is driving this session right now.
	Live bool
	// Capabilities is what this session's runtime can be observed to do. The
	// console renders the gaps: "no cost data" should read as a fact about the
	// runtime rather than as a bug in Relay.
	Capabilities map[string]string
	// Missing is the capabilities that are not observed, in stable order.
	Missing []string
	// Questions is what this session is blocked on right now.
	Questions []Question
}

// Question is one open NeedsInput, addressable by id so a phone or a console can
// answer a specific one.
type Question struct {
	ID       string
	Ask      string
	Prompt   string
	Options  []event.Option
	Tool     *event.ToolRef
	At       time.Time
	Deadline time.Time
}

// Detail assembles a session for the console.
func (r *Registry) Detail(ctx context.Context, id string) (Detail, error) {
	row, err := r.db.GetSession(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	turns, err := r.db.ListTurns(ctx, id, 200)
	if err != nil {
		return Detail{}, err
	}
	tools, err := r.db.ListToolCalls(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Session: row, Turns: turns, Tools: tools}
	if e, ok := r.Get(id); ok {
		d.Live = true
		caps := e.Capabilities()
		d.Capabilities = map[string]string{}
		for name, sup := range caps.All() {
			d.Capabilities[string(name)] = sup.String()
		}
		for _, c := range caps.Missing() {
			d.Missing = append(d.Missing, string(c))
		}
		d.Questions = e.Questions()
	}
	return d, nil
}

// Subject returns a session's subject for speech. It is the Namer the pinger
// takes, so a completion says "payments is done" rather than a uuid.
func (r *Registry) Subject(id string) string {
	if e, ok := r.Get(id); ok {
		return e.Row().Subject
	}
	row, err := r.db.GetSession(context.Background(), id)
	if err != nil {
		return ""
	}
	return row.Subject
}

// ------------------------------------------------------------- commands --

// Send pushes a turn into a session.
func (r *Registry) Send(ctx context.Context, id string, t adapter.Turn) (string, error) {
	e, ok := r.Get(id)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNoSuchSession, id)
	}
	return e.Send(ctx, t)
}

// Steer injects an utterance into a running turn. On a runtime that cannot —
// which is all three ACP runtimes — it returns an *adapter.UnsupportedError and
// the caller cancels and re-prompts (ADAPTERS.md §4).
func (r *Registry) Steer(ctx context.Context, id, turnID string, t adapter.Turn) error {
	e, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchSession, id)
	}
	return e.sess.Steer(ctx, turnID, t)
}

// Cancel stops a running turn.
func (r *Registry) Cancel(ctx context.Context, id, turnID string) error {
	e, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchSession, id)
	}
	if turnID == "" {
		turnID = e.Turn()
	}
	return e.sess.Cancel(ctx, turnID)
}

// Answer resolves the oldest open question on a session. This is the console's
// path; the phone answers a specific question by id through AnswerQuestion.
func (r *Registry) Answer(ctx context.Context, id string, reply event.Reply) error {
	e, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchSession, id)
	}
	return e.Answer(ctx, "", reply)
}

// AnswerQuestion resolves one specific question.
func (r *Registry) AnswerQuestion(ctx context.Context, id, questionID string, reply event.Reply) error {
	e, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchSession, id)
	}
	return e.Answer(ctx, questionID, reply)
}

// Close ends a session deliberately. A session closed this way is not restarted.
func (r *Registry) Close(ctx context.Context, id string) error {
	e, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchSession, id)
	}
	return e.Close(ctx)
}

// ------------------------------------------------------------ watching --

// ChangeKind is what happened to a session row.
type ChangeKind string

const (
	ChangeAdded   ChangeKind = "added"
	ChangeUpdated ChangeKind = "updated"
	ChangeClosed  ChangeKind = "closed"
)

// Change is a session row that moved. The console's live view is this stream.
type Change struct {
	Kind    ChangeKind    `json:"kind"`
	Session store.Session `json:"session"`
	At      time.Time     `json:"at"`
}

// Watch subscribes to session changes.
func (r *Registry) Watch(name string) *bus.Sub[Change] { return r.changes.Subscribe(name) }

// WatchIncidents subscribes to incidents.
func (r *Registry) WatchIncidents(name string) *bus.Sub[Incident] {
	return r.incidents.Subscribe(name)
}

// Incidents returns the recent incident ring, oldest first.
func (r *Registry) Incidents() []Incident {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Incident(nil), r.recent...)
}

func (r *Registry) record(i Incident) {
	if i.ID == "" {
		i.ID = r.newID()
	}
	if i.At.IsZero() {
		i.At = r.now()
	}

	r.mu.Lock()
	r.recent = append(r.recent, i)
	if len(r.recent) > r.recentCap {
		r.recent = r.recent[len(r.recent)-r.recentCap:]
	}
	r.mu.Unlock()

	lvl := slog.LevelWarn
	if i.Fatal {
		lvl = slog.LevelError
	}
	r.log.Log(context.Background(), lvl, "registry: "+string(i.Kind),
		"session", i.Session, "runtime", i.Runtime, "detail", i.Message)
	r.incidents.Publish(i)
}

// ------------------------------------------------------------- lifecycle --

func (r *Registry) flushLoop() {
	t := time.NewTicker(r.flushIv)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			for _, e := range r.Live() {
				e.flush(r.ctx)
			}
		}
	}
}

func (r *Registry) forget(id string) {
	r.mu.Lock()
	delete(r.entries, id)
	r.mu.Unlock()
}

// Shutdown stops every session, then every adapter, then the registry itself.
//
// Graceful means: sessions are asked to close and given until ctx expires; what
// is still running past that is reported rather than waited on forever, because
// a hung runtime must not hold the daemon open. Anything left is named in the
// returned result, which is how a stuck adapter becomes visible instead of
// becoming a mystery process after reboot.
func (r *Registry) Shutdown(ctx context.Context) ShutdownResult {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ShutdownResult{}
	}
	r.closed = true
	entries := make([]*Entry, 0, len(r.entries))
	for _, e := range r.entries {
		entries = append(entries, e)
	}
	adapters := make([]adapter.Adapter, 0, len(r.adapters))
	for _, a := range r.adapters {
		adapters = append(adapters, a)
	}
	r.mu.Unlock()

	var res ShutdownResult
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, e := range entries {
		wg.Add(1)
		go func(e *Entry) {
			defer wg.Done()
			if err := e.Close(ctx); err != nil {
				mu.Lock()
				res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", e.ID(), err))
				mu.Unlock()
				return
			}
			mu.Lock()
			res.Closed = append(res.Closed, e.ID())
			mu.Unlock()
		}(e)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		mu.Lock()
		res.TimedOut = true
		mu.Unlock()
	}

	for _, a := range adapters {
		if err := a.Close(ctx); err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s adapter: %v", a.Runtime(), err))
		}
	}

	r.cancel()
	// Pumps exit when their session's channel closes. A runtime that never
	// closes it would hold this forever, so wait with the caller's deadline.
	pumps := make(chan struct{})
	go func() { r.wg.Wait(); close(pumps) }()
	select {
	case <-pumps:
	case <-ctx.Done():
		res.TimedOut = true
	}

	r.changes.Close()
	r.incidents.Close()
	return res
}

// ShutdownResult says what stopped and what did not.
type ShutdownResult struct {
	Closed   []string `json:"closed"`
	Failed   []string `json:"failed,omitempty"`
	TimedOut bool     `json:"timed_out"`
}

// ------------------------------------------------------------- helpers --

func digest(v map[string]any) string {
	if len(v) == 0 {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
