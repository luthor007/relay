package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/store"
)

// Entry is one live session: the adapter session, the row that mirrors it, and
// whatever it is currently blocked on.
type Entry struct {
	reg  *Registry
	opts StartOptions

	mu       sync.Mutex
	sess     adapter.Session
	caps     adapter.Capabilities
	row      store.Session
	turn     string
	turnText strings.Builder
	turnAt   time.Time
	asks     []*question
	closing  bool
	finished bool
	dirty    bool
	restarts int

	done chan struct{}
}

type question struct {
	id   string
	ask  *event.NeedsInput
	at   time.Time
	kind event.InputKind
}

// ID is Relay's session id.
func (e *Entry) ID() string { return e.Row().ID }

// Runtime is which of the five this session runs on.
func (e *Entry) Runtime() adapter.Runtime { return adapter.Runtime(e.Row().Runtime) }

// Row is a snapshot of the registry row.
func (e *Entry) Row() store.Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.row
}

// Capabilities is what this session's runtime can be observed to do.
// Adapter is the live runtime session underneath this entry.
//
// It exists for one caller: internal/compaction, whose SessionSource is
// documented as "the registry is the implementation". Compaction needs the raw
// session for two things the registry's own Send cannot do — the type
// assertion to Compactable for Codex's thread/compact/start protocol call, and
// sending a turn that deliberately does *not* count as user activity, because
// MEMORY.md §9 is explicit that a silent memory pass must not move LastActive.
//
// Anything else should go through [Entry.Send], which keeps the turn
// accounting honest. Reaching for this to send an ordinary turn is a bug.
func (e *Entry) Adapter() adapter.Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sess
}

func (e *Entry) Capabilities() adapter.Capabilities {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.caps
}

// Turn is the id of the turn currently running, or "".
func (e *Entry) Turn() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.turn
}

// Done closes when the session's event stream has ended and the pump has
// finished applying it.
func (e *Entry) Done() <-chan struct{} { return e.done }

// Questions lists what this session is blocked on.
func (e *Entry) Questions() []Question {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Question, 0, len(e.asks))
	for _, q := range e.asks {
		out = append(out, Question{
			ID:       q.id,
			Ask:      string(q.ask.Ask),
			Prompt:   q.ask.Prompt,
			Options:  q.ask.Options,
			Tool:     q.ask.Tool,
			At:       q.at,
			Deadline: q.ask.Deadline,
		})
	}
	return out
}

// Send pushes a turn into this session.
func (e *Entry) Send(ctx context.Context, t adapter.Turn) (string, error) {
	e.mu.Lock()
	sess := e.sess
	e.mu.Unlock()

	turnID, err := sess.Send(ctx, t)
	if err != nil {
		return "", err
	}
	// The user's half of the exchange is recorded here rather than waiting for
	// an echo: only Claude Code replays injected turns, so an adapter-sourced
	// user turn is not something all five can be relied on to produce.
	now := e.reg.now()
	uid := turnID
	if uid == "" {
		uid = e.reg.newID()
	}
	_ = e.reg.db.PutTurn(ctx, store.Turn{
		ID:        e.ID() + ":user:" + uid,
		SessionID: e.ID(),
		Role:      "user",
		Text:      truncate(t.Text, MaxTurnText),
		At:        now,
		OK:        true,
	})

	e.mu.Lock()
	e.turn = turnID
	e.turnAt = now
	e.turnText.Reset()
	e.mu.Unlock()
	e.setState(store.SessionRunning)
	return turnID, nil
}

// Answer resolves an open question. An empty questionID answers the oldest,
// which is what "yes" spoken at the glasses means when only one is open.
func (e *Entry) Answer(ctx context.Context, questionID string, reply event.Reply) error {
	e.mu.Lock()
	var q *question
	for _, cand := range e.asks {
		if questionID == "" || cand.id == questionID {
			q = cand
			break
		}
	}
	e.mu.Unlock()

	if q == nil {
		return fmt.Errorf("%w: %s", ErrNoOpenQuestion, e.ID())
	}
	return q.ask.Reply(ctx, reply)
}

// Close ends the session deliberately. It is not restarted afterwards.
func (e *Entry) Close(ctx context.Context) error {
	e.mu.Lock()
	if e.closing {
		e.mu.Unlock()
		return nil
	}
	e.closing = true
	sess := e.sess
	e.mu.Unlock()

	err := sess.Close(ctx)
	e.setState(store.SessionClosed)
	return err
}

// ----------------------------------------------------------------- pump --

// pump is the fan-in for one session: apply the event to the registry, then
// publish it.
//
// The order matters and is deliberate. By the time a subscriber sees a
// TurnCompleted, the row already says idle and the cost is already counted, so
// an API client that reacts by re-reading the list gets the new state rather
// than the old one.
func (e *Entry) pump() {
	defer close(e.done)

	e.mu.Lock()
	ch := e.sess.Events()
	e.mu.Unlock()

	for ev := range ch {
		e.apply(ev)
		e.reg.bus.Publish(ev)
	}
	e.ended()
}

func (e *Entry) apply(ev event.Event) {
	m := ev.Envelope()

	// A replayed event is history being re-read, not something happening. It
	// goes on the bus so a consumer rebuilding a transcript sees it, but it must
	// not move the session's state or add to its cost: ACP's session/load
	// replays a whole conversation, and counting that would bill a reattach.
	if m.Replay {
		return
	}

	at := m.At
	if at.IsZero() {
		at = e.reg.now()
	}

	switch v := ev.(type) {
	case event.TurnStarted:
		e.mu.Lock()
		e.turn = m.Turn
		e.turnAt = at
		e.turnText.Reset()
		e.mu.Unlock()
		e.touch(at)
		e.setState(store.SessionRunning)

	case event.TextDelta:
		e.mu.Lock()
		if e.turnText.Len() < MaxTurnText {
			e.turnText.WriteString(v.Text)
		}
		e.mu.Unlock()
		e.touch(at)

	case event.ToolStarted:
		e.touch(at)
		_ = e.reg.db.PutToolCall(context.Background(), store.ToolCall{
			ID:         e.ID() + ":" + v.ID,
			SessionID:  e.ID(),
			TurnID:     m.Turn,
			Tool:       v.Tool,
			Target:     v.Target,
			ArgsDigest: digest(v.RawInput), // a digest, never the arguments
			At:         at,
		})

	case event.ToolOutput:
		e.touch(at)
		// Only the status is written, and only as an update. ACP's
		// tool_call_update may carry a toolCallId and nothing else, so an upsert
		// here would blank the tool name and target that the tool_call already
		// established — the adapter merges updates onto what it has, and so does
		// this. The chunk itself is not stored: it is transcript, and the
		// transcript stays where the runtime wrote it (MEMORY.md §3).
		if v.Status != event.ToolUnknown {
			e.updateToolStatus(v.ID, string(v.Status))
		}

	case *event.NeedsInput:
		e.addQuestion(v, at)

	case event.TurnCompleted:
		e.completeTurn(v, at)

	case event.Error:
		e.touch(at)
		if v.Fatal {
			e.reg.record(Incident{
				Runtime: m.Runtime, Session: e.ID(), Kind: IncidentSessionFailed,
				Message: v.Code + " " + v.Message, Fatal: true,
			})
			e.setState(store.SessionClosed)
		}

	default:
		e.touch(at)
	}
}

func (e *Entry) updateToolStatus(toolID, status string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.reg.db.SQL().ExecContext(ctx,
		`UPDATE tool_call SET result_status = ? WHERE id = ?`,
		status, e.ID()+":"+toolID); err != nil {
		e.reg.log.Error("registry: update tool status", "session", e.ID(), "error", err)
	}
}

func (e *Entry) touch(at time.Time) {
	e.mu.Lock()
	if at.After(e.row.LastActive) {
		e.row.LastActive = at
		e.dirty = true
	}
	e.mu.Unlock()
}

func (e *Entry) addQuestion(n *event.NeedsInput, at time.Time) {
	q := &question{id: e.reg.newID(), ask: n, at: at, kind: n.Ask}

	e.mu.Lock()
	e.asks = append(e.asks, q)
	e.mu.Unlock()

	e.touch(at)
	e.setState(store.SessionAwaiting)

	// The question resolves through the adapter, on some other goroutine, and
	// possibly not by us at all — Codex's serverRequest/resolved says an
	// approval was answered in a terminal. Either way the session stops being
	// blocked, and the registry has to notice without being told.
	go func() {
		select {
		case <-n.Done():
		case <-e.reg.ctx.Done():
			return
		}
		e.mu.Lock()
		for i, cand := range e.asks {
			if cand == q {
				e.asks = append(e.asks[:i], e.asks[i+1:]...)
				break
			}
		}
		remaining := len(e.asks)
		running := e.turn != ""
		closing := e.closing
		e.mu.Unlock()

		if remaining > 0 || closing {
			return
		}
		if running {
			e.setState(store.SessionRunning)
		} else {
			e.setState(store.SessionIdle)
		}
	}()
}

func (e *Entry) completeTurn(v event.TurnCompleted, at time.Time) {
	e.mu.Lock()
	turnID := v.Envelope().Turn
	if turnID == "" {
		turnID = e.turn
	}
	text := e.turnText.String()
	e.turnText.Reset()
	e.turn = ""
	if at.After(e.row.LastActive) {
		e.row.LastActive = at
	}
	// Cost and tokens accumulate. Every field is a pointer and nil means the
	// runtime does not report it (ADAPTERS.md §5): ACP carries no usage object
	// at all, so the console shows a gap rather than a free turn, and Codex
	// carries tokens with no dollar figure anywhere in its contract.
	var turnCost *float64
	var turnTokens *int64
	if v.Usage != nil {
		if v.Usage.CostUSD != nil {
			turnCost = v.Usage.CostUSD
			e.row.CostUSD = addF(e.row.CostUSD, *v.Usage.CostUSD)
		}
		if v.Usage.TotalTokens != nil {
			turnTokens = v.Usage.TotalTokens
			e.row.TokensTotal = addI(e.row.TokensTotal, *v.Usage.TotalTokens)
		}
		if v.Usage.InputTokens != nil {
			e.row.TokensInput = addI(e.row.TokensInput, *v.Usage.InputTokens)
		}
		if v.Usage.ContextWindow != nil {
			w := *v.Usage.ContextWindow
			e.row.ContextWindow = &w
		}
	}
	e.dirty = true
	id := e.row.ID
	e.mu.Unlock()

	if turnID == "" {
		turnID = e.reg.newID()
	}
	_ = e.reg.db.PutTurn(context.Background(), store.Turn{
		ID:         id + ":agent:" + turnID,
		SessionID:  id,
		Role:       "agent",
		Text:       truncate(text, MaxTurnText),
		At:         at,
		StopReason: string(v.StopReason),
		OK:         v.OK,
		Duration:   v.Duration,
		CostUSD:    turnCost,
		Tokens:     turnTokens,
	})

	e.mu.Lock()
	blocked := len(e.asks) > 0
	e.mu.Unlock()
	if blocked {
		// A turn can complete with an approval still outstanding on some other
		// turn. Awaiting outranks idle, because DASHBOARD.md §3.1 puts blocked
		// sessions at the top and a session that is still blocked must stay
		// there.
		e.setState(store.SessionAwaiting)
		return
	}
	e.setState(store.SessionIdle)
}

// ended runs when the session's event channel closes.
func (e *Entry) ended() {
	e.mu.Lock()
	e.finished = true
	closing := e.closing
	asks := append([]*question(nil), e.asks...)
	e.asks = nil
	e.mu.Unlock()

	// A session that dies with questions outstanding leaves pings that can never
	// be answered. Withdraw them so the notification is retracted rather than
	// waiting for an answer nobody can give.
	for _, q := range asks {
		q.ask.Withdraw("the session ended")
	}

	e.flush(context.Background())

	if closing {
		e.setState(store.SessionClosed)
		e.reg.forget(e.ID())
		return
	}

	// Unexpected. SYSTEM.md §6.2: this is the weakest seam in the system, so it
	// gets a name rather than a silent disappearance.
	e.reg.record(Incident{
		Runtime: e.Row().Runtime, Session: e.ID(), Kind: IncidentSessionExited,
		Message: "the runtime's event stream closed without being asked to",
	})
	e.setState(store.SessionClosed)
	e.reg.forget(e.ID())
	e.reg.supervise(e)
}

func (e *Entry) setState(s store.SessionState) {
	e.mu.Lock()
	if e.row.State == s {
		e.mu.Unlock()
		return
	}
	// Closed is terminal. A late event from a dying runtime must not resurrect a
	// session in the list.
	if e.row.State == store.SessionClosed {
		e.mu.Unlock()
		return
	}
	e.row.State = s
	e.dirty = false
	row := e.row
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.reg.db.PutSession(ctx, row); err != nil {
		e.reg.log.Error("registry: persist session state", "session", row.ID, "error", err)
	}
	kind := ChangeUpdated
	if s == store.SessionClosed {
		kind = ChangeClosed
	}
	e.reg.changes.Publish(Change{Kind: kind, Session: row, At: e.reg.now()})
}

// flush writes last_active back for a session that has been busy without
// changing state. An event stream runs at hundreds of events a turn and a write
// per event is pointless I/O.
func (e *Entry) flush(ctx context.Context) {
	e.mu.Lock()
	if !e.dirty {
		e.mu.Unlock()
		return
	}
	e.dirty = false
	row := e.row
	e.mu.Unlock()

	if err := e.reg.db.PutSession(ctx, row); err != nil && !errors.Is(err, context.Canceled) {
		e.reg.log.Error("registry: flush session", "session", row.ID, "error", err)
		return
	}
	e.reg.changes.Publish(Change{Kind: ChangeUpdated, Session: row, At: e.reg.now()})
}

func addF(p *float64, v float64) *float64 {
	var out float64
	if p != nil {
		out = *p
	}
	out += v
	return &out
}

func addI(p *int64, v int64) *int64 {
	var out int64
	if p != nil {
		out = *p
	}
	out += v
	return &out
}

// truncate cuts at n bytes without splitting a rune. A half-written character
// is not a smaller string, it is a broken one, and it travels through JSON into
// a console that renders it as a replacement glyph.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
