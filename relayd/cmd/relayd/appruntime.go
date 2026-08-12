package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/luthor007/relay/relayd/internal/appruntime"
	"github.com/luthor007/relay/relayd/internal/apps"
	"github.com/luthor007/relay/relayd/internal/appstore"
	"github.com/luthor007/relay/relayd/internal/index"
)

// SubsystemApps is APP-PLATFORM.md on the health screen.
const SubsystemApps = "apps"

// appPlatform is the app runtime as the daemon holds it.
type appPlatform struct {
	runtime     *apps.Runtime
	dispatcher  *apps.Dispatcher
	provisioner *appruntime.Provisioner
	store       *appstore.Store
}

// startApps builds the app platform, or explains why it did not.
//
// The sentence it returns is the product on a box with no Node: "third-party
// apps need a JavaScript runtime and this machine has none" is a complete
// answer, and it is what the console shows instead of a blank.
//
// Until this existed, `internal/apps` was thirty files of tested runtime that
// `cmd/relayd` did not import — an app could be installed, its permissions
// recorded, and nothing on the machine could ever trigger it.
func startApps(ctx context.Context, dataDir string, screen apps.Screen, log *slog.Logger) (*appPlatform, string) {
	if dataDir == "" {
		return nil, "there is no data directory, so apps have nowhere to live"
	}

	detector, err := index.NewDetector()
	if err != nil {
		// The runtime refuses without one, and correctly: an app's log line is
		// text and text goes through the detector before it is recorded.
		return nil, "no secret detector, so no app runtime: " + err.Error()
	}

	runtimeDir := filepath.Join(dataDir, "app-runtime")
	rt, err := apps.New(ctx, apps.Options{
		RuntimeDir: runtimeDir,
		Redact:     detector,
		// The phone, so `ctx.ui` reaches somebody. Nil here would still mint the
		// capability and answer "no render surface" — see apps.UICap — but a
		// daemon that has a phone socket and does not pass it is the join this
		// whole file exists to stop being missing.
		Screen:    screen,
		AccessLog: &apps.MemoryAccessLog{},
		EgressLog: &apps.MemoryEgressLog{},
		Limits:    apps.DefaultLimits(),
		Log:       log,
	})
	if err != nil {
		// The overwhelmingly likely cause is no Node on PATH, which is a true
		// thing to say rather than a stack trace: APP-PLATFORM.md §5's runtime
		// is Node, and a box without one runs no third-party apps and is
		// otherwise completely fine.
		return nil, "no app runtime on this machine: " + err.Error()
	}

	dispatcher, err := apps.NewDispatcher(apps.DispatcherOptions{
		Runtime: rt,
		// The user's timezone, because APP-PLATFORM.md §4 says schedules are in
		// it and means it — a cron read in UTC fires "every morning at 8" at
		// midnight for half the year.
		Location: time.Local,
	})
	if err != nil {
		return nil, "the app dispatcher could not be built: " + err.Error()
	}

	prov, err := appruntime.New(appruntime.Options{
		Runtime:    rt,
		Dispatcher: dispatcher,
		Dir:        appruntime.PackagesDir(dataDir),
	})
	if err != nil {
		return nil, "the app provisioner could not be built: " + err.Error()
	}

	// The same root `relay install` writes to. Inventing a second path here
	// would mean the CLI installs an app the daemon never sees, and the symptom
	// is an app that is listed and never runs.
	store, err := appstore.OpenStore(appstore.StoreRoot(dataDir))
	if err != nil {
		return nil, "the app store could not be opened: " + err.Error()
	}

	// Everything installed becomes triggerable again. Without this the
	// dispatcher is empty after every restart, which is indistinguishable from
	// the app being broken — and is exactly the shape of failure that let
	// "nothing constructs the runtime" go unnoticed.
	for _, problem := range prov.Load(store) {
		log.Warn("relayd: an installed app could not be started", "error", problem)
	}

	return &appPlatform{runtime: rt, dispatcher: dispatcher, provisioner: prov, store: store}, ""
}

// status is the health line, read from the constructed dispatcher.
//
// Counted from what is registered rather than from what the store holds: a
// record that failed to attach is an app the user believes is installed and
// which nothing can trigger, and the difference between those two numbers is
// the only place that shows.
func (a *appPlatform) status(why string) string {
	if a == nil {
		if why != "" {
			return why
		}
		return "no app runtime on this machine"
	}
	running := len(a.dispatcher.List())
	installed := 0
	if records, err := a.store.List(); err == nil {
		installed = len(records)
	}
	switch {
	case installed == 0:
		return "on, with no apps installed"
	case running == installed:
		return plural(running) + " running on " + a.runtime.SandboxName()
	default:
		return plural(running) + " running of " + plural(installed) + " installed; the rest could not be started"
	}
}

func plural(n int) string {
	if n == 1 {
		return "1 app"
	}
	return itoa(n) + " apps"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// phrase offers an utterance to the installed apps.
//
// APP-PLATFORM.md §4's phrase trigger, and the reason the app platform is on the
// utterance path at all. It runs *after* routing has decided, so an app never
// takes an utterance away from an agent session — a phrase match is an addition
// to what the orchestrator did, not a substitute for it.
func (a *appPlatform) phrase(ctx context.Context, text string) {
	if a == nil || text == "" {
		return
	}
	a.dispatcher.Phrase(ctx, text)
}
