package vault_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/vault"
)

const secret = "tok_51QabcdefghijklmnopZ9"

func open(t *testing.T, opts vault.Options) vault.Vault {
	t.Helper()
	if opts.DBPath == "" {
		opts.DBPath = filepath.Join(t.TempDir(), "vault.db")
	}
	if opts.Clock == nil {
		now := time.Unix(1770000000, 0).UTC()
		opts.Clock = func() time.Time { return now }
	}
	v, err := vault.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

func typed() vault.Provenance {
	return vault.Provenance{Kind: vault.SourceTyped, At: time.Unix(1770000000, 0).UTC()}
}

func TestKeychainBackendRoundTrip(t *testing.T) {
	ctx := context.Background()
	kr := vault.NewMemoryKeyring()
	v := open(t, vault.Options{Keyring: kr})

	if v.Status().Backend != vault.BackendKeychain {
		t.Fatalf("backend is %s, want keychain when one works", v.Status().Backend)
	}
	if v.Status().Degraded {
		t.Fatal("a working keychain is not a degraded state")
	}

	e, err := v.Put(ctx, vault.Input{Service: "stripe", Label: "live", Secret: secret, Source: typed()})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := v.Reveal(ctx, e.ID)
	if err != nil || got != secret {
		t.Fatalf("Reveal: %q %v", got, err)
	}

	// The secret is in the keychain, not in the database.
	if _, err := kr.Get("relay", e.ID); err != nil {
		t.Fatalf("the keychain does not hold the secret: %v", err)
	}
}

// The build container has no D-Bus and no secret service, which is also the
// shape of a headless always-on box — the machine this product installs on.
// Degrading has to work, and has to be visible.
func TestDegradesWhenThereIsNoKeychain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	kr := vault.NewMemoryKeyring()
	kr.FailAll = true

	v := open(t, vault.Options{DBPath: filepath.Join(dir, "vault.db"), Keyring: kr})

	st := v.Status()
	if st.Backend != vault.BackendFile {
		t.Fatalf("backend is %s, want file", st.Backend)
	}
	if st.KeySource != vault.KeySourceFile {
		t.Fatalf("key source is %s, want file", st.KeySource)
	}
	if !st.Degraded || st.Reason == "" {
		t.Fatalf("degradation must be visible: %+v", st)
	}

	e, err := v.Put(ctx, vault.Input{Service: "twilio", Secret: secret, Source: typed()})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := v.Reveal(ctx, e.ID)
	if err != nil || got != secret {
		t.Fatalf("Reveal: %q %v", got, err)
	}

	// The key file is 0600 and the database holds ciphertext, not plaintext.
	info, err := os.Stat(filepath.Join(dir, "vault.key"))
	if err != nil {
		t.Fatalf("key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file is %o, want 0600", perm)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("the plaintext secret is in the vault database file")
	}
}

// The file backend survives a restart: the key comes back off disk and old
// ciphertext still opens.
func TestFileBackendSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	kr := vault.NewMemoryKeyring()
	kr.FailAll = true
	opts := vault.Options{DBPath: filepath.Join(dir, "vault.db"), Keyring: kr}

	v1, err := vault.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	e, err := v1.Put(ctx, vault.Input{Service: "openrouter", Secret: secret, Source: typed()})
	if err != nil {
		t.Fatal(err)
	}
	if err := v1.Close(); err != nil {
		t.Fatal(err)
	}

	v2, err := vault.Open(ctx, opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer v2.Close()

	got, err := v2.Reveal(ctx, e.ID)
	if err != nil || got != secret {
		t.Fatalf("after reopen: %q %v", got, err)
	}
}

// DASHBOARD.md §3.2: never display a secret after it is stored. The type is
// what enforces it — Entry has no field that could carry one.
func TestEntryNeverCarriesTheSecret(t *testing.T) {
	ctx := context.Background()
	v := open(t, vault.Options{Keyring: vault.NewMemoryKeyring()})

	e, err := v.Put(ctx, vault.Input{Service: "stripe", Secret: secret, Source: typed()})
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range []vault.Entry{e, mustGet(t, v, e.ID), mustList(t, v)[0]} {
		blob, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(blob), secret) {
			t.Fatalf("an Entry carries the secret: %s", blob)
		}
		if entry.LastFour != "opZ9" {
			t.Fatalf("LastFour is %q, want the last four characters", entry.LastFour)
		}
	}
}

// Four characters of a six-character token is the token.
func TestShortSecretsHaveNoLastFour(t *testing.T) {
	for _, s := range []string{"abc", "abcd", "abcde12345"} {
		if got := vault.LastFour(s); got != "" {
			t.Fatalf("LastFour(%q) = %q, want empty — that is most of the secret", s, got)
		}
	}
	if got := vault.LastFour("sk-proj-abcdefgh"); got != "efgh" {
		t.Fatalf("LastFour = %q", got)
	}
}

// MEMORY.md §6: newest validated wins, and provenance is kept — which session,
// what date, and whether somebody else was in the room.
func TestProvenanceIsKept(t *testing.T) {
	ctx := context.Background()
	v := open(t, vault.Options{Keyring: vault.NewMemoryKeyring()})
	when := time.Unix(1740000000, 0).UTC()

	e, err := v.Put(ctx, vault.Input{
		Service: "twilio", Secret: secret,
		Source: vault.Provenance{
			Kind: vault.SourceTranscript, Runtime: "claude-code",
			Session: "sess-march", Path: "/home/u/.claude/projects/x/y.jsonl",
			At: when, SharedSession: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Source.Kind != vault.SourceTranscript || e.Source.Session != "sess-march" {
		t.Fatalf("provenance lost: %+v", e.Source)
	}
	if !e.Source.At.Equal(when) {
		t.Fatalf("source time is %v, want %v", e.Source.At, when)
	}
	// A key in your transcript may not be yours, and the proposal has to say so.
	if !e.Source.SharedSession {
		t.Fatal("the shared-session flag did not survive")
	}

	// A credential with no provenance cannot be reasoned about later, so it is
	// refused rather than stored anonymously.
	if _, err := v.Put(ctx, vault.Input{Service: "x", Secret: "y"}); err == nil {
		t.Fatal("a credential without provenance must be refused")
	}
	if _, err := v.Put(ctx, vault.Input{Service: "x", Source: typed()}); err == nil {
		t.Fatal("an empty secret must be refused")
	}
	if _, err := v.Put(ctx, vault.Input{Secret: "y", Source: typed()}); err == nil {
		t.Fatal("a credential without a service must be refused")
	}
}

func TestTouchAndValidation(t *testing.T) {
	ctx := context.Background()
	v := open(t, vault.Options{Keyring: vault.NewMemoryKeyring()})

	e, err := v.Put(ctx, vault.Input{Service: "stripe", Secret: secret, Source: typed()})
	if err != nil {
		t.Fatal(err)
	}
	if !e.LastUsedAt.IsZero() {
		t.Fatal("a new credential has never been used")
	}

	if err := v.Touch(ctx, e.ID, "codex"); err != nil {
		t.Fatal(err)
	}
	when := time.Unix(1770001000, 0).UTC()
	if err := v.RecordValidation(ctx, e.ID, "ok", when); err != nil {
		t.Fatal(err)
	}

	got := mustGet(t, v, e.ID)
	if got.LastUsedBy != "codex" || got.LastUsedAt.IsZero() {
		t.Fatalf("Touch: %+v", got)
	}
	if got.LastValidationReason != "ok" || !got.LastValidatedAt.Equal(when) {
		t.Fatalf("RecordValidation: %+v", got)
	}

	for _, err := range []error{
		v.Touch(ctx, "nope", "codex"),
		v.RecordValidation(ctx, "nope", "ok", when),
		v.Revoke(ctx, "nope"),
	} {
		if !errors.Is(err, vault.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	}
	if _, err := v.Get(ctx, "nope"); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("Get: %v", err)
	}
	if _, err := v.Reveal(ctx, "nope"); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("Reveal: %v", err)
	}
}

// Revoking destroys the secret and keeps the row: the console has to be able
// to say a credential was revoked, and when.
func TestRevokeDestroysTheSecretAndKeepsTheRow(t *testing.T) {
	ctx := context.Background()
	kr := vault.NewMemoryKeyring()
	v := open(t, vault.Options{Keyring: kr})

	e, err := v.Put(ctx, vault.Input{Service: "stripe", Secret: secret, Source: typed()})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Revoke(ctx, e.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := v.Reveal(ctx, e.ID); !errors.Is(err, vault.ErrRevoked) {
		t.Fatalf("Reveal after revoke: %v", err)
	}
	if _, err := kr.Get("relay", e.ID); !errors.Is(err, vault.ErrNotFound) {
		t.Fatal("revoking must remove the secret from the keychain")
	}

	got := mustGet(t, v, e.ID)
	if !got.Revoked() || got.RevokedAt.IsZero() {
		t.Fatalf("the row must record that it was revoked: %+v", got)
	}
	if got.LastFour != "opZ9" {
		t.Fatal("the display form survives revocation so the console can still name it")
	}
	if len(mustList(t, v)) != 1 {
		t.Fatal("a revoked credential stays in the list")
	}
}

func TestFileBackendCanBeForced(t *testing.T) {
	ctx := context.Background()
	kr := vault.NewMemoryKeyring()
	v := open(t, vault.Options{Keyring: kr, Backend: vault.BackendFile})

	st := v.Status()
	if st.Backend != vault.BackendFile {
		t.Fatalf("backend is %s", st.Backend)
	}
	// The keychain worked, so it holds the encryption key rather than the
	// secret — MEMORY.md §6's "an encrypted file whose key is in the keychain".
	if st.KeySource != vault.KeySourceKeychain {
		t.Fatalf("key source is %s, want keychain", st.KeySource)
	}
	if st.Degraded {
		t.Fatal("forcing the file backend with a working keychain is not degradation")
	}

	e, err := v.Put(ctx, vault.Input{Service: "simba", Secret: secret, Source: typed()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Reveal(ctx, e.ID)
	if err != nil || got != secret {
		t.Fatalf("Reveal: %q %v", got, err)
	}
}

func mustGet(t *testing.T, v vault.Vault, id string) vault.Entry {
	t.Helper()
	e, err := v.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return e
}

func mustList(t *testing.T, v vault.Vault) []vault.Entry {
	t.Helper()
	es, err := v.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return es
}
