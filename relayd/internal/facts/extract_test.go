package facts_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/facts"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/store"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

// fakeModel is a llm.Provider that answers from a script and records what it
// was asked. Nothing in this package makes a network call in a test.
type fakeModel struct {
	reply string
	err   error

	calls  int
	prompt string
}

func (m *fakeModel) Vendor() string { return "fake" }
func (m *fakeModel) Model() string  { return "fake-small" }
func (m *fakeModel) API() llm.API   { return llm.APIOpenAI }

func (m *fakeModel) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	m.calls++
	var b strings.Builder
	for _, msg := range req.Messages {
		b.WriteString(msg.Text)
	}
	m.prompt = b.String()
	if m.err != nil {
		return llm.Response{}, m.err
	}
	return llm.Response{Model: "fake-small", Text: m.reply}, nil
}

func (m *fakeModel) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return nil, errors.New("not used")
}

func (m *fakeModel) Probe(context.Context) llm.ProbeResult {
	return llm.ProbeResult{Reason: llm.ReasonOK}
}

var _ llm.Provider = (*fakeModel)(nil)

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func putSummary(t *testing.T, db *store.DB, session, text string, at time.Time) {
	t.Helper()
	_, err := db.PutSummary(context.Background(), store.Summary{
		Kind:      store.SummaryCluster,
		Runtime:   "claude-code",
		SessionID: session,
		Path:      "/transcripts/" + session + ".jsonl",
		Text:      text,
		CreatedAt: at,
	}, nil)
	if err != nil {
		t.Fatalf("PutSummary: %v", err)
	}
}

func extractor(db *store.DB, m llm.Provider) *facts.LLM {
	return &facts.LLM{DB: db, Model: m, Redact: facts.Detector()}
}

// A fact the model did not cite has no provenance, so it never becomes an
// observation. §5's first rule, applied where the citation is still checkable.
func TestAnUncitedFactIsDroppedAtTheBoundary(t *testing.T) {
	db := openDB(t)
	putSummary(t, db, "s1", "migrated the auth tables from Firebase to Supabase", base)

	m := &fakeModel{reply: `[
	  {"predicate":"prefers","object":"Supabase","text":"prefers Supabase over Firebase","confidence":0.7,"sources":[1],"replaces":["Firebase"]},
	  {"predicate":"uses","object":"Kubernetes","text":"uses Kubernetes","confidence":0.9,"sources":[]},
	  {"predicate":"uses","object":"Terraform","text":"uses Terraform","confidence":0.9,"sources":[42]}
	]`}

	got, err := extractor(db, m).Extract(context.Background(), facts.Scope{Runtime: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got.Observations) != 1 {
		t.Fatalf("want only the cited fact, got %+v", got.Observations)
	}
	o := got.Observations[0]
	if o.Object != "Supabase" || len(o.Evidence) != 1 {
		t.Fatalf("unexpected observation: %+v", o)
	}
	if o.Evidence[0].SessionID != "s1" || o.Evidence[0].Path == "" {
		t.Fatalf("the citation lost its pointer: %+v", o.Evidence[0])
	}
	if len(o.Replaces) != 1 || o.Replaces[0] != "Firebase" {
		t.Fatalf("replaces was lost: %+v", o.Replaces)
	}
}

// Citations resolve to the notes they name, not to all of them.
func TestCitationsSelectTheirOwnEvidence(t *testing.T) {
	db := openDB(t)
	putSummary(t, db, "s1", "one: deployed to Vercel", base.Add(-3*day))
	putSummary(t, db, "s1", "two: wired up Stripe billing", base.Add(-2*day))
	putSummary(t, db, "s1", "three: fixed a flaky test", base.Add(-day))

	m := &fakeModel{reply: `[{"predicate":"deploys_on","object":"Vercel","text":"deploys on Vercel","confidence":0.8,"sources":[1]}]`}
	got, err := extractor(db, m).Extract(context.Background(), facts.Scope{Runtime: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Observations) != 1 {
		t.Fatalf("want one observation, got %+v", got.Observations)
	}
	if n := len(got.Observations[0].Evidence); n != 1 {
		t.Fatalf("want one citation, got %d — attaching every source to every fact is not evidence", n)
	}
	if got.Sources != 3 {
		t.Fatalf("want three sources shown, got %d", got.Sources)
	}
}

// A key posted to a model provider has already left the machine, so the
// detector runs on the way out as well as on the way in.
func TestTheModelNeverSeesACredential(t *testing.T) {
	db := openDB(t)
	key := "sk_live_" + strings.Repeat("d", 24)
	putSummary(t, db, "s1", "set STRIPE_SECRET_KEY="+key+" in the deploy env", base)

	m := &fakeModel{reply: `[]`}
	if _, err := extractor(db, m).Extract(context.Background(), facts.Scope{Runtime: "claude-code", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.prompt, key) {
		t.Fatalf("the prompt carried a credential to the provider:\n%s", m.prompt)
	}
	if !strings.Contains(m.prompt, "[relay:redacted") {
		t.Fatalf("the redaction marker is missing, so nothing was scanned:\n%s", m.prompt)
	}
}

// An extractor with no detector does not run at all.
func TestExtractionWithoutADetectorRefuses(t *testing.T) {
	db := openDB(t)
	putSummary(t, db, "s1", "deployed to Vercel", base)
	e := &facts.LLM{DB: db, Model: &fakeModel{reply: `[]`}}
	if _, err := e.Extract(context.Background(), facts.Scope{Runtime: "claude-code", SessionID: "s1"}); err == nil {
		t.Fatal("extraction ran with no secret detector")
	}
}

// A failed model call is a reported skip, not a lost turn — internal/summarize
// runs this behind a summary that has already landed.
func TestAFailedModelCallIsAReportedSkip(t *testing.T) {
	db := openDB(t)
	putSummary(t, db, "s1", "deployed to Vercel", base)

	m := &fakeModel{err: errors.New("provider said no")}
	got, err := extractor(db, m).Extract(context.Background(), facts.Scope{Runtime: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatalf("a failed model call became an error: %v", err)
	}
	if got.Skipped == "" || !strings.Contains(got.Skipped, "provider said no") {
		t.Fatalf("the skip does not say what happened: %+v", got)
	}
	if len(got.Observations) != 0 {
		t.Fatal("observations from a failed call")
	}
}

// "Nothing to read" and "the model failed" must not look the same.
func TestAnEmptySessionSaysSoRatherThanFailing(t *testing.T) {
	db := openDB(t)
	m := &fakeModel{reply: `[]`}
	got, err := extractor(db, m).Extract(context.Background(), facts.Scope{Runtime: "codex", SessionID: "nothing"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped == "" {
		t.Fatal("an empty session produced a silent empty batch")
	}
	if m.calls != 0 {
		t.Fatal("the model was called with nothing to read")
	}
}

func TestNoModelIsAStateNotAnError(t *testing.T) {
	db := openDB(t)
	putSummary(t, db, "s1", "deployed to Vercel", base)
	e := &facts.LLM{DB: db, Redact: facts.Detector()}
	got, err := e.Extract(context.Background(), facts.Scope{Runtime: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped != "no model configured" {
		t.Fatalf("want a named skip, got %q", got.Skipped)
	}
}

// Unparseable output is a skip, because a small model returning prose instead
// of JSON is a Tuesday.
func TestUnparseableOutputIsASkip(t *testing.T) {
	db := openDB(t)
	putSummary(t, db, "s1", "deployed to Vercel", base)
	m := &fakeModel{reply: "Sure! Here are some facts about this developer."}
	got, err := extractor(db, m).Extract(context.Background(), facts.Scope{Runtime: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped != "unparseable model output" {
		t.Fatalf("want the named skip, got %q", got.Skipped)
	}
}

// A session that appears to yield thirty durable preferences has produced
// thirty guesses.
func TestExtractionIsBounded(t *testing.T) {
	db := openDB(t)
	putSummary(t, db, "s1", "a long session", base)

	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 30; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"predicate":"uses","object":"tool` + string(rune('a'+i%26)) + `x` +
			string(rune('a'+i/26)) + `","text":"uses a tool","confidence":0.5,"sources":[1]}`)
	}
	b.WriteString("]")

	got, err := extractor(db, &fakeModel{reply: b.String()}).
		Extract(context.Background(), facts.Scope{Runtime: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Observations) > facts.MaxPerRun {
		t.Fatalf("want at most %d observations, got %d", facts.MaxPerRun, len(got.Observations))
	}
}

// Episodes are the other half of "runs against the index and the episodes",
// and they arrive as raw transcript that has never seen a detector.
func TestEpisodesAreASourceAndAreRedactedFirst(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	key := "sk_live_" + strings.Repeat("e", 24)
	if err := db.PutEpisode(ctx, store.Episode{
		ID: "ep1", Kind: "conversation", StartedAt: base.Add(-2 * day),
		Transcript: "we agreed to move everything to Vercel. the key is " + key,
	}); err != nil {
		t.Fatalf("PutEpisode: %v", err)
	}

	m := &fakeModel{reply: `[{"predicate":"deploys_on","object":"Vercel","text":"deploys on Vercel","confidence":0.8,"sources":[1]}]`}
	got, err := extractor(db, m).ExtractEpisodes(ctx, base.Add(-30*day), 10)
	if err != nil {
		t.Fatalf("ExtractEpisodes: %v", err)
	}
	if strings.Contains(m.prompt, key) {
		t.Fatal("an episode transcript carried a credential to the provider")
	}
	if len(got.Observations) != 1 {
		t.Fatalf("want one observation, got %+v", got.Observations)
	}
	e := got.Observations[0].Evidence[0]
	if e.Runtime != facts.EpisodeRuntime || e.SessionID != "ep1" {
		t.Fatalf("episode evidence does not point at the episode: %+v", e)
	}
	if !e.At.Equal(base.Add(-2 * day)) {
		t.Fatalf("episode evidence lost its date: %v", e.At)
	}
}

// ------------------------------------------------------------ the live path --

func TestTheLivePathExtractsAndWrites(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	putSummary(t, db, "s1", "moved the backend off Firebase and onto Supabase", base)

	s, err := facts.Open(db, facts.Options{Redactor: facts.Detector(), Now: func() time.Time { return base }})
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeModel{reply: `[{"predicate":"prefers","object":"Supabase","text":"prefers Supabase over Firebase","confidence":0.7,"sources":[1]}]`}
	u, err := facts.NewUpdater(facts.UpdaterOptions{
		Extractor: extractor(db, m), Store: s, MinInterval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}

	up, err := u.Session(ctx, facts.Scope{Runtime: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if len(up.Result.Created) != 1 {
		t.Fatalf("nothing was written: %+v", up)
	}
	got, err := s.List(ctx, facts.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Object != "Supabase" || len(got[0].Evidence) != 1 {
		t.Fatalf("unexpected tier: %+v", got)
	}
}

// A turn boundary arrives every few seconds. One model call per boundary is the
// difference between a background task and a machine that is busy all day.
func TestTheLivePathThrottlesPerSession(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	putSummary(t, db, "s1", "deployed to Vercel", base)

	s, err := facts.Open(db, facts.Options{Redactor: facts.Detector(), Now: func() time.Time { return base }})
	if err != nil {
		t.Fatal(err)
	}
	now := base
	m := &fakeModel{reply: `[{"predicate":"deploys_on","object":"Vercel","text":"deploys on Vercel","confidence":0.6,"sources":[1]}]`}
	u, err := facts.NewUpdater(facts.UpdaterOptions{
		Extractor: extractor(db, m), Store: s,
		MinInterval: facts.DefaultMinInterval,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	sc := facts.Scope{Runtime: "claude-code", SessionID: "s1"}

	if _, err := u.Session(ctx, sc); err != nil {
		t.Fatal(err)
	}
	up, err := u.Session(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if !up.Throttled {
		t.Fatal("the second boundary in a second was not throttled")
	}
	if up.Batch.Skipped == "" {
		t.Fatal("a throttled run must say so; 'no facts' and 'we did not look' are different")
	}
	if m.calls != 1 {
		t.Fatalf("want one model call, got %d", m.calls)
	}

	now = base.Add(facts.DefaultMinInterval + time.Second)
	if up, err = u.Session(ctx, sc); err != nil || up.Throttled {
		t.Fatalf("the throttle never lifts: %+v %v", up, err)
	}
	if m.calls != 2 {
		t.Fatalf("want two model calls, got %d", m.calls)
	}
}

// The bridge is what makes summarize.Live write facts without summarize
// knowing this package exists.
func TestBridgeSatisfiesSummarizeAndReportsWhatLanded(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	putSummary(t, db, "s1", "wired Stripe into checkout", base)

	s, err := facts.Open(db, facts.Options{Redactor: facts.Detector(), Now: func() time.Time { return base }})
	if err != nil {
		t.Fatal(err)
	}
	// One good fact and one the tier will refuse for having no citation.
	m := &fakeModel{reply: `[
	  {"predicate":"uses","object":"Stripe","text":"uses Stripe for payments","confidence":0.8,"sources":[1]},
	  {"predicate":"uses","object":"Braintree","text":"uses Braintree","confidence":0.8,"sources":[]}
	]`}
	u, err := facts.NewUpdater(facts.UpdaterOptions{Extractor: extractor(db, m), Store: s, MinInterval: -1})
	if err != nil {
		t.Fatal(err)
	}

	var fx summarize.FactExtractor = facts.Bridge{Updater: u}
	res, err := fx.Extract(ctx, summarize.FactScope{Runtime: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	if len(res.Facts) != 1 || res.Facts[0].Object != "Stripe" {
		t.Fatalf("the bridge reported what was proposed rather than what landed: %+v", res.Facts)
	}
	if len(res.Facts[0].Evidence) != 1 {
		t.Fatalf("the bridge dropped the evidence: %+v", res.Facts[0])
	}
	if res.ModelCalls != 1 {
		t.Fatalf("want one model call reported, got %d", res.ModelCalls)
	}
}

func TestBridgeWithNoUpdaterIsAStatedSkip(t *testing.T) {
	res, err := facts.Bridge{}.Extract(context.Background(), summarize.FactScope{Runtime: "codex", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped == "" {
		t.Fatal("an unwired bridge returned a silent empty result")
	}
}
