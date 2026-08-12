package apps

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Host is relayd's side of the capability channel: the only thing an app can
// talk to, and the second of the two places the grant is enforced.
//
// The first place is [Capabilities], which decides what the runner builds `ctx`
// from — so an ungranted capability is not a property on the object at all. This
// is the second: a dispatch table containing only the granted methods. The app
// is untrusted code and the runner is a file inside the app's own root, so
// "the runner would never send that frame" is not a boundary. This is.
//
// A call to a method the grant did not mint answers [CodeNoCapability], never
// "denied". The difference matters: a refusal tells an app that the capability
// exists and the user said no, which turns the channel into a probe of what the
// user declined.

// LogLine is one line of an app's output.
type LogLine struct {
	At         time.Time `json:"at"`
	AppID      string    `json:"app"`
	Invocation string    `json:"invocation,omitempty"`
	// Stream is "app" for ctx.log, "stdout" or "stderr" for the process's own
	// output. An app that prints to stdout is still an app whose output the user
	// may read, so it is captured and redacted the same way.
	Stream  string         `json:"stream"`
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

// LogSink receives app output. Nil means the lines are counted and dropped, and
// [Invocation] says how many — a log that silently vanishes is worse than one
// that is absent.
type LogSink interface {
	AppLogged(ctx context.Context, l LogLine) error
}

// LogSinkFunc adapts a function to [LogSink].
type LogSinkFunc func(ctx context.Context, l LogLine) error

func (f LogSinkFunc) AppLogged(ctx context.Context, l LogLine) error { return f(ctx, l) }

// MemoryLogSink keeps lines in memory.
type MemoryLogSink struct {
	mu    sync.Mutex
	lines []LogLine
}

func (s *MemoryLogSink) AppLogged(_ context.Context, l LogLine) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, l)
	return nil
}

// All returns every line, oldest first.
func (s *MemoryLogSink) All() []LogLine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]LogLine(nil), s.lines...)
}

// handler runs one capability call. emit is non-nil only for streaming methods.
type handler func(ctx context.Context, args json.RawMessage, emit func(any) error) (any, error)

// HostOptions configures a [Host].
type HostOptions struct {
	Installed  Installed
	Invocation string

	Memory  *MemoryCap
	Glasses *GlassesCap
	Agent   *AgentCap
	Storage Storage
	Fetch   *Fetcher
	UI      *UICap

	// Redact is required. Every string an app hands back — a log line, a spoken
	// sentence, a note — goes through the detector, because an app that read a
	// credential out of a transcript and printed it must not turn relayd's log
	// into where that credential ends up.
	Redact Redactor
	Log    LogSink
	Now    func() time.Time
	// MaxConcurrent bounds how many capability calls one app may have in flight.
	MaxConcurrent int
}

// DefaultMaxConcurrent bounds an app's in-flight capability calls.
const DefaultMaxConcurrent = 8

// Host serves one invocation's capability channel.
type Host struct {
	inst   Installed
	inv    string
	table  map[Method]handler
	redact Redactor
	log    LogSink
	now    func() time.Time
	sem    chan struct{}

	mu  sync.Mutex // guards the writer
	out *bufio.Writer

	calls       atomic.Int64
	refused     atomic.Int64
	failed      atomic.Int64
	logsDropped atomic.Int64

	// appErr is the app's own failure, if onTrigger threw.
	appErrMu sync.Mutex
	appErr   *appError
	finished atomic.Bool
}

// NewHost builds the dispatcher for one invocation.
//
// The table is built from [Methods], which is built from the granted scopes, so
// there is no path by which a method reaches the table without a scope having
// paid for it. A capability whose backing implementation is missing — no glasses
// paired, no note store — is still in the table and answers [CodeUnavailable],
// because "you were granted the camera and this box has none" and "you were
// never granted the camera" are different facts and an app should be able to say
// which it hit.
func NewHost(o HostOptions) (*Host, error) {
	if o.Redact == nil {
		return nil, ErrNoRedactor
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = DefaultMaxConcurrent
	}
	h := &Host{
		inst: o.Installed, inv: o.Invocation, redact: o.Redact, log: o.Log,
		now: o.Now, sem: make(chan struct{}, o.MaxConcurrent),
		table: map[Method]handler{},
	}

	impl := map[Method]handler{
		MethodMemorySearch:        h.memorySearch(o.Memory),
		MethodMemoryRecentEpisode: h.memoryRecent(o.Memory),
		MethodMemoryGet:           h.memoryGet(o.Memory),
		MethodMemoryExtract:       h.memoryExtract(o.Memory),
		MethodMemoryWrite:         h.memoryWrite(o.Memory),
		MethodGlassesSay:          h.glassesSay(o.Glasses),
		MethodGlassesCapture:      h.glassesCapture(o.Glasses),
		MethodGlassesListen:       h.glassesListen(o.Glasses),
		MethodAgentAsk:            h.agentAsk(o.Agent),
		MethodAgentStream:         h.agentStream(o.Agent),
		MethodUIRender:            h.uiRender(o.UI),
		MethodUIAsk:               h.uiAsk(o.UI),
		MethodStorageGet:          h.storageGet(o.Storage),
		MethodStorageSet:          h.storageSet(o.Storage),
		MethodStorageDelete:       h.storageDelete(o.Storage),
		MethodFetch:               h.fetch(o.Fetch),
		MethodLog:                 h.appLog,
	}
	for _, m := range Methods(o.Installed.Granted) {
		fn, ok := impl[m]
		if !ok {
			return nil, fmt.Errorf("apps: capability %s has no implementation", m)
		}
		h.table[m] = fn
	}
	return h, nil
}

// Methods lists what this host will answer, for a test and for the console.
func (h *Host) Methods() []Method {
	out := make([]Method, 0, len(h.table))
	for m := range h.table {
		out = append(out, m)
	}
	return out
}

// Serve runs the channel until the app says it is done, the reader closes, or
// ctx is cancelled.
//
// It returns the app's own error when onTrigger threw. A transport error — the
// process died mid-frame — is returned as an error, because that is relayd's
// problem and not the app's.
func (h *Host) Serve(ctx context.Context, r io.Reader, w io.Writer, start startFrame) error {
	h.out = bufio.NewWriter(w)
	if err := h.write(start); err != nil {
		return fmt.Errorf("apps: could not send the start frame: %w", err)
	}

	var wg sync.WaitGroup
	defer wg.Wait()

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxFrameBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f callFrame
		if err := json.Unmarshal(line, &f); err != nil {
			// A frame this side cannot parse is the runner or the app being
			// wrong about the protocol. Neither is recoverable, and continuing
			// would leave the app waiting for a reply that will never come.
			return fmt.Errorf("apps: unreadable frame from the app process: %w", err)
		}
		switch f.T {
		case frameDone:
			h.finished.Store(true)
			return nil
		case frameFailed:
			h.finished.Store(true)
			h.appErrMu.Lock()
			h.appErr = f.Error
			h.appErrMu.Unlock()
			return nil
		case frameCall:
			wg.Add(1)
			select {
			case h.sem <- struct{}{}:
			case <-ctx.Done():
				wg.Done()
				return ctx.Err()
			}
			go func(f callFrame) {
				defer wg.Done()
				defer func() { <-h.sem }()
				h.dispatch(ctx, f)
			}(f)
		default:
			return fmt.Errorf("apps: unknown frame %q from the app process", f.T)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("apps: reading from the app process: %w", err)
	}
	return nil
}

// MaxFrameBytes bounds one frame from the app. A frame larger than this is an
// app trying to make relayd allocate, which is the cheapest denial of service
// available to code running next to the thing it wants to stall.
const MaxFrameBytes = 4 << 20

// AppError is the app's own failure, if onTrigger threw.
func (h *Host) AppError() error {
	h.appErrMu.Lock()
	defer h.appErrMu.Unlock()
	if h.appErr == nil {
		return nil
	}
	msg := h.appErr.Message
	if h.appErr.Name != "" {
		msg = h.appErr.Name + ": " + msg
	}
	return errors.New(msg)
}

// Finished reports whether the app said it was done, rather than the channel
// closing under it.
func (h *Host) Finished() bool { return h.finished.Load() }

// Counts is what happened on this channel.
func (h *Host) Counts() (calls, refused, failed, logsDropped int64) {
	return h.calls.Load(), h.refused.Load(), h.failed.Load(), h.logsDropped.Load()
}

func (h *Host) dispatch(ctx context.Context, f callFrame) {
	h.calls.Add(1)
	fn, ok := h.table[f.Method]
	if !ok {
		h.refused.Add(1)
		_, known := RequiredScope(f.Method)
		msg := fmt.Sprintf("this app has no %s capability", f.Method)
		if !known {
			msg = fmt.Sprintf("%s is not part of the SDK", f.Method)
		}
		h.reply(resultFrame{T: frameErr, ID: f.ID, Error: &wireError{Code: CodeNoCapability, Message: msg}})
		return
	}

	emit := func(v any) error {
		return h.write(resultFrame{T: frameChunk, ID: f.ID, Value: v})
	}
	res, err := fn(ctx, f.Args, emit)
	if err != nil {
		h.failed.Add(1)
		h.reply(resultFrame{T: frameErr, ID: f.ID, Error: toWireError(err)})
		return
	}
	h.reply(resultFrame{T: frameOK, ID: f.ID, Result: res})
}

func toWireError(err error) *wireError {
	switch {
	case errors.Is(err, ErrDenied):
		return &wireError{Code: CodeDenied, Message: err.Error()}
	// ErrUnavailable is what the `unavailable` helper wraps, and it was missing
	// from this switch: every caller of that helper — glasses.say, .capture,
	// .listen — answered `failed` on a box with no glasses paired. The codes
	// mean different things to an app (see CodeUnavailable's own comment: "this
	// one is about the box, not the grant"), and the difference is whether the
	// author goes looking for a bug in their app or tells the user to pair
	// their glasses.
	case errors.Is(err, ErrUnavailable), errors.Is(err, ErrNoNoteStore), errors.Is(err, ErrNoStreaming):
		return &wireError{Code: CodeUnavailable, Message: err.Error()}
	case errors.Is(err, errBadArgs):
		return &wireError{Code: CodeBadArgs, Message: err.Error()}
	default:
		return &wireError{Code: CodeFailed, Message: err.Error()}
	}
}

var errBadArgs = errors.New("apps: bad arguments")

func (h *Host) reply(f resultFrame) {
	if err := h.write(f); err != nil {
		// Nothing useful to do: the app process is gone or the pipe is broken,
		// and the supervisor is about to notice. Counting it keeps the
		// invocation record honest about a reply that never landed.
		h.failed.Add(1)
	}
}

func (h *Host) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.out.Write(append(b, '\n')); err != nil {
		return err
	}
	return h.out.Flush()
}

// ------------------------------------------------------------- handlers --

func unavailable(what string) error {
	return fmt.Errorf("%w: %s", ErrUnavailable, what)
}

// ErrUnavailable is a granted capability with nothing behind it on this box.
var ErrUnavailable = errors.New("apps: unavailable on this box")

func decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("%w: %v", errBadArgs, err)
	}
	return nil
}

func (h *Host) memorySearch(m *MemoryCap) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if m == nil {
			return nil, unavailable("this box has no memory wired")
		}
		var a struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
			Since *int64 `json:"since"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		q := Query{Text: a.Query, Limit: a.Limit}
		if a.Since != nil {
			q.Since = time.UnixMilli(*a.Since)
		}
		return m.Search(ctx, q)
	}
}

func (h *Host) memoryRecent(m *MemoryCap) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if m == nil {
			return nil, unavailable("this box has no memory wired")
		}
		var a struct {
			Kind   string `json:"kind"`
			Within int64  `json:"within"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		return m.Recent(ctx, a.Kind, time.Duration(a.Within)*time.Millisecond)
	}
}

func (h *Host) memoryGet(m *MemoryCap) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if m == nil {
			return nil, unavailable("this box has no memory wired")
		}
		var a struct {
			ID string `json:"episodeId"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		if a.ID == "" {
			return nil, fmt.Errorf("%w: get needs an episode id", errBadArgs)
		}
		return m.Get(ctx, a.ID)
	}
}

func (h *Host) memoryExtract(m *MemoryCap) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if m == nil {
			return nil, unavailable("this box has no memory wired")
		}
		var a struct {
			Episode Episode `json:"episode"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		return m.ExtractCommitments(ctx, a.Episode)
	}
}

func (h *Host) memoryWrite(m *MemoryCap) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if m == nil {
			return nil, unavailable("this box has no memory wired")
		}
		var a struct {
			Note Note `json:"note"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		id, err := m.Write(ctx, a.Note)
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": id}, nil
	}
}

func (h *Host) glassesSay(g *GlassesCap) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if g == nil {
			return nil, unavailable("no glasses are paired with this box")
		}
		var a struct {
			Text string `json:"text"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		return nil, g.Say(ctx, a.Text)
	}
}

// uiRender draws a view.
//
// The arguments are the view itself rather than a wrapper, matching the SDK's
// `ui.render(v)`. It is decoded through [ParseViewJSON] rather than the plain
// decoder because a field that does not belong to a block's kind has to be an
// error the app can read, and Go's decoder throws unknown keys away without a
// word — an app that set `body` on a `speak` block believes it will be drawn.
func (h *Host) uiRender(u *UICap) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if u == nil {
			return nil, unavailable("this box has no render surface")
		}
		v, err := ParseViewJSON(args)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", errBadArgs, err)
		}
		return nil, h.uiErr(u.Render(ctx, v))
	}
}

// uiAsk draws a question and waits for the answer.
func (h *Host) uiAsk(u *UICap) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if u == nil {
			return nil, unavailable("this box has no render surface")
		}
		v, err := ParseViewJSON(args)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", errBadArgs, err)
		}
		ok, err := u.Ask(ctx, v)
		if err != nil {
			return nil, h.uiErr(err)
		}
		// The bare boolean, matching `ask(): Promise<boolean>`. There is
		// deliberately no third value for "nobody answered": an app must treat
		// silence as a no, and a field it could branch on is how "confirm
		// before you send" becomes "send".
		return ok, nil
	}
}

// uiErr maps the two failures an app can act on to codes it can read.
//
// A view that will not render is the app's own bug and answers bad_arguments
// with the validator's sentence, which is the same sentence the SDK would have
// given it locally. No phone is not a bug at all and answers unavailable — the
// box changed, not the app.
func (h *Host) uiErr(err error) error {
	var ve *ViewError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ve):
		return fmt.Errorf("%w: %s", errBadArgs, ve.Message)
	case errors.Is(err, ErrNoPhone):
		return unavailable(ErrNoPhone.Error())
	}
	return err
}

func (h *Host) glassesCapture(g *GlassesCap) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if g == nil {
			return nil, unavailable("no glasses are paired with this box")
		}
		var a struct {
			Immediate bool `json:"immediate"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		return g.Capture(ctx, a.Immediate)
	}
}

func (h *Host) glassesListen(g *GlassesCap) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if g == nil {
			return nil, unavailable("no glasses are paired with this box")
		}
		var a struct {
			TimeoutMs int64 `json:"timeoutMs"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		return g.Listen(ctx, time.Duration(a.TimeoutMs)*time.Millisecond)
	}
}

func (h *Host) agentAsk(a *AgentCap) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if a == nil {
			return nil, unavailable("no agent is configured on this box")
		}
		var in struct {
			Prompt string `json:"prompt"`
			Model  string `json:"model"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		return a.Ask(ctx, in.Prompt, in.Model)
	}
}

func (h *Host) agentStream(a *AgentCap) handler {
	return func(ctx context.Context, args json.RawMessage, emit func(any) error) (any, error) {
		if a == nil {
			return nil, unavailable("no agent is configured on this box")
		}
		var in struct {
			Prompt string `json:"prompt"`
		}
		if err := decode(args, &in); err != nil {
			return nil, err
		}
		err := a.Stream(ctx, in.Prompt, func(chunk string) error { return emit(chunk) })
		if err != nil {
			return nil, err
		}
		return nil, nil
	}
}

func (h *Host) storageGet(s Storage) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if s == nil {
			return nil, unavailable("this box has no app storage directory")
		}
		var a struct {
			Key string `json:"key"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		if err := ValidStorageKey(a.Key); err != nil {
			return nil, fmt.Errorf("%w: %v", errBadArgs, err)
		}
		raw, err := s.Get(ctx, h.inst.Manifest.ID, a.Key)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			return nil, nil
		}
		return raw, nil
	}
}

func (h *Host) storageSet(s Storage) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if s == nil {
			return nil, unavailable("this box has no app storage directory")
		}
		var a struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		if err := ValidStorageKey(a.Key); err != nil {
			return nil, fmt.Errorf("%w: %v", errBadArgs, err)
		}
		return nil, s.Set(ctx, h.inst.Manifest.ID, a.Key, a.Value)
	}
}

func (h *Host) storageDelete(s Storage) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if s == nil {
			return nil, unavailable("this box has no app storage directory")
		}
		var a struct {
			Key string `json:"key"`
		}
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		if err := ValidStorageKey(a.Key); err != nil {
			return nil, fmt.Errorf("%w: %v", errBadArgs, err)
		}
		return nil, s.Delete(ctx, h.inst.Manifest.ID, a.Key)
	}
}

func (h *Host) fetch(f *Fetcher) handler {
	return func(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
		if f == nil {
			return nil, unavailable("egress is not wired on this box")
		}
		var a FetchRequest
		if err := decode(args, &a); err != nil {
			return nil, err
		}
		return f.Do(ctx, a)
	}
}

// appLog is `ctx.log`. It is a capability with no scope — an app logging is an
// app talking about itself — and it is redacted, because what an app talks about
// is whatever it just read out of the user's memory.
func (h *Host) appLog(ctx context.Context, args json.RawMessage, _ func(any) error) (any, error) {
	var a struct {
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	h.emitLog(ctx, "app", a.Level, a.Message, a.Data)
	return nil, nil
}

// emitLog redacts and forwards one line.
func (h *Host) emitLog(ctx context.Context, stream, level, message string, data map[string]any) {
	clean, _ := h.redact.Redact(message)
	var cleanData map[string]any
	if len(data) > 0 {
		cleanData = make(map[string]any, len(data))
		for k, v := range data {
			if s, ok := v.(string); ok {
				c, _ := h.redact.Redact(s)
				cleanData[k] = c
				continue
			}
			cleanData[k] = v
		}
	}
	if level == "" {
		level = "info"
	}
	line := LogLine{
		At: h.now(), AppID: h.inst.Manifest.ID, Invocation: h.inv,
		Stream: stream, Level: level, Message: clean, Data: cleanData,
	}
	if h.log == nil {
		h.logsDropped.Add(1)
		return
	}
	if err := h.log.AppLogged(ctx, line); err != nil {
		h.logsDropped.Add(1)
	}
}
