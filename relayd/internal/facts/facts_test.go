package facts_test

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/facts"
	"github.com/luthor007/relay/relayd/internal/store"
)

var (
	day  = 24 * time.Hour
	base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
)

func openStore(t *testing.T, now time.Time) (*facts.Store, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s, err := facts.Open(db, facts.Options{
		Redactor: facts.Detector(),
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("facts.Open: %v", err)
	}
	return s, db
}

func ev(session string, at time.Time) facts.Evidence {
	return facts.Evidence{
		Runtime: "claude-code", SessionID: session,
		Path:  "/home/u/.claude/projects/x/" + session + ".jsonl",
		Quote: "we switched the backend over", At: at,
	}
}

func obs(p facts.Predicate, object, text string, e ...facts.Evidence) facts.Observation {
	return facts.Observation{
		Predicate: p, Object: object, Text: text, Confidence: 0.6, Evidence: e,
	}
}

func mustReconcile(t *testing.T, s *facts.Store, o ...facts.Observation) facts.Result {
	t.Helper()
	res, err := s.Reconcile(context.Background(), o)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func live(t *testing.T, s *facts.Store) []facts.Fact {
	t.Helper()
	got, err := s.List(context.Background(), facts.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return got
}

// ------------------------------------------------- rule 1: evidence or die --

// MEMORY.md §5: a fact that cannot point at where it came from is deleted, not
// kept at low confidence. The package has no exported way to write one.
func TestAFactWithNoEvidenceIsNeverStored(t *testing.T) {
	s, _ := openStore(t, base)

	res := mustReconcile(t, s, obs(facts.Uses, "Stripe", "uses Stripe for payments"))
	if len(res.Rejected) != 1 {
		t.Fatalf("want one rejection, got %+v", res)
	}
	if !strings.Contains(res.Rejected[0].Reason, "no evidence") {
		t.Fatalf("rejection reason does not name the rule: %q", res.Rejected[0].Reason)
	}
	if got := live(t, s); len(got) != 0 {
		t.Fatalf("an unevidenced fact reached the tier: %+v", got)
	}
}

// Evidence without a date cannot be decayed, so it is not evidence.
func TestEvidenceWithoutARuntimeSessionOrDateIsNotEvidence(t *testing.T) {
	s, _ := openStore(t, base)

	for name, bad := range map[string]facts.Evidence{
		"no runtime": {SessionID: "s1", At: base},
		"no session": {Runtime: "codex", At: base},
		"no date":    {Runtime: "codex", SessionID: "s1"},
	} {
		res := mustReconcile(t, s, obs(facts.Uses, "Stripe", "uses Stripe", bad))
		if len(res.Rejected) != 1 {
			t.Fatalf("%s: want a rejection, got %+v", name, res)
		}
	}
	if got := live(t, s); len(got) != 0 {
		t.Fatalf("something got in: %+v", got)
	}
}

// Sweep is the rule enforced against the table rather than against the writer.
func TestSweepDeletesFactsWhoseEvidenceIsGone(t *testing.T) {
	ctx := context.Background()
	s, db := openStore(t, base)

	mustReconcile(t, s, obs(facts.DeploysOn, "Vercel", "deploys on Vercel", ev("s1", base)))
	got := live(t, s)
	if len(got) != 1 {
		t.Fatalf("want one fact, got %d", len(got))
	}
	id := got[0].ID

	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM fact_evidence WHERE fact_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	res, err := s.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(res.Unevidenced) != 1 || res.Unevidenced[0] != id {
		t.Fatalf("Sweep did not name the fact: %+v", res)
	}

	// Deleted, not downgraded: the row is gone, including from the console's
	// deleted-inclusive view.
	all, err := s.List(ctx, facts.Filter{IncludeDeleted: true, IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("an unevidenced fact survived a sweep: %+v", all)
	}
}

// ------------------------------------------------------ rule 2: decay --

// Decay is on last observation, not creation. A habit first seen in 2024 that
// still shows up last week is strong; one that stopped is weak.
func TestDecayRunsOnLastObservationNotCreation(t *testing.T) {
	s, _ := openStore(t, base)

	// Same first sighting, two years apart in their last.
	old := obs(facts.Prefers, "Firebase", "prefers Firebase", ev("s1", base.Add(-700*day)))
	kept := obs(facts.Uses, "Postgres", "uses Postgres",
		ev("s2", base.Add(-700*day)), ev("s3", base.Add(-2*day)))
	mustReconcile(t, s, old, kept)

	got := live(t, s)
	if len(got) != 2 {
		t.Fatalf("want two facts, got %d", len(got))
	}
	byObject := map[string]facts.Fact{}
	for _, f := range got {
		byObject[f.Object] = f
	}
	stale, fresh := byObject["Firebase"], byObject["Postgres"]

	if !stale.FirstSeen.Equal(fresh.FirstSeen) {
		t.Fatalf("the two facts should share a first_seen: %v vs %v", stale.FirstSeen, fresh.FirstSeen)
	}
	if !fresh.LastSeen.After(stale.LastSeen) {
		t.Fatal("last_seen did not move with the newer evidence")
	}
	if stale.Strength(base) >= fresh.Strength(base) {
		t.Fatalf("the stale fact is not weaker: %v vs %v", stale.Strength(base), fresh.Strength(base))
	}
	if stale.Strength(base) > facts.StaleBelow {
		t.Fatalf("a fact last seen 700 days ago is still above the floor: %v", stale.Strength(base))
	}
	if fresh.Strength(base) < 0.5*fresh.Confidence {
		t.Fatalf("a fact seen two days ago decayed too far: %v", fresh.Strength(base))
	}
}

// One half-life halves the confidence, and the arithmetic is not a guess.
func TestOneHalfLifeHalvesTheConfidence(t *testing.T) {
	got := facts.Decay(0.8, base, base.Add(facts.DefaultHalfLife), facts.DefaultHalfLife)
	if math.Abs(got-0.4) > 1e-9 {
		t.Fatalf("one half-life gave %v, want 0.4", got)
	}
	if got := facts.Decay(0.8, base, base.Add(-day), facts.DefaultHalfLife); got != 0.8 {
		t.Fatalf("decay before the observation changed it: %v", got)
	}
}

// A filter with a floor is how routing asks for facts worth acting on.
func TestMinStrengthDropsDecayedFacts(t *testing.T) {
	s, _ := openStore(t, base)
	mustReconcile(t, s,
		obs(facts.Prefers, "Firebase", "prefers Firebase", ev("s1", base.Add(-900*day))),
		obs(facts.DeploysOn, "Vercel", "deploys on Vercel", ev("s2", base.Add(-1*day))))

	got, err := s.List(context.Background(), facts.Filter{MinStrength: facts.StaleBelow, At: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Object != "Vercel" {
		t.Fatalf("the floor did not drop the stale fact: %+v", got)
	}
}

// --------------------------------------------- rule 3: contradictions --

// MEMORY.md §5's own example: one preference and one piece of history.
func TestContradictionsSupersedeRatherThanAccumulate(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t, base)

	mustReconcile(t, s, obs(facts.Prefers, "Firebase", "prefers Firebase for the backend", ev("s1", base.Add(-300*day))))
	res := mustReconcile(t, s, obs(facts.Prefers, "Supabase",
		"prefers Supabase over Firebase now", ev("s2", base.Add(-2*day))))

	if len(res.Superseded) != 1 {
		t.Fatalf("the old preference was not superseded: %+v", res)
	}

	current := live(t, s)
	if len(current) != 1 || current[0].Object != "Supabase" {
		t.Fatalf("want one live preference (Supabase), got %+v", current)
	}

	// "you used to use Firebase" is still answerable, with its date.
	all, err := s.List(ctx, facts.Filter{IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("history was lost: %+v", all)
	}
	var old facts.Fact
	for _, f := range all {
		if f.Object == "Firebase" {
			old = f
		}
	}
	if !old.Superseded() {
		t.Fatal("the Firebase fact is not marked superseded")
	}
	if old.SupersededAt.IsZero() {
		t.Fatal("a superseded fact with no date cannot answer 'when'")
	}
	if old.SupersededBy != current[0].ID {
		t.Fatalf("superseded_by points at %q, want %q", old.SupersededBy, current[0].ID)
	}
}

// Supersession requires naming. Two objects under one predicate is not evidence
// that they are alternatives, and guessing deletes true facts silently.
func TestAnUnnamedRivalIsNotSuperseded(t *testing.T) {
	s, _ := openStore(t, base)

	mustReconcile(t, s, obs(facts.DeploysOn, "Vercel", "deploys the web app on Vercel", ev("s1", base.Add(-30*day))))
	res := mustReconcile(t, s, obs(facts.DeploysOn, "Fly.io", "deploys the daemon on Fly.io", ev("s2", base)))

	if len(res.Superseded) != 0 {
		t.Fatalf("an unnamed fact was superseded: %+v", res.Superseded)
	}
	if got := live(t, s); len(got) != 2 {
		t.Fatalf("want both hosts, got %+v", got)
	}
}

// An explicit Replaces is the extractor saying so out loud.
func TestReplacesSupersedesWithoutMentioningItInTheSentence(t *testing.T) {
	s, _ := openStore(t, base)
	mustReconcile(t, s, obs(facts.Uses, "Mailgun", "uses Mailgun for email", ev("s1", base.Add(-60*day))))

	o := obs(facts.Uses, "Postmark", "uses Postmark for email", ev("s2", base))
	o.Replaces = []string{"mailgun"}
	res := mustReconcile(t, s, o)

	if len(res.Superseded) != 1 {
		t.Fatalf("Replaces did not supersede: %+v", res)
	}
}

// Going back is a contradiction in the other direction, and only when named.
func TestASupersededFactIsRevivedOnlyWhenNamed(t *testing.T) {
	s, _ := openStore(t, base)
	mustReconcile(t, s, obs(facts.Prefers, "Firebase", "prefers Firebase", ev("s1", base.Add(-300*day))))
	mustReconcile(t, s, obs(facts.Prefers, "Supabase", "prefers Supabase over Firebase", ev("s2", base.Add(-100*day))))

	// Silence about Supabase is not a return to Firebase.
	quiet := mustReconcile(t, s, obs(facts.Prefers, "Firebase", "prefers Firebase", ev("s3", base.Add(-50*day))))
	if len(quiet.Suppressed) != 1 || len(quiet.Revived) != 0 {
		t.Fatalf("a superseded fact was revived without being named: %+v", quiet)
	}

	// Naming it is.
	back := mustReconcile(t, s, obs(facts.Prefers, "Firebase",
		"moved back to Firebase; Supabase did not work out", ev("s4", base)))
	if len(back.Revived) != 1 {
		t.Fatalf("naming the supersessor did not revive the old fact: %+v", back)
	}
	got := live(t, s)
	if len(got) != 1 || got[0].Object != "Firebase" {
		t.Fatalf("want Firebase live and alone, got %+v", got)
	}
}

// A fact a human wrote or corrected is never buried by the extractor.
func TestAnEditedFactIsNotSuperseded(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t, base)
	mustReconcile(t, s, obs(facts.Prefers, "Firebase", "prefers Firebase", ev("s1", base.Add(-100*day))))

	id := live(t, s)[0].ID
	text := "prefers Firebase, and I mean it"
	if _, err := s.Edit(ctx, id, facts.Edit{Text: &text}); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	res := mustReconcile(t, s, obs(facts.Prefers, "Supabase", "prefers Supabase over Firebase", ev("s2", base)))
	if len(res.Superseded) != 0 {
		t.Fatalf("an edited fact was superseded: %+v", res.Superseded)
	}
	if got := live(t, s); len(got) != 2 {
		t.Fatalf("want both facts visible for the user to reconcile, got %+v", got)
	}
}

// ------------------------------------------- rule 4: visible and editable --

// The extractor may refresh a corrected fact's dates; it may not rewrite its
// words. DASHBOARD.md §3.3 and M3's api both depend on that.
func TestReObservingAnEditedFactDoesNotRewriteIt(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t, base)
	mustReconcile(t, s, obs(facts.Writes, "Go", "writes Go", ev("s1", base.Add(-40*day))))

	id := live(t, s)[0].ID
	corrected := "writes Go for daemons, TypeScript for anything with a UI"
	if _, err := s.Edit(ctx, id, facts.Edit{Text: &corrected}); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	mustReconcile(t, s, obs(facts.Writes, "Go", "writes Go", ev("s2", base)))

	f, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.Text != corrected {
		t.Fatalf("the extractor overwrote a human correction: %q", f.Text)
	}
	if !f.Edited() {
		t.Fatal("edited_at was cleared")
	}
	if !f.LastSeen.Equal(base) {
		t.Fatalf("last_seen did not follow the new evidence: %v", f.LastSeen)
	}
	if len(f.Evidence) != 2 {
		t.Fatalf("want both citations, got %d", len(f.Evidence))
	}
}

// A fact the user deleted does not come back on the next turn.
func TestADeletedFactIsNotResurrected(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t, base)
	mustReconcile(t, s, obs(facts.Uses, "Jira", "uses Jira", ev("s1", base.Add(-10*day))))

	id := live(t, s)[0].ID
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	res := mustReconcile(t, s, obs(facts.Uses, "Jira", "uses Jira", ev("s2", base)))
	if len(res.Suppressed) != 1 {
		t.Fatalf("a deleted fact was re-derived: %+v", res)
	}
	if got := live(t, s); len(got) != 0 {
		t.Fatalf("a deleted fact came back: %+v", got)
	}
}

// The console's toggle: superseded facts are returned only when asked for.
func TestSupersededFactsAreBehindAFlag(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t, base)
	mustReconcile(t, s, obs(facts.Prefers, "Firebase", "prefers Firebase", ev("s1", base.Add(-200*day))))
	mustReconcile(t, s, obs(facts.Prefers, "Supabase", "prefers Supabase over Firebase", ev("s2", base)))

	if got, _ := s.List(ctx, facts.Filter{}); len(got) != 1 {
		t.Fatalf("the default view shows history: %+v", got)
	}
	if got, _ := s.List(ctx, facts.Filter{IncludeSuperseded: true}); len(got) != 2 {
		t.Fatalf("the toggle does not show history: %+v", got)
	}
}

// ------------------------------------------------ rule 5: no secrets here --

func TestAFactSentenceContainingACredentialIsRefused(t *testing.T) {
	s, _ := openStore(t, base)

	res := mustReconcile(t, s, obs(facts.Uses, "Stripe",
		"uses Stripe with sk_live_"+strings.Repeat("a", 24), ev("s1", base)))

	if len(res.Rejected) != 1 {
		t.Fatalf("a credential-bearing fact was accepted: %+v", res)
	}
	if !strings.Contains(res.Rejected[0].Reason, "Stripe secret key") {
		t.Fatalf("the rejection does not name the detector: %q", res.Rejected[0].Reason)
	}
	// And the rejection itself does not carry the key onward.
	if strings.Contains(res.Rejected[0].Text, "sk_live_") {
		t.Fatalf("the rejection echoed the secret: %q", res.Rejected[0].Text)
	}
	if got := live(t, s); len(got) != 0 {
		t.Fatalf("something reached the tier: %+v", got)
	}
}

func TestAHumanCannotEditACredentialIntoTheTier(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t, base)
	mustReconcile(t, s, obs(facts.Uses, "Stripe", "uses Stripe for payments", ev("s1", base)))
	id := live(t, s)[0].ID

	bad := "the key is sk_live_" + strings.Repeat("b", 24)
	if _, err := s.Edit(ctx, id, facts.Edit{Text: &bad}); err == nil {
		t.Fatal("Edit stored a credential")
	}
}

// Evidence quotes come off a transcript, so they get the same treatment.
func TestEvidenceQuotesAreRedacted(t *testing.T) {
	s, _ := openStore(t, base)
	e := ev("s1", base)
	e.Quote = "export STRIPE_KEY=sk_live_" + strings.Repeat("c", 24)
	mustReconcile(t, s, obs(facts.Uses, "Stripe", "uses Stripe for payments", e))

	got := live(t, s)
	if len(got) != 1 || len(got[0].Evidence) != 1 {
		t.Fatalf("want one evidenced fact, got %+v", got)
	}
	if strings.Contains(got[0].Evidence[0].Quote, "sk_live_") {
		t.Fatalf("a secret is in the evidence quote: %q", got[0].Evidence[0].Quote)
	}
}

// A store built without a detector does not open. Same shape as
// summarize.ErrNoRedactor, and for the same reason.
func TestAStoreWithoutADetectorRefusesToOpen(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := facts.Open(db, facts.Options{}); err == nil {
		t.Fatal("a fact store opened with no secret detector")
	}
}

// ------------------------------------------------------------ idempotence --

// MEMORY.md §4 re-runs extraction on every TurnCompleted. Running it twice must
// not double the evidence, move the dates, or inflate the confidence.
func TestReObservingTheSameSessionChangesNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t, base)
	o := obs(facts.Uses, "Stripe", "uses Stripe for payments", ev("s1", base.Add(-3*day)))

	first := mustReconcile(t, s, o)
	if len(first.Created) != 1 || first.NewEvidence != 1 {
		t.Fatalf("first run: %+v", first)
	}
	before, err := s.Get(ctx, first.Created[0])
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		again := mustReconcile(t, s, o)
		if again.NewEvidence != 0 {
			t.Fatalf("run %d invented evidence: %+v", i, again)
		}
	}

	after, err := s.Get(ctx, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Confidence != before.Confidence {
		t.Fatalf("confidence drifted from %v to %v on re-observation", before.Confidence, after.Confidence)
	}
	if len(after.Evidence) != 1 {
		t.Fatalf("evidence accumulated: %d rows", len(after.Evidence))
	}
	if !after.LastSeen.Equal(before.LastSeen) || !after.FirstSeen.Equal(before.FirstSeen) {
		t.Fatal("the dates moved without new evidence")
	}
}

// A genuinely new sighting is what raises confidence, and it never reaches 1.
func TestNewEvidenceRaisesConfidenceAndNeverReachesCertainty(t *testing.T) {
	s, _ := openStore(t, base)
	sessions := []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8"}
	var prev float64
	for i, sess := range sessions {
		res := mustReconcile(t, s, obs(facts.DeploysOn, "Vercel", "deploys on Vercel",
			ev(sess, base.Add(-time.Duration(len(sessions)-i)*day))))
		if res.NewEvidence != 1 {
			t.Fatalf("%s: want one new citation, got %+v", sess, res)
		}
		f := live(t, s)[0]
		if i > 0 && f.Confidence <= prev {
			t.Fatalf("%s: confidence did not rise: %v then %v", sess, prev, f.Confidence)
		}
		if f.Confidence >= 1 {
			t.Fatalf("%s: confidence reached certainty: %v", sess, f.Confidence)
		}
		prev = f.Confidence
	}
	if got := live(t, s); len(got) != 1 || len(got[0].Evidence) != len(sessions) {
		t.Fatalf("want one fact with %d citations, got %+v", len(sessions), got)
	}
}

// Case and punctuation are not two facts.
func TestObjectsAreNormalisedIntoOneFact(t *testing.T) {
	s, _ := openStore(t, base)
	mustReconcile(t, s,
		obs(facts.Uses, "Supabase", "uses Supabase", ev("s1", base.Add(-2*day))),
		obs(facts.Uses, "supabase.", "uses supabase", ev("s2", base)))
	if got := live(t, s); len(got) != 1 {
		t.Fatalf("want one fact, got %+v", got)
	}
}

// The closed predicate set is what makes contradiction detection possible, so
// an unknown one is a reported rejection rather than a silent new category.
func TestAnUnknownPredicateIsRejectedAndNamed(t *testing.T) {
	s, _ := openStore(t, base)
	res := mustReconcile(t, s, obs("enjoys", "kubernetes", "enjoys kubernetes", ev("s1", base)))
	if len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0].Reason, "enjoys") {
		t.Fatalf("want a named rejection, got %+v", res)
	}
}

func TestPredicateSpellingsAModelActuallyProduces(t *testing.T) {
	for in, want := range map[string]facts.Predicate{
		"prefers": facts.Prefers, "prefer": facts.Prefers,
		"uses": facts.Uses, "using": facts.Uses,
		"deploys on": facts.DeploysOn, "deploy_on": facts.DeploysOn, "deploys": facts.DeploysOn,
		"writes": facts.Writes, "codes_in": facts.Writes,
	} {
		got, ok := facts.ParsePredicate(in)
		if !ok || got != want {
			t.Fatalf("%q parsed to %q/%v, want %q", in, got, ok, want)
		}
	}
	if _, ok := facts.ParsePredicate("hates"); ok {
		t.Fatal("an open predicate set")
	}
}
