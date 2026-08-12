package appstore

import "fmt"

// `relay logs` — and the one thing it must not do.
//
// The log this package can write is a lifecycle log: installed, upgraded,
// provisioning deferred, removed. Those are events it observes. Invocations —
// "the trigger fired", "the app read episode 41", "it was killed at the
// timeout" — belong to the runtime, which is APP-PLATFORM.md §8 step 2 and is
// not built.
//
// So `relay logs` prints what exists and *says* that the rest does not exist
// yet. An empty log rendered as an empty log reads as "the app ran and said
// nothing", which is a different and false statement.

// lifecycleKinds are the events this package writes itself. Anything else in an
// app's log came from the runtime.
var lifecycleKinds = map[EventKind]bool{
	EventInstalled:         true,
	EventUpgraded:          true,
	EventDeclined:          true,
	EventRemoved:           true,
	EventProvisionDeferred: true,
	EventProvisionFailed:   true,
	EventProvisioned:       true,
}

// LogView is everything `relay logs <app>` needs.
type LogView struct {
	App    Installed `json:"app"`
	Events []Event   `json:"events"`
	// Note names what is absent and why. Empty only when the log actually
	// contains records of the app running.
	Note string `json:"note,omitempty"`
}

// View reads an app's log and works out what it is missing.
func (s *Store) View(name string, n int) (LogView, error) {
	rec, err := s.Get(name)
	if err != nil {
		return LogView{}, err
	}
	events, err := s.Log(rec.ID(), n)
	if err != nil {
		return LogView{}, err
	}
	v := LogView{App: rec, Events: events}
	runtimeRecords := 0
	for _, e := range events {
		if !lifecycleKinds[e.Kind] {
			runtimeRecords++
		}
	}
	if runtimeRecords == 0 {
		switch rec.State {
		case StateProvisioned:
			v.Note = fmt.Sprintf("These are lifecycle events. %s has a container and has not run yet — "+
				"nothing has fired one of its triggers.", rec.ShortName())
		default:
			v.Note = "These are lifecycle events, and they are all there is. " + NoRuntimeNote
		}
	}
	return v, nil
}
