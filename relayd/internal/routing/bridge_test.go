package routing_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/adapter/fake"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/logx"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/routing"
	"github.com/luthor007/relay/relayd/internal/store"
)

func newRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := registry.New(registry.Options{DB: db, Log: logx.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		reg.Shutdown(ctx)
	})
	return reg
}

// The whole loop, on the fake adapter: no runtime is installed in this
// container and none needs to be. Route, act, announce, undo.
func TestRoutingDrivesTheRealRegistry(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)

	claude := fake.New(fake.Options{Runtime: adapter.ClaudeCode})
	codex := fake.New(fake.Options{Runtime: adapter.Codex})
	reg.AddAdapter(claude)
	reg.AddAdapter(codex)

	payments, err := reg.Start(ctx, registry.StartOptions{
		Runtime: adapter.ClaudeCode, ID: "s1",
		Subject: "the payments refactor", Workspace: "/repo/payments",
	})
	if err != nil {
		t.Fatal(err)
	}
	docs, err := reg.Start(ctx, registry.StartOptions{
		Runtime: adapter.Codex, ID: "s2",
		Subject: "the api docs", Workspace: "/repo/api",
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := routing.New(routing.Options{
		Sessions: routing.FromRegistry(reg),
		Driver:   routing.RegistryDriver(reg),
		Log:      logx.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The escape hatch picks a session out of the real list.
	d, err := r.Route(ctx, routing.Request{Text: "talk to the payments one, add a rate limiter"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != routing.KindContinue || d.Session != payments.ID() {
		t.Fatalf("got %s/%s, want continue/%s", d.Kind, d.Session, payments.ID())
	}
	if d.Text != "add a rate limiter" {
		t.Fatalf("text = %q; the instruction said in the same breath is what gets sent", d.Text)
	}

	turn, err := reg.Send(ctx, d.Session, adapter.Turn{Text: d.Text})
	if err != nil {
		t.Fatal(err)
	}
	r.Confirm(d, d.Session, turn)

	// Wrong session. Undo moves it, for real.
	res, err := r.Undo(ctx, routing.UndoTarget{Ref: "api docs"})
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if res.To != docs.ID() {
		t.Fatalf("moved to %q, want %q", res.To, docs.ID())
	}

	var moved bool
	for _, sess := range codex.Sessions() {
		for _, sent := range sess.Sent() {
			if sent.Text == "add a rate limiter" {
				moved = true
			}
		}
	}
	if !moved {
		t.Fatal("the turn never reached the session it was moved to")
	}
	if !res.Cancelled {
		t.Errorf("the wrong session's turn should have been cancelled: %q", res.CancelNote)
	}
	if r.Focus() != docs.ID() {
		t.Errorf("focus = %q, want the session we moved to", r.Focus())
	}
}

// A runtime that cannot cancel is the ACP case, which is three of the five.
// Undo still moves the turn and says plainly that the old one will finish.
func TestUndoOnARuntimeThatCannotCancel(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)

	noCancel := adapter.Baseline(adapter.OpenClaw).
		With(adapter.CapCancel, adapter.SupportNo, "this fake cannot cancel")
	reg.AddAdapter(fake.New(fake.Options{Runtime: adapter.OpenClaw, Caps: &noCancel}))
	reg.AddAdapter(fake.New(fake.Options{Runtime: adapter.Codex}))

	if _, err := reg.Start(ctx, registry.StartOptions{
		Runtime: adapter.OpenClaw, ID: "s1", Subject: "the payments refactor", Workspace: "/repo/payments",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Start(ctx, registry.StartOptions{
		Runtime: adapter.Codex, ID: "s2", Subject: "the api docs", Workspace: "/repo/api",
	}); err != nil {
		t.Fatal(err)
	}

	r, err := routing.New(routing.Options{
		Sessions: routing.FromRegistry(reg),
		Driver:   routing.RegistryDriver(reg),
		Log:      logx.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.SetFocus("s1")

	d, _ := r.Route(ctx, routing.Request{Text: "add a rate limiter"})
	turn, err := reg.Send(ctx, d.Session, adapter.Turn{Text: d.Text})
	if err != nil {
		t.Fatal(err)
	}
	r.Confirm(d, d.Session, turn)

	res, err := r.Undo(ctx, routing.UndoTarget{Session: "s2"})
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if res.Cancelled {
		t.Fatal("this runtime cannot cancel, so undo must not claim it did")
	}
	if !strings.Contains(res.Announcement, "finish") {
		t.Errorf("announcement %q should tell the user the old turn is still running", res.Announcement)
	}
}

// FromDetect is where MEMORY.md §1's real machine meets the never-route rule.
func TestFromDetectMapsHistoryHonestly(t *testing.T) {
	sessions := func(n int) *int { return &n }
	bytes := func(n int64) *int64 { return &n }

	rep := detect.Report{
		At: now(),
		Findings: []detect.Finding{
			{ // Hermes: 2.5 GB, 27 sessions — the dominant one.
				Runtime: adapter.Hermes, Installed: true,
				StateDirExists: true, Sessions: sessions(27), StoreBytes: bytes(2_500_000_000),
			},
			{ // OpenCode: installed, zero sessions. Counted, and empty.
				Runtime: adapter.OpenCode, Installed: true,
				StateDirExists: true, Sessions: sessions(0), StoreBytes: bytes(0),
			},
			{ // OpenClaw: installed, and nobody looked inside.
				Runtime: adapter.OpenClaw, Installed: true,
			},
			{ // Codex: not installed at all.
				Runtime: adapter.Codex,
			},
		},
	}
	attached := map[adapter.Runtime]bool{
		adapter.Hermes: true, adapter.OpenCode: true, adapter.OpenClaw: true,
	}

	want := map[adapter.Runtime]routing.History{
		adapter.Hermes:   routing.HistorySome,
		adapter.OpenCode: routing.HistoryNone,
		adapter.OpenClaw: routing.HistoryUnknown,
		adapter.Codex:    routing.HistoryNone,
	}
	for _, p := range routing.FromDetect(rep, attached) {
		if p.History != want[p.Runtime] {
			t.Errorf("%s history = %s, want %s", p.Runtime, p.History, want[p.Runtime])
		}
		if p.Runtime == adapter.OpenClaw && p.Used() {
			t.Error("a store nobody opened must not count as used")
		}
	}

	// And the router routes to the only one with history.
	rr, err := routing.NewRuntimeRouter(routing.RuntimeOptions{
		Profiles:     routing.StaticProfiles(routing.FromDetect(rep, attached)...),
		Entitlements: routing.Entitlements{routing.APIKeysOnly},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := rr.Choose(context.Background(), routing.RuntimeRequest{})
	if got.Runtime != adapter.Hermes {
		t.Fatalf("got %s, want hermes — the only one with history", got.Runtime)
	}
}

// A live session is history whatever the install-time scan said, and it is also
// where the load signal comes from.
func TestLiveLoadFoldsInWhatIsRunningNow(t *testing.T) {
	ps := []routing.RuntimeProfile{neverRun(adapter.Codex), used(adapter.ClaudeCode)}

	busy := session("s1", "one", "/repo/a", adapter.Codex, time.Minute)
	busy.State = store.SessionRunning

	got := routing.LiveLoad(ps, []routing.SessionView{busy})
	for _, p := range got {
		switch p.Runtime {
		case adapter.Codex:
			if !p.Busy || p.LiveSessions != 1 {
				t.Errorf("codex busy=%v live=%d", p.Busy, p.LiveSessions)
			}
			if p.History != routing.HistorySome {
				t.Error("a session running right now is history by any definition")
			}
		case adapter.ClaudeCode:
			if p.Busy {
				t.Error("claude-code is not busy")
			}
		}
	}
}

// FromRegistry only offers sessions relayd is actually driving. Routing a turn
// to a row we would have to resume first — and resume is unverified on three of
// the five (ADAPTERS.md §8) — is a wrong continue with an extra step.
func TestFromRegistryOffersOnlyLiveSessions(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	reg.AddAdapter(fake.New(fake.Options{Runtime: adapter.ClaudeCode}))

	e, err := reg.Start(ctx, registry.StartOptions{
		Runtime: adapter.ClaudeCode, ID: "s1", Subject: "one", Workspace: "/repo/a",
	})
	if err != nil {
		t.Fatal(err)
	}
	src := routing.FromRegistry(reg)

	live, err := src.Live(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != "s1" {
		t.Fatalf("live = %+v", live)
	}
	if live[0].Subject != "one" || live[0].Workspace != "/repo/a" {
		t.Errorf("the view lost the row: %+v", live[0])
	}
	if !live[0].Capabilities.Has(adapter.CapSteer) {
		t.Error("the capability descriptor should come through — the load step reads it")
	}

	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	<-e.Done()

	live, err = src.Live(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("a closed session is still being offered: %+v", live)
	}
}
