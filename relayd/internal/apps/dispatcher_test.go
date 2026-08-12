package apps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/episode"
)

const echoApp = `
export default {
  async onTrigger(ctx) {
    ctx.log(JSON.stringify({ type: ctx.trigger.type, ...ctx.trigger }));
  },
};
`

func newDispatcher(t *testing.T, tr *testRuntime) *Dispatcher {
	t.Helper()
	d, err := NewDispatcher(DispatcherOptions{Runtime: tr.Runtime, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestOnlyTheAppsThatDeclaredTheTriggerAreWoken(t *testing.T) {
	tr := newTestRuntime(t)
	d := newDispatcher(t, tr)

	phrase := tr.install(t, writeApp(t, manifestWith("dev.test.phrase", ``,
		`{"type":"phrase","match":"wrap up the standup"}`, ""), echoApp))
	touch := tr.install(t, writeApp(t, manifestWith("dev.test.touch", ``,
		`{"type":"touch","gesture":"doubleTap"}`, ""), echoApp))
	for _, i := range []Installed{phrase, touch} {
		if err := d.Add(i); err != nil {
			t.Fatal(err)
		}
	}

	invs := d.Phrase(context.Background(), "okay everyone, let's wrap up the Stand-up.")
	if len(invs) != 1 || invs[0].AppID != "dev.test.phrase" {
		t.Fatalf("phrase woke %+v", appIDs(invs))
	}
	if invs[0].Outcome != OutcomeCompleted {
		t.Errorf("%s: %s", invs[0].Outcome, invs[0].Error)
	}

	if got := d.Phrase(context.Background(), "nothing to see here"); len(got) != 0 {
		t.Errorf("a transcript with no wake phrase must wake nobody, got %v", appIDs(got))
	}

	invs = d.Touch(context.Background(), GestureDoubleTap)
	if len(invs) != 1 || invs[0].AppID != "dev.test.touch" {
		t.Fatalf("touch woke %+v", appIDs(invs))
	}
	if got := d.Touch(context.Background(), GestureLongPress); len(got) != 0 {
		t.Errorf("a gesture nobody asked for wakes nobody, got %v", appIDs(got))
	}
}

func appIDs(invs []Invocation) []string {
	out := make([]string, 0, len(invs))
	for _, i := range invs {
		out = append(out, i.AppID)
	}
	return out
}

func TestPhraseMatchingSurvivesPunctuationAndCase(t *testing.T) {
	tr := newTestRuntime(t)
	d := newDispatcher(t, tr)
	inst := tr.install(t, writeApp(t, manifestWith("dev.test.phrase", ``,
		`{"type":"phrase","match":"wrap up the standup"}`, ""), echoApp))
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	for _, transcript := range []string{
		"wrap up the standup",
		"Wrap Up The Standup.",
		"okay — wrap up the stand up, please",
		"...WRAP  UP   THE STANDUP!!!",
	} {
		if got := d.Phrase(context.Background(), transcript); len(got) != 1 {
			t.Errorf("%q did not wake the app", transcript)
		}
	}
}

func TestMemoryTriggersComeFromThePipeline(t *testing.T) {
	tr := newTestRuntime(t)
	d := newDispatcher(t, tr)

	meeting := tr.install(t, writeApp(t, manifestWith("dev.test.meeting", ``,
		`{"type":"memory","event":"meeting.ended"}`, ""), echoApp))
	commit := tr.install(t, writeApp(t, manifestWith("dev.test.commit", ``,
		`{"type":"memory","event":"commitment.detected"}`, ""), echoApp))
	anyEp := tr.install(t, writeApp(t, manifestWith("dev.test.any", ``,
		`{"type":"memory","event":"episode.created"}`, ""), echoApp))
	for _, i := range []Installed{meeting, commit, anyEp} {
		if err := d.Add(i); err != nil {
			t.Fatal(err)
		}
	}

	// A meeting with commitments fires all three, specific first.
	invs := d.EpisodeStored(context.Background(),
		episode.Episode{ID: "ep-1", Kind: episode.KindMeeting},
		episode.WriteResult{EpisodeID: "ep-1", Commitments: 2})
	got := appIDs(invs)
	if len(got) != 3 || got[0] != "dev.test.meeting" || got[2] != "dev.test.any" {
		t.Fatalf("woke %v", got)
	}

	// A focus episode with no commitments fires only the general one, because
	// nothing here emits an event it did not observe.
	invs = d.EpisodeStored(context.Background(),
		episode.Episode{ID: "ep-2", Kind: episode.KindFocus},
		episode.WriteResult{EpisodeID: "ep-2"})
	if got := appIDs(invs); len(got) != 1 || got[0] != "dev.test.any" {
		t.Fatalf("woke %v", got)
	}

	// The episode id reaches the app, so it knows which one to look at.
	var last map[string]any
	probeJSON(t, tr.logged(), &last)
	if last["episodeId"] == nil {
		t.Errorf("the trigger must carry the episode id: %v", last)
	}
}

func TestDaySyncedCarriesNoEpisodeBecauseADayIsNotOne(t *testing.T) {
	tr := newTestRuntime(t)
	d := newDispatcher(t, tr)
	inst := tr.install(t, writeApp(t, manifestWith("dev.test.day", ``,
		`{"type":"memory","event":"day.synced"}`, ""), echoApp))
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}
	invs := d.DaySynced(context.Background(), time.Now())
	if len(invs) != 1 || invs[0].Outcome != OutcomeCompleted {
		t.Fatalf("%+v", invs)
	}
	for _, l := range tr.logged() {
		if strings.Contains(l, "episodeId") {
			t.Errorf("day.synced must not claim an episode: %s", l)
		}
	}
}

func TestScheduleFiresOnceAcrossATickThatCoversSeveralMinutes(t *testing.T) {
	tr := newTestRuntime(t)
	d := newDispatcher(t, tr)
	inst := tr.install(t, writeApp(t, manifestWith("dev.test.cron",
		`{"scope":"schedule","reason":"To post yesterday's summary every morning."}`,
		`{"type":"schedule","cron":"*/5 * * * *"}`, ""), echoApp))
	if err := d.Add(inst); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	// First sight is not a firing: otherwise every restart would re-run every
	// scheduled app, which for a "post the summary" app means posting it twice.
	if due := d.Due(base); len(due) != 0 {
		t.Errorf("first sight fired %v", due)
	}
	if due := d.Due(base.Add(3 * time.Minute)); len(due) != 0 {
		t.Errorf("no boundary crossed, got %v", due)
	}
	// One tick covering 09:03 → 09:31 crosses six firing minutes and must fire
	// once, not six times.
	if due := d.Due(base.Add(31 * time.Minute)); len(due) != 1 {
		t.Errorf("crossing several boundaries fired %d times", len(due))
	}
	if due := d.Due(base.Add(32 * time.Minute)); len(due) != 0 {
		t.Errorf("nothing crossed since, got %v", due)
	}
}

func TestAnAppTheMachineCannotContainIsRefusedAtAddTime(t *testing.T) {
	tr := newTestRuntime(t, func(o *Options) { o.DisableSandbox = true })
	d := newDispatcher(t, tr)
	inst := tr.install(t, writeApp(t, manifestWith("dev.test.reader",
		`{"scope":"memory.read","reason":"To find the meeting you just left."}`,
		`{"type":"phrase","match":"read"}`, ""), echoApp))

	err := d.Add(inst)
	if !errors.Is(err, ErrCannotContain) {
		t.Fatalf("want ErrCannotContain at add time, got %v", err)
	}
	if got := d.List(); len(got) != 0 {
		t.Errorf("a listed app that fails on every wake word is worse than a refusal: %v", got)
	}
}

func TestToolTriggerRequiresTheApptoHaveDeclaredOne(t *testing.T) {
	tr := newTestRuntime(t)
	d := newDispatcher(t, tr)
	noTool := tr.install(t, writeApp(t, manifestWith("dev.test.notool", ``,
		`{"type":"phrase","match":"x"}`, ""), echoApp))
	if err := d.Add(noTool); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Tool(context.Background(), "dev.test.notool", nil); err == nil {
		t.Error("an app that did not declare a tool trigger is not callable by the agent")
	}
	if _, err := d.Tool(context.Background(), "dev.test.missing", nil); err == nil {
		t.Error("an app that is not installed is not callable either")
	}
}
