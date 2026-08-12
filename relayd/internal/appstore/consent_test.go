package appstore_test

import (
	"context"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/appstore"
)

// APP-PLATFORM.md §2: "Permission `reason` strings are mandatory and are shown
// verbatim at install." Verbatim is testable, so it is tested — the sheet's
// struct carries the author's bytes and [Sheet.Text] prints them unreflowed,
// unabbreviated and unquoted-except-by-quotation-marks.
func TestReasonsAreShownVerbatim(t *testing.T) {
	e, err := fixtureRegistry(t).Resolve(context.Background(), "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	sheet := appstore.NewSheet(e, "github:luthor007/relay-apps@main", nil)
	text := sheet.Text()

	if len(sheet.Rows) != len(e.Manifest.Permissions) {
		t.Fatalf("sheet has %d rows for %d permissions", len(sheet.Rows), len(e.Manifest.Permissions))
	}
	for i, p := range e.Manifest.Permissions {
		row := sheet.Rows[i]
		if row.Scope != p.Scope {
			t.Errorf("row %d scope = %q, want %q", i, row.Scope, p.Scope)
		}
		if row.Reason != p.Reason {
			t.Errorf("row %d reason = %q, want the manifest's %q", i, row.Reason, p.Reason)
		}
		if !strings.Contains(text, p.Reason) {
			t.Errorf("the rendered sheet does not carry %q verbatim:\n%s", p.Reason, text)
		}
		// The box describes the scope; the author describes the purpose. If an
		// app could write the "grants" line it could describe memory.read as
		// something it is not.
		if row.Grants != p.Scope.Grants() {
			t.Errorf("row %d grants = %q, want this box's own %q", i, row.Grants, p.Scope.Grants())
		}
	}
	for _, mark := range []string{"…", "...", "[truncated]"} {
		if strings.Contains(text, mark) {
			t.Errorf("the sheet abbreviates something with %q", mark)
		}
	}
	// The three facts that make the decision decidable at all.
	for _, want := range []string{
		"dev.alexis.standup-notes",
		"github:luthor007/relay-apps@main",
		"https://github.com/luthor007/standup-notes",
		"runs on your box",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("sheet is missing %q:\n%s", want, text)
		}
	}
}

// The notices are what the box enforces regardless of what the app says. They
// are derived from the manifest, so an app that asks for nothing cannot be
// shown a warning about egress and an app that asks for the camera cannot
// avoid the one about the LEDs.
func TestNoticesFollowTheManifest(t *testing.T) {
	ctx := context.Background()
	reg := fixtureRegistry(t)

	standup, err := reg.Resolve(ctx, "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	commute, err := reg.Resolve(ctx, "com.example.commute-brief")
	if err != nil {
		t.Fatal(err)
	}

	quiet := strings.Join(appstore.NewSheet(standup, "r", nil).Notices, "\n")
	if !strings.Contains(quiet, "no network access at all") {
		t.Errorf("an app without net.fetch should be shown as having no network:\n%s", quiet)
	}
	if !strings.Contains(quiet, "Every memory read is recorded") {
		t.Errorf("memory.read implies the read log (§5):\n%s", quiet)
	}
	if strings.Contains(quiet, "LEDs") {
		t.Errorf("an app with no camera scope must not be given a camera notice:\n%s", quiet)
	}

	noisy := appstore.NewSheet(commute, "r", nil)
	joined := strings.Join(noisy.Notices, "\n")
	if !strings.Contains(joined, "default-deny") {
		t.Errorf("net.fetch implies the egress notice:\n%s", joined)
	}
	if strings.Contains(joined, "Every memory read is recorded") {
		t.Errorf("an app with no memory.read must not be described as reading memory:\n%s", joined)
	}
	text := noisy.Text()
	for _, host := range commute.Manifest.AllowedHosts {
		if !strings.Contains(text, host) {
			t.Errorf("the sheet must name every host it may reach; missing %q:\n%s", host, text)
		}
	}
	// Review posture, stated as what it is (§5).
	if !strings.Contains(joined, "not a guarantee") {
		t.Errorf("the sheet should not oversell the review:\n%s", joined)
	}
}

func TestSheetShowsTriggers(t *testing.T) {
	e, err := fixtureRegistry(t).Resolve(context.Background(), "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	text := appstore.NewSheet(e, "r", nil).Text()
	// "when will this run" is part of the decision, and one of the triggers is
	// the agent calling it as a tool, which nobody would guess.
	for _, want := range []string{"wrap up the standup", "meeting.ended", "your agent decides"} {
		if !strings.Contains(text, want) {
			t.Errorf("sheet does not say when the app runs (%q):\n%s", want, text)
		}
	}
}

// An upgrade that widens what the app may do has to ask again. This is the
// mechanism by which an app granted memory.read does not quietly acquire the
// camera.
func TestUpgradeDiff(t *testing.T) {
	ctx := context.Background()
	upstream, err := fixtureRegistry(t).Resolve(ctx, "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	fork, err := appstore.New(fixtureSource(t, "fork")).Resolve(ctx, "dev.alexis.standup-notes")
	if err != nil {
		t.Fatal(err)
	}
	prev := appstore.Installed{
		Manifest: upstream.Manifest,
		Grants:   appstore.NewSheet(upstream, "r", nil).Grants(),
	}

	sheet := appstore.NewSheet(fork, "fork", &prev)
	if sheet.Upgrade == nil {
		t.Fatal("no upgrade block for an app that is already installed")
	}
	if !sheet.Upgrade.NeedsConsent() {
		t.Error("a new scope must re-ask")
	}
	if len(sheet.Upgrade.NewScopes) != 1 || sheet.Upgrade.NewScopes[0] != appstore.ScopeGlassesCamera {
		t.Errorf("new scopes = %v, want glasses.camera", sheet.Upgrade.NewScopes)
	}
	if !strings.Contains(sheet.Text(), "permissions it did not have") {
		t.Errorf("the sheet must say what is new:\n%s", sheet.Text())
	}
	if !strings.Contains(sheet.Question(), "Upgrade") {
		t.Errorf("question = %q", sheet.Question())
	}

	// The same version, same words: nothing widened.
	same := appstore.NewSheet(upstream, "r", &prev)
	if same.Upgrade.NeedsConsent() {
		t.Errorf("identical grants must not re-ask: %+v", same.Upgrade)
	}

	// The scope is unchanged and the sentence is not. A different sentence is a
	// different decision.
	reworded := upstream
	reworded.Manifest.Permissions = append([]appstore.Permission(nil), upstream.Manifest.Permissions...)
	reworded.Manifest.Permissions[0].Reason = "To read everything you have ever said, actually."
	sheet = appstore.NewSheet(reworded, "r", &prev)
	if !sheet.Upgrade.NeedsConsent() {
		t.Error("a changed reason must re-ask; the sentence is what was agreed to")
	}
	if len(sheet.Upgrade.ChangedReasons) != 1 {
		t.Errorf("changed = %v", sheet.Upgrade.ChangedReasons)
	}

	// Dropping a scope is strictly less access: worth showing, not worth asking.
	shrunk := upstream
	shrunk.Manifest.Permissions = upstream.Manifest.Permissions[:1]
	sheet = appstore.NewSheet(shrunk, "r", &prev)
	if sheet.Upgrade.NeedsConsent() {
		t.Error("asking for less must not re-ask")
	}
	if len(sheet.Upgrade.Dropped) != 3 {
		t.Errorf("dropped = %v, want the three it no longer wants", sheet.Upgrade.Dropped)
	}
}

// `relay setup --yes` takes every default. There is no default for this
// question, and an unattended run must not answer it.
func TestUnattendedRunsCannotConsent(t *testing.T) {
	ok, err := appstore.DenyAll.Review(context.Background(), appstore.Sheet{})
	if ok {
		t.Fatal("DenyAll consented")
	}
	if err == nil || !strings.Contains(err.Error(), "needs a person") {
		t.Errorf("err = %v", err)
	}
}
