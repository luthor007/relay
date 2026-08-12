package vault_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/index"
	"github.com/luthor007/relay/relayd/internal/vault"
)

// Synthetic throughout. relayd/testdata/secrets/ is the only credential-shaped
// corpus in this repository and every record in it is invented; these follow the
// same rule and are shaped to match the measured tier-1 patterns.
const (
	stripeKey  = "sk_live_" + "4eC39HqLyjWDarjtT1zdp7dc"
	stripeKey2 = "sk_live_" + "9zQ21XvBnmKapqrsU4wfe6ab"
	githubTok  = "ghp_" + "0123456789abcdefghijklmnopqrstuvwxyz"
	md5ish     = "d41d8cd98f00b204e9800998ecf8427e"
)

func openQueue(t *testing.T, opts vault.Options) (vault.Vault, vault.Proposals) {
	t.Helper()
	if opts.Keyring == nil {
		opts.Keyring = vault.NewMemoryKeyring()
	}
	v := open(t, opts)
	return v, v.Proposals()
}

func transcript(at time.Time) vault.Provenance {
	return vault.Provenance{
		Kind: vault.SourceTranscript, Runtime: "claude-code",
		Session: "0e1f-2a3b", Path: "/home/u/.claude/projects/api/0e1f.jsonl", At: at,
	}
}

func stripeCandidate(at time.Time) vault.Candidate {
	return vault.Candidate{
		Service: "stripe", Label: "Stripe secret key", Detector: "stripe_secret",
		Tier: index.TierVendor, Secret: stripeKey, Source: transcript(at),
	}
}

var march = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

// ------------------------------------------------- nothing captured silently --

// Detection produces a question, not a credential.
func TestProposingDoesNotStoreACredential(t *testing.T) {
	ctx := context.Background()
	v, q := openQueue(t, vault.Options{})

	p, err := q.Propose(ctx, stripeCandidate(march))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !p.Open() {
		t.Fatal("a fresh proposal is not open")
	}
	list, err := v.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("proposing stored a credential: %+v", list)
	}
	open, err := q.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != p.ID {
		t.Fatalf("the question is not in the queue: %+v", open)
	}
}

// The proposal never carries the candidate, only its last four.
func TestAProposalNeverCarriesTheSecret(t *testing.T) {
	ctx := context.Background()
	_, q := openQueue(t, vault.Options{})
	p, err := q.Propose(ctx, stripeCandidate(march))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), stripeKey) {
		t.Fatalf("a Proposal carries the secret: %s", blob)
	}
	if p.LastFour != stripeKey[len(stripeKey)-4:] {
		t.Fatalf("LastFour is %q", p.LastFour)
	}
}

// MEMORY.md §6's own sentence, and the shared-session warning that is the only
// thing standing between a colleague's key and our vault.
func TestTheProposalLineSaysWhenTheSessionHadAnotherParticipant(t *testing.T) {
	ctx := context.Background()
	_, q := openQueue(t, vault.Options{})

	c := vault.Candidate{
		Service: "twilio", Label: "Twilio auth token", Detector: "twilio_token",
		Tier: index.TierVendor, Secret: "SK" + strings.Repeat("7", 30),
		Source: transcript(march),
	}
	solo, err := q.Propose(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	line := solo.Line()
	for _, want := range []string{"Twilio auth token", "March 2026", "your twilio credential?"} {
		if !strings.Contains(strings.ToLower(line), strings.ToLower(want)) {
			t.Fatalf("the prompt is missing %q: %s", want, line)
		}
	}
	if strings.Contains(line, "may not be yours") {
		t.Fatalf("a solo session was warned about: %s", line)
	}

	shared := c
	shared.Secret = "SK" + strings.Repeat("8", 30)
	shared.Source.SharedSession = true
	sp, err := q.Propose(ctx, shared)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sp.Line(), "may not be yours") {
		t.Fatalf("a shared session was not flagged: %s", sp.Line())
	}
}

// ------------------------------------------------------------- the tier rule --

// MEMORY.md §12.2 rule 1: a tier-2 hit is redacted before indexing and must
// never auto-create a vault entry, because one in four would be a checksum.
func TestATierTwoShapeIsNeverProposed(t *testing.T) {
	ctx := context.Background()
	_, q := openQueue(t, vault.Options{})

	_, err := q.Propose(ctx, vault.Candidate{
		Service: "twilio", Detector: "hex32", Tier: index.TierShape,
		Secret: md5ish, Source: transcript(march),
	})
	if !errors.Is(err, vault.ErrNotProposable) {
		t.Fatalf("a shape match was proposed: %v", err)
	}
	if open, _ := q.List(ctx); len(open) != 0 {
		t.Fatalf("the queue took it anyway: %+v", open)
	}
}

// A transcript candidate that will not say which tier found it is refused
// rather than assumed to be tier 1.
func TestATranscriptCandidateMustDeclareItsTier(t *testing.T) {
	ctx := context.Background()
	_, q := openQueue(t, vault.Options{})
	c := stripeCandidate(march)
	c.Tier = 0
	if _, err := q.Propose(ctx, c); !errors.Is(err, vault.ErrNotProposable) {
		t.Fatalf("an untiered transcript candidate was accepted: %v", err)
	}
}

// FromFinding is the seam to internal/index's measured ruleset, so there is one
// ruleset rather than two.
func TestFromFindingRefusesShapeMatchesAndVendorlessRules(t *testing.T) {
	det := index.MustDetector()
	src := transcript(march)

	var vendor, shape, vendorless int
	for _, f := range det.Scan("stripe " + stripeKey + " digest " + md5ish) {
		c, ok := vault.FromFinding(f, src)
		switch {
		case f.Tier != index.TierVendor:
			if ok {
				t.Fatalf("a %s match became a candidate: %+v", f.Tier, f)
			}
			shape++
		case f.Service == "":
			if ok {
				t.Fatalf("a vendorless match became a candidate: %+v", f)
			}
			vendorless++
		default:
			if !ok || c.Secret != f.Value || c.Service != f.Service {
				t.Fatalf("a tier-1 vendor match did not convert: %+v -> %+v %v", f, c, ok)
			}
			vendor++
		}
	}
	if vendor == 0 {
		t.Fatal("the fixture matched no tier-1 vendor rule; the test proves nothing")
	}
	if shape == 0 {
		t.Fatal("the fixture matched no tier-2 rule; the refusal is untested")
	}
}

// ------------------------------------------------------------- no nagging --

// The same key in forty sessions is one question, and dismissing it sticks.
func TestTheSameKeyIsOneQuestionForever(t *testing.T) {
	ctx := context.Background()
	_, q := openQueue(t, vault.Options{})

	first, err := q.Propose(ctx, stripeCandidate(march))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		c := stripeCandidate(march.AddDate(0, 0, i+1))
		c.Source.Session = "another-session"
		again, err := q.Propose(ctx, c)
		if err != nil {
			t.Fatalf("re-proposing failed: %v", err)
		}
		if again.ID != first.ID {
			t.Fatalf("the same key produced a second question: %s vs %s", again.ID, first.ID)
		}
	}
	if open, _ := q.List(ctx); len(open) != 1 {
		t.Fatalf("want one open question, got %d", len(open))
	}

	if err := q.Dismiss(ctx, first.ID, "that is a colleague's key"); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	_, err = q.Propose(ctx, stripeCandidate(march.AddDate(0, 1, 0)))
	if !errors.Is(err, vault.ErrDecided) {
		t.Fatalf("a dismissed key was proposed again: %v", err)
	}
	if open, _ := q.List(ctx); len(open) != 0 {
		t.Fatalf("the dismissed question is still open: %+v", open)
	}

	// And the reason survives, because "why did we say no" is what keeps it no.
	hist, err := q.History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Reason != "that is a colleague's key" {
		t.Fatalf("the dismissal reason was lost: %+v", hist)
	}
	if hist[0].Decision != vault.Dismissed || hist[0].DecidedAt.IsZero() {
		t.Fatalf("the decision was not recorded: %+v", hist[0])
	}
}

// A key already in the vault is not offered again.
func TestAKeyAlreadyHeldIsNotProposed(t *testing.T) {
	ctx := context.Background()
	v, q := openQueue(t, vault.Options{})

	if _, err := v.Put(ctx, vault.Input{Service: "stripe", Secret: stripeKey, Source: typed()}); err != nil {
		t.Fatal(err)
	}
	_, err := q.Propose(ctx, stripeCandidate(march))
	if !errors.Is(err, vault.ErrKnown) {
		t.Fatalf("a held credential was proposed: %v", err)
	}
}

// Dismissing destroys the candidate. Keeping ciphertext for an answer of "no"
// is a liability with no reader.
func TestDismissDestroysTheCandidate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	kr := vault.NewMemoryKeyring()
	kr.FailAll = true
	_, q := openQueue(t, vault.Options{DBPath: filepath.Join(dir, "vault.db"), Keyring: kr})

	p, err := q.Propose(ctx, stripeCandidate(march))
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Dismiss(ctx, p.ID, "not mine"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Accept(ctx, p.ID, ""); !errors.Is(err, vault.ErrDecided) {
		t.Fatalf("a dismissed proposal could still be accepted: %v", err)
	}
	if err := q.Dismiss(ctx, p.ID, "again"); !errors.Is(err, vault.ErrDecided) {
		t.Fatalf("a decided proposal was decided twice: %v", err)
	}
}

// The vault database never holds the candidate in the clear, even before a
// decision.
func TestTheCandidateIsSealedOnDisk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		failAll bool
	}{{"with a keychain", false}, {"without one", true}} {
		t.Run(tc.name, func(t *testing.T) {
			kr := vault.NewMemoryKeyring()
			kr.FailAll = tc.failAll
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".db")
			_, q := openQueue(t, vault.Options{DBPath: path, Keyring: kr})

			if _, err := q.Propose(ctx, stripeCandidate(march)); err != nil {
				t.Fatal(err)
			}
			// The database runs in WAL mode, so a fresh write may still be in
			// the -wal file rather than the main one. Reading only vault.db
			// would pass without proving anything.
			files, err := filepath.Glob(path + "*")
			if err != nil {
				t.Fatal(err)
			}
			if len(files) < 2 {
				t.Fatalf("want the database and its WAL, got %v", files)
			}
			var onDisk bool
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), stripeKey) {
					t.Fatalf("an undecided candidate is in %s in the clear", f)
				}
				// The control: the row itself did reach one of these files, so
				// the absence of the secret is a fact about the secret rather
				// than about the flush.
				if strings.Contains(string(raw), "stripe_secret") {
					onDisk = true
				}
			}
			if !onDisk {
				t.Fatal("the proposal row never reached disk, so this test proved nothing")
			}
		})
	}
}

// ------------------------------------------------------ validate before trust --

// A provider that says the key is wrong stops the accept. A silently-wrong
// credential is worse than a missing one.
func TestAcceptRefusesACredentialTheProviderRejected(t *testing.T) {
	ctx := context.Background()
	var called int
	v, q := openQueue(t, vault.Options{
		Validator: vault.ValidatorFunc(func(_ context.Context, service, secret string) (vault.Validation, error) {
			called++
			if secret != stripeKey || service != "stripe" {
				t.Fatalf("the validator got %q for %q", secret, service)
			}
			return vault.Validation{Reason: "expired", Detail: "key revoked"}, nil
		}),
	})

	p, err := q.Propose(ctx, stripeCandidate(march))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Accept(ctx, p.ID, ""); !errors.Is(err, vault.ErrInvalidCredential) {
		t.Fatalf("a rejected credential was stored: %v", err)
	}
	if called != 1 {
		t.Fatalf("want one real call, got %d", called)
	}
	if list, _ := v.List(ctx); len(list) != 0 {
		t.Fatalf("something was stored anyway: %+v", list)
	}
	// The question stays open, so the console can say why rather than losing it.
	still, err := q.Get(ctx, p.ID)
	if err != nil || !still.Open() {
		t.Fatalf("the refused proposal was closed: %+v %v", still, err)
	}
}

func TestAcceptStoresAndRecordsAValidationThatHappened(t *testing.T) {
	ctx := context.Background()
	at := march.Add(72 * time.Hour)
	v, q := openQueue(t, vault.Options{
		Validator: vault.ValidatorFunc(func(context.Context, string, string) (vault.Validation, error) {
			return vault.Validation{Reason: "ok", At: at}, nil
		}),
	})

	p, err := q.Propose(ctx, stripeCandidate(march))
	if err != nil {
		t.Fatal(err)
	}
	e, err := q.Accept(ctx, p.ID, "live key")
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if e.Service != "stripe" || e.Label != "live key" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	if e.LastValidationReason != "ok" || !e.LastValidatedAt.Equal(at) {
		t.Fatalf("the validation was not recorded: %+v", e)
	}
	// Provenance is kept: which session, what date, and whether it was shared.
	if e.Source.Kind != vault.SourceTranscript || e.Source.Session != "0e1f-2a3b" || !e.Source.At.Equal(march) {
		t.Fatalf("provenance was lost: %+v", e.Source)
	}
	secret, err := v.Reveal(ctx, e.ID)
	if err != nil || secret != stripeKey {
		t.Fatalf("Reveal: %q %v", secret, err)
	}
	// The proposal is closed and points at what it became.
	done, err := q.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Decision != vault.Accepted || done.Credential != e.ID {
		t.Fatalf("the accepted proposal does not point at its credential: %+v", done)
	}
}

// A provider that does not answer must not stop somebody saving a key they have
// in front of them — but it must not be recorded as a validation either.
func TestAProviderOutageStoresWithoutClaimingValidation(t *testing.T) {
	ctx := context.Background()
	_, q := openQueue(t, vault.Options{
		Validator: vault.ValidatorFunc(func(context.Context, string, string) (vault.Validation, error) {
			return vault.Validation{}, errors.New("dial tcp: connection refused")
		}),
	})

	p, err := q.Propose(ctx, stripeCandidate(march))
	if err != nil {
		t.Fatal(err)
	}
	e, err := q.Accept(ctx, p.ID, "")
	if err != nil {
		t.Fatalf("an outage blocked an accept: %v", err)
	}
	if e.LastValidationReason != "unavailable" {
		t.Fatalf("the reason does not say what happened: %q", e.LastValidationReason)
	}
	if !e.LastValidatedAt.IsZero() {
		t.Fatal("a validation that never happened has a timestamp on it")
	}
}

// No validator at all is a supported state and a visible one.
func TestWithNoValidatorNothingIsClaimed(t *testing.T) {
	ctx := context.Background()
	_, q := openQueue(t, vault.Options{})
	p, err := q.Propose(ctx, stripeCandidate(march))
	if err != nil {
		t.Fatal(err)
	}
	e, err := q.Accept(ctx, p.ID, "")
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !e.LastValidatedAt.IsZero() || e.LastValidationReason != "" {
		t.Fatalf("an unvalidated credential claims a probe: %+v", e)
	}
}

// ------------------------------------------------------- newest validated wins --

func TestNewestValidatedWinsAndBothAreKept(t *testing.T) {
	ctx := context.Background()
	now := march
	v := open(t, vault.Options{Keyring: vault.NewMemoryKeyring(), Clock: func() time.Time { return now }})

	old, err := v.Put(ctx, vault.Input{Service: "stripe", Label: "old", Secret: stripeKey, Source: typed()})
	if err != nil {
		t.Fatal(err)
	}
	now = march.AddDate(0, 1, 0)
	fresh, err := v.Put(ctx, vault.Input{Service: "stripe", Label: "rotated", Secret: stripeKey2, Source: typed()})
	if err != nil {
		t.Fatal(err)
	}

	// Neither validated: the newer one wins on date.
	got, err := v.Current(ctx, "stripe")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != fresh.ID {
		t.Fatalf("Current chose %s, want the newer %s", got.Label, fresh.Label)
	}

	// The older one validates and the newer one does not: validated wins,
	// because "newest validated" is the rule and not "newest".
	if err := v.RecordValidation(ctx, old.ID, "ok", march.AddDate(0, 0, 2)); err != nil {
		t.Fatal(err)
	}
	if got, err = v.Current(ctx, "stripe"); err != nil || got.ID != old.ID {
		t.Fatalf("Current chose %+v, want the validated one", got)
	}

	// Then the newer one validates later, and takes it back.
	if err := v.RecordValidation(ctx, fresh.ID, "ok", march.AddDate(0, 2, 0)); err != nil {
		t.Fatal(err)
	}
	if got, err = v.Current(ctx, "stripe"); err != nil || got.ID != fresh.ID {
		t.Fatalf("Current chose %+v, want the newest validated", got)
	}

	// Both are still there with their own provenance. Two Stripe keys means one
	// is probably rotated; nothing is thrown away to say so.
	list, err := v.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("a credential was replaced rather than kept: %+v", list)
	}

	// A revoked credential never wins.
	if err := v.Revoke(ctx, fresh.ID); err != nil {
		t.Fatal(err)
	}
	if got, err = v.Current(ctx, "stripe"); err != nil || got.ID != old.ID {
		t.Fatalf("Current returned a revoked credential: %+v %v", got, err)
	}
}

func TestCurrentSaysWhenThereIsNothing(t *testing.T) {
	v := open(t, vault.Options{Keyring: vault.NewMemoryKeyring()})
	if _, err := v.Current(context.Background(), "twilio"); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ------------------------------------------------------------------ discovery --

// mapFS is a filesystem in a map. No test in this package touches a real
// runtime config, because CI has none of the five installed.
type mapFS struct {
	files map[string]string
	fail  map[string]error
}

func (m mapFS) ReadFile(name string) ([]byte, error) {
	if err, ok := m.fail[name]; ok {
		return nil, err
	}
	v, ok := m.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(v), nil
}

func (m mapFS) Stat(name string) (fs.FileInfo, error) {
	if _, ok := m.files[name]; !ok {
		return nil, os.ErrNotExist
	}
	return nil, errors.New("mapFS: no file info")
}

func TestDiscoveryFindsKeysInARuntimeConfigWithoutKnowingItsSchema(t *testing.T) {
	ctx := context.Background()
	home := "/home/u"
	path := filepath.Join(home, ".local", "share", "opencode", "auth.json")

	got, err := vault.Discover(ctx, vault.DiscoverOptions{
		Home: home,
		Now:  func() time.Time { return march },
		FS: mapFS{files: map[string]string{
			path: `{
			  "anthropic": {"type": "api", "key": "` + githubTok + `"},
			  "nested": [{"deep": {"token": "` + stripeKey + `"}}],
			  "notes": "the digest is ` + md5ish + `",
			  "port": 8080
			}`,
		}},
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got.Read) != 1 || got.Read[0] != path {
		t.Fatalf("the documented path was not read: %+v", got)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("want two candidates, got %+v", got.Candidates)
	}

	services := map[string]vault.Candidate{}
	for _, c := range got.Candidates {
		services[c.Service] = c
		if c.Source.Kind != vault.SourceConfig {
			t.Fatalf("a discovered key is not marked as config-sourced: %+v", c.Source)
		}
		if c.Source.Path != path {
			t.Fatalf("provenance lost the file: %+v", c.Source)
		}
		if c.Tier != index.TierVendor {
			t.Fatalf("a non-vendor rule produced a candidate: %+v", c)
		}
	}
	if _, ok := services["github"]; !ok {
		t.Fatalf("the top-level key was missed: %+v", got.Candidates)
	}
	if _, ok := services["stripe"]; !ok {
		t.Fatalf("the key nested inside an array was missed: %+v", got.Candidates)
	}
	// The 32-hex digest in "notes" is a tier-2 shape and must not be here.
	for _, c := range got.Candidates {
		if c.Secret == md5ish {
			t.Fatal("a checksum was proposed as a credential")
		}
	}
	// The JSON key path becomes the label, so two entries in one file are
	// tellable apart.
	if !strings.Contains(services["github"].Label, "anthropic.key") {
		t.Fatalf("the label does not name where it came from: %q", services["github"].Label)
	}
}

// MEMORY.md §7's rule: "not there" and "we could not read it" lead to opposite
// decisions, so they are never the same empty list.
func TestDiscoveryTellsMissingFromUnreadable(t *testing.T) {
	ctx := context.Background()
	got, err := vault.Discover(context.Background(), vault.DiscoverOptions{
		Home: "/home/u",
		Files: []ConfigFileAlias{
			{Runtime: "opencode", Path: "/gone/auth.json"},
			{Runtime: "opencode", Path: "/denied/auth.json"},
			{Runtime: "opencode", Path: "/garbage/auth.json"},
		},
		FS: mapFS{
			files: map[string]string{"/garbage/auth.json": "this is not json"},
			fail:  map[string]error{"/denied/auth.json": os.ErrPermission},
		},
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	_ = ctx
	if len(got.Missing) != 1 || got.Missing[0] != "/gone/auth.json" {
		t.Fatalf("missing files: %+v", got.Missing)
	}
	if len(got.Unreadable) != 2 {
		t.Fatalf("want two unreadable files, got %+v", got.Unreadable)
	}
	joined := strings.Join(got.Unreadable, " ")
	if !strings.Contains(joined, "/denied/auth.json") || !strings.Contains(joined, "/garbage/auth.json") {
		t.Fatalf("the unreadable list does not name both: %+v", got.Unreadable)
	}
	if len(got.Candidates) != 0 {
		t.Fatalf("candidates from nothing: %+v", got.Candidates)
	}
}

// A machine where nothing is installed is the measured normal case, not a
// failure.
func TestDiscoveryOnACleanMachineIsSuccess(t *testing.T) {
	got, err := vault.Discover(context.Background(), vault.DiscoverOptions{
		Home: "/home/u", FS: mapFS{files: map[string]string{}},
	})
	if err != nil {
		t.Fatalf("an empty machine was an error: %v", err)
	}
	if len(got.Candidates) != 0 || len(got.Read) != 0 {
		t.Fatalf("something was found on an empty machine: %+v", got)
	}
	if len(got.Missing) == 0 {
		t.Fatal("the scan did not look anywhere")
	}
}

// The documented path is the one MEMORY.md §4 names, and it is marked as
// documented so a guess is never reported as though it were measured.
func TestTheDocumentedOpenCodePathIsInTheDefaultList(t *testing.T) {
	files := vault.DefaultConfigFiles(vault.DiscoverOptions{Home: "/home/u"})
	var documented int
	for _, f := range files {
		if f.Documented {
			documented++
			if f.Path != "/home/u/.local/share/opencode/auth.json" {
				t.Fatalf("the documented path is %q", f.Path)
			}
		}
	}
	if documented != 1 {
		t.Fatalf("want exactly one documented path, got %d", documented)
	}
}

// Discovered keys go through the same queue as everything else: enumerable at
// install, with the user watching, and still a question.
func TestDiscoveredKeysAreProposedNotStored(t *testing.T) {
	ctx := context.Background()
	v, q := openQueue(t, vault.Options{})

	found, err := vault.Discover(ctx, vault.DiscoverOptions{
		Home: "/home/u",
		Now:  func() time.Time { return march },
		FS: mapFS{files: map[string]string{
			"/home/u/.local/share/opencode/auth.json": `{"stripe":{"key":"` + stripeKey + `"}}`,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Candidates) != 1 {
		t.Fatalf("want one candidate, got %+v", found.Candidates)
	}
	p, err := q.Propose(ctx, found.Candidates[0])
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if list, _ := v.List(ctx); len(list) != 0 {
		t.Fatalf("discovery stored a credential: %+v", list)
	}
	if !strings.Contains(p.Line(), "/home/u/.local/share/opencode/auth.json") {
		t.Fatalf("the prompt does not say where it came from: %s", p.Line())
	}
}

// ConfigFileAlias keeps the discovery test readable without importing the type
// under two names.
type ConfigFileAlias = vault.ConfigFile
