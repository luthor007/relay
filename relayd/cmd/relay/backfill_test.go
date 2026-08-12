package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixtureKey is a synthetic GitLab PAT, and its shape is deliberate.
//
// It has to be tier 1, because MEMORY.md §12.2 rule 1 says only tier 1 may
// reach the vault queue and this fixture is the whole point of the test. It
// must NOT be one of the four literal shapes scripts/build-public-repo.sh's
// credential guard refuses — the Stripe secret and restricted prefixes, the
// Google one, and the Anthropic one, all four spelled out in that script and
// deliberately not spelled out here — nor one of the markers
// TestCredentialsNeverReturnAFullSecret greps for, because unlike
// relayd/testdata/secrets this file is not excluded from the public repo.
// glpat- is tier 1, is on neither list, and never belonged to anybody.
//
// Every other credential-shaped fixture in relayd follows this rule, for the
// same reason. The vendor-specific patterns are exercised where they belong,
// against the corpus in testdata/secrets, which stays private.
const fixtureKey = "glpat-Nq7TESTONLYnotarealkey42"

// One Claude Code transcript, in the shape MEMORY.md §4 documents: one JSONL per
// session, carrying cwd, gitBranch, timestamp, version and aiTitle.
//
// The third message is the archaeology MEMORY.md §1 is about: somebody pasted a
// key into a session months ago and forgot. Every backfill test runs over it,
// so the detector's before-indexing ordering is exercised on every one of them.
const claudeTranscript = `{"type":"user","cwd":"/src/relay","gitBranch":"main","version":"2.1.226","timestamp":"2026-08-01T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"why is the CRC wrong on the glasses link"}]}}
{"type":"assistant","aiTitle":"CRC-16 investigation","timestamp":"2026-08-01T10:00:20Z","message":{"role":"assistant","content":[{"type":"text","text":"The vendor spec says ARC but the disassembly initialises to 0xFFFF, which is MODBUS."}]}}
{"type":"user","timestamp":"2026-08-01T10:01:00Z","message":{"role":"user","content":[{"type":"text","text":"the runner uses my gitlab token ` + fixtureKey + ` if you need the registry"}]}}
`

func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "-src-relay")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "11111111-2222-3333-4444-555555555555.jsonl")
	if err := os.WriteFile(file, []byte(claudeTranscript), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return home
}

func runBackfill(t *testing.T, home string, args ...string) string {
	t.Helper()
	return runBackfillWith(t, globals{configPath: filepath.Join(home, "none.toml")}, args...)
}

// runBackfillForced re-runs with --force, which is a global flag rather than a
// subcommand word — the same spelling `relay reindex` uses for the same idea.
func runBackfillForced(t *testing.T, home string) string {
	t.Helper()
	return runBackfillWith(t, globals{configPath: filepath.Join(home, "none.toml"), force: true})
}

func runBackfillWith(t *testing.T, g globals, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := backfillCmd(context.Background(), g, args, &out); err != nil {
		t.Fatalf("relay backfill: %v\n%s", err, out.String())
	}
	return out.String()
}

func openIndexDB(t *testing.T, home string) *sql.DB {
	t.Helper()
	return openDataDB(t, home, "relay.db")
}

func openVaultDB(t *testing.T, home string) *sql.DB {
	t.Helper()
	return openDataDB(t, home, "vault.db")
}

func openDataDB(t *testing.T, home, name string) *sql.DB {
	t.Helper()
	// Read the file the command wrote, with a plain driver rather than our own
	// store: a test that reads through the code under test cannot tell a wrong
	// write from a matching wrong read.
	//
	// Resolve the directory the way config.DataDir does rather than hardcoding
	// the XDG path. On darwin the data dir is ~/Library/Application Support/Relay,
	// so a hardcoded ~/.local/share/relay passes in a Linux container and fails
	// on every macOS machine — which is exactly what it did.
	dir := filepath.Join(home, ".local", "share", "relay")
	if runtime.GOOS == "darwin" {
		dir = filepath.Join(home, "Library", "Application Support", "Relay")
	}
	path := filepath.Join(dir, name)
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestBackfillIndexesAndIsResumable is the caller the five readers never had.
func TestBackfillIndexesAndIsResumable(t *testing.T) {
	home := fakeHome(t)

	first := runBackfill(t, home)
	if !strings.Contains(first, "1 indexed") {
		t.Fatalf("first run did not index the session:\n%s", first)
	}

	// Incremental and resumable, keyed on (runtime, session_id, mtime). 3.6 GB
	// through a small model is an hour or two, so a second run must not redo it.
	second := runBackfill(t, home)
	if !strings.Contains(second, "0 indexed") || !strings.Contains(second, "1 already current") {
		t.Errorf("second run was not incremental:\n%s", second)
	}

	// A missing store is success with zero sessions, not an error — MEMORY.md §1
	// found two of five runtimes with no data at all on a real machine. And
	// "nothing found" always comes with "and here is where we looked", because a
	// reader that assumes a path and silently finds nothing reports an empty
	// history as success.
	for _, want := range []string{"absent", "looked in"} {
		if !strings.Contains(first, want) {
			t.Errorf("output never says %q; a silent empty result is the OpenClaw trap:\n%s",
				want, first)
		}
	}
}

func TestBackfillStoresAPointerAndNotACopy(t *testing.T) {
	home := fakeHome(t)
	runBackfill(t, home)
	db := openIndexDB(t, home)

	// MEMORY.md §3: the index holds runtime, session id, path and byte offset so
	// anything can be re-read in full on demand. We are building an index, not a
	// copy, and the 3.6 GB stays on disk where it already is.
	var path string
	var offset int64
	err := db.QueryRow(`select path, byte_offset from session_index`).Scan(&path, &offset)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Error("no pointer back to the transcript")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the pointer does not resolve: %v", err)
	}

	rows, err := db.Query(`select name from pragma_table_info('session_index')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "transcript", "body", "text", "content", "messages_json":
			t.Errorf("session_index has a %q column: the index is becoming a copy", name)
		}
	}
}

// TestBackfillStealsTheRuntimesOwnTitle covers MEMORY.md §4's "two things worth
// stealing rather than rebuilding": Claude Code and Hermes both title their own
// sessions, so for a large share of the corpus the summariser's first job is
// already done.
func TestBackfillStealsTheRuntimesOwnTitle(t *testing.T) {
	home := fakeHome(t)
	runBackfill(t, home)
	db := openIndexDB(t, home)

	var title, workspace, branch string
	err := db.QueryRow(`select title, workspace, git_branch from session_index`).
		Scan(&title, &workspace, &branch)
	if err != nil {
		t.Fatal(err)
	}
	if title != "CRC-16 investigation" {
		t.Errorf("title = %q, want the transcript's own aiTitle", title)
	}
	if workspace != "/src/relay" || branch != "main" {
		t.Errorf("free metadata lost: workspace=%q branch=%q", workspace, branch)
	}
}

// TestBackfillLeavesUnreportedCostNull is the same rule the adapters follow.
//
// A zero would claim an observation the store never made. Claude Code
// transcripts usually carry usage and no cost at all, so the honest value is
// NULL — and the console shows a dash rather than "free".
func TestBackfillLeavesUnreportedCostNull(t *testing.T) {
	home := fakeHome(t)
	runBackfill(t, home)
	db := openIndexDB(t, home)

	var cost, tokens sql.NullFloat64
	if err := db.QueryRow(`select cost_usd, tokens_total from session_index`).
		Scan(&cost, &tokens); err != nil {
		t.Fatal(err)
	}
	if cost.Valid {
		t.Errorf("cost_usd = %v; the transcript reported none, and 0 means free",
			cost.Float64)
	}
	if tokens.Valid {
		t.Errorf("tokens_total = %v; the transcript reported none", tokens.Float64)
	}
}

// TestBackfillProposesTierOneCredentialsAndStoresNone is the producer half of
// MEMORY.md §6's third arrival path.
//
// Before this, `relay backfill` counted its vault candidates into a log line
// and threw the values away, then printed a paragraph telling the user to
// "review them in the console" — a queue that was empty by construction, on
// every machine, forever. Nothing anywhere called Propose except a unit test.
//
// The two halves of §6 that this asserts are opposite and both required: a
// tier-1 detection has to become a question, and it must not become a stored
// credential. A backfill that quietly captured the keys it found would be the
// single worst thing this product could do on its first run.
func TestBackfillProposesTierOneCredentialsAndStoresNone(t *testing.T) {
	home := fakeHome(t)
	out := runBackfill(t, home)

	db := openVaultDB(t, home)

	var n int
	if err := db.QueryRow(`select count(*) from credential_proposal`).Scan(&n); err != nil {
		t.Fatalf("no proposal table at all — backfill never opened the vault: %v", err)
	}
	if n != 1 {
		t.Fatalf("got %d proposals, want exactly 1 for the one key in the transcript", n)
	}

	var service, detector, sourceKind, decision, lastFour string
	var ciphertext []byte
	err := db.QueryRow(`select service, detector, source_kind, decision, last_four, ciphertext
		from credential_proposal`).
		Scan(&service, &detector, &sourceKind, &decision, &lastFour, &ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if service != "gitlab" {
		t.Errorf("service = %q; a proposal that cannot name a vendor is not a question "+
			"anybody can answer", service)
	}
	if sourceKind != "transcript" {
		t.Errorf("source_kind = %q, want transcript — provenance is what tells the user "+
			"where this key came from", sourceKind)
	}
	if decision != "" {
		t.Errorf("decision = %q; backfill answers nothing on the user's behalf", decision)
	}
	// The tier is what the console's danger confirmation keys on. internal/index
	// writes "<rule> (tier1)" into secret_marker and the console parses that
	// shape; the vault used to store the bare rule id, so the first proposal
	// ever displayed would have read "unknown" and demanded a typed service name
	// for a tier-1 hit — which is the only tier that can be in this queue.
	if !strings.Contains(detector, "tier1") {
		t.Errorf("detector = %q; the console reads the tier out of this string and "+
			"shows an unknown tier as more dangerous than a tier-1 match", detector)
	}
	if len(ciphertext) == 0 {
		t.Error("the candidate was not sealed; accepting it would have nothing to store")
	}

	// Nothing is captured silently. This is the assertion that matters most in
	// the whole file: a detection is a question, and until somebody answers it
	// the credential table is empty.
	var stored int
	if err := db.QueryRow(`select count(*) from credential`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("backfill stored %d credentials; MEMORY.md §6 says a detection is a "+
			"proposal, never a saved key", stored)
	}

	// The plaintext must not reach the terminal. Last four is the display form
	// during the proposal too, not only after it is stored.
	if strings.Contains(out, fixtureKey) {
		t.Fatalf("backfill printed the key it found:\n%s", out)
	}
	if !strings.Contains(out, "proposal") {
		t.Errorf("backfill never mentioned the proposal it made:\n%s", out)
	}

	// A re-run must not ask twice. The proposal id is an HMAC fingerprint of the
	// secret, so the same key in forty sessions is one question — and --force
	// re-indexes everything, which is exactly the path that would duplicate it.
	runBackfillForced(t, home)
	if err := db.QueryRow(`select count(*) from credential_proposal`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("after a forced re-run there are %d proposals; a queue that re-asks is "+
			"one people learn to dismiss unread", n)
	}
}
