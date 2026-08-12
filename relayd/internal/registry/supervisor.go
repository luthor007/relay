package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/store"
)

// IncidentKind names a way the session layer went wrong. They are named rather
// than free text because SYSTEM.md §6.2 says the subprocess seam is the weakest
// in the system: "the mitigation is that each adapter is small, isolated per
// runtime, and covered by recorded-output fixtures so a format change fails a
// test rather than a user's morning". A failure the console can group and count
// is a failure somebody notices.
type IncidentKind string

const (
	// IncidentStartFailed is a session that never opened.
	IncidentStartFailed IncidentKind = "start_failed"

	// IncidentSessionExited is a session whose event stream closed without being
	// asked to: the process died, the socket dropped, the runtime crashed.
	IncidentSessionExited IncidentKind = "session_exited"

	// IncidentSessionFailed is a fatal Error event — the session is gone, not
	// just this turn.
	IncidentSessionFailed IncidentKind = "session_failed"

	// IncidentRestarted is a session brought back with its history intact,
	// through the runtime's own resume.
	IncidentRestarted IncidentKind = "restarted"

	// IncidentRestartedFresh is a session brought back WITHOUT its history,
	// because the runtime cannot reattach. Deliberately a different name: the
	// user is talking to something that has forgotten the conversation, and
	// calling that the same thing as a resume would be the silent failure this
	// package exists to avoid.
	IncidentRestartedFresh IncidentKind = "restarted_fresh"

	// IncidentRestartFailed is a session that could not be brought back.
	IncidentRestartFailed IncidentKind = "restart_failed"

	// IncidentOrphanDetached is a row that said running or awaiting at startup
	// with nothing driving it — relayd was restarted or killed under it.
	IncidentOrphanDetached IncidentKind = "orphan_detached"

	// IncidentReconcileMissing is a session in our table that the runtime's own
	// store no longer has.
	IncidentReconcileMissing IncidentKind = "reconcile_missing"

	// IncidentDegraded is a capability this session cannot report. It is not a
	// failure, it is the honest version of one (ADAPTERS.md §5).
	IncidentDegraded IncidentKind = "degraded"
)

// Incident is something that went wrong, or something that quietly is not
// working, in a form the console can list.
//
// Incidents are deliberately NOT published onto the event bus as event.Error.
// The bus carries what an adapter observed on a runtime's protocol; a session
// dying is something relayd observed about the adapter. Pushing it onto the same
// stream would put words in a runtime's mouth, which is exactly the rule
// ADAPTERS.md §5 sets — an adapter never emits an event it cannot observe.
type Incident struct {
	ID      string       `json:"id"`
	At      time.Time    `json:"at"`
	Kind    IncidentKind `json:"kind"`
	Runtime string       `json:"runtime,omitempty"`
	Session string       `json:"session,omitempty"`
	Message string       `json:"message"`
	Fatal   bool         `json:"fatal,omitempty"`
}

// RestartMode is what happens to a session that ends without being asked to.
type RestartMode string

const (
	// RestartNever leaves a dead session dead. The incident is still recorded.
	RestartNever RestartMode = "never"

	// RestartOnFailure brings a session back through the runtime's own resume.
	// It is the default, and it deliberately does NOT start a fresh session when
	// the runtime cannot reattach — see RestartPolicy.FreshOnResumeUnsupported.
	RestartOnFailure RestartMode = "on_failure"
)

// RestartPolicy is how hard relayd tries to bring a session back.
type RestartPolicy struct {
	Mode RestartMode

	// MaxAttempts caps the retries per session. Default 3.
	MaxAttempts int

	// Backoff is the wait before the first retry; it doubles. Default 2s.
	Backoff time.Duration

	// FreshOnResumeUnsupported starts a NEW session when the runtime cannot
	// reattach to the old one. Off by default, and that default is the point.
	//
	// ACP's agentCapabilities.loadSession is SupportUnknown on all three ACP
	// runtimes (ADAPTERS.md §8), so "restart" there means a session with no
	// memory of the conversation wearing the same name. Silently substituting
	// one for the other is the failure mode where a user keeps talking to
	// something that has forgotten everything and cannot tell. With this off,
	// the session is closed and an incident says the runtime cannot reattach.
	FreshOnResumeUnsupported bool
}

func (p RestartPolicy) withDefaults() RestartPolicy {
	if p.Mode == "" {
		p.Mode = RestartOnFailure
	}
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 3
	}
	if p.Backoff <= 0 {
		p.Backoff = 2 * time.Second
	}
	return p
}

// supervise decides what happens after a session ended unexpectedly.
func (r *Registry) supervise(e *Entry) {
	if r.restart.Mode == RestartNever {
		return
	}

	r.mu.RLock()
	closed := r.closed
	r.mu.RUnlock()
	if closed {
		return
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.restartSession(e)
	}()
}

func (r *Registry) restartSession(old *Entry) {
	row := old.Row()
	rt := adapter.Runtime(row.Runtime)

	a, ok := r.Adapter(rt)
	if !ok {
		r.record(Incident{
			Runtime: row.Runtime, Session: row.ID, Kind: IncidentRestartFailed,
			Message: "no adapter is registered for this runtime, so the session cannot be brought back",
			Fatal:   true,
		})
		return
	}

	old.mu.Lock()
	attempts := old.restarts
	opts := old.opts
	caps := old.caps
	old.mu.Unlock()

	backoff := r.restart.Backoff
	for attempt := attempts + 1; attempt <= r.restart.MaxAttempts; attempt++ {
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2

		ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
		sess, fresh, err := r.reopen(ctx, a, caps, row, opts)
		cancel()
		if err != nil {
			r.record(Incident{
				Runtime: row.Runtime, Session: row.ID, Kind: IncidentRestartFailed,
				Message: fmt.Sprintf("attempt %d of %d: %v", attempt, r.restart.MaxAttempts, err),
				Fatal:   attempt == r.restart.MaxAttempts || errors.Is(err, adapter.ErrUnsupported),
			})
			if errors.Is(err, adapter.ErrUnsupported) || errors.Is(err, adapter.ErrAuthRequired) {
				// Retrying will not fix either of those.
				return
			}
			continue
		}

		opts.ID = sess.ID()
		e, err := r.Adopt(r.ctx, sess, opts)
		if err != nil {
			r.record(Incident{
				Runtime: row.Runtime, Session: row.ID, Kind: IncidentRestartFailed,
				Message: err.Error(), Fatal: true,
			})
			_ = sess.Close(context.Background())
			return
		}
		e.mu.Lock()
		e.restarts = attempt
		// Carry the accumulated cost forward: the conversation continued even if
		// the process did not.
		e.row.CostUSD, e.row.TokensTotal = row.CostUSD, row.TokensTotal
		e.row.Subject, e.row.Entities = row.Subject, row.Entities
		e.mu.Unlock()

		kind := IncidentRestarted
		msg := "brought back through the runtime's own resume, history intact"
		if fresh {
			kind = IncidentRestartedFresh
			msg = "this runtime cannot reattach, so this is a NEW session with no memory of the conversation"
		}
		r.record(Incident{
			Runtime: row.Runtime, Session: e.ID(), Kind: kind,
			Message: fmt.Sprintf("%s (was %s, attempt %d)", msg, row.ID, attempt),
		})
		return
	}
}

func (r *Registry) reopen(
	ctx context.Context,
	a adapter.Adapter,
	caps adapter.Capabilities,
	row store.Session,
	opts StartOptions,
) (adapter.Session, bool, error) {
	if caps.Get(adapter.CapResume) == adapter.SupportYes {
		ref := adapter.SessionRef{
			Runtime:   adapter.Runtime(row.Runtime),
			ID:        row.ID,
			Native:    row.NativeID,
			Workspace: row.Workspace,
		}
		sess, err := a.Resume(ctx, ref, opts.sessionOptions(row.ID))
		if err == nil {
			return sess, false, nil
		}
		if !errors.Is(err, adapter.ErrUnsupported) && !errors.Is(err, adapter.ErrSessionNotFound) {
			return nil, false, err
		}
	}

	if !r.restart.FreshOnResumeUnsupported {
		return nil, false, fmt.Errorf(
			"%w: %s cannot reattach to a session (%s), and starting a fresh one would silently drop the conversation",
			adapter.ErrUnsupported, row.Runtime, caps.Get(adapter.CapResume))
	}

	sess, err := a.Start(ctx, opts.sessionOptions(""))
	if err != nil {
		return nil, false, err
	}
	return sess, true, nil
}

// RecoverResult is what a restart of relayd found waiting for it.
type RecoverResult struct {
	// Detached are rows that claimed to be running or awaiting with nothing
	// driving them. They are moved to idle: the session may well still exist in
	// the runtime's own store and be resumable, but relayd is not driving it and
	// must not claim otherwise.
	Detached []string `json:"detached"`
}

// Recover reconciles the table with the fact that this process just started.
//
// A row saying "running" after a restart is a lie with a plausible shape, and
// DASHBOARD.md §3.1 puts blocked sessions at the top of the list — so a stale
// "awaiting" row is worse than merely wrong, it is the thing the user is
// supposed to act on first. Both become idle, each with an incident.
func (r *Registry) Recover(ctx context.Context) (RecoverResult, error) {
	var res RecoverResult
	for _, state := range []store.SessionState{store.SessionRunning, store.SessionAwaiting} {
		rows, err := r.db.ListSessions(ctx, store.SessionFilter{State: state})
		if err != nil {
			return res, err
		}
		for _, row := range rows {
			if _, live := r.Get(row.ID); live {
				continue
			}
			row.State = store.SessionIdle
			if err := r.db.PutSession(ctx, row); err != nil {
				return res, err
			}
			res.Detached = append(res.Detached, row.ID)
			r.record(Incident{
				Runtime: row.Runtime, Session: row.ID, Kind: IncidentOrphanDetached,
				Message: fmt.Sprintf("was %s when relayd stopped; nothing is driving it now", state),
			})
			r.changes.Publish(Change{Kind: ChangeUpdated, Session: row, At: r.now()})
		}
	}
	return res, nil
}

// ReapOrphans drops live entries whose session has already ended.
//
// The pump normally does this itself when the event channel closes. This is the
// belt for the braces: an adapter that closes a session without closing its
// channel would otherwise leave the registry claiming a session that is gone,
// and that is precisely the class of bug SYSTEM.md §6.2 warns about.
func (r *Registry) ReapOrphans(ctx context.Context) []string {
	var reaped []string
	for _, e := range r.Live() {
		select {
		case <-e.Done():
			r.forget(e.ID())
			reaped = append(reaped, e.ID())
			r.record(Incident{
				Runtime: e.Row().Runtime, Session: e.ID(), Kind: IncidentOrphanDetached,
				Message: "the session had already ended but was still listed as live",
			})
		default:
		}
	}
	return reaped
}
