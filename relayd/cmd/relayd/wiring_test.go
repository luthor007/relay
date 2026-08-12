package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/luthor007/relay/relayd/internal/index"
	"github.com/luthor007/relay/relayd/internal/store"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// The gap these tests exist to close
//
// Every package in relayd/internal is tested and correct on its own, and for a
// while almost none of them were reachable. cmd/relayd opened the database and
// then called api.New *without* DB, so every store-backed console screen
// rendered empty in the shipped binary — while internal/api's own tests passed,
// because those tests construct what production did not.
//
// No unit test can catch that by construction: the composition root is the one
// thing that has no unit above it. So these tests take the only view that would
// have — seed a database, start the real run() with the real flags, and ask the
// HTTP API whether the data came back.
//
// The assertion that matters is "not empty". A screen returning [] looks
// identical to a screen with nothing to show, which is exactly why the bug
// survived five milestones and a verify pass.

// seedDataDir writes one of each thing a console screen reads, into the same
// files the daemon will open.
func seedDataDir(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)

	// A live session, and a blocked one. DASHBOARD.md §3.1 wants the blocked one
	// pinned at the top, unmissable — it is the single failure mode that stops
	// all work rather than degrading it.
	for _, s := range []store.Session{
		{
			ID: "sess-running", Agent: "claude", Subject: "payments refactor",
			Workspace: "/src/api", State: store.SessionRunning,
			CreatedAt: now.Add(-time.Hour), LastActive: now,
		},
		{
			ID: "sess-awaiting", Agent: "codex", Subject: "migration",
			Workspace: "/src/api", State: store.SessionAwaiting,
			CreatedAt: now.Add(-2 * time.Hour), LastActive: now.Add(-time.Minute),
		},
	} {
		if err := db.PutSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	// One index row. MEMORY.md §3: a pointer — runtime, session id, path, byte
	// offset — never a copy of the transcript.
	if err := db.PutSessionIndex(ctx, store.SessionIndex{
		ID: "idx-1", Runtime: "claude", SessionID: "sess-historical",
		Path: filepath.Join(dir, "transcript.jsonl"), ByteOffset: 0,
		Title: "the CRC investigation", Workspace: "/src/glasses",
		StartedAt: now.Add(-48 * time.Hour), EndedAt: now.Add(-47 * time.Hour),
		Messages: 42, IndexedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

// startDaemon runs the real composition root and returns its base URL.
func startDaemon(t *testing.T, dir string) string {
	t.Helper()
	return startDaemonWithConfig(t, dir, filepath.Join(dir, "none.toml"))
}

// startDaemonWithConfig is the same thing with a config file that exists, for
// the tests that need the daemon to have models.
func startDaemonWithConfig(t *testing.T, dir, cfgPath string) string {
	base, _ := startStoppableDaemon(t, dir, cfgPath)
	return base
}

// startStoppableDaemon also hands back a stop that waits for the daemon to
// finish, for the tests that restart one on the same data directory.
//
// Waiting is the part that matters. Two relayds holding the same relay.db is
// not a restart, it is a second daemon — and a test that opened the new one
// before the old one had closed its handles would be asserting about whichever
// process answered first.
func startStoppableDaemon(t *testing.T, dir, cfgPath string) (string, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	addrs := make(chan net.Addr, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- run(ctx, []string{
			"--listen", "127.0.0.1:0",
			"--data-dir", dir,
			"--config", cfgPath,
			"--token", "wiring-token",
			"--log-level", "error",
		}, func(a net.Addr) { addrs <- a })
	}()

	var base string
	select {
	case addr := <-addrs:
		base = "http://" + addr.String()
	case err := <-errc:
		t.Fatalf("relayd exited before serving: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("relayd never came up")
	}

	var once sync.Once
	return base, func() {
		once.Do(func() {
			cancel()
			select {
			case <-errc:
			case <-time.After(30 * time.Second):
				t.Fatal("relayd did not shut down")
			}
		})
	}
}

func get(t *testing.T, base, path string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("Authorization", "Bearer wiring-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// TestConsoleScreensSeeTheDatabase is the regression test for the missing DB.
//
// It fails if api.Options ever loses a field that main has in hand, which is the
// exact shape of the original defect.
func TestConsoleScreensSeeTheDatabase(t *testing.T) {
	dir := t.TempDir()
	seedDataDir(t, dir)
	base := startDaemon(t, dir)

	t.Run("sessions are not empty", func(t *testing.T) {
		code, body := get(t, base, "/v1/sessions")
		if code != 200 {
			t.Fatalf("GET /v1/sessions = %d: %s", code, body)
		}
		var out struct {
			Sessions []struct {
				ID      string `json:"id"`
				Subject string `json:"subject"`
				State   string `json:"state"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		if len(out.Sessions) == 0 {
			t.Fatal("the seeded sessions did not come back — the daemon is not " +
				"passing its database to the API, which is how the console renders " +
				"empty while every api test passes")
		}
		// Three, not two: DASHBOARD.md §3.1 is one list over both tiers — the two
		// live rows from the registry and the historical row from the index. If
		// this ever drops to two, the index came unwired and the console silently
		// became a view of only what is running right now.
		if len(out.Sessions) != 3 {
			t.Errorf("got %d sessions, want 3 — two registry rows and one index row",
				len(out.Sessions))
		}
		var sawHistorical bool
		for _, s := range out.Sessions {
			if s.Subject == "the CRC investigation" {
				sawHistorical = true
			}
		}
		if !sawHistorical {
			t.Error("the indexed session is missing: the registry is wired and the " +
				"index is not, so the console shows today and forgets everything else")
		}

		// Both seeded rows said something was driving them. Nothing is: this is a
		// cold start against a database from a previous run. Registry.Recover
		// detaches *both* running and awaiting to idle, and the awaiting one is
		// the more important of the two — a stale "waiting on input" row is not
		// merely wrong, it is the row DASHBOARD.md §3.1 pins to the top for the
		// user to act on first. Being told to answer a session that no process is
		// attached to is worse than not being told anything.
		for _, s := range out.Sessions {
			if s.State == "running" || s.State == "awaiting" {
				t.Errorf("session %s came back as %q after a cold start; nothing is "+
					"driving it, so the row is a lie with a plausible shape", s.ID, s.State)
			}
		}
	})

	// Each of these is a screen. An endpoint that 404s or 500s is unwired; one
	// that returns 200 has at least been given what it needs to answer.
	for _, path := range []string{
		"/v1/health",
		"/v1/credentials",
		"/v1/facts",
		"/v1/connectors",
		"/v1/audit",
	} {
		t.Run("screen"+strings.ReplaceAll(path, "/", "_"), func(t *testing.T) {
			code, body := get(t, base, path)
			if code != 200 {
				t.Fatalf("GET %s = %d: %s", path, code, body)
			}
			if !json.Valid(body) {
				t.Fatalf("GET %s returned invalid JSON: %s", path, body)
			}
		})
	}
}

// TestEveryConsoleEndpointRefusesWithoutTheToken is the other half.
//
// DASHBOARD.md §4 calls the console the highest-value target in the system,
// above the glasses and above relayd's own API, because it can write to the
// vault. A screen that works but does not check the token is worse than one that
// does not work at all.
func TestEveryConsoleEndpointRefusesWithoutTheToken(t *testing.T) {
	dir := t.TempDir()
	seedDataDir(t, dir)
	base := startDaemon(t, dir)

	for _, path := range []string{
		"/v1/sessions",
		"/v1/health",
		"/v1/credentials",
		"/v1/facts",
		"/v1/connectors",
		// The proposals screen carries what the user has been talking about
		// often enough for Relay to notice. That is not less sensitive than the
		// list of what is already connected.
		"/v1/connectors/proposals",
		"/v1/audit",
	} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s = %d, want 401", path, resp.StatusCode)
		}
	}
}

// TestCredentialsNeverReturnAFullSecret walks the shipped daemon rather than the
// api package, because the guarantee is only worth anything end to end.
//
// DASHBOARD.md §3.2: last four characters, never the key. A UI that shows the
// secret back to you is a UI that gets screenshotted into a support ticket.
func TestCredentialsNeverReturnAFullSecret(t *testing.T) {
	dir := t.TempDir()
	seedDataDir(t, dir)
	base := startDaemon(t, dir)

	code, body := get(t, base, "/v1/credentials")
	if code != 200 {
		t.Fatalf("GET /v1/credentials = %d: %s", code, body)
	}
	// Nothing credential-shaped may appear in a list response, whatever the
	// vault happens to hold.
	//
	// Two of these are split across a concatenation for the same reason
	// internal/index/ruleset.go splits one of its patterns: written whole they
	// are literal shapes scripts/build-public-repo.sh refuses, and this file is
	// published. The strings the test compares against are identical either way.
	for _, marker := range []string{"sk-", "sk_" + "live_", "rk_" + "live_", "AIza", "ghp_"} {
		if strings.Contains(string(body), marker) {
			t.Fatalf("a %q-shaped value reached the credential list: %s", marker, body)
		}
	}
}

// TestSpokenUtteranceReachesTheRouterAndIsAnnounced walks the whole path in the
// shipped daemon: WebSocket → api → the utterance seam → routing.Router → back
// out as a spoken announcement.
//
// Before this wiring, api.New answered every utterance with
// CodeNotImplemented and the milestone name, because nothing constructed a
// Router. The test asserts the *absence* of that error as much as the presence
// of the announcement — a daemon that silently accepts utterances and does
// nothing with them looks identical from the socket.
func TestSpokenUtteranceReachesTheRouterAndIsAnnounced(t *testing.T) {
	dir := t.TempDir()
	seedDataDir(t, dir)
	base := startDaemon(t, dir)

	ws := strings.Replace(base, "http://", "ws://", 1) + "/v1/ws?token=wiring-token"
	conn, resp, err := websocket.Dial(context.Background(), ws, nil)
	if err != nil {
		t.Fatalf("dial %s: %v (resp %v)", ws, err, resp)
	}
	defer conn.CloseNow()

	send := func(id, typ string, payload any) {
		b, _ := json.Marshal(payload)
		env, _ := json.Marshal(map[string]any{
			"v": 1, "id": id, "type": typ,
			"at": time.Now().UnixMilli(), "payload": json.RawMessage(b),
		})
		if err := conn.Write(context.Background(), websocket.MessageText, env); err != nil {
			t.Fatalf("write %s: %v", typ, err)
		}
	}

	// "new session" is ORCHESTRATOR.md §4's escape hatch: the always-correct
	// manual path, which is what ships before any classifier.
	send("u-1", "utterance", map[string]any{
		"text":  "new session — run the tests on the payments branch",
		"final": true, "source": "glasses", "confidence": 0.95,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var announced bool
	for !announced {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("no announcement before the socket went quiet: %v", err)
		}
		var env struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		switch env.Type {
		case "error":
			t.Fatalf("the daemon refused to route: %s", env.Payload)
		case "speak":
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			if p.Text == "" {
				t.Fatal("an empty announcement is not an announcement")
			}
			t.Logf("announced: %q", p.Text)
			announced = true
		}
	}
}

// TestTheConsoleURLTheDaemonPrintsActuallyServes.
//
// relayd prints "console  http://127.0.0.1:8787/?token=…" on startup. It printed
// that line for an entire milestone while answering it with 404, because
// web.Mount — written for exactly this, with a comment explaining how it
// coexists with the API's routes — was never called.
//
// Advertising an address you do not serve is worse than not advertising one, so
// this asserts the two together: the console answers, and mounting it at "/" did
// not shadow the API.
func TestTheConsoleURLTheDaemonPrintsActuallyServes(t *testing.T) {
	dir := t.TempDir()
	seedDataDir(t, dir)
	base := startDaemon(t, dir)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET / = %d; the daemon prints this URL at startup: %s",
			resp.StatusCode, body)
	}
	if !strings.Contains(strings.ToLower(string(body)), "<html") {
		t.Errorf("GET / did not return the console document: %s", body)
	}

	// web.Mount registers "/" only, the lowest-priority pattern in Go's
	// ServeMux. If that ever stops being true the console silently swallows the
	// API and every screen breaks at once.
	code, _ := get(t, base, "/v1/sessions")
	if code != 200 {
		t.Errorf("GET /v1/sessions = %d after mounting the console at /", code)
	}
	resp, err = http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("GET /healthz = %d after mounting the console at /", resp.StatusCode)
	}
}

// TestTheDaemonReportsWhatItWired is the general form of this file's defect.
//
// The failure this codebase keeps producing is a subsystem that is fully built,
// fully tested, and constructed by nothing: the suite stays green and the
// product does nothing. A unit test for the subsystem cannot catch it and
// neither can a unit test for the composition root, because the bug *is* the
// absence of a call. So the daemon reports what it wired and this asserts it
// from outside the process, through the same health screen the console shows.
//
// Adding a subsystem means adding it here. That is the point.
func TestTheDaemonReportsWhatItWired(t *testing.T) {
	base := startDaemon(t, t.TempDir())
	subs := subsystems(t, base)
	h := struct{ Subsystems map[string]string }{Subsystems: subs}

	// Every subsystem that needs no key or model. Each of these was, at some
	// point on this branch, a fully-tested package that nothing constructed.
	for _, name := range []string{
		// MEMORY.md §9's idle pass — 5,490 tested lines with no caller.
		"compaction",
		// MEMORY.md §4's live ingestion — summarize.Live had no caller, so
		// nothing was summarised except by backfill.
		"summaries",
		// ORCHESTRATOR.md §4b's shared bus.
		"tool_bus",
		// MEMORY.md §6's confirmation queue. The daemon always opens a vault on
		// a temp data dir, so this must read "on" — and when it does not, the
		// console silently falls back to listing the index's raw secret markers
		// and answers every accept with 501, which is a screen that looks like
		// it works.
		"credential_proposals",
		// MEMORY.md §8's second question. This needs no key and no model: a
		// profile source is all a RuntimeRouter takes, so a default daemon must
		// have one. Without it routing.Options.Runtime is nil, and a new
		// session is announced with no runtime however carefully the
		// entitlement table was written.
		"runtime_routing",
	} {
		got, ok := h.Subsystems[name]
		if !ok {
			t.Errorf("the daemon reports no %s subsystem at all; wired: %v", name, h.Subsystems)
			continue
		}
		if got != "on" {
			t.Errorf("%s is %q on a daemon that should have it", name, got)
		}
	}

	// And the ones that legitimately need something this daemon has not got.
	// They must still be *reported*, with the reason: a subsystem missing from
	// this map is one nobody decided about, which is the bug.
	//
	// "entitlements" is here rather than above on purpose: MEMORY.md §8 says
	// the set starts empty and step 3 is then skipped *with a note*. Off is the
	// correct state for a daemon nobody has told, and the note is what stops a
	// user concluding their subscription is being used when it is not.
	for _, name := range []string{
		"work_model", "voice_model", "fact_extraction", "embedder", "entitlements",
		// ORCHESTRATOR.md §4b. This daemon has no config file, so no connector
		// is configured and the honest report is a reason rather than "on" —
		// the proposer would have an empty set and would never propose anything
		// however often something was mentioned. It is here rather than absent
		// because a subsystem missing from this map is one nobody decided
		// about, and "connectors are off on this box" is a decision.
		"connector_proposals",
	} {
		why, ok := h.Subsystems[name]
		if !ok {
			t.Errorf("%s is not reported at all; wired: %v", name, h.Subsystems)
			continue
		}
		if why == "on" {
			t.Errorf("%s claims to be on with no model configured", name)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s is off and does not say why, so the console shows a blank", name)
		}
	}
	if why := h.Subsystems["entitlements"]; !strings.Contains(why, "none recorded") {
		t.Errorf("entitlements = %q on a daemon with no [routing] section; it must say "+
			"nobody has told it, not merely that it is off", why)
	}
}

// subsystems reads /v1/health's report. Every wiring row on this branch lands
// in that map, so it is worth one helper rather than five copies of the decode.
func subsystems(t *testing.T, base string) map[string]string {
	t.Helper()
	code, body := get(t, base, "/v1/health")
	if code != 200 {
		t.Fatalf("GET /v1/health: %d", code)
	}
	var h struct {
		Subsystems map[string]string `json:"subsystems"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return h.Subsystems
}

// ---------------------------------------------- MEMORY.md §6, end to end --

// vaultFixtureKey is a synthetic GitLab PAT. Tier 1, because that is the only
// tier §12.2 lets into the proposal queue, and deliberately none of the four
// literal shapes scripts/build-public-repo.sh's credential guard refuses — this
// file ships in the public repo and testdata/secrets does not.
const vaultFixtureKey = "glpat-Nq7TESTONLYnotarealkey42"

// seedProposal writes one undecided transcript proposal into the vault the
// daemon will open, through internal/vault directly.
//
// Seeding through the real package rather than through SQL is deliberate here:
// the id is an HMAC under the vault's own key and the candidate is sealed with
// it, so a hand-written row could not be accepted at all. Closing the vault
// afterwards matters too — the daemon opens the same file, and on a machine
// with no keychain it reads back the same 0600 key file this call created.
func seedProposal(t *testing.T, dir string) string {
	t.Helper()
	ctx := context.Background()

	// One secret marker in the index too, so the fallback this test is checking
	// for the absence of has something to return. Without it an unwired daemon
	// answers with an empty list and no note, which is indistinguishable from a
	// wired one whose queue happens to be empty — and this test would then be
	// asserting a coincidence rather than the join.
	idx, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	err = idx.PutSecretMarker(ctx, store.SecretMarker{
		ID: "marker-1", Runtime: "claude", SessionID: "sess-historical",
		Path: filepath.Join(dir, "transcript.jsonl"),
		// The shape internal/index writes, tier and all.
		Detector: "gitlab_pat (tier1)", Service: "gitlab", At: time.Now(),
	})
	idx.Close()
	if err != nil {
		t.Fatal(err)
	}

	v, err := vaultOpen(ctx, vault.Options{DBPath: filepath.Join(dir, "vault.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	p, err := v.Proposals().Propose(ctx, vault.Candidate{
		Service:  "gitlab",
		Label:    "GitLab PAT",
		Detector: "gitlab_pat",
		Tier:     index.TierVendor,
		Secret:   vaultFixtureKey,
		Source: vault.Provenance{
			Kind:    vault.SourceTranscript,
			Runtime: "claude",
			Session: "sess-historical",
			Path:    filepath.Join(dir, "transcript.jsonl"),
			At:      time.Now().Add(-90 * 24 * time.Hour),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func post(t *testing.T, base, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer wiring-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// TestAVaultProposalIsAcceptedThroughTheConsole walks MEMORY.md §6 end to end
// in the shipped daemon: a detected key waits as a question, a human answers
// yes through the console's own HTTP surface, and the vault holds it afterwards.
//
// Five subsystems — the detector, the queue, the vault, the audit log and the
// console — were each written, each tested alone, and had never once run in
// sequence. cmd/relayd called api.New without Proposals, so GET
// /v1/credentials/proposals fell through to listing the index's raw secret
// markers and POST .../accept answered 501 after recording a refusal. The
// console has a branch for that exact string, which is why nobody noticed that
// no proposal had ever been accepted on any machine.
//
// The absence of "note" in the list response is as load-bearing as the 200 on
// the accept. That note is the marker fallback's signature: a list that carries
// it is the index's detections dressed as proposals, and it looks identical to
// the real queue from the console's side.
func TestAVaultProposalIsAcceptedThroughTheConsole(t *testing.T) {
	dir := t.TempDir()
	seedDataDir(t, dir)
	id := seedProposal(t, dir)
	base := startDaemon(t, dir)

	code, body := get(t, base, "/v1/credentials/proposals")
	if code != 200 {
		t.Fatalf("GET /v1/credentials/proposals = %d: %s", code, body)
	}
	var list struct {
		Proposals []struct {
			ID       string `json:"id"`
			Service  string `json:"service"`
			Detector string `json:"detector"`
			LastFour string `json:"last_four"`
			Session  string `json:"session"`
			FoundAt  int64  `json:"found_at"`
		} `json:"proposals"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if list.Note != "" {
		t.Fatalf("the proposal list carries the marker fallback's note (%q), so the "+
			"real queue is not wired and these are index detections, not offers", list.Note)
	}
	if len(list.Proposals) != 1 {
		t.Fatalf("got %d proposals, want the one that was seeded: %s", len(list.Proposals), body)
	}
	p := list.Proposals[0]
	if p.ID != id || p.Service != "gitlab" {
		t.Errorf("proposal = %+v, want the seeded gitlab candidate %s", p, id)
	}
	// The console reads the tier out of this string and treats an unknown tier
	// as more dangerous than a tier-1 match — it demands a typed service name
	// before accepting one. A tier-1 hit is the only thing that can be in this
	// queue, so reading "unknown" here would put a danger confirmation in front
	// of every proposal the product will ever make.
	if !strings.Contains(p.Detector, "tier1") {
		t.Errorf("detector = %q; the console parses the tier out of this and would show "+
			"an unknown tier for the only tier that can be here", p.Detector)
	}
	if p.LastFour != vaultFixtureKey[len(vaultFixtureKey)-4:] {
		t.Errorf("last_four = %q, want the last four of the candidate", p.LastFour)
	}
	if p.FoundAt == 0 {
		t.Error("found_at is zero; the proposal line says 'in a session from March' and " +
			"this is where that date comes from")
	}
	if strings.Contains(string(body), vaultFixtureKey) {
		t.Fatal("the proposal list returned the whole candidate")
	}

	// Yes.
	code, body = post(t, base, "/v1/credentials/proposals/"+id+"/accept", `{"label":"GitLab PAT"}`)
	if code == http.StatusNotImplemented {
		t.Fatalf("accept answered 501: the daemon has a vault and no proposal queue, "+
			"which is the state this row exists to end: %s", body)
	}
	if code != 200 {
		t.Fatalf("POST accept = %d: %s", code, body)
	}
	var accepted struct {
		Credential struct {
			ID       string `json:"id"`
			Service  string `json:"service"`
			LastFour string `json:"last_four"`
			Source   string `json:"source"`
		} `json:"credential"`
	}
	if err := json.Unmarshal(body, &accepted); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	credID := accepted.Credential.ID
	if credID == "" {
		t.Fatal("accepting returned no credential")
	}
	if accepted.Credential.LastFour != vaultFixtureKey[len(vaultFixtureKey)-4:] {
		t.Errorf("last_four = %q after accepting", accepted.Credential.LastFour)
	}
	// Provenance survives the accept. MEMORY.md §6 keeps which session and what
	// date precisely so two keys for one service can be told apart later, and a
	// credential that forgot it came out of a transcript is the lie provenance
	// exists to prevent.
	if accepted.Credential.Source != "transcript" {
		t.Errorf("source = %q after accepting, want transcript", accepted.Credential.Source)
	}
	if strings.Contains(string(body), vaultFixtureKey) {
		t.Fatal("accepting echoed the secret back")
	}

	// It is in the vault now, and the question is gone from the queue.
	code, body = get(t, base, "/v1/credentials")
	if code != 200 {
		t.Fatalf("GET /v1/credentials = %d: %s", code, body)
	}
	if !strings.Contains(string(body), credID) {
		t.Errorf("the accepted credential is not in the list: %s", body)
	}
	if strings.Contains(string(body), vaultFixtureKey) {
		t.Fatal("the credential list returned the whole secret")
	}

	code, body = get(t, base, "/v1/credentials/proposals")
	if code != 200 {
		t.Fatalf("GET proposals after accept = %d: %s", code, body)
	}
	list.Proposals = nil
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Proposals) != 0 {
		t.Errorf("the accepted proposal is still an open question: %s", body)
	}

	// And it is the key that was proposed, not merely a row with the right last
	// four. This is the only assertion in the file that opens the plaintext, and
	// it is done outside the daemon on purpose: api.CredentialStore has no
	// Reveal precisely so the HTTP surface cannot answer this question, so the
	// test has to ask the vault directly. Without it, "accepted" could mean the
	// candidate was destroyed and an empty credential written in its place, and
	// every other assertion here would still pass.
	v, err := vaultOpen(context.Background(), vault.Options{DBPath: filepath.Join(dir, "vault.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	secret, err := v.Reveal(context.Background(), credID)
	if err != nil {
		t.Fatalf("the accepted credential does not reveal: %v", err)
	}
	if secret != vaultFixtureKey {
		t.Fatal("the accepted credential is not the candidate that was proposed")
	}

	// DASHBOARD.md §4: the evidence exists in a place the user can see without
	// our help. An accept that leaves no trace, or leaves a denial, is worse
	// than one that did not happen.
	code, body = get(t, base, "/v1/audit")
	if code != 200 {
		t.Fatalf("GET /v1/audit = %d: %s", code, body)
	}
	var log struct {
		Entries []struct {
			Action  string            `json:"action"`
			Outcome string            `json:"outcome"`
			Reason  string            `json:"reason"`
			Detail  map[string]string `json:"detail"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &log); err != nil {
		t.Fatal(err)
	}
	var recorded bool
	for _, e := range log.Entries {
		if e.Action != "credential.proposal.accept" {
			continue
		}
		if e.Outcome == "denied" {
			t.Errorf("the audit log records the accept as denied (%q); that is what a "+
				"daemon with no proposal queue writes", e.Reason)
		}
		if e.Detail["credential"] == credID {
			recorded = true
		}
	}
	if !recorded {
		t.Errorf("no audit entry ties the accept to credential %s: %s", credID, body)
	}
}

// TestAVaultCredentialResolvesForARealModelCall is the other end of MEMORY.md
// §6, and the only test in this tree that proves a stored secret is ever used.
//
// Accepting a proposal is half a feature. A vault whose contents nothing reads
// is a very careful way of storing nothing: the reason §6 accepts the risk of
// keeping keys at all is "so the agents can use them". cmd/relayd resolves a
// `credential = "vault:<id>"` through credentialLookup, and until this test
// nothing anywhere asserted that the secret which comes out the far end of that
// resolution is the one that went in.
//
// The last_used half is the join that had no caller at all. Nothing in the tree
// called vault.Touch, so last_used_at and last_used_by were permanently empty
// and DASHBOARD.md §3.4 — "access nobody has touched in a month is the kind that
// gets forgotten and then exploited" — could never be evaluated on a real box.
func TestAVaultCredentialResolvesForARealModelCall(t *testing.T) {
	dir := t.TempDir()
	seedDataDir(t, dir)

	const secret = "glpat-Nq7MODELCALLnotarealkey99"

	// A stand-in for the model provider that records what it was authenticated
	// with. Counting calls is not optional: if the utterance below ever stops
	// escalating, this test would pass without a single HTTP request — a green
	// tick over a claim nothing checked, which is the exact failure this file
	// exists to catch.
	var mu sync.Mutex
	var auths []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"x","choices":[{"message":{"role":"assistant",`+
			`"content":"the tests pass"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)
	}))
	defer provider.Close()

	// Typed, not proposed: this half of §6 is about consumption, and a key the
	// user pasted in is the cleanest provenance there is.
	ctx := context.Background()
	v, err := vaultOpen(ctx, vault.Options{DBPath: filepath.Join(dir, "vault.db")})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := v.Put(ctx, vault.Input{
		Service: "gitlab", Label: "model key", Secret: secret,
		Source: vault.Provenance{Kind: vault.SourceTyped, At: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	v.Close()

	// config.toml holds a reference and never a secret — that is the whole point
	// of vault:, and the assertion below that the file contains no key is the
	// same rule the installer enforces on the way in.
	cfgPath := filepath.Join(dir, "relay.toml")
	cfg := "listen = \"127.0.0.1:0\"\n\n" +
		"[models.small]\nvendor = \"custom\"\napi = \"openai\"\nbase_url = \"" + provider.URL + "\"\n" +
		"model = \"x\"\ncredential = \"vault:" + entry.ID + "\"\n\n" +
		"[models.big]\nvendor = \"custom\"\napi = \"openai\"\nbase_url = \"" + provider.URL + "\"\n" +
		"model = \"x\"\ncredential = \"vault:" + entry.ID + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg, secret) {
		t.Fatal("the fixture config holds the secret itself, which is what vault: exists to avoid")
	}

	base := startDaemonWithConfig(t, dir, cfgPath)

	// Something that is not on the allowlist, so it escalates to the big model
	// rather than being answered from the registry with no model call at all.
	ws := strings.Replace(base, "http://", "ws://", 1) + "/v1/ws?token=wiring-token"
	conn, resp, err := websocket.Dial(context.Background(), ws, nil)
	if err != nil {
		t.Fatalf("dial %s: %v (resp %v)", ws, err, resp)
	}
	defer conn.CloseNow()

	payload, _ := json.Marshal(map[string]any{
		"text":  "why is the CRC wrong on the glasses link",
		"final": true, "source": "glasses", "confidence": 0.95,
	})
	env, _ := json.Marshal(map[string]any{
		"v": 1, "id": "u-model", "type": "utterance",
		"at": time.Now().UnixMilli(), "payload": json.RawMessage(payload),
	})
	if err := conn.Write(context.Background(), websocket.MessageText, env); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(auths)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	seen := append([]string(nil), auths...)
	mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the model provider was never called, so this test proved nothing about " +
			"the credential — the utterance did not escalate, or no model was built")
	}
	for _, a := range seen {
		if a != "Bearer "+secret {
			t.Fatalf("the provider was authenticated with %q; the vault reference did not "+
				"resolve to the stored secret", a)
		}
	}

	// And the vault noticed. Without the Touch join this reads zero forever, and
	// the console's "last used" column is blank on every credential the daemon
	// has been using all week.
	code, body := get(t, base, "/v1/credentials")
	if code != 200 {
		t.Fatalf("GET /v1/credentials = %d: %s", code, body)
	}
	var list struct {
		Credentials []struct {
			ID         string `json:"id"`
			LastUsedAt int64  `json:"last_used_at"`
			LastUsedBy string `json:"last_used_by"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range list.Credentials {
		if c.ID != entry.ID {
			continue
		}
		found = true
		if c.LastUsedAt == 0 {
			t.Error("last_used_at is zero after a model call resolved this credential; " +
				"DASHBOARD.md §3.4's \"nobody has touched this in a month\" is unanswerable")
		}
		if c.LastUsedBy == "" {
			t.Error("last_used_by is empty, so the console can say a key was used and not " +
				"by what — which is the half of §3.4 that makes it actionable")
		}
	}
	if !found {
		t.Fatalf("the credential is not in the list at all: %s", body)
	}
	if strings.Contains(string(body), secret) {
		t.Fatal("the credential list returned the whole secret")
	}
}

// ------------------------------------------------- MEMORY.md §8, end to end --

// The entitlement table in internal/routing has been correct and tested since
// it was written and had never once decided anything, for two separate reasons
// that both had to be fixed:
//
//  1. nothing anywhere recorded an entitlement, so the set was empty on every
//     machine; and
//  2. cmd/relayd left routing.Options.Runtime nil, so there was no
//     RuntimeRouter to consult a set with. Router.chooseRuntime returned nil
//     unless the user named a runtime out loud, and Decision.Runtime was only
//     ever a copy of an existing session's.
//
// Recording alone would have changed nothing observable, which is why both
// tests below exist: this one proves the declared set reaches the constructed
// router, and the next proves the router names a runtime the daemon speaks.

// TestRecordedEntitlementsReachTheRouter walks the whole chain in one
// assertion, because the health value is read from Router.Entitlements() —
// which reads through the RuntimeRouter, not from any variable in main().
//
// For "on: claude-subscription" to appear, all five of these must hold: the
// [routing] section parsed, main converted it against routing's own table,
// RuntimeOptions carried it, NewRuntimeRouter kept it, and the Router was given
// the RuntimeRouter. Delete any one of them and this goes red.
func TestRecordedEntitlementsReachTheRouter(t *testing.T) {
	dir := t.TempDir()

	cfgPath := filepath.Join(dir, "relay.toml")
	cfg := "listen = \"127.0.0.1:0\"\n\n[routing]\nentitlements = [\"claude-subscription\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	base := startDaemonWithConfig(t, dir, cfgPath)
	subs := subsystems(t, base)

	if got := subs["entitlements"]; !strings.Contains(got, "claude-subscription") {
		t.Errorf("entitlements = %q; the daemon does not know what the user pays for, so "+
			"MEMORY.md §8 step 3 is skipped on a machine that recorded an answer", got)
	}
	if got := subs["runtime_routing"]; got != "on" {
		t.Errorf("runtime_routing = %q; without a runtime router an entitlement cannot "+
			"decide anything however carefully it was recorded", got)
	}
}

// TestARecordedEntitlementNamesTheRuntimeItAnnounces is the end of the chain:
// a spoken sentence comes back naming the runtime the work will go to.
//
// The machine is built rather than borrowed — HOME points at a fixture holding
// .claude/projects/<slug>/*.jsonl, which is what detectClaudeCode counts, and
// PATH holds one executable `claude` that answers --version. That yields
// StatusInUse -> HistorySome, which matters: the never-route-without-history
// rule outranks the entitlement, so a runtime installed and never opened is
// excluded before the table is ever consulted.
//
// Honest about what this does and does not prove. With one usable runtime the
// router would also have reached Claude Code by RuntimeOnlyOne, so this asserts
// that RuntimeChoice reached Decision.Runtime and routing.Announce rendered it
// — none of which can happen while Options.Runtime is nil. The entitlement's
// own effect is asserted by the health test above and by internal/routing's
// table tests. A two-runtime version cannot be built here: the Codex adapter
// dials a real JSON-RPC app-server at startup and the three ACP runtimes
// complete a real handshake, so a stub binary attaches for none of them.
func TestARecordedEntitlementNamesTheRuntimeItAnnounces(t *testing.T) {
	dir := t.TempDir()
	seedDataDir(t, dir)

	// A machine with exactly one runtime, and one that has been used.
	fakeHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fakeHome, ".claude", "projects", "-src-api"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(fakeHome, ".claude", "projects", "-src-api", "one.jsonl"),
		[]byte("{\"type\":\"user\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 2.1.226\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// t.Setenv, so this test can never be t.Parallel: it is rewriting the
	// process's idea of which machine it is on.
	t.Setenv("HOME", fakeHome)
	t.Setenv("PATH", binDir)

	cfgPath := filepath.Join(dir, "relay.toml")
	cfg := "listen = \"127.0.0.1:0\"\n\n[routing]\nentitlements = [\"claude-subscription\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	base := startDaemonWithConfig(t, dir, cfgPath)

	ws := strings.Replace(base, "http://", "ws://", 1) + "/v1/ws?token=wiring-token"
	conn, resp, err := websocket.Dial(context.Background(), ws, nil)
	if err != nil {
		t.Fatalf("dial %s: %v (resp %v)", ws, err, resp)
	}
	defer conn.CloseNow()

	payload, _ := json.Marshal(map[string]any{
		"text":  "new session — run the tests on the payments branch",
		"final": true, "source": "glasses", "confidence": 0.95,
	})
	env, _ := json.Marshal(map[string]any{
		"v": 1, "id": "u-ent", "type": "utterance",
		"at": time.Now().UnixMilli(), "payload": json.RawMessage(payload),
	})
	if err := conn.Write(context.Background(), websocket.MessageText, env); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("no announcement naming a runtime before the socket went quiet: %v", err)
		}
		var e struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(data, &e); err != nil || e.Type != "speak" {
			continue
		}
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		t.Logf("announced: %q", p.Text)
		// The whole sentence, not just the label, and that is not pedantry. A
		// daemon with no runtime router says "Starting a new session." — no
		// label at all — but a daemon whose HOME fixture went missing says
		// "You have not used Claude Code yet. Start there?", which contains the
		// label and is the opposite outcome. Asserting on the substring alone
		// passes for a machine where the never-route rule fired, which is a
		// green tick over a route that did not happen. Verified by pointing
		// HOME at an empty directory and watching this line, and only this
		// line, catch it.
		if !strings.Contains(p.Text, "Starting a new Claude Code session") {
			t.Fatalf("the daemon announced %q; it did not route this work to a runtime", p.Text)
		}
		return
	}
}

// ------------------------------------------ ORCHESTRATOR.md §4b, end to end --

// connectorConfig is a machine with one connector that could be connected.
//
// The address is deliberately dead. Nothing in the proposal path ever contacts
// the printer — proposing, listing, accepting and enumerating tools are all
// decisions about access, not calls to a service — so a test that needed a real
// PrusaLink on the network would be testing the wrong thing and would be
// unrunnable on every machine that does not have one.
//
// min_episodes is 1 rather than the default 3 for the same reason: three
// separate conversations is a property of internal/connector's own tests, and
// repeating it here would only make this test slower at proving the wiring.
func connectorConfig(t *testing.T, dir string) string {
	t.Helper()
	t.Setenv("RELAY_TEST_PRUSA_KEY", "prusalink-TESTONLY-notarealkey")
	path := filepath.Join(dir, "relay.toml")
	body := "listen = \"127.0.0.1:0\"\n\n" +
		"[connectors]\nmin_episodes = 1\n\n" +
		"[connectors.prusa]\n" +
		"address = \"http://127.0.0.1:1\"\n" +
		"credential = \"env:RELAY_TEST_PRUSA_KEY\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mention speaks one sentence naming the Prusa over the real WebSocket.
//
// The utterance path is used rather than the summary path because it is the one
// that can be driven from outside the process on this machine. Producing a turn
// summary needs an agent runtime emitting TurnCompleted and none is installed
// here, so the summary feed — which is wired, in cmd/relayd/memory.go — runs
// zero times in this container and is not what this row's evidence rests on.
func mention(t *testing.T, base, text string) {
	t.Helper()
	ws := strings.Replace(base, "http://", "ws://", 1) + "/v1/ws?token=wiring-token"
	conn, resp, err := websocket.Dial(context.Background(), ws, nil)
	if err != nil {
		t.Fatalf("dial %s: %v (resp %v)", ws, err, resp)
	}
	defer conn.CloseNow()

	payload, _ := json.Marshal(map[string]any{
		"text": text, "final": true, "source": "glasses", "confidence": 0.95,
	})
	env, _ := json.Marshal(map[string]any{
		"v": 1, "id": "u-mention", "type": "utterance",
		"at": time.Now().UnixMilli(), "payload": json.RawMessage(payload),
	})
	if err := conn.Write(context.Background(), websocket.MessageText, env); err != nil {
		t.Fatal(err)
	}

	// Wait for the announcement, which is spoken after routing and before the
	// utterance handler returns. Without it the GET below can race the socket
	// and read the list before the sentence has been observed at all.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("the daemon never answered the utterance: %v", err)
		}
		var e struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &e); err != nil {
			continue
		}
		if e.Type == "error" {
			t.Fatalf("the daemon refused the utterance: %s", data)
		}
		if e.Type == "speak" {
			return
		}
	}
}

type proposalList struct {
	Proposals []struct {
		Connector string   `json:"connector"`
		Title     string   `json:"title"`
		Access    string   `json:"access"`
		Evidence  string   `json:"evidence"`
		Opens     string   `json:"opens"`
		Line      string   `json:"line"`
		Scopes    []string `json:"scopes"`
		Episodes  int      `json:"episodes"`
	} `json:"proposals"`
	Available bool   `json:"available"`
	Note      string `json:"note"`
}

// proposals reads the screen, retrying until it is non-empty or the deadline
// passes. An empty list is a legitimate answer, so the caller says which it
// wanted.
func listProposals(t *testing.T, base string, wantSome bool) proposalList {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, body := get(t, base, "/v1/connectors/proposals")
		if code != 200 {
			t.Fatalf("GET /v1/connectors/proposals = %d: %s", code, body)
		}
		var list proposalList
		if err := json.Unmarshal(body, &list); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		if !list.Available {
			t.Fatalf("the proposals screen reports itself unavailable on a daemon with a "+
				"connector configured (%q); api.Options.ConnectorProposals is not wired, "+
				"so a proposal could be made and never seen", list.Note)
		}
		if !wantSome || len(list.Proposals) > 0 || time.Now().After(deadline) {
			return list
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// busTools lists what the shared MCP bus offers, as Claude Code would see it.
func busTools(t *testing.T, base string) []string {
	t.Helper()
	initialize(t, base)
	out := rpc(t, base, "tools/list", map[string]any{})
	result, _ := out["result"].(map[string]any)
	raw, _ := result["tools"].([]any)
	var names []string
	for _, r := range raw {
		tool, _ := r.(map[string]any)
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	return names
}

func liveGrants(t *testing.T, base string) map[string][]string {
	t.Helper()
	code, body := get(t, base, "/v1/connectors")
	if code != 200 {
		t.Fatalf("GET /v1/connectors = %d: %s", code, body)
	}
	var out struct {
		Connectors []struct {
			Connector string   `json:"connector"`
			Scopes    []string `json:"scopes"`
			Revoked   bool     `json:"revoked"`
		} `json:"connectors"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	live := map[string][]string{}
	for _, c := range out.Connectors {
		if !c.Revoked {
			live[c.Connector] = c.Scopes
		}
	}
	return live
}

// TestAMentionedConnectorIsProposedAndNothingIsAutoGranted walks
// ORCHESTRATOR.md §4b end to end through the shipped daemon.
//
// connector.Proposer was written, tested, and had no caller — and wiring it
// alone would have produced nothing, for three separate reasons this test
// covers in one pass. There was no config section, so Proposer.Set was empty
// and nothing could ever be proposed however often it was mentioned. There was
// no API surface at all — api.Options.Proposals is MEMORY.md §6's *credential*
// queue — so a proposal could not reach the console or the glasses. And the set
// was never registered on the tool bus, so a grant would have written a row and
// changed nothing any of the five runtimes could see: a decision with no
// effect, which is this codebase's own defect wearing a different hat.
//
// The order of the assertions is the argument. Evidence first, then the offer,
// then the absence of a grant, then the grant, then the tools — because §4b's
// first rule is that nothing is auto-granted, and the only way to observe a
// negative is to check it before the thing that would make it positive.
func TestAMentionedConnectorIsProposedAndNothingIsAutoGranted(t *testing.T) {
	dir := t.TempDir()
	seedDataDir(t, dir)
	base := startDaemonWithConfig(t, dir, connectorConfig(t, dir))

	// (a) No evidence, no proposal. This is proposal.go's first documented
	// property, and it is what stops the feature being "a settings screen that
	// talks": a configured connector is not by itself a reason to suggest one.
	if list := listProposals(t, base, false); len(list.Proposals) != 0 {
		t.Fatalf("a connector was proposed before anything was said about it: %+v", list.Proposals)
	}

	// (b) Rule 1, observed where it now actually matters. The gateway takes no
	// API token, so a fresh machine listing zero tools is the load-bearing half
	// of its security story — and until this row, no daemon in any test had a
	// connector configured at all, so that guarantee was only ever checked on a
	// machine where it could not fail.
	if tools := busTools(t, base); len(tools) != 0 {
		t.Fatalf("the bus offers %v with a connector configured and nothing granted", tools)
	}

	// (c) Say it out loud.
	mention(t, base, "the prusa has been chewing through filament all afternoon")

	// (d) And it is offered, with the reason attached.
	list := listProposals(t, base, true)
	if len(list.Proposals) != 1 {
		t.Fatalf("got %d proposals after mentioning the printer, want 1: %+v",
			len(list.Proposals), list)
	}
	p := list.Proposals[0]
	if p.Connector != "prusa" {
		t.Errorf("proposal names %q", p.Connector)
	}
	if p.Access != "read" {
		t.Errorf("access = %q; §4b proposes the read half and only the read half", p.Access)
	}
	if len(p.Scopes) != 1 || p.Scopes[0] != "prusa:read" {
		t.Errorf("scopes = %v, want exactly [prusa:read]", p.Scopes)
	}
	// (e) Rule 2 in the response body. A write half offered alongside the read
	// one is not a second decision, it is the same click — so the write scope
	// must not appear anywhere in what the console is handed.
	if strings.Contains(strings.ToLower(string(mustJSON(t, list))), "prusa:write") {
		t.Errorf("the proposal offers the write half: %+v", p)
	}
	if !strings.Contains(p.Line, "mentioned your Prusa") {
		t.Errorf("line = %q; §4b's sentence is the product and it is built from counts", p.Line)
	}
	if p.Episodes != 1 {
		t.Errorf("episodes = %d after one utterance; an unattributed mention keyed by its "+
			"own timestamp would inflate this", p.Episodes)
	}
	if p.Opens == "" || strings.Contains(strings.ToLower(p.Opens), "read your printer") {
		t.Errorf("opens = %q; a reason that restates the permission is not a reason", p.Opens)
	}

	// (f) The proposal granted nothing. A suggestion that quietly connects
	// something is the exact failure §4b rule 1 exists to name.
	if live := liveGrants(t, base); len(live) != 0 {
		t.Fatalf("making a proposal granted %v", live)
	}

	// (f2) And the endpoint is not a general grant endpoint with a longer path.
	// Rule 1 would leak straight through here: if accept simply granted
	// whatever name it was handed, "nothing is auto-granted" would hold only
	// for connectors nobody had thought to POST.
	if code, body := post(t, base, "/v1/connectors/proposals/gmail/accept", `{}`); code != 404 {
		t.Fatalf("accepting a connector that was never proposed = %d, want 404: %s", code, body)
	}
	if live := liveGrants(t, base); len(live) != 0 {
		t.Fatalf("accepting an unproposed connector granted %v", live)
	}

	// (g) Yes.
	code, body := post(t, base, "/v1/connectors/proposals/prusa/accept", `{}`)
	if code != 200 {
		t.Fatalf("POST accept = %d: %s", code, body)
	}

	// (h) One grant, one half.
	live := liveGrants(t, base)
	if got := live["prusa"]; len(got) != 1 || got[0] != "prusa:read" {
		t.Fatalf("after accepting, prusa's scopes are %v; accepting a proposal must grant "+
			"the read half and nothing else", got)
	}
	if len(listProposals(t, base, false).Proposals) != 0 {
		t.Error("the accepted proposal is still an open question")
	}

	// (i) And the grant reached the bus. Without the Register join this is
	// still zero: the accept button would write a grant row and no agent on the
	// machine would gain a single tool.
	tools := busTools(t, base)
	has := func(name string) bool {
		for _, n := range tools {
			if n == name {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"prusa_status", "prusa_files"} {
		if !has(want) {
			t.Errorf("%s is not on the bus after granting prusa:read; tools = %v", want, tools)
		}
	}
	// The write half was never granted, so the two tools that heat a bed in
	// another room are not callable. Read and write are separate grants and the
	// gateway is where that is enforced.
	for _, never := range []string{"prusa_print", "prusa_stop"} {
		if has(never) {
			t.Errorf("%s is on the bus with only the read half granted; tools = %v", never, tools)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestADismissedProposalStaysDismissedAcrossARestart is the half of §4b that
// cannot be honest without a store.
//
//	A proposal the user said no to is not a proposal to make again next week:
//	unused access is a risk and repeated asking is how blind-accept is trained.
//
// Proposer.dismissed is an in-memory map. Without migration 0004 and the
// Memory seam behind it, "not now" survives exactly as long as the process
// does — so the user is asked again on the next restart, which on a laptop is
// tomorrow morning. Every other assertion in the file above stays green against
// an implementation with no persistence at all; only this one goes red.
func TestADismissedProposalStaysDismissedAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	seedDataDir(t, dir)
	cfgPath := connectorConfig(t, dir)

	base, stop := startStoppableDaemon(t, dir, cfgPath)
	mention(t, base, "the prusa finished the bracket overnight")
	if len(listProposals(t, base, true).Proposals) != 1 {
		t.Fatal("nothing was proposed, so there is nothing to dismiss")
	}

	code, body := post(t, base, "/v1/connectors/proposals/prusa/dismiss",
		`{"reason":"I will connect it later"}`)
	if code != 200 {
		t.Fatalf("POST dismiss = %d: %s", code, body)
	}
	if got := listProposals(t, base, false).Proposals; len(got) != 0 {
		t.Fatalf("the dismissed proposal is still being offered: %+v", got)
	}
	stop()

	// A new daemon on the same data directory, and the same thing said again.
	// The mention is deliberate: a proposal that stayed away because there was
	// no evidence would prove nothing about the dismissal.
	base, _ = startStoppableDaemon(t, dir, cfgPath)
	mention(t, base, "the prusa is printing again this morning")
	if got := listProposals(t, base, false).Proposals; len(got) != 0 {
		t.Fatalf("the daemon proposed a connector the user had already declined: %+v — "+
			"the dismissal lived in a map and died with the process", got)
	}

	// And the evidence itself survived, which is the other half of why the
	// table exists: with DefaultMinEpisodes at 3 over a seven-day window, a
	// daemon that forgets every mention on restart never proposes anything on a
	// machine that reboots daily.
	db, err := store.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sightings int
	if err := db.SQL().QueryRow(`SELECT count(*) FROM connector_sighting`).Scan(&sightings); err != nil {
		t.Fatal(err)
	}
	if sightings < 2 {
		t.Errorf("connector_sighting holds %d rows after two mentions across two daemons", sightings)
	}

	// The sighting table holds no text, and that is asserted against the schema
	// rather than left to a reviewer noticing. Persisting the utterance so the
	// console could "show why" would put unredacted user speech into relay.db —
	// MEMORY.md §6's "detect secrets before indexing, never after" broken at the
	// schema level.
	rows, err := db.SQL().Query(`SELECT name FROM pragma_table_info('connector_sighting')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}
	sort.Strings(cols)
	if strings.Join(cols, ",") != "at,connector,episode" {
		t.Errorf("connector_sighting columns are %v; the evidence sentence is generated "+
			"from counts and this table must never be able to hold what was said", cols)
	}
}
