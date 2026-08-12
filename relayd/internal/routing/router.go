package routing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/logx"
)

// Errors this package returns.
var (
	// ErrNoDriver is Undo or a redirect with nothing to drive.
	ErrNoDriver = errors.New("routing: no driver is wired, so nothing can be moved")
	// ErrNothingToUndo is Undo with an empty journal, or with a journal whose
	// last entry was a control verb rather than a routed turn.
	ErrNothingToUndo = errors.New("routing: there is no routed turn to move")
	// ErrNoSuchSession is a reference that resolved to nothing.
	ErrNoSuchSession = errors.New("routing: no live session by that name")
)

// DefaultJournal is how many decisions the router remembers. Undo only ever
// needs the last one; the rest is what the console shows when someone asks why
// their utterance went where it went.
const DefaultJournal = 32

// Options configures a [Router].
type Options struct {
	// Sessions is the live list. Required.
	Sessions Sessions

	// Runtime answers question 2. Optional: without it, KindNew decisions carry
	// no runtime and the caller picks. With it, every new session is announced
	// with the runtime and the reason.
	Runtime *RuntimeRouter

	// Driver moves turns for Undo. Optional; Undo returns ErrNoDriver without
	// one, rather than reporting a move that did not happen.
	Driver Driver

	// Auto turns the automatic session router on. **Off by default**, which is
	// ORCHESTRATOR.md §4's ordering rather than an oversight: the manual path
	// plus the announcement is the shipping default, and the scorer is opt-in
	// until there is real usage to tune it against.
	Auto bool

	// Scoring are the weights the scorer uses. Zero value means the defaults.
	Scoring Scoring

	// TieBreak is the LLM tie-break, consulted only when Auto is on and two
	// candidates are within Scoring.Margin of each other. Nil skips it, which
	// turns a tie into an ask — the safe direction.
	TieBreak TieBreaker

	// Journal is how many decisions to remember. Default DefaultJournal.
	Journal int

	Now func() time.Time
	Log *slog.Logger
}

// Router is the whole of question 1, plus the wiring to question 2.
type Router struct {
	sessions Sessions
	runtime  *RuntimeRouter
	driver   Driver
	auto     bool
	scoring  Scoring
	tie      TieBreaker
	now      func() time.Time
	log      *slog.Logger

	mu      sync.Mutex
	focus   string
	journal []JournalEntry
	cap     int
}

// New builds a router.
func New(o Options) (*Router, error) {
	if o.Sessions == nil {
		return nil, fmt.Errorf("routing: a router needs a session list")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = logx.Discard()
	}
	if o.Journal <= 0 {
		o.Journal = DefaultJournal
	}
	return &Router{
		sessions: o.Sessions,
		runtime:  o.Runtime,
		driver:   o.Driver,
		auto:     o.Auto,
		scoring:  o.Scoring.withDefaults(),
		tie:      o.TieBreak,
		now:      o.Now,
		log:      o.Log,
		cap:      o.Journal,
	}, nil
}

// Auto reports whether the automatic session router is on.
func (r *Router) Auto() bool { return r.auto }

// RoutesRuntimes reports whether question 2 can be answered at all — that is,
// whether a [RuntimeRouter] was wired. Without one, KindNew decisions carry no
// runtime unless the user named one out loud.
func (r *Router) RoutesRuntimes() bool { return r.runtime != nil }

// Entitlements is what the *constructed* router will actually consult.
//
// It reads through the RuntimeRouter rather than remembering what a caller
// passed in, and that is the whole point of the method: a health screen built
// from a local variable in main() would keep claiming the entitlement after the
// join that carries it into the router was deleted. This cannot. Nil means
// either no runtime router or nothing recorded, which the caller has to tell
// apart with [Router.RoutesRuntimes] — they are different states and only one
// of them is the user's choice.
func (r *Router) Entitlements() Entitlements {
	if r.runtime == nil {
		return nil
	}
	return r.runtime.Entitlements()
}

// Focus is the session the conversation is currently in.
func (r *Router) Focus() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.focus
}

// SetFocus moves the conversation. The console's "talk to this one" button and
// the router's own decisions both land here.
func (r *Router) SetFocus(id string) {
	r.mu.Lock()
	r.focus = id
	r.mu.Unlock()
}

// Route decides where an utterance goes.
//
// It never sends anything. The caller acts on the decision, and calls
// [Router.Confirm] with the session and turn it actually produced — which is
// the only way the router can know the id of a session it asked for but did not
// create.
func (r *Router) Route(ctx context.Context, req Request) (Decision, error) {
	if req.At.IsZero() {
		req.At = r.now()
	}
	live, err := r.sessions.Live(ctx)
	if err != nil {
		return Decision{}, fmt.Errorf("routing: read the session list: %w", err)
	}

	cmd := ParseCommand(req.Text)
	var d Decision
	switch cmd.Kind {
	case CmdNone:
		d = r.routeUtterance(ctx, req, live)
	default:
		d = r.routeCommand(ctx, req, cmd, live)
	}

	d.Announcement = Announce(d)
	r.remember(req, d)
	return d, nil
}

// ------------------------------------------------------- the manual path --

func (r *Router) routeCommand(ctx context.Context, req Request, cmd Command, live []SessionView) Decision {
	d := Decision{Command: &cmd, Reason: ReasonExplicit, Text: cmd.Rest, Confidence: req.Confidence}

	switch cmd.Kind {
	case CmdNewSession:
		d.Kind = KindNew
		d.Subject = cmd.Subject
		d.Workspace = req.Workspace
		d.Because = "you asked for a new session"
		d.RuntimeChoice = r.chooseRuntime(ctx, req, cmd, live)
		if d.RuntimeChoice != nil {
			d.Runtime = d.RuntimeChoice.Runtime
			if !d.RuntimeChoice.Chosen() {
				d.Kind = KindAsk
				d.Question = d.RuntimeChoice.Ask
				d.Reason = ReasonAmbiguous
				d.Because = d.RuntimeChoice.Because
			}
		}
		return d

	case CmdSwitch:
		match, cands, err := r.resolve(cmd.Ref, live)
		if err != nil {
			d.Kind = KindAsk
			d.Reason = ReasonAmbiguous
			d.Candidates = cands
			d.Question = UnknownRefLine(cmd.Ref, cands)
			d.Because = "nothing running matches " + cmd.Ref
			return d
		}
		// A misheard session name is a wrong continue with extra steps, so a
		// low-confidence reference is confirmed rather than acted on.
		if req.Confidence > 0 && req.Confidence < MinSwitchConfidence {
			d.Kind = KindAsk
			d.Reason = ReasonAmbiguous
			d.Candidates = cands
			d.Question = "Did you mean " + match.Name() + "?"
			d.Because = "I am not sure I heard the session name"
			return d
		}
		d.Kind = KindContinue
		d.Session = match.ID
		d.Subject = match.Name()
		d.Runtime = match.Runtime
		d.Candidates = cands
		d.Because = "you asked for " + cmd.Ref
		return d

	case CmdUndo:
		d.Kind = KindControl
		d.Reason = ReasonControl
		d.Because = "you asked to undo"
		if cmd.Ref != "" {
			if match, _, err := r.resolve(cmd.Ref, live); err == nil {
				d.Session = match.ID
				d.Subject = match.Name()
				d.Runtime = match.Runtime
			}
		}
		return d

	case CmdStop, CmdStatus:
		d.Kind = KindControl
		d.Reason = ReasonControl
		target, ok := r.target(live)
		if ok {
			d.Session = target.ID
			d.Subject = target.Name()
			d.Runtime = target.Runtime
		}
		d.Because = "a control verb"
		return d

	case CmdAnswer:
		d.Kind = KindControl
		d.Reason = ReasonControl
		d.Because = "an answer to a blocked session"
		// An answer belongs to whichever session is actually blocked. If none
		// is, this was not an answer at all — it was the word "yes" in a
		// sentence, and it goes to the normal path.
		if s, ok := firstAwaiting(live); ok {
			d.Session = s.ID
			d.Subject = s.Name()
			d.Runtime = s.Runtime
			return d
		}
		return r.routeUtterance(ctx, req, live)

	case CmdList:
		d.Kind = KindControl
		d.Reason = ReasonControl
		d.Because = "a control verb"
		return d
	}

	return r.routeUtterance(ctx, req, live)
}

// routeUtterance is everything that is not a command.
func (r *Router) routeUtterance(ctx context.Context, req Request, live []SessionView) Decision {
	if r.auto {
		if d, ok := r.routeAuto(ctx, req, live); ok {
			return d
		}
	}
	return r.routeManual(ctx, req, live)
}

// routeManual is the shipping default.
//
// The model is a terminal window, not a classifier: there is a session the
// conversation is in, and utterances go there until the user says otherwise.
// That is what makes the manual path usable rather than an interrogation — and
// it is still announced every time, so a wrong assumption is caught in one
// clause instead of discovered in a poisoned context.
func (r *Router) routeManual(ctx context.Context, req Request, live []SessionView) Decision {
	d := Decision{Text: req.Text, Confidence: req.Confidence, Workspace: req.Workspace}

	if focus, ok := r.focused(live); ok {
		d.Kind = KindContinue
		d.Session = focus.ID
		d.Subject = focus.Name()
		d.Runtime = focus.Runtime
		d.Reason = ReasonFocus
		d.Because = "it is the session you are in"
		return d
	}

	switch len(live) {
	case 0:
		d.Kind = KindNew
		d.Reason = ReasonNothingLive
		d.Because = "nothing is running"
		d.RuntimeChoice = r.chooseRuntime(ctx, req, Command{}, live)
		if d.RuntimeChoice != nil {
			d.Runtime = d.RuntimeChoice.Runtime
			if !d.RuntimeChoice.Chosen() {
				d.Kind = KindAsk
				d.Question = d.RuntimeChoice.Ask
				d.Reason = ReasonAmbiguous
				d.Because = d.RuntimeChoice.Because
			}
		}
		return d

	case 1:
		d.Kind = KindContinue
		d.Session = live[0].ID
		d.Subject = live[0].Name()
		d.Runtime = live[0].Runtime
		d.Reason = ReasonOnlyLive
		d.Because = "it is the only session running"
		return d

	default:
		cands := make([]Candidate, 0, len(live))
		for _, s := range live {
			cands = append(cands, Candidate{Session: s, Score: 0, Why: "running"})
		}
		sortCandidates(cands)
		d.Kind = KindAsk
		d.Reason = ReasonAmbiguous
		d.Candidates = cands
		d.Question = AskLine(cands)
		d.Because = "more than one session is running and you have not said which"
		return d
	}
}

// focused returns the current focus if it is still live.
func (r *Router) focused(live []SessionView) (SessionView, bool) {
	id := r.Focus()
	if id == "" {
		return SessionView{}, false
	}
	for _, s := range live {
		if s.ID == id {
			return s, true
		}
	}
	return SessionView{}, false
}

// target is what a bare control verb applies to: the focus if it is live, else
// the only live session, else nothing — "stop" with three sessions running and
// no focus is a question, not a guess.
func (r *Router) target(live []SessionView) (SessionView, bool) {
	if s, ok := r.focused(live); ok {
		return s, true
	}
	var busy []SessionView
	for _, s := range live {
		if s.Busy() {
			busy = append(busy, s)
		}
	}
	if len(busy) == 1 {
		return busy[0], true
	}
	if len(live) == 1 {
		return live[0], true
	}
	return SessionView{}, false
}

func firstAwaiting(live []SessionView) (SessionView, bool) {
	for _, s := range live {
		if s.Awaiting() {
			return s, true
		}
	}
	return SessionView{}, false
}

// resolve matches a spoken session reference against the live list.
func (r *Router) resolve(ref string, live []SessionView) (SessionView, []Candidate, error) {
	want := tokens(ref)
	cands := make([]Candidate, 0, len(live))
	for _, s := range live {
		score := overlap(want, s.terms())
		why := "name match"
		if score == 0 {
			why = "running"
		}
		cands = append(cands, Candidate{Session: s, Score: score, Why: why})
	}
	sortCandidates(cands)

	var matched []Candidate
	for _, c := range cands {
		if c.Score > 0 {
			matched = append(matched, c)
		}
	}
	switch {
	case len(matched) == 0:
		return SessionView{}, cands, ErrNoSuchSession
	case len(matched) == 1:
		return matched[0].Session, cands, nil
	case matched[0].Score > matched[1].Score:
		return matched[0].Session, cands, nil
	default:
		// Two sessions match the name equally well. Asking is correct; picking
		// the more recent one is exactly the silent 80% router the doc warns
		// about.
		return SessionView{}, matched, ErrNoSuchSession
	}
}

func (r *Router) chooseRuntime(ctx context.Context, req Request, cmd Command, live []SessionView) *RuntimeChoice {
	if r.runtime == nil {
		if cmd.Runtime != "" {
			return &RuntimeChoice{
				Runtime: cmd.Runtime,
				Reason:  RuntimeExplicit,
				Because: "you asked for it by name",
			}
		}
		return nil
	}
	rr := RuntimeRequest{
		Text:      req.Text,
		Workspace: req.Workspace,
		Runtime:   cmd.Runtime,
		Family:    FamilyOf(req.Text),
	}
	if s, ok := sameWorkspace(live, req.Workspace); ok && cmd.Runtime == "" {
		rr.Continuity = &s
	}
	c, err := r.runtime.Choose(ctx, rr)
	if err != nil {
		r.log.Warn("routing: runtime choice failed", "err", err)
		return nil
	}
	return &c
}

// sameWorkspace finds a live session already working in this directory, which
// is MEMORY.md §8's continuity signal at its cheapest.
func sameWorkspace(live []SessionView, workspace string) (SessionView, bool) {
	if strings.TrimSpace(workspace) == "" {
		return SessionView{}, false
	}
	for _, s := range live {
		if s.Workspace != "" && s.Workspace == workspace {
			return s, true
		}
	}
	return SessionView{}, false
}

// ---------------------------------------------------------------- journal --

// JournalEntry is one decision, and what became of it.
type JournalEntry struct {
	At       time.Time
	Text     string
	Decision Decision
	// Session is where the turn actually went, which is only known after the
	// caller acted: a KindNew decision has no session id until the session
	// exists.
	Session string
	Turn    string
	// Confirmed marks an entry the caller has acted on. An unconfirmed entry is
	// a decision that was announced and then never carried out — worth showing
	// in the console, and not something undo can move.
	Confirmed bool
	// Undone marks an entry Undo has already moved. Undoing twice would move
	// the same turn to a third place, which is not what "undo" means.
	Undone  bool
	MovedTo string
}

// Confirm records what the caller actually did with a decision.
//
// It is what makes undo possible, and it is also what makes the focus sticky:
// a session the user was routed to becomes the session the conversation is in.
func (r *Router) Confirm(d Decision, sessionID, turnID string) {
	if sessionID != "" {
		r.SetFocus(sessionID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// The most recent unconfirmed entry of the same kind. A KindNew decision
	// has no session id until the caller has created one, so matching on the
	// decision's own session would never find it — which is exactly the entry
	// undo needs most.
	for i := len(r.journal) - 1; i >= 0; i-- {
		e := &r.journal[i]
		if e.Confirmed || e.Decision.Kind != d.Kind {
			continue
		}
		if d.Text != "" && e.Text != d.Text {
			continue
		}
		if sessionID != "" {
			e.Session = sessionID
		}
		e.Turn = turnID
		e.Confirmed = true
		return
	}
}

func (r *Router) remember(req Request, d Decision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := JournalEntry{At: req.At, Text: d.Text, Decision: d, Session: d.Session}
	if e.Text == "" {
		e.Text = req.Text
	}
	r.journal = append(r.journal, e)
	if len(r.journal) > r.cap {
		r.journal = r.journal[len(r.journal)-r.cap:]
	}
}

// Journal returns the recent decisions, oldest first.
func (r *Router) Journal() []JournalEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]JournalEntry(nil), r.journal...)
}

// Last returns the most recent routed turn — the one Undo would move.
func (r *Router) Last() (JournalEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.journal) - 1; i >= 0; i-- {
		e := r.journal[i]
		if e.Decision.Undoable() && e.Confirmed && !e.Undone {
			return e, true
		}
	}
	return JournalEntry{}, false
}

// ------------------------------------------------------------------- undo --

// Driver is the slice of the registry Undo needs. It exists so this package
// decides and announces without owning process lifecycle, and so undo is
// testable without a runtime.
type Driver interface {
	// Send pushes a turn into an existing session.
	Send(ctx context.Context, sessionID, text string) (turnID string, err error)
	// Start opens a session and returns its id.
	Start(ctx context.Context, spec NewSession) (sessionID string, err error)
	// Cancel stops a turn. A runtime that cannot cancel returns an error
	// matching adapter.ErrUnsupported, and Undo reports that rather than
	// claiming the turn was pulled back.
	Cancel(ctx context.Context, sessionID, turnID string) error
}

// NewSession is what Undo needs to open a session somewhere else.
type NewSession struct {
	Runtime   adapter.Runtime
	Subject   string
	Workspace string
}

// UndoTarget says where the turn should go instead.
type UndoTarget struct {
	// Session is an existing session id.
	Session string
	// Ref is a spoken reference, resolved against the live list.
	Ref string
	// New starts a session instead.
	New     bool
	Runtime adapter.Runtime
	Subject string
}

// UndoResult is what actually happened.
type UndoResult struct {
	From string
	To   string
	// NewSession is true when the turn moved into a session Undo created.
	NewSession bool
	Text       string
	Turn       string

	// Cancelled says whether the turn in the wrong session was stopped.
	// CancelNote says why not — usually that the runtime cannot cancel, which
	// is a fact about the runtime and has to reach the user rather than being
	// swallowed.
	Cancelled  bool
	CancelNote string

	Announcement string
}

// Undo moves the last routed turn to a different session.
//
// This is ORCHESTRATOR.md §4's third guardrail, and it is the one that makes
// the other two safe to rely on: an announcement the user disagrees with is
// only useful if disagreeing does something. The turn is re-sent to the new
// destination and the old one is cancelled where the runtime allows it — and
// where it does not, the result says so instead of implying a rollback that did
// not happen.
func (r *Router) Undo(ctx context.Context, to UndoTarget) (UndoResult, error) {
	if r.driver == nil {
		return UndoResult{}, ErrNoDriver
	}
	last, ok := r.Last()
	if !ok {
		return UndoResult{}, ErrNothingToUndo
	}
	if last.Session == "" && !to.New {
		// The decision was never confirmed, so there is no turn on record to
		// move. Saying so beats moving something we are guessing at.
		return UndoResult{}, ErrNothingToUndo
	}

	live, err := r.sessions.Live(ctx)
	if err != nil {
		return UndoResult{}, fmt.Errorf("routing: read the session list: %w", err)
	}

	res := UndoResult{From: last.Session, Text: last.Text}

	// 1. Stop the wrong turn first. Doing this after the re-send would leave
	//    both sessions working on the same instruction.
	if last.Session != "" && last.Turn != "" {
		switch err := r.driver.Cancel(ctx, last.Session, last.Turn); {
		case err == nil:
			res.Cancelled = true
		case errors.Is(err, adapter.ErrUnsupported):
			res.CancelNote = "that runtime cannot stop a turn once it has started, so it will finish this one"
		default:
			res.CancelNote = err.Error()
		}
	}

	// 2. Work out where it is going.
	target := to.Session
	if target == "" && to.Ref != "" {
		match, _, err := r.resolve(to.Ref, live)
		if err != nil {
			return res, fmt.Errorf("%w: %s", ErrNoSuchSession, to.Ref)
		}
		target = match.ID
	}
	if target == "" && !to.New {
		return res, fmt.Errorf("%w: undo needs somewhere to move it to", ErrNoSuchSession)
	}

	if to.New {
		rt := to.Runtime
		if rt == "" {
			rt = last.Decision.Runtime
		}
		id, err := r.driver.Start(ctx, NewSession{
			Runtime:   rt,
			Subject:   to.Subject,
			Workspace: last.Decision.Workspace,
		})
		if err != nil {
			return res, fmt.Errorf("routing: undo could not start a session: %w", err)
		}
		target, res.NewSession = id, true
	}

	turn, err := r.driver.Send(ctx, target, last.Text)
	if err != nil {
		return res, fmt.Errorf("routing: undo could not move the turn: %w", err)
	}
	res.To, res.Turn = target, turn

	r.mu.Lock()
	for i := len(r.journal) - 1; i >= 0; i-- {
		if r.journal[i].At.Equal(last.At) && r.journal[i].Text == last.Text {
			r.journal[i].Undone = true
			r.journal[i].MovedTo = target
			break
		}
	}
	r.mu.Unlock()
	r.SetFocus(target)

	res.Announcement = undoLine(res, nameOf(live, target))
	return res, nil
}

func undoLine(res UndoResult, name string) string {
	var b strings.Builder
	if name == "" {
		name = "a new session"
	}
	b.WriteString("Moved that to ")
	b.WriteString(name)
	b.WriteString(".")
	if res.CancelNote != "" {
		b.WriteString(" ")
		b.WriteString(strings.ToUpper(res.CancelNote[:1]) + res.CancelNote[1:])
		b.WriteString(".")
	}
	return b.String()
}

func nameOf(live []SessionView, id string) string {
	for _, s := range live {
		if s.ID == id {
			return s.Name()
		}
	}
	return ""
}
