package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/audit"
)

func ctx() context.Context { return context.Background() }

// logs runs a test body against both implementations, because the two have to
// agree on everything except durability.
func logs(t *testing.T, fn func(t *testing.T, l audit.Log)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { fn(t, audit.NewMemory()) })
	t.Run("file", func(t *testing.T) {
		l, err := audit.OpenFile(filepath.Join(t.TempDir(), "audit", "audit.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		fn(t, l)
	})
}

// DASHBOARD.md §4: it must record the attempt, not only the success. A log that
// only holds what worked cannot answer "did anything try".
func TestTheAttemptIsRecordedEvenWhenTheMutationFails(t *testing.T) {
	logs(t, func(t *testing.T, l audit.Log) {
		a, err := audit.Begin(ctx(), l, audit.Entry{
			Actor:   audit.Actor{Kind: "console", From: "127.0.0.1:51234"},
			Action:  audit.ActionCredentialAdd,
			Target:  "cred-1",
			Service: "stripe",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Fail(ctx(), errors.New("the keychain is locked")); err != nil {
			t.Fatal(err)
		}

		got, err := l.List(ctx(), audit.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("entries = %d, want an attempt and an outcome: %+v", len(got), got)
		}
		if got[0].Outcome != audit.OutcomeAttempted {
			t.Fatalf("first entry = %q, want the attempt", got[0].Outcome)
		}
		if got[1].Outcome != audit.OutcomeFailed || got[1].Attempt != got[0].ID {
			t.Fatalf("outcome = %+v, should link back to %s", got[1], got[0].ID)
		}
		if !strings.Contains(got[1].Reason, "keychain") {
			t.Fatalf("the reason should be the one the keychain gave: %q", got[1].Reason)
		}
		// Who, what, when, from where.
		if got[0].Actor.Kind != "console" || got[0].Actor.From == "" ||
			got[0].Action != audit.ActionCredentialAdd || got[0].At.IsZero() {
			t.Fatalf("attempt is missing one of who/what/when/where: %+v", got[0])
		}
	})
}

func TestADeniedRequestIsRecordedSeparatelyFromAFailedOne(t *testing.T) {
	logs(t, func(t *testing.T, l audit.Log) {
		a, err := audit.Begin(ctx(), l, audit.Entry{
			Actor: audit.Actor{Kind: "console"}, Action: audit.ActionCredentialRevoke, Target: "c1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Deny(ctx(), "this session may not write to the vault"); err != nil {
			t.Fatal(err)
		}
		got, _ := l.List(ctx(), audit.Filter{Outcomes: []audit.Outcome{audit.OutcomeDenied}})
		if len(got) != 1 || got[0].Outcome != audit.OutcomeDenied {
			t.Fatalf("denied entries = %+v", got)
		}
	})
}

func TestAnAttemptGetsExactlyOneOutcome(t *testing.T) {
	l := audit.NewMemory()
	a, err := audit.Begin(ctx(), l, audit.Entry{Action: audit.ActionCredentialAdd})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.OK(ctx(), nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Fail(ctx(), errors.New("nope")); err == nil {
		t.Fatal("a second outcome for one attempt was accepted")
	}
}

func TestBeginWithNoLogRefusesRatherThanSucceedingQuietly(t *testing.T) {
	if _, err := audit.Begin(ctx(), nil, audit.Entry{Action: audit.ActionCredentialAdd}); !errors.Is(err, audit.ErrNoLog) {
		t.Fatalf("err = %v, want ErrNoLog — an unlogged vault write must not proceed", err)
	}
}

func TestFilters(t *testing.T) {
	logs(t, func(t *testing.T, l audit.Log) {
		for i := range 5 {
			if _, err := l.Append(ctx(), audit.Entry{
				Action: audit.ActionCredentialAdd, Target: "c" + strconv.Itoa(i),
				Outcome: audit.OutcomeOK,
			}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := l.Append(ctx(), audit.Entry{
			Action: audit.ActionConnectorRevoke, Target: "gmail", Outcome: audit.OutcomeOK,
		}); err != nil {
			t.Fatal(err)
		}

		byAction, _ := l.List(ctx(), audit.Filter{Action: audit.ActionConnectorRevoke})
		if len(byAction) != 1 || byAction[0].Target != "gmail" {
			t.Fatalf("by action = %+v", byAction)
		}
		byTarget, _ := l.List(ctx(), audit.Filter{Target: "c3"})
		if len(byTarget) != 1 {
			t.Fatalf("by target = %+v", byTarget)
		}
		// The limit keeps the most recent, because that is what a console shows.
		limited, _ := l.List(ctx(), audit.Filter{Limit: 2})
		if len(limited) != 2 || limited[1].Target != "gmail" {
			t.Fatalf("limited = %+v", limited)
		}
	})
}

// The chain is what makes a deleted line visible. Without it, "append-only" is
// a promise about our code rather than a property of the artefact.
func TestTheChainDetectsADeletedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := audit.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if _, err := l.Append(ctx(), audit.Entry{
			Action: audit.ActionCredentialAdd, Target: "c" + strconv.Itoa(i), Outcome: audit.OutcomeOK,
		}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := l.All(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.Verify(all); err != nil {
		t.Fatalf("a log we just wrote does not verify: %v", err)
	}
	_ = l.Close()

	// Somebody removes the middle line.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d", len(lines))
	}
	edited := append([]string{}, lines[:2]...)
	edited = append(edited, lines[3])
	if err := os.WriteFile(path, []byte(strings.Join(edited, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	l2, err := audit.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	after, err := l2.All(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.Verify(after); !errors.Is(err, audit.ErrBroken) {
		t.Fatalf("Verify after a deletion = %v, want ErrBroken", err)
	}
}

func TestTheChainDetectsAnEditedField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, _ := audit.OpenFile(path)
	if _, err := l.Append(ctx(), audit.Entry{
		Action: audit.ActionCredentialRevoke, Target: "stripe-live", Outcome: audit.OutcomeOK,
	}); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()

	raw, _ := os.ReadFile(path)
	var e audit.Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &e); err != nil {
		t.Fatal(err)
	}
	e.Target = "something-harmless"
	b, _ := json.Marshal(e)
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	l2, _ := audit.OpenFile(path)
	defer l2.Close()
	after, _ := l2.All(ctx())
	if err := audit.Verify(after); !errors.Is(err, audit.ErrBroken) {
		t.Fatalf("Verify after an edit = %v, want ErrBroken", err)
	}
}

// A restart must continue the chain, or every reboot would look like tampering.
func TestTheChainSurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := audit.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := l.Append(ctx(), audit.Entry{Action: audit.ActionCredentialAdd, Outcome: audit.OutcomeOK})
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()

	l2, err := audit.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	second, err := l2.Append(ctx(), audit.Entry{Action: audit.ActionCredentialRevoke, Outcome: audit.OutcomeOK})
	if err != nil {
		t.Fatal(err)
	}
	if second.Prev != first.Hash {
		t.Fatalf("after a reopen the chain restarted: prev = %q, want %q", second.Prev, first.Hash)
	}
	if second.Seq != first.Seq+1 {
		t.Fatalf("sequence = %d after %d", second.Seq, first.Seq)
	}
	all, _ := l2.All(ctx())
	if err := audit.Verify(all); err != nil {
		t.Fatal(err)
	}
}

// A half-written final line is a machine that died mid-append, not a reason to
// refuse to open the log.
func TestATruncatedFinalLineDoesNotStopTheLogOpening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, _ := audit.OpenFile(path)
	good, _ := l.Append(ctx(), audit.Entry{Action: audit.ActionCredentialAdd, Outcome: audit.OutcomeOK})
	_ = l.Close()

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"half-writ`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	l2, err := audit.OpenFile(path)
	if err != nil {
		t.Fatalf("a truncated tail made the log unopenable: %v", err)
	}
	defer l2.Close()
	next, err := l2.Append(ctx(), audit.Entry{Action: audit.ActionCredentialRevoke, Outcome: audit.OutcomeOK})
	if err != nil {
		t.Fatal(err)
	}
	if next.Prev != good.Hash {
		t.Fatalf("chain resumed from %q, want the last complete entry %q", next.Prev, good.Hash)
	}
}

// Nothing in an Entry is a secret. If a field is ever added that one fits in,
// this is what fails.
func TestNoEntryFieldCanHoldASecret(t *testing.T) {
	banned := []string{"secret", "token", "password", "apikey", "key", "credential", "value", "plaintext"}
	tp := reflect.TypeOf(audit.Entry{})
	for i := range tp.NumField() {
		name := strings.ToLower(tp.Field(i).Name)
		for _, b := range banned {
			if name == b {
				t.Fatalf("audit.Entry has a %s field; the log must never be able to carry one", tp.Field(i).Name)
			}
		}
	}

	const secret = "glpat-TESTONLYneverIssuedToAnybody02"
	l := audit.NewMemory()
	if _, err := l.Append(ctx(), audit.Entry{
		Action: audit.ActionCredentialAdd, Service: "stripe", Target: "cred-1",
		Detail: map[string]string{"last_four": secret[len(secret)-4:]},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := l.List(ctx(), audit.Filter{})
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), secret) {
		t.Fatalf("a secret round-tripped through the audit log: %s", b)
	}
}

func TestDurabilityIsReportedHonestly(t *testing.T) {
	m := audit.NewMemory()
	if m.Durable() || m.Path() != "" {
		t.Fatal("the memory log claims to be durable")
	}
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	f, err := audit.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !f.Durable() || f.Path() != path {
		t.Fatalf("file log = durable %v, path %q", f.Durable(), f.Path())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("audit log mode = %v, want 0600", perm)
	}
}

func TestTimestampsComeFromTheInjectedClock(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	l := audit.NewMemory()
	l.Now = func() time.Time { return at }
	l.NewID = func() string { return "fixed" }
	e, err := l.Append(ctx(), audit.Entry{Action: audit.ActionCredentialAdd, Outcome: audit.OutcomeOK})
	if err != nil {
		t.Fatal(err)
	}
	if !e.At.Equal(at) || e.ID != "fixed" {
		t.Fatalf("entry = %+v", e)
	}
}
