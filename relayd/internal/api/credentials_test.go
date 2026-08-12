package api_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/api"
	"github.com/luthor007/relay/relayd/internal/audit"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// The secret every test in this file hunts for. It is long enough that
// vault.LastFour returns something, and distinctive enough that a substring
// search over a response body is a real assertion.
const testSecret = "glpat-TESTONLYneverIssuedToAnybody01"

func newVault(t *testing.T) vault.Vault {
	t.Helper()
	v, err := vault.Open(context.Background(), vault.Options{
		DBPath:  filepath.Join(t.TempDir(), "vault.db"),
		Keyring: vault.NewMemoryKeyring(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

// ------------------------------------------------------- the type property --

// DASHBOARD.md §3.2 and MEMORY.md §6: the list endpoint must be *incapable* of
// returning a full secret, as a property of the type rather than of the handler.
//
// The mechanism is that api.CredentialStore is vault.Vault minus Reveal. This
// asserts the narrowing is real in both directions: the vault has the method,
// and the interface the API holds does not.
func TestTheAPICannotReachAPlaintextSecretAtAll(t *testing.T) {
	store := reflect.TypeOf((*api.CredentialStore)(nil)).Elem()
	full := reflect.TypeOf((*vault.Vault)(nil)).Elem()

	if _, ok := full.MethodByName("Reveal"); !ok {
		t.Fatal("vault.Vault no longer has Reveal, so this test is asserting nothing")
	}
	if _, ok := store.MethodByName("Reveal"); ok {
		t.Fatal("api.CredentialStore has Reveal: the console can now read plaintext secrets")
	}
	for i := range store.NumMethod() {
		name := strings.ToLower(store.Method(i).Name)
		for _, banned := range []string{"reveal", "secret", "plaintext", "export", "unseal"} {
			if strings.Contains(name, banned) {
				t.Fatalf("api.CredentialStore.%s could hand the console a secret", store.Method(i).Name)
			}
		}
	}

	// And the view type has nowhere to put one either.
	view := reflect.TypeOf(api.CredentialView{})
	for i := range view.NumField() {
		name := strings.ToLower(view.Field(i).Name)
		for _, banned := range []string{"secret", "token", "key", "password", "value", "plaintext"} {
			if name == banned {
				t.Fatalf("api.CredentialView has a %s field", view.Field(i).Name)
			}
		}
	}
}

// The empirical half of the same claim: store a secret, then sweep every read
// route the console has and assert none of them ever says it.
func TestNoReadRouteEverReturnsAStoredSecret(t *testing.T) {
	v := newVault(t)
	r := newRig(t, api.Options{Credentials: v})

	body := `{"service":"stripe","label":"live","secret":"` + testSecret + `"}`
	resp, out := r.do(t, "POST", "/v1/credentials", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add = %d: %s", resp.StatusCode, out)
	}
	if strings.Contains(string(out), testSecret) {
		t.Fatalf("the create response echoed the secret back: %s", out)
	}

	created := decode[map[string]api.CredentialView](t, resp, out, http.StatusCreated)
	id := created["credential"].ID
	if id == "" {
		t.Fatalf("no id came back: %s", out)
	}

	for _, path := range []string{
		"/v1/credentials",
		"/v1/credentials/proposals",
		"/v1/health",
		"/v1/audit",
		"/v1/connectors",
		"/v1/facts",
		"/v1/sessions",
		"/v1/runtimes",
		"/v1/mcp",
	} {
		resp, b := r.get(t, path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d: %s", path, resp.StatusCode, b)
		}
		if strings.Contains(string(b), testSecret) {
			t.Fatalf("%s returned the stored secret: %s", path, b)
		}
	}

	// Mutations return the entry too, and they must not leak it either.
	resp, b := r.do(t, "POST", "/v1/credentials/"+id+"/validate", "")
	if strings.Contains(string(b), testSecret) {
		t.Fatalf("validate leaked the secret: %s", b)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("validate = %d: %s", resp.StatusCode, b)
	}
}

func TestTheListShowsTheLastFourAndTheProvenance(t *testing.T) {
	v := newVault(t)
	r := newRig(t, api.Options{Credentials: v})

	r.do(t, "POST", "/v1/credentials", `{
		"service":"twilio","label":"main","secret":"`+testSecret+`",
		"source":"transcript","source_runtime":"claude-code",
		"source_session":"s-march","shared_session":true}`)

	resp, b := r.get(t, "/v1/credentials")
	list := decode[api.CredentialList](t, resp, b, http.StatusOK)
	if !list.Available || len(list.Credentials) != 1 {
		t.Fatalf("list = %+v", list)
	}
	c := list.Credentials[0]
	if c.LastFour != testSecret[len(testSecret)-4:] {
		t.Fatalf("last four = %q", c.LastFour)
	}
	// MEMORY.md §6: which session, what date, and whether it may not be theirs.
	if c.Source != "transcript" || c.SourceSession != "s-march" || !c.SharedSession {
		t.Fatalf("provenance = %+v", c)
	}
	if c.CreatedAt == 0 {
		t.Fatal("no created_at, so 'when it was added' is unanswerable")
	}
	if list.Vault.Backend == "" {
		t.Fatal("the console cannot say where the secret actually lives")
	}
}

// ---------------------------------------------------------------- auditing --

func auditEntries(t *testing.T, r *rig) api.AuditList {
	t.Helper()
	resp, b := r.get(t, "/v1/audit")
	return decode[api.AuditList](t, resp, b, http.StatusOK)
}

func TestEveryCredentialMutationIsAudited(t *testing.T) {
	v := newVault(t)
	r := newRig(t, api.Options{Credentials: v})

	resp, out := r.do(t, "POST", "/v1/credentials",
		`{"service":"stripe","secret":"`+testSecret+`"}`)
	id := decode[map[string]api.CredentialView](t, resp, out, http.StatusCreated)["credential"].ID

	r.do(t, "POST", "/v1/credentials/"+id+"/validate", "")
	r.do(t, "POST", "/v1/credentials/"+id+"/rotate", `{"secret":"sk_live_rotated_0000zzzz"}`)
	r.do(t, "POST", "/v1/credentials/"+id+"/revoke", "")

	log := auditEntries(t, r)
	if !log.Intact {
		t.Fatalf("the audit chain does not verify: %s", log.Broken)
	}
	want := map[string]bool{
		"credential.add": false, "credential.validate": false,
		"credential.rotate": false, "credential.revoke": false,
	}
	attempts := map[string]bool{}
	for _, e := range log.Entries {
		if e.Outcome == string(audit.OutcomeAttempted) {
			attempts[e.Action] = true
			continue
		}
		if e.Outcome == string(audit.OutcomeOK) {
			want[e.Action] = true
		}
		// Who, when, from where — on every line.
		if e.Actor == "" || e.At == 0 {
			t.Fatalf("an audit line is missing who or when: %+v", e)
		}
	}
	for action, saw := range want {
		if !saw {
			t.Fatalf("%s produced no successful audit entry: %+v", action, log.Entries)
		}
		if !attempts[action] {
			t.Fatalf("%s produced no *attempt* entry, only an outcome", action)
		}
	}
	if log.Pending != 0 {
		t.Fatalf("pending = %d, every mutation finished", log.Pending)
	}
}

// The attempt, not only the success. A revoke of something that is not there
// still has to leave evidence that somebody tried.
func TestAFailedMutationStillLeavesEvidence(t *testing.T) {
	v := newVault(t)
	r := newRig(t, api.Options{Credentials: v})

	resp, b := r.do(t, "POST", "/v1/credentials/nope/revoke", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}

	log := auditEntries(t, r)
	var sawAttempt, sawFailure bool
	for _, e := range log.Entries {
		if e.Action != "credential.revoke" || e.Target != "nope" {
			continue
		}
		switch e.Outcome {
		case string(audit.OutcomeAttempted):
			sawAttempt = true
		case string(audit.OutcomeFailed):
			sawFailure = true
		}
	}
	if !sawAttempt || !sawFailure {
		t.Fatalf("attempt=%v failure=%v; both are required: %+v", sawAttempt, sawFailure, log.Entries)
	}
}

// brokenAudit is a log that cannot write. A vault mutation that cannot be
// recorded is not a vault mutation we make.
type brokenAudit struct{}

func (brokenAudit) Append(context.Context, audit.Entry) (audit.Entry, error) {
	return audit.Entry{}, errors.New("the audit log is not writable")
}
func (brokenAudit) List(context.Context, audit.Filter) ([]audit.Entry, error) {
	return []audit.Entry{}, nil
}
func (brokenAudit) Durable() bool { return true }
func (brokenAudit) Path() string  { return "/var/relay/audit.jsonl" }
func (brokenAudit) Close() error  { return nil }

func TestAVaultWriteThatCannotBeRecordedIsRefused(t *testing.T) {
	v := newVault(t)
	r := newRig(t, api.Options{Credentials: v, Audit: brokenAudit{}})

	resp, b := r.do(t, "POST", "/v1/credentials",
		`{"service":"stripe","secret":"`+testSecret+`"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "audit") {
		t.Fatalf("the refusal should say why: %s", b)
	}

	// And nothing was stored. A mutation that half-happened is the failure this
	// ordering exists to prevent.
	entries, err := v.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the credential was stored anyway: %+v", entries)
	}
}

// ------------------------------------------------------- rotate and revoke --

func TestRotationKeepsTheIdSoAVaultReferenceStaysValid(t *testing.T) {
	v := newVault(t)
	r := newRig(t, api.Options{Credentials: v})

	resp, out := r.do(t, "POST", "/v1/credentials",
		`{"service":"stripe","label":"live","secret":"`+testSecret+`"}`)
	first := decode[map[string]api.CredentialView](t, resp, out, http.StatusCreated)["credential"]

	const rotated = "sk_live_rotatedvalue_9999abcd"
	resp, out = r.do(t, "POST", "/v1/credentials/"+first.ID+"/rotate",
		`{"secret":"`+rotated+`"}`)
	second := decode[map[string]any](t, resp, out, http.StatusOK)
	cred, _ := second["credential"].(map[string]any)

	if cred["id"] != first.ID {
		t.Fatalf("rotation changed the id: %v vs %v — every vault:<id> reference just broke", cred["id"], first.ID)
	}
	if cred["last_four"] == first.LastFour {
		t.Fatalf("last four did not change: %v", cred["last_four"])
	}
	if cred["label"] != "live" {
		t.Fatalf("the label was lost on rotation: %v", cred["label"])
	}
	// The rotated key is typed, whatever the old one was: carrying the old
	// provenance forward would claim this key came out of a March transcript.
	if cred["source"] != string(vault.SourceTyped) {
		t.Fatalf("source after rotation = %v", cred["source"])
	}
	if strings.Contains(string(out), rotated) || strings.Contains(string(out), testSecret) {
		t.Fatalf("rotation echoed a secret: %s", out)
	}
}

func TestARevokedCredentialStaysVisibleAndSaysWhen(t *testing.T) {
	v := newVault(t)
	r := newRig(t, api.Options{Credentials: v})

	resp, out := r.do(t, "POST", "/v1/credentials",
		`{"service":"stripe","secret":"`+testSecret+`"}`)
	id := decode[map[string]api.CredentialView](t, resp, out, http.StatusCreated)["credential"].ID

	resp, out = r.do(t, "POST", "/v1/credentials/"+id+"/revoke", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d: %s", resp.StatusCode, out)
	}

	resp, b := r.get(t, "/v1/credentials")
	list := decode[api.CredentialList](t, resp, b, http.StatusOK)
	if len(list.Credentials) != 1 {
		t.Fatalf("a revoked credential vanished from the list: %+v", list.Credentials)
	}
	if !list.Credentials[0].Revoked || list.Credentials[0].RevokedAt == 0 {
		t.Fatalf("revoked = %+v", list.Credentials[0])
	}
}

// -------------------------------------------------------------- validation --

func TestValidationRecordsTheReasonAndNeverClaimsAnUntestedKeyIsFine(t *testing.T) {
	v := newVault(t)

	// No validator wired: the honest answer is "not tested here", never "ok".
	r := newRig(t, api.Options{Credentials: v})
	resp, out := r.do(t, "POST", "/v1/credentials",
		`{"service":"stripe","secret":"`+testSecret+`","validate":true}`)
	created := decode[map[string]any](t, resp, out, http.StatusCreated)
	val, _ := created["validation"].(map[string]any)
	if val == nil || val["probed"] != false {
		t.Fatalf("an unwired validator reported %v", val)
	}
	if val["reason"] == "ok" {
		t.Fatal("an untested credential was reported as ok")
	}

	// With one wired, the reason lands on the entry so the list can show it.
	probed := newRig(t, api.Options{
		Credentials: v,
		Validator: api.ValidatorFunc(func(_ context.Context, e vault.Entry) (api.Validation, error) {
			if e.Service != "stripe" {
				t.Errorf("the validator got the wrong entry: %+v", e)
			}
			return api.Validation{Probed: true, Reason: "expired", Detail: "HTTP 401: revoked"}, nil
		}),
	})
	id := created["credential"].(map[string]any)["id"].(string)
	resp, out = probed.do(t, "POST", "/v1/credentials/"+id+"/validate", "")
	got := decode[map[string]any](t, resp, out, http.StatusOK)
	v2, _ := got["validation"].(map[string]any)
	if v2["reason"] != "expired" {
		t.Fatalf("validation = %v", v2)
	}

	entry, err := v.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if entry.LastValidationReason != "expired" || entry.LastValidatedAt.IsZero() {
		t.Fatalf("the reason was not persisted: %+v", entry)
	}
}

// --------------------------------------------------------------- proposals --

func TestProposalsComeFromTheIndexMarkersWhenNoQueueIsWired(t *testing.T) {
	r := newRig(t, api.Options{Credentials: newVault(t)})

	_, err := r.DB.SQL().Exec(`
		INSERT INTO secret_marker (id, runtime, session_id, path, byte_offset, detector, service, vault_id, at)
		VALUES ('m1', 'claude-code', 's-march', '/home/u/.claude/s.jsonl', 4096,
		        'twilio_auth_token (tier 1)', 'twilio', '', ?)`, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}

	resp, b := r.get(t, "/v1/credentials/proposals")
	got := decode[struct {
		Proposals []api.Proposal `json:"proposals"`
		Note      string         `json:"note"`
	}](t, resp, b, http.StatusOK)

	if len(got.Proposals) != 1 {
		t.Fatalf("proposals = %+v", got.Proposals)
	}
	p := got.Proposals[0]
	if p.Service != "twilio" || p.Session != "s-march" || p.ByteOffset != 4096 {
		t.Fatalf("proposal = %+v", p)
	}
	// The tier has to survive to the screen: MEMORY.md §12.2 measured a 26%
	// false-positive rate on tier 2.
	if !strings.Contains(p.Detector, "tier 1") {
		t.Fatalf("the detector tier was dropped: %q", p.Detector)
	}
}

func TestAcceptingAProposalWithNoQueueIsStillRecorded(t *testing.T) {
	r := newRig(t, api.Options{Credentials: newVault(t)})

	resp, b := r.do(t, "POST", "/v1/credentials/proposals/m1/accept", `{}`)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}

	log := auditEntries(t, r)
	var attempted, denied bool
	for _, e := range log.Entries {
		if e.Action != "credential.proposal.accept" {
			continue
		}
		switch e.Outcome {
		case string(audit.OutcomeAttempted):
			attempted = true
		case string(audit.OutcomeDenied):
			denied = true
		}
	}
	if !attempted || !denied {
		t.Fatalf("an attempt on an unbuilt surface left no trace: %+v", log.Entries)
	}
}

// --------------------------------------------------------- no vault at all --

func TestTheCredentialScreenRendersWithNoVault(t *testing.T) {
	r := newRig(t, api.Options{})
	resp, b := r.get(t, "/v1/credentials")
	list := decode[api.CredentialList](t, resp, b, http.StatusOK)
	if list.Available || list.Note == "" {
		t.Fatalf("an unwired vault should render empty with a reason: %+v", list)
	}
	if list.Credentials == nil {
		t.Fatal("credentials is null rather than [], which the console has to special-case")
	}

	resp, b = r.do(t, "POST", "/v1/credentials", `{"service":"x","secret":"`+testSecret+`"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("adding to a vault that is not there = %d: %s", resp.StatusCode, b)
	}
}
