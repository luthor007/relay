package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/api"
	"github.com/luthor007/relay/relayd/internal/bus"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/voice"
)

// ------------------------------------------------------------ authorization --

// DASHBOARD.md §5: one API for both deployments, so the cloud host is a proxy
// plus an auth layer rather than a second backend — which only holds if the
// authorization decisions do not move with it. Authentication is swapped here
// and the vault check still applies.
func TestAuthenticationSwapsButAuthorizationDoesNot(t *testing.T) {
	readOnly := api.AuthenticatorFunc(func(*http.Request) (api.Identity, error) {
		return api.Identity{
			Kind: "account", Subject: "acct_1", Cloud: true,
			Scopes: []api.Scope{api.ScopeRead}, AuthAt: time.Now(),
		}, nil
	})
	r := newRig(t, api.Options{Authenticator: readOnly, Credentials: newVault(t), Cloud: true})

	// Reading is fine.
	if resp, b := r.get(t, "/v1/credentials"); resp.StatusCode != http.StatusOK {
		t.Fatalf("read = %d: %s", resp.StatusCode, b)
	}
	// Writing to the vault is not, and the refusal is a 403 rather than a 401:
	// the console must not send the user back to log in again.
	resp, b := r.do(t, "POST", "/v1/credentials", `{"service":"x","secret":"`+testSecret+`"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("vault write with a read-only identity = %d, want 403: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "forbidden") {
		t.Fatalf("code = %s", b)
	}

	// And the attempt is on the record. "Somebody without the vault scope tried
	// to add a key" is exactly what the log is for.
	log := auditEntries(t, r)
	var denied bool
	for _, e := range log.Entries {
		if e.Outcome == "denied" && e.ActorID == "acct_1" && e.From != "" {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("a refused vault request left no trace: %+v", log.Entries)
	}
}

// DASHBOARD.md §4, cloud: every vault write is re-authenticated regardless of
// session age.
func TestACloudSessionMustReauthenticateBeforeAVaultWrite(t *testing.T) {
	stale := api.AuthenticatorFunc(func(*http.Request) (api.Identity, error) {
		return api.Identity{
			Kind: "account", Subject: "acct_1", Cloud: true,
			Scopes: api.AllScopes(),
			AuthAt: time.Now().Add(-2 * time.Hour),
		}, nil
	})
	r := newRig(t, api.Options{
		Authenticator: stale, Credentials: newVault(t), Cloud: true,
		VaultReauth: 5 * time.Minute,
	})

	// Reading and ordinary writing are unaffected — session age is only a vault
	// question.
	if resp, _ := r.get(t, "/v1/credentials"); resp.StatusCode != http.StatusOK {
		t.Fatalf("read = %d", resp.StatusCode)
	}
	resp, b := r.do(t, "POST", "/v1/credentials", `{"service":"x","secret":"`+testSecret+`"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stale vault write = %d, want 401: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "reauthenticate") {
		t.Fatalf("the console needs to know it is a re-auth, not a logout: %s", b)
	}
}

// The self-hosted token is presented on every request, so it is always fresh
// and the same rule costs the free tier nothing.
func TestTheSelfHostedTokenIsNeverStale(t *testing.T) {
	r := newRig(t, api.Options{Credentials: newVault(t), VaultReauth: time.Nanosecond})
	resp, b := r.do(t, "POST", "/v1/credentials", `{"service":"x","secret":"`+testSecret+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("token vault write = %d: %s", resp.StatusCode, b)
	}
}

func TestForwardedForIsIgnoredUnlessAProxyIsDeclared(t *testing.T) {
	spoof := func(r *rig) string {
		t.Helper()
		req, _ := http.NewRequest("POST", r.HTTP.URL+"/v1/credentials/x/revoke", nil)
		req.Header.Set("Authorization", "Bearer "+r.Srv.Token())
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		resp, err := r.HTTP.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		log := auditEntries(t, r)
		for _, e := range log.Entries {
			if e.Target == "x" {
				return e.From
			}
		}
		t.Fatalf("no audit entry for the attempt: %+v", log.Entries)
		return ""
	}

	direct := newRig(t, api.Options{Credentials: newVault(t)})
	if from := spoof(direct); from == "203.0.113.9" {
		t.Fatal("a client set its own address in the audit log")
	}

	proxied := newRig(t, api.Options{Credentials: newVault(t), TrustForwardedFor: true})
	if from := spoof(proxied); from != "203.0.113.9" {
		t.Fatalf("with a declared proxy the forwarded address should win, got %q", from)
	}
}

// ---------------------------------------------------------------- sessions --

// DASHBOARD.md §3.1: "every session across all five runtimes, live and
// historical, from the registry and the index".
func TestTheSessionListUnionsTheRegistryAndTheIndex(t *testing.T) {
	r := newRig(t, api.Options{})
	live, _ := r.start(t, "payments")

	insertIndexRow(t, r, "codex", "rollout-7", "/home/u/.codex/rollout-7.jsonl", "the auth migration", 1_700_000_000_000)

	resp, b := r.get(t, "/v1/sessions")
	list := decode[api.SessionList](t, resp, b, http.StatusOK)
	if len(list.Sessions) != 2 {
		t.Fatalf("union = %d sessions: %+v", len(list.Sessions), list.Sessions)
	}

	var fromIndex, fromRegistry *api.SessionSummary
	for i := range list.Sessions {
		switch list.Sessions[i].Source {
		case "index":
			fromIndex = &list.Sessions[i]
		case "registry":
			fromRegistry = &list.Sessions[i]
		}
	}
	if fromIndex == nil || fromRegistry == nil {
		t.Fatalf("sources = %+v", list.Sessions)
	}
	if fromRegistry.ID != live.ID() {
		t.Fatalf("registry row = %+v", fromRegistry)
	}
	if fromIndex.Title != "the auth migration" || fromIndex.Runtime != "codex" {
		t.Fatalf("index row = %+v", fromIndex)
	}
	// The pointer, not a copy.
	if fromIndex.Transcript == nil || fromIndex.Transcript.Path == "" {
		t.Fatalf("the historical row has no transcript pointer: %+v", fromIndex)
	}
	// A historical session is not "closed" — that would claim we saw it end.
	if fromIndex.State != "archived" {
		t.Fatalf("index state = %q", fromIndex.State)
	}

	// source=live drops the historical half.
	resp, b = r.get(t, "/v1/sessions?source=live")
	onlyLive := decode[api.SessionList](t, resp, b, http.StatusOK)
	if len(onlyLive.Sessions) != 1 {
		t.Fatalf("source=live = %+v", onlyLive.Sessions)
	}

	resp, b = r.get(t, "/v1/sessions?source=nonsense")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad source = %d: %s", resp.StatusCode, b)
	}
}

// One conversation must not appear twice because two tiers know about it.
func TestARegistrySessionAndItsIndexRowAreOneRow(t *testing.T) {
	r := newRig(t, api.Options{})
	e, _ := r.start(t, "payments")

	// Give the registry row a native id and index that same id.
	row, err := r.Reg.Session(context.Background(), e.ID())
	if err != nil {
		t.Fatal(err)
	}
	row.NativeID = "cc-native-1"
	if err := r.DB.PutSession(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	insertIndexRow(t, r, "claude-code", "cc-native-1", "/home/u/.claude/cc.jsonl", "payments refactor", 1_700_000_000_000)

	resp, b := r.get(t, "/v1/sessions")
	list := decode[api.SessionList](t, resp, b, http.StatusOK)
	if len(list.Sessions) != 1 {
		t.Fatalf("the same session appeared %d times: %+v", len(list.Sessions), list.Sessions)
	}
	got := list.Sessions[0]
	if got.Source != "both" {
		t.Fatalf("source = %q, want both", got.Source)
	}
	// The live tier keeps the state; the index contributes the transcript
	// pointer and the runtime's own title.
	if got.Subject != "payments" || got.Title != "payments refactor" || got.Transcript == nil {
		t.Fatalf("merged row = %+v", got)
	}
}

func TestBlockedSessionsAreSeparatelyQueryable(t *testing.T) {
	r := newRig(t, api.Options{})
	_, _ = r.start(t, "docs")
	e2, fs2 := r.start(t, "payments")
	fs2.Ask("turn-1", event.InputSpec{Ask: event.InputPermission, Prompt: "may I?"})
	waitFor(t, func() bool { return len(e2.Questions()) == 1 })

	resp, b := r.get(t, "/v1/sessions/blocked")
	got := decode[struct {
		Sessions []api.SessionSummary `json:"sessions"`
		Count    int                  `json:"count"`
	}](t, resp, b, http.StatusOK)

	if got.Count != 1 || len(got.Sessions) != 1 {
		t.Fatalf("blocked = %+v", got)
	}
	if got.Sessions[0].ID != e2.ID() || !got.Sessions[0].Blocked {
		t.Fatalf("blocked session = %+v", got.Sessions[0])
	}
}

// --------------------------------------------------------------- transcript --

// MEMORY.md §3: the index holds a pointer, and the measured corpus is 3.6 GB.
// The read has to be a window, always, with the size fixed before the file is
// opened.
func TestTheTranscriptIsRangeReadAndNeverLoadedWhole(t *testing.T) {
	r := newRig(t, api.Options{})

	// 3 MiB stands in for the 786 MB store: far more than any window.
	path := filepath.Join(t.TempDir(), "big.jsonl")
	line := strings.Repeat("x", 255) + "\n"
	var sb strings.Builder
	for sb.Len() < 3<<20 {
		sb.WriteString(line)
	}
	whole := sb.String()
	if err := os.WriteFile(path, []byte(whole), 0o600); err != nil {
		t.Fatal(err)
	}
	insertIndexRow(t, r, "claude-code", "big-1", path, "a long one", 1_700_000_000_000)

	resp, b := r.get(t, "/v1/sessions/claude-code/big-1/transcript")
	first := decode[api.TranscriptChunk](t, resp, b, http.StatusOK)

	if first.Length != api.TranscriptWindow {
		t.Fatalf("default window = %d bytes, want %d", first.Length, api.TranscriptWindow)
	}
	if int64(len(b)) > 2*api.TranscriptWindow {
		t.Fatalf("the response was %d bytes for a %d-byte window", len(b), api.TranscriptWindow)
	}
	if first.Size != int64(len(whole)) {
		t.Fatalf("size = %d, want %d", first.Size, len(whole))
	}
	if first.EOF || first.NextOffset != api.TranscriptWindow {
		t.Fatalf("first window = %+v", first)
	}
	if first.Text != whole[:api.TranscriptWindow] {
		t.Fatal("the window is not the start of the file")
	}

	// The next window continues where the first stopped.
	resp, b = r.get(t, fmt.Sprintf("/v1/sessions/claude-code/big-1/transcript?offset=%d&limit=100", first.NextOffset))
	second := decode[api.TranscriptChunk](t, resp, b, http.StatusOK)
	if second.Length != 100 || second.Text != whole[api.TranscriptWindow:api.TranscriptWindow+100] {
		t.Fatalf("second window = %+v", second)
	}

	// An oversized ask is clamped rather than refused.
	resp, b = r.get(t, "/v1/sessions/claude-code/big-1/transcript?limit=999999999")
	clamped := decode[api.TranscriptChunk](t, resp, b, http.StatusOK)
	if clamped.Length != api.MaxTranscriptWindow {
		t.Fatalf("clamped window = %d, want %d", clamped.Length, api.MaxTranscriptWindow)
	}
}

// Hermes and OpenClaw keep every session in one store, so the offset is into
// the session, not the file.
func TestTheOffsetIsRelativeToTheSessionNotTheFile(t *testing.T) {
	r := newRig(t, api.Options{})
	path := filepath.Join(t.TempDir(), "shared.jsonl")
	body := "OTHER PERSON'S SESSION\nMINE STARTS HERE\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	offset := int64(strings.Index(body, "MINE"))
	insertIndexRowAt(t, r, "hermes", "h-2", path, "mine", offset)

	resp, b := r.get(t, "/v1/sessions/hermes/h-2/transcript")
	chunk := decode[api.TranscriptChunk](t, resp, b, http.StatusOK)
	if !strings.HasPrefix(chunk.Text, "MINE STARTS HERE") {
		t.Fatalf("the window started at the file, not the session: %q", chunk.Text)
	}
	if chunk.SessionOffset != offset || !chunk.EOF {
		t.Fatalf("chunk = %+v", chunk)
	}
}

func TestTranscriptMarkersTravelWithTheText(t *testing.T) {
	r := newRig(t, api.Options{})
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	insertIndexRow(t, r, "claude-code", "s-1", path, "a session", 1_700_000_000_000)
	if _, err := r.DB.SQL().Exec(`
		INSERT INTO secret_marker (id, runtime, session_id, path, byte_offset, detector, service, vault_id, at)
		VALUES ('m1','claude-code','s-1',?,0,'stripe_secret_key (tier 1)','stripe','',1)`, path); err != nil {
		t.Fatal(err)
	}

	resp, b := r.get(t, "/v1/sessions/claude-code/s-1/transcript")
	chunk := decode[api.TranscriptChunk](t, resp, b, http.StatusOK)
	if len(chunk.Markers) != 1 || chunk.Markers[0].Service != "stripe" || chunk.Markers[0].Captured {
		t.Fatalf("markers = %+v", chunk.Markers)
	}
}

func TestATranscriptPathThatIsNotAbsoluteIsRefused(t *testing.T) {
	r := newRig(t, api.Options{})
	insertIndexRow(t, r, "codex", "rel-1", "../../etc/passwd", "nope", 1)

	resp, b := r.get(t, "/v1/sessions/codex/rel-1/transcript")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, b)
	}
}

func TestAnUnindexedSessionSaysSoRatherThanFailing(t *testing.T) {
	r := newRig(t, api.Options{})
	e, _ := r.start(t, "payments")
	resp, b := r.get(t, "/v1/sessions/"+e.ID()+"/transcript")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "Backfill") {
		t.Fatalf("the message should name what has not run yet: %s", b)
	}
}

// -------------------------------------------------------------------- facts --

func TestFactsRenderEmptyRatherThanFourOhFourWithNoStore(t *testing.T) {
	r := newRig(t, api.Options{}, withoutDB)
	resp, b := r.get(t, "/v1/facts")
	list := decode[api.FactList](t, resp, b, http.StatusOK)
	if list.Available || list.Note == "" {
		t.Fatalf("no store should render empty with a reason: %+v", list)
	}
	if list.Facts == nil {
		t.Fatal("facts is null rather than [], which the screen has to special-case")
	}
}

func TestFactsCarryEvidenceAndHideSupersededBehindAToggle(t *testing.T) {
	r := newRig(t, api.Options{})
	insertFact(t, r, "f1", "prefers", "supabase", "prefers Supabase over Firebase", 0)
	insertFact(t, r, "f0", "uses", "firebase", "uses Firebase", 1_600_000_000_000)
	insertEvidence(t, r, "e1", "f1", "claude-code", "s-1", "we moved to supabase")

	resp, b := r.get(t, "/v1/facts")
	list := decode[api.FactList](t, resp, b, http.StatusOK)
	if len(list.Facts) != 1 || list.Facts[0].ID != "f1" {
		t.Fatalf("live facts = %+v", list.Facts)
	}
	if len(list.Superseded) != 0 {
		t.Fatalf("superseded facts leaked into the default view: %+v", list.Superseded)
	}
	if len(list.Facts[0].Evidence) != 1 || list.Facts[0].Evidence[0].Quote == "" {
		t.Fatalf("evidence = %+v", list.Facts[0].Evidence)
	}
	if list.Facts[0].Evidence[0].At == 0 {
		t.Fatal("evidence with no date cannot answer 'when did you learn this'")
	}

	// "You used to use Firebase" has to still be answerable.
	resp, b = r.get(t, "/v1/facts?superseded=1")
	withHistory := decode[api.FactList](t, resp, b, http.StatusOK)
	if len(withHistory.Superseded) != 1 || withHistory.Superseded[0].ID != "f0" {
		t.Fatalf("history = %+v", withHistory.Superseded)
	}
	if !withHistory.Superseded[0].Superseded || withHistory.Superseded[0].SupersededAt == 0 {
		t.Fatalf("superseded fact = %+v", withHistory.Superseded[0])
	}
}

// MEMORY.md §5: editable, not just deletable.
func TestAFactCanBeCorrectedInOneField(t *testing.T) {
	r := newRig(t, api.Options{})
	insertFact(t, r, "f1", "uses", "postgres", "uses Postgres for everything", 0)

	resp, b := r.do(t, "PATCH", "/v1/facts/f1", `{"text":"uses Postgres for the API only"}`)
	got := decode[struct {
		Fact api.FactView `json:"fact"`
	}](t, resp, b, http.StatusOK)

	if got.Fact.Text != "uses Postgres for the API only" {
		t.Fatalf("text = %q", got.Fact.Text)
	}
	if got.Fact.EditedAt == 0 {
		t.Fatal("edited_at is unset, so the extractor cannot tell a human has been here")
	}
	// The object was not touched, and a PATCH must not blank what it was not
	// given.
	if got.Fact.Object != "postgres" {
		t.Fatalf("object = %q", got.Fact.Object)
	}

	if resp, b := r.do(t, "PATCH", "/v1/facts/f1", `{"text":""}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("blanking a fact = %d: %s", resp.StatusCode, b)
	}
	if resp, _ := r.do(t, "PATCH", "/v1/facts/nope", `{"text":"x"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("editing a missing fact = %d", resp.StatusCode)
	}
}

func TestDeletingAFactKeepsItOutOfEveryView(t *testing.T) {
	r := newRig(t, api.Options{})
	insertFact(t, r, "f1", "uses", "vercel", "deploys on Vercel", 0)

	if resp, b := r.do(t, "DELETE", "/v1/facts/f1", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d: %s", resp.StatusCode, b)
	}
	resp, b := r.get(t, "/v1/facts?superseded=1")
	list := decode[api.FactList](t, resp, b, http.StatusOK)
	if len(list.Facts) != 0 || len(list.Superseded) != 0 {
		t.Fatalf("a deleted fact is still visible: %+v", list)
	}
	// Deleting twice is a 404, not a silent success: the console should not show
	// a row that is already gone as freshly removed.
	if resp, _ := r.do(t, "DELETE", "/v1/facts/f1", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete = %d", resp.StatusCode)
	}
}

// ------------------------------------------------------ connectors and MCP --

func TestConnectorsStateWhatTheyOpenAndFlagUnusedAccess(t *testing.T) {
	r := newRig(t, api.Options{})
	now := time.Now()

	insertGrant(t, r, "g1", "gmail", `["gmail.readonly","gmail.send"]`,
		now.Add(-90*24*time.Hour), now.Add(-60*24*time.Hour))
	insertGrant(t, r, "g2", "github", `["repo:read"]`, now.Add(-3*24*time.Hour), now.Add(-time.Hour))

	resp, b := r.get(t, "/v1/connectors")
	list := decode[api.ConnectorList](t, resp, b, http.StatusOK)
	if len(list.Connectors) != 2 {
		t.Fatalf("connectors = %+v", list.Connectors)
	}

	byName := map[string]api.ConnectorView{}
	for _, c := range list.Connectors {
		byName[c.Connector] = c
	}

	gmail := byName["gmail"]
	// ORCHESTRATOR.md §4b: scope in the user's words, and read and write are
	// separate grants — a reason that restates the permission is not a reason.
	if len(gmail.Opens) != 2 {
		t.Fatalf("opens = %+v", gmail.Opens)
	}
	if gmail.Opens[0] == "gmail.readonly" || !strings.Contains(strings.ToLower(gmail.Opens[0]), "read") {
		t.Fatalf("the read scope was not put into words: %q", gmail.Opens[0])
	}
	if !strings.Contains(strings.ToLower(gmail.Opens[1]), "send") {
		t.Fatalf("the send scope was not put into words: %q", gmail.Opens[1])
	}
	// Access nobody has touched in a month says so itself.
	if !gmail.Unused || gmail.UnusedDays < 55 {
		t.Fatalf("60-day-idle gmail = %+v", gmail)
	}
	if byName["github"].Unused {
		t.Fatalf("github was used an hour ago: %+v", byName["github"])
	}
}

func TestRevokingAConnectorSaysWhichRuntimesItReached(t *testing.T) {
	r := newRig(t, api.Options{})
	insertGrant(t, r, "g1", "gmail", `["gmail.readonly"]`, time.Now(), time.Now())

	// Nothing wired to rewrite the runtimes' configs: the grant is revoked here
	// and the response must not claim more than that.
	resp, b := r.do(t, "POST", "/v1/connectors/g1/revoke", "")
	got := decode[struct {
		Connector string           `json:"connector"`
		Revoke    api.RevokeResult `json:"revoke"`
	}](t, resp, b, http.StatusOK)

	if got.Connector != "gmail" {
		t.Fatalf("connector = %q", got.Connector)
	}
	if len(got.Revoke.Runtimes) != len(adapter.Runtimes()) {
		t.Fatalf("a revoke has to answer for all five runtimes: %+v", got.Revoke.Runtimes)
	}
	for _, rt := range got.Revoke.Runtimes {
		if rt.Reached {
			t.Fatalf("%s was reported reached with nothing wired: %+v", rt.Runtime, rt)
		}
		if rt.Reason == "" {
			t.Fatalf("%s was not reached and does not say why", rt.Runtime)
		}
	}

	// The grant row is revoked either way — the failure to avoid is a connector
	// our own table still calls live.
	resp, b = r.get(t, "/v1/connectors")
	list := decode[api.ConnectorList](t, resp, b, http.StatusOK)
	if !list.Connectors[0].Revoked {
		t.Fatalf("the grant is still live: %+v", list.Connectors[0])
	}

	// And it is audited, attempt first.
	log := auditEntries(t, r)
	var attempted, ok bool
	for _, e := range log.Entries {
		if e.Action != "connector.revoke" {
			continue
		}
		if e.Outcome == "attempted" {
			attempted = true
		}
		if e.Outcome == "ok" {
			ok = true
		}
	}
	if !attempted || !ok {
		t.Fatalf("connector revoke audit = %+v", log.Entries)
	}
}

func TestRevokingAConnectorReachesAllFiveWhenSomethingIsWired(t *testing.T) {
	r := newRig(t, api.Options{
		Connectors: revokerFunc(func(_ context.Context, connector string) (api.RevokeResult, error) {
			var res api.RevokeResult
			for _, rt := range adapter.Runtimes() {
				res.Runtimes = append(res.Runtimes, api.RuntimeRevoke{Runtime: string(rt), Reached: true})
			}
			res.Sessions = []string{"live-1"}
			return res, nil
		}),
	})
	insertGrant(t, r, "g1", "gmail", `["gmail.readonly"]`, time.Now(), time.Now())

	resp, b := r.do(t, "POST", "/v1/connectors/g1/revoke", "")
	got := decode[struct {
		Revoke api.RevokeResult `json:"revoke"`
	}](t, resp, b, http.StatusOK)
	for _, rt := range got.Revoke.Runtimes {
		if !rt.Reached {
			t.Fatalf("%s not reached", rt.Runtime)
		}
	}
	// ORCHESTRATOR.md §4b's catch: a session already running may not re-read its
	// tool list, so the response says which ones were dealt with.
	if len(got.Revoke.Sessions) != 1 {
		t.Fatalf("sessions = %+v", got.Revoke.Sessions)
	}
}

func TestRevokingAConnectorThatIsNotThere(t *testing.T) {
	r := newRig(t, api.Options{})
	resp, b := r.do(t, "POST", "/v1/connectors/nope/revoke", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
}

type revokerFunc func(ctx context.Context, connector string) (api.RevokeResult, error)

func (f revokerFunc) Revoke(ctx context.Context, c string) (api.RevokeResult, error) {
	return f(ctx, c)
}

// MEMORY.md §7: the union, and the distinction between "none" and "we could not
// tell".
func TestTheMCPRegistryIsTheReconciledUnion(t *testing.T) {
	inv := detect.MCPInventory{
		Origins: []detect.MCPOrigin{
			{Runtime: adapter.ClaudeCode, Origin: "claude mcp list --json", Readable: true,
				Servers: []detect.MCPServer{{Name: "gmail", Command: "npx", Args: []string{"gmail-mcp"}}}},
			{Runtime: adapter.OpenClaw, Readable: false, Reason: "not installed"},
		},
		Servers: []detect.MCPEntry{{
			MCPServer: detect.MCPServer{Name: "gmail", Command: "npx", Args: []string{"gmail-mcp"}, Transport: "stdio"},
			Runtimes:  []adapter.Runtime{adapter.ClaudeCode, adapter.Codex},
			Names: map[adapter.Runtime]string{
				adapter.ClaudeCode: "gmail",
				adapter.Codex:      "google-mail",
			},
		}},
	}
	r := newRig(t, api.Options{
		MCP: func(context.Context) (detect.MCPInventory, error) { return inv, nil },
	})

	resp, b := r.get(t, "/v1/mcp")
	got := decode[struct {
		MCP api.MCPView `json:"mcp"`
	}](t, resp, b, http.StatusOK)

	if !got.MCP.Probed || len(got.MCP.Servers) != 1 {
		t.Fatalf("mcp = %+v", got.MCP)
	}
	srv := got.MCP.Servers[0]
	if !srv.Shared || len(srv.Runtimes) != 2 {
		t.Fatalf("a server in two runtimes should be shared: %+v", srv)
	}
	// The same server named three different things has to show all three, or
	// the user cannot tell which row is theirs.
	if srv.Names["codex"] != "google-mail" {
		t.Fatalf("per-runtime names = %+v", srv.Names)
	}
	if srv.Display != "npx gmail-mcp" {
		t.Fatalf("display = %q", srv.Display)
	}
	if len(got.MCP.Unreadable) != 1 || !strings.Contains(got.MCP.Unreadable[0], "openclaw") {
		t.Fatalf("unreadable = %+v", got.MCP.Unreadable)
	}
	if !strings.Contains(got.MCP.Headline, "MCP server") {
		t.Fatalf("headline = %q", got.MCP.Headline)
	}
}

func TestNoMCPPassIsNotTheSameAsNoServers(t *testing.T) {
	r := newRig(t, api.Options{})
	resp, b := r.get(t, "/v1/mcp")
	got := decode[struct {
		MCP  api.MCPView `json:"mcp"`
		Note string      `json:"note"`
	}](t, resp, b, http.StatusOK)
	if got.MCP.Probed {
		t.Fatal("an unprobed registry claims to have been probed")
	}
	if got.Note == "" {
		t.Fatal("an empty registry with no explanation reads as 'you have none'")
	}
}

// ---------------------------------------------------- runtimes and health --

func TestHealthReportsWhatDetectionFoundOnDisk(t *testing.T) {
	sessions := 27
	bytes := int64(2_500_000_000)
	r := newRig(t, api.Options{
		Machine: func(context.Context) (detect.Report, error) {
			return detect.Report{Findings: []detect.Finding{
				{
					Runtime: adapter.Hermes, Installed: true, BinaryName: "hermes",
					BinaryPath: "/usr/local/bin/hermes", Version: "1.2.3",
					StateDir: "/home/u/.hermes", StateDirExists: true,
					StateDirSource: detect.SourceAsked,
					Sessions:       &sessions, StoreBytes: &bytes,
					Running: []detect.Process{{PID: 42}},
				},
				{Runtime: adapter.OpenClaw, StateDirSource: detect.SourceDefault},
			}}, nil
		},
	})

	resp, b := r.get(t, "/v1/health")
	h := decode[api.Health](t, resp, b, http.StatusOK)
	if len(h.Runtimes) != 5 {
		t.Fatalf("health must list all five runtimes, got %d", len(h.Runtimes))
	}

	byName := map[string]api.RuntimeState{}
	for _, rt := range h.Runtimes {
		byName[rt.Runtime] = rt
	}
	hermes := byName["hermes"]
	if !hermes.Detected || !hermes.Installed || hermes.Status != "in_use" {
		t.Fatalf("hermes = %+v", hermes)
	}
	if hermes.Version != "1.2.3" || hermes.Running != 1 {
		t.Fatalf("hermes = %+v", hermes)
	}
	if hermes.Stored == nil || *hermes.Stored != 27 {
		t.Fatalf("stored sessions = %v", hermes.Stored)
	}
	if !hermes.StateDirTrusted {
		t.Fatal("a state dir the runtime told us is a fact, not a guess")
	}
	// A runtime detection did not cover is unknown, not absent.
	if byName["codex"].Detected {
		t.Fatalf("codex = %+v", byName["codex"])
	}
	// And the one whose path we assumed says so.
	if byName["openclaw"].StateDirTrusted {
		t.Fatal("a defaulted ~/.openclaw path was reported as trusted")
	}
}

func TestHealthSaysWhenTheAuditTrailIsNotDurable(t *testing.T) {
	r := newRig(t, api.Options{})
	resp, b := r.get(t, "/v1/health")
	h := decode[api.Health](t, resp, b, http.StatusOK)
	if h.Audit.Durable {
		t.Fatal("the default in-memory log claims durability")
	}
	if h.Audit.Note == "" {
		t.Fatal("a non-durable audit trail with no warning reads as a working one")
	}
}

// DASHBOARD.md §3.5: the re-probe button, because "my glasses stopped talking"
// is almost always an expired credential.
func TestTheReprobeButtonNamesTheExpiredCredential(t *testing.T) {
	r := newRig(t, api.Options{
		Prober: api.ProberFunc(func(context.Context) api.Probes {
			return api.Probes{
				Models: map[string]llm.ProbeResult{
					"small": {Vendor: "openrouter", Model: "openai/gpt-5.6-luna", Reason: llm.ReasonOK,
						Latency: 120 * time.Millisecond, At: time.Now()},
					"big": {Vendor: "openrouter", Model: "anthropic/opus-5", Reason: llm.ReasonExpired,
						Detail: "HTTP 401: no credit", At: time.Now()},
				},
				Voice: []voice.Check{
					{Option: "simba", Label: "Simba 3.2", Probed: true, Reason: llm.ReasonOK, Bytes: 4096},
				},
			}
		}),
	})

	// Before anything probes, health says nothing rather than guessing.
	resp, b := r.get(t, "/v1/health")
	if h := decode[api.Health](t, resp, b, http.StatusOK); h.Probe != nil {
		t.Fatalf("health invented a probe result: %+v", h.Probe)
	}

	resp, b = r.do(t, "POST", "/v1/health/probe", "")
	rep := decode[api.ProbeReport](t, resp, b, http.StatusOK)
	if rep.OK {
		t.Fatal("one model is expired and the report says everything is fine")
	}
	if len(rep.Models) != 2 || rep.Models[0].Role != "small" || rep.Models[1].Role != "big" {
		t.Fatalf("models = %+v", rep.Models)
	}
	if !rep.Models[0].OK || rep.Models[1].OK {
		t.Fatalf("model outcomes = %+v", rep.Models)
	}
	if rep.Models[1].Reason != "expired" || !strings.Contains(rep.Models[1].Detail, "401") {
		t.Fatalf("the provider's own words were lost: %+v", rep.Models[1])
	}
	if len(rep.Voice) != 1 || !rep.Voice[0].OK {
		t.Fatalf("voice = %+v", rep.Voice)
	}

	// The page now already says which one, without pressing anything.
	resp, b = r.get(t, "/v1/health")
	h := decode[api.Health](t, resp, b, http.StatusOK)
	if h.Probe == nil || h.Probe.OK {
		t.Fatalf("health did not cache the probe: %+v", h.Probe)
	}
	if h.Probe.Models[1].Reason != "expired" {
		t.Fatalf("cached probe = %+v", h.Probe.Models)
	}
}

func TestReprobingWithNoModelClientSaysSo(t *testing.T) {
	r := newRig(t, api.Options{})
	resp, b := r.do(t, "POST", "/v1/health/probe", "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
}

func TestHealthNamesTheConfiguredModelsAndVoiceWithoutTheSecrets(t *testing.T) {
	r := newRig(t, api.Options{Setup: &api.Setup{
		Small: api.ModelSetup{Vendor: "openrouter", Model: "openai/gpt-5.6-luna", Credential: "env:OPENROUTER_API_KEY"},
		Big:   api.ModelSetup{Vendor: "openrouter", Model: "anthropic/opus-5", Credential: "vault:c1"},
		Voice: api.VoiceSetup{Provider: "simba", Model: "simba-3.2", Fallback: "edge-tts"},
	}})
	resp, b := r.get(t, "/v1/health")
	h := decode[api.Health](t, resp, b, http.StatusOK)
	if h.Setup == nil || h.Setup.Big.Model != "anthropic/opus-5" {
		t.Fatalf("setup = %+v", h.Setup)
	}
	if h.Setup.Voice.Fallback == "" {
		t.Fatal("the keyless fallback is what stops the device being mute; it must be shown")
	}
	// References only, the same rule config.toml itself follows.
	if !strings.HasPrefix(h.Setup.Small.Credential, "env:") {
		t.Fatalf("credential = %q", h.Setup.Small.Credential)
	}
}

// ------------------------------------------------------------------ billing --

func TestBillingIsALinkOnCloudAndAbsentOnSelfHosted(t *testing.T) {
	self := newRig(t, api.Options{})
	resp, b := self.get(t, "/v1/billing/portal")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("self-hosted billing = %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "self_hosted") {
		t.Fatalf("the code should let the console hide the tab: %s", b)
	}

	expires := time.Now().Add(15 * time.Minute)
	cloud := newRig(t, api.Options{
		Cloud: true,
		Billing: api.BillingPortalFunc(func(_ context.Context, id api.Identity) (string, time.Time, error) {
			return "https://billing.stripe.com/p/session/test_123", expires, nil
		}),
	})
	resp, b = cloud.get(t, "/v1/billing/portal")
	link := decode[api.BillingLink](t, resp, b, http.StatusOK)
	if !strings.HasPrefix(link.URL, "https://billing.stripe.com/") || link.Provider != "stripe" {
		t.Fatalf("link = %+v", link)
	}
	if link.ExpiresAt != expires.UnixMilli() {
		t.Fatalf("expiry = %d", link.ExpiresAt)
	}
}

// ------------------------------------------------------------ live updates --

// DASHBOARD.md §5's optimistic UI needs to learn what actually landed.
func TestConsoleMutationsReachTheSSEStream(t *testing.T) {
	r := newRig(t, api.Options{Credentials: newVault(t)})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", r.HTTP.URL+"/v1/events?token="+r.Srv.Token(), nil)
	resp, err := r.HTTP.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	events := readSSE(resp)
	waitEvent(t, events, "event: sessions")

	r.do(t, "POST", "/v1/credentials", `{"service":"stripe","secret":"`+testSecret+`"}`)

	frame := waitEvent(t, events, "event: credential")
	if strings.Contains(frame, testSecret) {
		t.Fatalf("the live stream carried a secret: %s", frame)
	}
	var ev api.ConsoleEvent
	data := frame[strings.Index(frame, "data: ")+len("data: "):]
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatalf("%v: %s", err, frame)
	}
	if ev.Action != "credential.add" || ev.Outcome != "ok" {
		t.Fatalf("event = %+v", ev)
	}
}

// ---------------------------------------------------------------- helpers --

func insertIndexRow(t *testing.T, r *rig, runtime, sessionID, path, title string, at int64) {
	t.Helper()
	insertIndexRowAt(t, r, runtime, sessionID, path, title, 0)
	if _, err := r.DB.SQL().Exec(
		`UPDATE session_index SET started_at = ?, ended_at = ? WHERE runtime = ? AND session_id = ?`,
		at, at, runtime, sessionID); err != nil {
		t.Fatal(err)
	}
}

func insertIndexRowAt(t *testing.T, r *rig, runtime, sessionID, path, title string, offset int64) {
	t.Helper()
	_, err := r.DB.SQL().Exec(`
		INSERT INTO session_index (id, runtime, session_id, path, byte_offset, title,
		                           workspace, git_branch, model, started_at, ended_at,
		                           message_count, tool_call_count, source_mtime, source_size, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, '', '', '', 1, 1, 0, 0, 0, 0, 1)`,
		runtime+"/"+sessionID, runtime, sessionID, path, offset, title)
	if err != nil {
		t.Fatal(err)
	}
}

func insertFact(t *testing.T, r *rig, id, predicate, object, text string, supersededAt int64) {
	t.Helper()
	var superseded any
	if supersededAt != 0 {
		superseded = supersededAt
	}
	_, err := r.DB.SQL().Exec(`
		INSERT INTO fact (id, subject, predicate, object, text, confidence, first_seen, last_seen, superseded_at)
		VALUES (?, 'user', ?, ?, ?, 0.8, 1, 2, ?)`,
		id, predicate, object, text, superseded)
	if err != nil {
		t.Fatal(err)
	}
}

func insertEvidence(t *testing.T, r *rig, id, factID, runtime, session, quote string) {
	t.Helper()
	_, err := r.DB.SQL().Exec(`
		INSERT INTO fact_evidence (id, fact_id, runtime, session_id, path, byte_offset, quote, at)
		VALUES (?, ?, ?, ?, '/home/u/.claude/s.jsonl', 128, ?, ?)`,
		id, factID, runtime, session, quote, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
}

func insertGrant(t *testing.T, r *rig, id, connector, scopes string, granted, used time.Time) {
	t.Helper()
	_, err := r.DB.SQL().Exec(
		`INSERT INTO "grant" (id, connector, scopes, granted_at, last_used_at) VALUES (?, ?, ?, ?, ?)`,
		id, connector, scopes, granted.UnixMilli(), used.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
}

// readSSE splits a live event stream into frames.
func readSSE(resp *http.Response) chan string {
	events := make(chan string, 32)
	go func() {
		defer close(events)
		buf := make([]byte, 4096)
		var acc strings.Builder
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				acc.WriteString(string(buf[:n]))
				for {
					s := acc.String()
					i := strings.Index(s, "\n\n")
					if i < 0 {
						break
					}
					select {
					case events <- s[:i]:
					default:
					}
					acc.Reset()
					acc.WriteString(s[i+2:])
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return events
}

// A historical row on the sessions list has to be clickable. The registry never
// drove it, so the detail comes from the index — with the transcript pointer,
// and with no invented turns.
func TestAHistoricalSessionHasDetailFromTheIndex(t *testing.T) {
	r := newRig(t, api.Options{})
	insertIndexRow(t, r, "codex", "rollout-7", "/home/u/.codex/rollout-7.jsonl", "the auth migration", 1_700_000_000_000)

	resp, b := r.get(t, "/v1/sessions/codex/rollout-7")
	d := decode[api.SessionDetail](t, resp, b, http.StatusOK)
	if d.Session.Title != "the auth migration" || d.Session.Source != "index" {
		t.Fatalf("session = %+v", d.Session)
	}
	if d.Transcript == nil || d.Transcript.Path == "" {
		t.Fatalf("no transcript pointer: %+v", d)
	}
	// MEMORY.md §3 keeps a pointer, not a copy: there are no turns to show and
	// the screen must not be handed an invented set.
	if len(d.Turns) != 0 || len(d.Tools) != 0 {
		t.Fatalf("the index tier produced turns from nowhere: %+v", d)
	}

	resp, _ = r.get(t, "/v1/sessions/codex/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// DASHBOARD.md §3.5 asks for "installed, authenticated and running". Nothing
// observes the middle one yet, so it must read as unknown rather than as a
// claim in either direction.
func TestRuntimeAuthenticationIsUnknownRatherThanGuessed(t *testing.T) {
	r := newRig(t, api.Options{})
	resp, b := r.get(t, "/v1/runtimes")
	got := decode[struct {
		Runtimes []api.RuntimeState `json:"runtimes"`
	}](t, resp, b, http.StatusOK)

	for _, rt := range got.Runtimes {
		if rt.Authenticated != nil {
			t.Fatalf("%s claims to know its login state: %+v", rt.Runtime, rt)
		}
		if rt.AuthNote == "" {
			t.Fatalf("%s is unknown and does not say why", rt.Runtime)
		}
	}

	yes := true
	wired := newRig(t, api.Options{
		RuntimeAuth: func(context.Context) (map[string]api.RuntimeAuth, error) {
			return map[string]api.RuntimeAuth{"claude-code": {OK: yes}}, nil
		},
	})
	resp, b = wired.get(t, "/v1/runtimes")
	got = decode[struct {
		Runtimes []api.RuntimeState `json:"runtimes"`
	}](t, resp, b, http.StatusOK)
	for _, rt := range got.Runtimes {
		switch rt.Runtime {
		case "claude-code":
			if rt.Authenticated == nil || !*rt.Authenticated {
				t.Fatalf("claude-code = %+v", rt)
			}
		default:
			if rt.Authenticated != nil {
				t.Fatalf("%s was answered for by a source that did not mention it", rt.Runtime)
			}
		}
	}
}

// The audit screen exists so the user can see the evidence without our help,
// which means the console has to be able to say the log is intact.
func TestTheAuditScreenReportsChainIntegrity(t *testing.T) {
	r := newRig(t, api.Options{Credentials: newVault(t)})
	r.do(t, "POST", "/v1/credentials", `{"service":"stripe","secret":"`+testSecret+`"}`)

	log := auditEntries(t, r)
	if !log.Intact || log.Broken != "" {
		t.Fatalf("chain = %+v", log)
	}
	if len(log.Entries) < 2 {
		t.Fatalf("entries = %+v", log.Entries)
	}
	// Filtering narrows without claiming the whole log verified.
	resp, b := r.get(t, "/v1/audit?action=credential.add&outcome=ok")
	filtered := decode[api.AuditList](t, resp, b, http.StatusOK)
	if len(filtered.Entries) != 1 || filtered.Entries[0].Action != "credential.add" {
		t.Fatalf("filtered = %+v", filtered.Entries)
	}
	if filtered.Intact {
		t.Fatal("a filtered window claimed to have verified the chain")
	}
}

// The phone's socket carries session commands and consent decisions, so a
// read-only credential must not be able to open it.
func TestThePhoneSocketNeedsMoreThanReadScope(t *testing.T) {
	readOnly := api.AuthenticatorFunc(func(*http.Request) (api.Identity, error) {
		return api.Identity{Kind: "account", Scopes: []api.Scope{api.ScopeRead}, AuthAt: time.Now()}, nil
	})
	r := newRig(t, api.Options{Authenticator: readOnly})

	// Watching is fine. The stream never ends, so this only looks at the status
	// line and hangs up.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", r.HTTP.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+r.Srv.Token())
	sse, err := r.HTTP.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sseStatus := sse.StatusCode
	sse.Body.Close()
	cancel()
	if sseStatus != http.StatusOK {
		t.Fatalf("SSE with read scope = %d", sseStatus)
	}

	resp, b := r.get(t, "/v1/ws")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("websocket with read scope = %d, want 403: %s", resp.StatusCode, b)
	}
	// So are session commands over REST, for the same reason.
	resp, b = r.do(t, "POST", "/v1/sessions/whatever/cancel", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cancel with read scope = %d: %s", resp.StatusCode, b)
	}
}

// The state filter has to mean the same thing across both tiers.
func TestAskingForALiveStateExcludesTheHistoricalTier(t *testing.T) {
	r := newRig(t, api.Options{})
	r.start(t, "payments")
	insertIndexRow(t, r, "codex", "old-1", "/home/u/.codex/old-1.jsonl", "last year", 1)

	resp, b := r.get(t, "/v1/sessions?state=idle")
	list := decode[api.SessionList](t, resp, b, http.StatusOK)
	for _, v := range list.Sessions {
		if v.Source == "index" {
			t.Fatalf("state=idle returned an archived session: %+v", v)
		}
	}

	resp, b = r.get(t, "/v1/sessions?state=archived")
	archived := decode[api.SessionList](t, resp, b, http.StatusOK)
	if len(archived.Sessions) != 1 || archived.Sessions[0].NativeID != "old-1" {
		t.Fatalf("state=archived = %+v", archived.Sessions)
	}
}

// The opening session.list frame must prove the ping subscription already
// exists, or a ping delivered right after a phone connects reaches nobody.
//
// This guards a race that was real: the subscription used to be taken inside
// the push goroutine, so there was a window after the socket was announced open
// in which bus.Topic had no subscriber to deliver to. It surfaced as a flake
// under load, which is the worst way for "the user was never told a session is
// blocked" to show up.
func TestAPingRightAfterConnectingIsNeverLost(t *testing.T) {
	r := newRig(t, api.Options{})
	for i := range 10 {
		c := dial(t, r)
		c.await(t, api.TypeSessionList)

		id := fmt.Sprintf("ping-%d", i)
		if err := r.Srv.Deliver(context.Background(), bus.Ping{
			ID: id, Class: bus.ClassInformational, At: time.Now(),
			Line: "payments is done",
		}); err != nil {
			t.Fatal(err)
		}
		frame := c.await(t, api.TypeNotify)
		var n api.Notify
		if err := json.Unmarshal(frame.Payload, &n); err != nil {
			t.Fatal(err)
		}
		if n.Ping != id {
			t.Fatalf("round %d got ping %q, want %q", i, n.Ping, id)
		}
	}
}

// ------------------------------------- ORCHESTRATOR.md §4b's proposal queue --

// fakeProposals stands in for cmd/relayd's adapter over connector.Proposer.
//
// It records exactly what the handler asked for. The interesting field is the
// one that does not exist: Accept takes no access half, so there is nothing
// this fake could record about which half was requested — which is the point
// the type is making. What it can record is that Accept was reached at all, and
// with which name.
type fakeProposals struct {
	list      []api.ConnectorProposal
	accepted  []string
	acceptBy  []string
	dismissed []string
	reasons   []string
	err       error
}

func (f *fakeProposals) Proposals(context.Context) ([]api.ConnectorProposal, error) {
	return f.list, f.err
}

func (f *fakeProposals) Accept(_ context.Context, name, by string) (api.ConnectorGrantResult, error) {
	if f.err != nil {
		return api.ConnectorGrantResult{}, f.err
	}
	f.accepted = append(f.accepted, name)
	f.acceptBy = append(f.acceptBy, by)
	// The half is chosen here, in the implementation, and never by the caller.
	return api.ConnectorGrantResult{
		ID: "grant-1", Connector: name, Scopes: []string{name + ":read"},
	}, nil
}

func (f *fakeProposals) Dismiss(_ context.Context, name, reason string) error {
	if f.err != nil {
		return f.err
	}
	f.dismissed = append(f.dismissed, name)
	f.reasons = append(f.reasons, reason)
	return nil
}

func onePrinterProposal() []api.ConnectorProposal {
	return []api.ConnectorProposal{{
		Connector: "prusa", Title: "Prusa 3D printer", Access: "read",
		Evidence: "You have mentioned your Prusa 3D printer four times this week.",
		Opens:    "I could tell you how far through a print is without you going to look",
		Line:     "You have mentioned your Prusa 3D printer four times this week. Want me to connect it?",
		Scopes:   []string{"prusa:read"}, Episodes: 4, Mentions: 6,
	}}
}

// A daemon with no connectors must say so rather than returning an empty list.
// "You have nothing worth connecting" and "Relay cannot tell" are different
// answers and the screen renders them differently.
func TestConnectorProposalsSayWhyTheyAreEmpty(t *testing.T) {
	r := newRig(t, api.Options{})

	resp, b := r.get(t, "/v1/connectors/proposals")
	out := decode[api.ConnectorProposalList](t, resp, b, http.StatusOK)
	if out.Available {
		t.Error("a daemon with no proposal source reported the screen as available")
	}
	if out.Note == "" {
		t.Error("an empty proposal list with no note reads as 'you have nothing to connect'")
	}
	if out.Proposals == nil {
		t.Error("the list must be [] rather than null, so the console can render it")
	}
}

// The unwired mutation paths degrade the same way handleRevokeConnector does:
// 503 with a sentence, and the attempt on the record. "Somebody tried to accept
// a connector proposal on a box that has none" is exactly what the log is for,
// and dropping it because the feature is unconfigured would be a hole.
func TestAcceptingWithNoProposalSourceIsRecordedAndRefused(t *testing.T) {
	r := newRig(t, api.Options{})

	resp, b := r.do(t, "POST", "/v1/connectors/proposals/prusa/accept", `{}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("accept with nothing wired = %d, want 503: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), api.CodeUnavailable) {
		t.Errorf("code = %s", b)
	}

	var refused bool
	for _, e := range auditEntries(t, r).Entries {
		if e.Action == "connector.proposal.accept" && e.Outcome == "denied" {
			refused = true
		}
	}
	if !refused {
		t.Error("the refused accept left no trace in the audit log")
	}
}

// Accepting a proposal grants access to a service. That is a vault-scope
// decision, not a read-scope one, and a read-only console session must not be
// able to connect anything.
func TestAcceptingAProposalNeedsTheVaultScope(t *testing.T) {
	readOnly := api.AuthenticatorFunc(func(*http.Request) (api.Identity, error) {
		return api.Identity{
			Kind: "account", Subject: "acct_1",
			Scopes: []api.Scope{api.ScopeRead}, AuthAt: time.Now(),
		}, nil
	})
	f := &fakeProposals{list: onePrinterProposal()}
	r := newRig(t, api.Options{Authenticator: readOnly, ConnectorProposals: f})

	// Reading the offer is fine.
	if resp, b := r.get(t, "/v1/connectors/proposals"); resp.StatusCode != http.StatusOK {
		t.Fatalf("read = %d: %s", resp.StatusCode, b)
	}
	for _, path := range []string{
		"/v1/connectors/proposals/prusa/accept",
		"/v1/connectors/proposals/prusa/dismiss",
	} {
		resp, b := r.do(t, "POST", path, `{}`)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s with a read-only identity = %d, want 403: %s", path, resp.StatusCode, b)
		}
	}
	if len(f.accepted) != 0 || len(f.dismissed) != 0 {
		t.Fatal("a read-only identity reached the answer path")
	}
}

// §4b rule 2: read and write are separate grants, and the write half costs a
// SECOND decision. An endpoint that takes the half as an argument makes the two
// one click apart, so this asserts the request cannot influence it — the body
// below asks for write in three different spellings and is ignored, because the
// handler never reads it.
func TestAcceptingAProposalCanOnlyEverGrantTheReadHalf(t *testing.T) {
	f := &fakeProposals{list: onePrinterProposal()}
	r := newRig(t, api.Options{ConnectorProposals: f})

	resp, b := r.do(t, "POST", "/v1/connectors/proposals/prusa/accept",
		`{"access":"write","scopes":["prusa:write"],"half":"write"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("accept = %d: %s", resp.StatusCode, b)
	}
	if strings.Contains(string(b), "prusa:write") {
		t.Fatalf("a request that asked for the write half got it: %s", b)
	}

	var out struct {
		Grant api.ConnectorGrantResult `json:"grant"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if len(out.Grant.Scopes) != 1 || out.Grant.Scopes[0] != "prusa:read" {
		t.Errorf("scopes = %v, want exactly [prusa:read]", out.Grant.Scopes)
	}
	if len(f.accepted) != 1 || f.accepted[0] != "prusa" {
		t.Fatalf("the handler accepted %v", f.accepted)
	}
	// The surface the decision came from is recorded with the grant, because
	// "granted from the console" and "granted by the installer" are different
	// stories about the same row.
	if len(f.acceptBy) != 1 || f.acceptBy[0] != "console" {
		t.Errorf("accepted by %v, want console", f.acceptBy)
	}
}

// A dismissal changes nothing and is still a decision: it silences a connector
// for a month. A month of silence nobody can account for is exactly what the
// audit log exists to make impossible.
func TestDismissingAProposalIsAudited(t *testing.T) {
	f := &fakeProposals{list: onePrinterProposal()}
	r := newRig(t, api.Options{ConnectorProposals: f})

	resp, b := r.do(t, "POST", "/v1/connectors/proposals/prusa/dismiss",
		`{"reason":"I do not want it on the network"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dismiss = %d: %s", resp.StatusCode, b)
	}
	if len(f.reasons) != 1 || f.reasons[0] != "I do not want it on the network" {
		t.Errorf("the reason did not reach the store: %v", f.reasons)
	}

	log := auditEntries(t, r)
	if !log.Intact {
		t.Fatalf("the audit chain does not verify: %s", log.Broken)
	}
	var attempted, ok bool
	for _, e := range log.Entries {
		if e.Action != "connector.proposal.dismiss" {
			continue
		}
		switch e.Outcome {
		case "attempted":
			attempted = true
		case "ok":
			ok = true
		}
	}
	// Both, and in that order by construction: the attempt is written before
	// the work runs so a crash in the middle leaves the evidence you want.
	if !attempted || !ok {
		t.Errorf("dismiss left attempted=%v ok=%v in the log", attempted, ok)
	}
}

// A second factor, not just a fresh password
//
// CONTROL-PLANE.md §3 item 5. Freshness and assurance are different questions
// and the vault needs both answered: a password typed one second ago is fresh
// and is still one factor, which means a stolen password reaches the highest
// value target in the system.

// cloudSession builds an authenticator for one account, at a given assurance
// and age.
func cloudSession(assurance string, age time.Duration) api.Authenticator {
	return api.AuthenticatorFunc(func(*http.Request) (api.Identity, error) {
		return api.Identity{
			Kind: "account", Subject: "acct_1", Cloud: true,
			Scopes:    api.AllScopes(),
			AuthAt:    time.Now().Add(-age),
			Assurance: assurance,
		}, nil
	})
}

func TestAFreshPasswordIsStillOneFactorAndTheVaultRefusesIt(t *testing.T) {
	r := newRig(t, api.Options{
		Authenticator: cloudSession(api.AssuranceSingleFactor, time.Second),
		Credentials:   newVault(t), Cloud: true, VaultReauth: 5 * time.Minute,
	})

	// Reading is unaffected. The whole design of DASHBOARD.md §4 is that the
	// console stays usable and the vault is the part that costs something.
	if resp, _ := r.get(t, "/v1/credentials"); resp.StatusCode != http.StatusOK {
		t.Fatalf("read = %d", resp.StatusCode)
	}

	resp, b := r.do(t, "POST", "/v1/credentials", `{"service":"x","secret":"`+testSecret+`"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a one-factor vault write = %d, want 401: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "reauthenticate") {
		t.Fatalf("the console cannot tell this from a logout: %s", b)
	}
	// And it says which of the two rules refused, because the answers differ:
	// one is "type your code", the other is "sign in again".
	if !strings.Contains(string(b), "second factor") {
		t.Errorf("the refusal does not name the missing factor: %s", b)
	}
}

func TestASecondFactorGetsIntoTheVault(t *testing.T) {
	// Without this the test above passes for a server that refuses everything.
	r := newRig(t, api.Options{
		Authenticator: cloudSession(api.AssuranceSecondFactor, time.Second),
		Credentials:   newVault(t), Cloud: true, VaultReauth: 5 * time.Minute,
	})
	resp, b := r.do(t, "POST", "/v1/credentials", `{"service":"x","secret":"`+testSecret+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a two-factor vault write = %d: %s", resp.StatusCode, b)
	}
}

func TestASecondFactorDoesNotExemptASessionFromAging(t *testing.T) {
	// The other direction, and the one an implementation is likely to get wrong
	// by treating aal2 as a pass rather than as one of two conditions. A session
	// that presented a factor this morning is not a session that presented one
	// before this write.
	r := newRig(t, api.Options{
		Authenticator: cloudSession(api.AssuranceSecondFactor, 2*time.Hour),
		Credentials:   newVault(t), Cloud: true, VaultReauth: 5 * time.Minute,
	})
	resp, b := r.do(t, "POST", "/v1/credentials", `{"service":"x","secret":"`+testSecret+`"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a stale two-factor vault write = %d, want 401: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "fresh") {
		t.Errorf("the refusal does not name staleness: %s", b)
	}
}

func TestTheSelfHostedTokenNeedsNoSecondFactor(t *testing.T) {
	// The printed token carries no assurance level and must not be judged
	// against one: it is presented on every request, so there is no session to
	// age and no account service to enrol a factor with. A free tier that
	// suddenly could not write to its own vault would be this rule leaking
	// across the tier boundary.
	r := newRig(t, api.Options{Credentials: newVault(t), VaultReauth: time.Nanosecond})
	resp, b := r.do(t, "POST", "/v1/credentials", `{"service":"y","secret":"`+testSecret+`"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("token vault write = %d: %s", resp.StatusCode, b)
	}
}
