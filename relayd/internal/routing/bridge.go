package routing

import (
	"context"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/registry"
)

// FromRegistry adapts the session registry to [Sessions].
//
// It reads only what the registry is already driving. A session in the table
// that relayd is not driving — from before a restart, or reconciled out of a
// runtime's own store — is history rather than a routing destination: sending a
// turn to it would mean resuming it first, and resuming is per-runtime and
// unverified on three of the five (ADAPTERS.md §8). Routing to a session we
// cannot actually reach would be exactly the wrong-continue failure with an
// extra step in it.
func FromRegistry(reg *registry.Registry) Sessions {
	return SessionsFunc(func(ctx context.Context) ([]SessionView, error) {
		var out []SessionView
		for _, e := range reg.Live() {
			row := e.Row()
			v := SessionView{
				ID:           row.ID,
				Runtime:      adapter.Runtime(row.Runtime),
				Subject:      row.Subject,
				Workspace:    row.Workspace,
				Entities:     row.Entities,
				LastActive:   row.LastActive,
				State:        row.State,
				Turn:         e.Turn(),
				Capabilities: e.Capabilities(),
			}
			if files, err := recentFiles(ctx, reg, row.ID); err == nil {
				v.Files = files
			}
			out = append(out, v)
		}
		return out, nil
	})
}

// maxRoutingFiles is how many of a session's touched files the scorer looks at.
// The signal is "did this session go anywhere near what you just said", and the
// last handful answers that as well as the last thousand.
const maxRoutingFiles = 24

func recentFiles(ctx context.Context, reg *registry.Registry, session string) ([]string, error) {
	d, err := reg.Detail(ctx, session)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for i := len(d.Tools) - 1; i >= 0 && len(out) < maxRoutingFiles; i-- {
		t := d.Tools[i].Target
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out, nil
}

// RegistryDriver adapts the registry to [Driver], which is what makes undo move
// a real turn.
func RegistryDriver(reg *registry.Registry) Driver { return regDriver{reg: reg} }

type regDriver struct{ reg *registry.Registry }

func (d regDriver) Send(ctx context.Context, sessionID, text string) (string, error) {
	return d.reg.Send(ctx, sessionID, adapter.Turn{Text: text})
}

func (d regDriver) Start(ctx context.Context, spec NewSession) (string, error) {
	e, err := d.reg.Start(ctx, registry.StartOptions{
		Runtime:   spec.Runtime,
		Subject:   spec.Subject,
		Workspace: spec.Workspace,
	})
	if err != nil {
		return "", err
	}
	return e.ID(), nil
}

func (d regDriver) Cancel(ctx context.Context, sessionID, turnID string) error {
	return d.reg.Cancel(ctx, sessionID, turnID)
}

// FromDetect turns a detection report into runtime profiles.
//
// The mapping of detect.Status onto [History] is the whole point of this
// function and it is deliberately conservative:
//
//	in_use        → HistorySome    binary and history both present
//	history_only  → HistorySome    the history is still ours to route to
//	never_run     → HistoryNone    installed, never opened — the two runtimes
//	                               MEMORY.md §1 found on a real machine
//	absent        → HistoryNone    nothing to have a history in
//
// A finding whose session count was never taken and whose store size is unknown
// comes back HistoryUnknown, which the router treats as "do not route here".
// That is the same refusal detect itself makes: a store nobody opened reported
// as empty is the worst failure available, because it looks like a clean
// install.
func FromDetect(rep detect.Report, attached map[adapter.Runtime]bool) []RuntimeProfile {
	out := make([]RuntimeProfile, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		p := RuntimeProfile{
			Runtime:      f.Runtime,
			Installed:    f.Installed,
			Attached:     attached[f.Runtime],
			Sessions:     f.Sessions,
			Capabilities: adapter.Baseline(f.Runtime),
			History:      historyOf(f),
		}
		if len(f.Running) > 0 {
			// A process of its own is not a turn we are driving, so it does not
			// set Busy. It does say the runtime is in use by a human right now,
			// which the console shows and the router does not act on.
			p.LastUsed = rep.At
		}
		out = append(out, p)
	}
	return out
}

func historyOf(f detect.Finding) History {
	switch f.Status() {
	case detect.StatusInUse, detect.StatusHistoryOnly:
		return HistorySome
	case detect.StatusNeverRun:
		if f.Sessions == nil && f.StoreBytes == nil {
			// Installed, and nobody looked inside. Not the same as empty.
			return HistoryUnknown
		}
		return HistoryNone
	case detect.StatusAbsent:
		return HistoryNone
	}
	return HistoryUnknown
}

// LiveLoad folds the registry's live sessions into a set of profiles, so the
// load step of MEMORY.md §8 reads what relayd is actually driving rather than
// what the machine looked like at install time.
func LiveLoad(ps []RuntimeProfile, live []SessionView) []RuntimeProfile {
	busy := map[adapter.Runtime]int{}
	count := map[adapter.Runtime]int{}
	last := map[adapter.Runtime]time.Time{}
	for _, s := range live {
		count[s.Runtime]++
		if s.Busy() {
			busy[s.Runtime]++
		}
		if s.LastActive.After(last[s.Runtime]) {
			last[s.Runtime] = s.LastActive
		}
	}
	out := make([]RuntimeProfile, 0, len(ps))
	for _, p := range ps {
		p.Busy = busy[p.Runtime] > 0
		p.LiveSessions = count[p.Runtime]
		if t := last[p.Runtime]; !t.IsZero() {
			p.LastUsed = t
			// A live session is history, whatever the install-time scan said.
			p.History = HistorySome
		}
		out = append(out, p)
	}
	return out
}
