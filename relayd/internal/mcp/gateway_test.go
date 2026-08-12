package mcp_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/audit"
	"github.com/luthor007/relay/relayd/internal/mcp"
)

// grantSet is a hand-held grant table: the tests decide exactly which halves
// exist, which is the only way to assert that read and write are separate.
type grantSet map[string]bool

func (g grantSet) Allowed(_ context.Context, connector string, a mcp.Access) (bool, string) {
	if g[a.Scope(connector)] {
		return true, ""
	}
	return false, connector + " is not connected"
}

type fakeConnector struct {
	calls atomic.Int64
	tools []mcp.Tool
}

func (f *fakeConnector) ProviderName() string { return "fake" }

func (f *fakeConnector) Tools(context.Context) []mcp.Tool { return f.tools }

func readTool(handler mcp.Handler) mcp.Tool {
	return mcp.Tool{
		Name: "printer_status", Connector: "printer", Access: mcp.AccessRead,
		Description: "what the printer is doing", Handler: handler,
	}
}

func writeTool(handler mcp.Handler) mcp.Tool {
	return mcp.Tool{
		Name: "printer_print", Connector: "printer", Access: mcp.AccessWrite,
		Description: "print a file",
		Consequence: "starts a print on your printer",
		Handler:     handler,
	}
}

func newFake() *fakeConnector {
	f := &fakeConnector{}
	run := func(ctx context.Context, c mcp.Call) (mcp.Result, error) {
		f.calls.Add(1)
		return mcp.Result{Text: "ok", Target: "benchy.gcode"}, nil
	}
	f.tools = []mcp.Tool{readTool(run), writeTool(run)}
	return f
}

func newGateway(t *testing.T, o mcp.Options) (*mcp.Gateway, *fakeConnector) {
	t.Helper()
	f := newFake()
	if o.NewID == nil {
		var n atomic.Int64
		o.NewID = func() string { n.Add(1); return "id-" + string(rune('a'+n.Load()-1)) }
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	}
	g := mcp.NewGateway(o)
	g.Register(context.Background(), f)
	return g, f
}

// Rule 1, ORCHESTRATOR.md §4b: nothing is auto-granted. A gateway with no grant
// source hands out nothing, and the zero value is the safe one.
func TestNothingIsGrantedByDefault(t *testing.T) {
	g, f := newGateway(t, mcp.Options{})
	ctx := context.Background()

	if got := len(g.All(ctx)); got != 2 {
		t.Fatalf("the bus should hold both tools, got %d", got)
	}
	if got := g.Tools(ctx); len(got) != 0 {
		t.Fatalf("an ungranted tool must not be visible, got %v", got)
	}
	_, err := g.Call(ctx, mcp.Call{Tool: "printer_status"})
	if !errors.Is(err, mcp.ErrNotGranted) {
		t.Fatalf("want ErrNotGranted, got %v", err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("the handler ran despite the refusal")
	}
}

// Rule 2: read and write are separate grants. Granting read must not open the
// write half, and the refusal has to say which half is missing.
func TestReadGrantDoesNotOpenTheWriteHalf(t *testing.T) {
	g, f := newGateway(t, mcp.Options{
		Grants: grantSet{"printer:read": true},
	})
	ctx := context.Background()

	visible := g.Tools(ctx)
	if len(visible) != 1 || visible[0].Name != "printer_status" {
		t.Fatalf("only the read tool should be visible, got %v", names(visible))
	}
	if _, err := g.Call(ctx, mcp.Call{Tool: "printer_status"}); err != nil {
		t.Fatalf("the granted read call failed: %v", err)
	}

	_, err := g.Call(ctx, mcp.Call{Tool: "printer_print"})
	if !errors.Is(err, mcp.ErrNotGranted) {
		t.Fatalf("want ErrNotGranted for the write half, got %v", err)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("the write handler must not have run, calls=%d", f.calls.Load())
	}
}

// Rule 3: consequences outside the machine confirm every time. Not once, not
// per session — every call.
func TestConsequentialToolConfirmsEveryTime(t *testing.T) {
	var asked atomic.Int64
	g, f := newGateway(t, mcp.Options{
		Grants: grantSet{"printer:write": true},
		Confirm: mcp.ConfirmerFunc(func(_ context.Context, c mcp.Confirmation) error {
			asked.Add(1)
			if !strings.Contains(c.Prompt(), "starts a print") {
				t.Errorf("the spoken prompt must say what happens outside the machine: %q", c.Prompt())
			}
			return nil
		}),
	})
	ctx := context.Background()

	for i := range 3 {
		if _, err := g.Call(ctx, mcp.Call{Tool: "printer_print", Arguments: map[string]any{"path": "benchy.gcode"}}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if asked.Load() != 3 {
		t.Fatalf("three calls must be three confirmations, got %d", asked.Load())
	}
	if f.calls.Load() != 3 {
		t.Fatalf("the tool should have run three times, got %d", f.calls.Load())
	}
}

func TestConsequentialToolWithNoConfirmerIsRefused(t *testing.T) {
	g, f := newGateway(t, mcp.Options{Grants: grantSet{"printer:write": true}})

	_, err := g.Call(context.Background(), mcp.Call{Tool: "printer_print"})
	if !errors.Is(err, mcp.ErrNoConfirmer) {
		t.Fatalf("want ErrNoConfirmer, got %v", err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("a consequential tool ran with nobody to confirm it")
	}
}

func TestDeclinedConfirmationDoesNotRunTheTool(t *testing.T) {
	g, f := newGateway(t, mcp.Options{
		Grants:  grantSet{"printer:write": true},
		Confirm: mcp.ConfirmerFunc(func(context.Context, mcp.Confirmation) error { return mcp.ErrDenied }),
	})

	_, err := g.Call(context.Background(), mcp.Call{Tool: "printer_print"})
	if !errors.Is(err, mcp.ErrDenied) {
		t.Fatalf("want ErrDenied, got %v", err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("the tool ran after the user said no")
	}
}

// A read tool has no consequence outside the machine, so it must not stop to
// ask. Confirming everything is how a confirmation stops meaning anything.
func TestReadToolDoesNotConfirm(t *testing.T) {
	var asked atomic.Int64
	g, _ := newGateway(t, mcp.Options{
		Grants: grantSet{"printer:read": true},
		Confirm: mcp.ConfirmerFunc(func(context.Context, mcp.Confirmation) error {
			asked.Add(1)
			return nil
		}),
	})
	if _, err := g.Call(context.Background(), mcp.Call{Tool: "printer_status"}); err != nil {
		t.Fatal(err)
	}
	if asked.Load() != 0 {
		t.Fatalf("a read tool asked for confirmation %d time(s)", asked.Load())
	}
}

// One audit trail: every call through one place, including the refused ones.
func TestEveryCallIsRecordedIncludingRefusals(t *testing.T) {
	rec := &mcp.MemoryRecorder{}
	g, _ := newGateway(t, mcp.Options{
		Grants: grantSet{"printer:read": true},
		Record: rec,
	})
	ctx := context.Background()

	if _, err := g.Call(ctx, mcp.Call{Tool: "printer_status", Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Call(ctx, mcp.Call{Tool: "printer_print", Session: "s1"}); err == nil {
		t.Fatal("the ungranted write should have been refused")
	}

	calls := rec.Calls()
	if len(calls) != 2 {
		t.Fatalf("want 2 recorded calls, got %d: %+v", len(calls), calls)
	}
	if calls[0].Status != "completed" || calls[0].Tool != "printer_status" {
		t.Fatalf("unexpected first row: %+v", calls[0])
	}
	if calls[1].Status != "denied" || calls[1].Connector != "printer" {
		t.Fatalf("a refusal must be recorded as one: %+v", calls[1])
	}
}

func TestArgumentsAreNeverStored(t *testing.T) {
	rec := &mcp.MemoryRecorder{}
	g, _ := newGateway(t, mcp.Options{Grants: grantSet{"printer:read": true}, Record: rec})

	// Credential-shaped and entirely synthetic. It deliberately avoids the
	// vendor prefixes scripts/build-public-repo.sh refuses to publish outside
	// relayd/testdata/secrets/, because a test fixture is not worth a broken
	// release build.
	secret := "ghp_000000000000000000000000000000000000"
	_, err := g.Call(context.Background(), mcp.Call{
		Tool:      "printer_status",
		Arguments: map[string]any{"token": secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := rec.Calls()[0]
	if strings.Contains(row.ArgsDigest, secret) || row.ArgsDigest == "" {
		t.Fatalf("the digest must be a digest, got %q", row.ArgsDigest)
	}
	if strings.Contains(row.Target, secret) {
		t.Fatalf("an argument value leaked into the target: %q", row.Target)
	}
	if mcp.Digest(map[string]any{"token": secret}) != row.ArgsDigest {
		t.Fatal("the digest is not reproducible from the same arguments")
	}
}

func TestUnrecordedCallsAreCounted(t *testing.T) {
	g, _ := newGateway(t, mcp.Options{Grants: grantSet{"printer:read": true}})
	if _, err := g.Call(context.Background(), mcp.Call{Tool: "printer_status"}); err != nil {
		t.Fatal(err)
	}
	if got := g.Stats().Unrecorded; got != 1 {
		t.Fatalf("a call with no recorder must be counted as unrecorded, got %d", got)
	}
	if g.Stats().RecordDurable {
		t.Fatal("a gateway with no recorder must not claim a durable trail")
	}
}

func TestProviderRegistrationIsAudited(t *testing.T) {
	log := audit.NewMemory()
	g := mcp.NewGateway(mcp.Options{Audit: log})
	ctx := context.Background()
	g.Register(ctx, newFake())
	g.Remove(ctx, "fake")

	entries, err := log.List(ctx, audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var actions []audit.Action
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	want := []audit.Action{
		audit.ActionMCPRegister, audit.ActionMCPRegister,
		audit.ActionMCPRemove, audit.ActionMCPRemove,
	}
	if len(actions) != len(want) {
		t.Fatalf("want %v, got %v", want, actions)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("entry %d: want %s, got %s", i, want[i], actions[i])
		}
	}
	if err := audit.Verify(entries); err != nil {
		t.Fatalf("the chain must hold: %v", err)
	}
}

func TestUnusableToolsAreDropped(t *testing.T) {
	g := mcp.NewGateway(mcp.Options{Grants: grantSet{"printer:read": true}})
	g.Register(context.Background(), mcp.ProviderFunc{
		Name: "broken",
		Fn: func(context.Context) []mcp.Tool {
			return []mcp.Tool{
				{Name: "no_connector", Access: mcp.AccessRead, Handler: func(context.Context, mcp.Call) (mcp.Result, error) { return mcp.Result{}, nil }},
				{Name: "no_handler", Connector: "printer", Access: mcp.AccessRead},
				{Name: "no_access", Connector: "printer", Handler: func(context.Context, mcp.Call) (mcp.Result, error) { return mcp.Result{}, nil }},
			}
		},
	})
	if got := g.All(context.Background()); len(got) != 0 {
		t.Fatalf("a tool with no grant to spend must not reach the bus, got %v", names(got))
	}
}

func TestUnknownToolIsNotAProtocolCrash(t *testing.T) {
	g, _ := newGateway(t, mcp.Options{Grants: grantSet{"printer:read": true}})
	_, err := g.Call(context.Background(), mcp.Call{Tool: "nope"})
	if !errors.Is(err, mcp.ErrNoSuchTool) {
		t.Fatalf("want ErrNoSuchTool, got %v", err)
	}
}

func names(list []mcp.Tool) []string {
	out := make([]string, 0, len(list))
	for _, t := range list {
		out = append(out, t.Name)
	}
	return out
}
