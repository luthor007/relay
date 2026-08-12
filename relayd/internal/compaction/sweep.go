package compaction

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/logx"
)

// SessionView is one session as the sweeper finds it. It is a value: nothing
// here holds a runtime handle, so a test builds one in four lines.
type SessionView struct {
	ID        string
	Runtime   adapter.Runtime
	Workspace string
	Model     string

	// LastActive is when the session last did anything a user caused. A silent
	// memory pass must not move it — see [IsFlush].
	LastActive time.Time
	InTurn     bool

	Reading Reading
	Signals Signals

	Compactions    int
	LastCompaction time.Time
	Flushes        int
	LastFlush      time.Time

	// CompactUnavailable is why this session cannot be compacted, empty when it
	// can. The sweeper fills in the mechanism half itself; a caller adds
	// anything it knows that this package cannot see, such as an adapter
	// reporting the capability absent for this session.
	CompactUnavailable string
}

// Sessions is where candidates come from. It is an interface so this package
// neither opens the database nor holds the registry.
type Sessions interface {
	// Candidates returns the sessions worth considering. Returning every live
	// session is fine; [Decide] answers ActionNone for almost all of them.
	Candidates(ctx context.Context) ([]SessionView, error)
}

// Actor carries out a decision. Each method is expected to be slow — a
// compaction is ten to sixty seconds — and to respect ctx.
type Actor interface {
	// Compact triggers the runtime's own compaction through its documented
	// mechanism ([MechanismFor]).
	Compact(ctx context.Context, v SessionView) error
	// Flush sends the silent memory pass ([FlushTurn]) and folds the reply back
	// in. It must not speak, ping, or move LastActive.
	Flush(ctx context.Context, v SessionView) error
	// Handoff starts a fresh session seeded with the brief and moves the work
	// to it.
	Handoff(ctx context.Context, v SessionView, b Brief) error
	// StartNew closes the topic out: the next utterance opens a new session
	// rather than continuing this one. It does not have to start anything now.
	StartNew(ctx context.Context, v SessionView) error
}

// Briefs builds the handoff brief for a session, from the index and the facts.
// It is separate from [Actor] because building a brief reads and acting writes,
// and because a deployment with no index yet can supply nil and get compaction
// with a visible reason instead of a handoff.
type Briefs interface {
	Brief(ctx context.Context, v SessionView) (Brief, error)
}

// SweeperOptions configures a [Sweeper].
type SweeperOptions struct {
	Sessions Sessions
	Actor    Actor
	Briefs   Briefs

	// HermesLease is the compression lease. Nil means we have no way to take it,
	// and Hermes sessions are then skipped rather than raced — MEMORY.md §12.5's
	// standing instruction until the lease semantics are read from upstream.
	HermesLease Lease
	LeaseTTL    time.Duration

	Policy Policy

	// MaxPerSweep bounds how many sessions are acted on in one pass. The default
	// is one, and it is a product decision rather than a resource one:
	// compaction is ten to sixty seconds during which that session cannot answer,
	// and doing five at once means every session the user might speak to is busy
	// at the same moment.
	MaxPerSweep int

	Now func() time.Time
	Log *slog.Logger
}

// Sweeper is the idle pass. It is driven by whoever owns the timer; it does not
// own a goroutine, and it does not own a clock beyond the one handed to it.
type Sweeper struct {
	o SweeperOptions
}

// NewSweeper builds one.
func NewSweeper(o SweeperOptions) (*Sweeper, error) {
	if o.Sessions == nil {
		return nil, errors.New("compaction: a sweeper needs somewhere to get sessions from")
	}
	if o.Actor == nil {
		return nil, errors.New("compaction: a sweeper needs an actor; deciding without acting is a report, not a sweep")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = logx.Discard()
	}
	if o.MaxPerSweep <= 0 {
		o.MaxPerSweep = 1
	}
	if o.LeaseTTL <= 0 {
		o.LeaseTTL = DefaultLeaseTTL
	}
	o.Policy = o.Policy.withDefaults()
	return &Sweeper{o: o}, nil
}

// Policy is the policy in force, after defaults.
func (s *Sweeper) Policy() Policy { return s.o.Policy }

// Outcome is what happened to one session in one sweep.
type Outcome struct {
	Session  string
	Runtime  adapter.Runtime
	Decision Decision
	// Done is true when the action was carried out.
	Done bool
	// Skipped says why an action that was decided did not happen — the lease was
	// held, the per-sweep budget was spent, there was nothing to carry.
	Skipped string
	// Degraded says what we did instead of what we decided, and why. It is
	// separate from Skipped because "handed off" and "compacted because the
	// handoff had nothing to carry" are different events with the same decision.
	Degraded string
	Err      error
}

// SweepResult is one pass.
type SweepResult struct {
	Considered int
	Acted      int
	Outcomes   []Outcome
}

// DecideFor turns a view into a state and decides, without acting. The console
// shows this; [Sweeper.Sweep] uses it.
func (s *Sweeper) DecideFor(v SessionView) Decision {
	return Decide(s.state(v), s.o.Policy)
}

func (s *Sweeper) state(v SessionView) State {
	now := s.o.Now()

	idle := now.Sub(v.LastActive)
	if idle < 0 || v.LastActive.IsZero() {
		idle = 0
	}

	st := State{
		Session:            v.ID,
		Runtime:            v.Runtime,
		Reading:            v.Reading,
		Signals:            v.Signals,
		Idle:               idle,
		InTurn:             v.InTurn,
		Compactions:        v.Compactions,
		Flushes:            v.Flushes,
		CompactUnavailable: v.CompactUnavailable,
	}
	if v.Compactions > 0 && !v.LastCompaction.IsZero() {
		st.SinceCompaction = now.Sub(v.LastCompaction)
	}
	if v.Flushes > 0 && !v.LastFlush.IsZero() {
		st.SinceFlush = now.Sub(v.LastFlush)
	}

	// On an idle sweep there is no utterance, so the silence *is* the gap. A
	// caller that already knows the gap — because a user just spoke — sets it
	// and this leaves it alone.
	if st.Signals.Gap == 0 {
		st.Signals.Gap = idle
	}

	if st.CompactUnavailable == "" {
		if m, ok := MechanismFor(v.Runtime); !ok {
			st.CompactUnavailable = "no documented way to compact " + label(v.Runtime)
		} else if m.RequiresLease && s.o.HermesLease == nil {
			st.CompactUnavailable = "no compression lease available, and racing Hermes for it is not an option"
		}
	}
	return st
}

// Sweep considers every candidate and acts on at most MaxPerSweep of them.
//
// It returns an error only when the candidate list itself could not be read. A
// session that failed to compact is an [Outcome] with an error on it, because
// one broken runtime must not stop the other four from being tidied.
func (s *Sweeper) Sweep(ctx context.Context) (SweepResult, error) {
	views, err := s.o.Sessions.Candidates(ctx)
	if err != nil {
		return SweepResult{}, err
	}

	res := SweepResult{Considered: len(views)}
	for _, v := range views {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		d := s.DecideFor(v)
		out := Outcome{Session: v.ID, Runtime: v.Runtime, Decision: d}
		if !d.Acts() {
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		if res.Acted >= s.o.MaxPerSweep {
			out.Skipped = "one session at a time; this waits for the next sweep"
			res.Outcomes = append(res.Outcomes, out)
			continue
		}

		s.apply(ctx, v, &out)
		if out.Done {
			res.Acted++
		}
		res.Outcomes = append(res.Outcomes, out)
	}
	return res, nil
}

func (s *Sweeper) apply(ctx context.Context, v SessionView, out *Outcome) {
	switch out.Decision.Action {
	case ActionFlush:
		out.Err = s.o.Actor.Flush(ctx, v)
	case ActionNew:
		out.Err = s.o.Actor.StartNew(ctx, v)
	case ActionCompact:
		s.compact(ctx, v, out)
	case ActionHandoff:
		s.handoff(ctx, v, out)
	default:
		out.Skipped = "no action"
		return
	}
	out.Done = out.Err == nil && out.Skipped == ""
	if out.Err != nil {
		s.o.Log.Warn("compaction: action failed",
			"session", v.ID, "runtime", v.Runtime, "action", out.Decision.Action, "err", out.Err)
	}
}

func (s *Sweeper) compact(ctx context.Context, v SessionView, out *Outcome) {
	m, ok := MechanismFor(v.Runtime)
	if !ok {
		out.Skipped = "no documented way to compact " + label(v.Runtime)
		return
	}

	lease := LeaseFor(m, s.o.HermesLease)
	if lease == nil {
		out.Skipped = "no compression lease available"
		return
	}
	got, err := lease.Acquire(ctx, v.ID, s.o.LeaseTTL)
	if err != nil {
		out.Err = err
		return
	}
	if !got {
		// Somebody else is compressing. This is an ordinary outcome and the
		// answer is to leave it alone until the next sweep.
		out.Skipped = "the compression lease is held; not racing it"
		return
	}
	defer func() {
		if err := lease.Release(context.WithoutCancel(ctx), v.ID); err != nil {
			s.o.Log.Warn("compaction: releasing the compression lease failed", "session", v.ID, "err", err)
		}
	}()

	out.Err = s.o.Actor.Compact(ctx, v)
	if out.Err == nil && !m.Observable {
		// ADAPTERS.md §5's rule applied to us: we asked, the call did not fail,
		// and that is all we know. Saying "compacted" would be claiming an
		// observation this runtime cannot give us.
		out.note("asked " + label(v.Runtime) + " to compact; " + m.Method +
			" reports no completion we can observe, so the next pressure reading is the only confirmation")
	}
}

// note appends a degradation reason rather than replacing one. A handoff that
// fell back to a compaction on an unobservable runtime degraded twice, and both
// halves are things the console should be able to say.
func (o *Outcome) note(s string) {
	if o.Degraded == "" {
		o.Degraded = s
		return
	}
	o.Degraded += "; " + s
}

func (s *Sweeper) handoff(ctx context.Context, v SessionView, out *Outcome) {
	if s.o.Briefs == nil {
		s.fallBackToCompact(ctx, v, out, "no brief builder configured")
		return
	}
	b, err := s.o.Briefs.Brief(ctx, v)
	switch {
	case errors.Is(err, ErrNothingToCarry):
		s.fallBackToCompact(ctx, v, out, "nothing observed to carry into a new session")
		return
	case err != nil:
		out.Err = err
		return
	case b.Empty():
		s.fallBackToCompact(ctx, v, out, "the brief came back empty")
		return
	}
	out.Err = s.o.Actor.Handoff(ctx, v, b)
}

// fallBackToCompact is the visible degrade: a handoff we cannot write becomes a
// compaction, which at least summarises something real, and the reason travels
// with the outcome instead of being logged and forgotten.
func (s *Sweeper) fallBackToCompact(ctx context.Context, v SessionView, out *Outcome, why string) {
	if v.CompactUnavailable != "" {
		out.Skipped = why + ", and " + v.CompactUnavailable
		return
	}
	out.note(why + "; compacting instead")
	s.compact(ctx, v, out)
	if out.Skipped != "" {
		out.Skipped = why + ", and " + out.Skipped
	}
}
