package appstore_test

import (
	"context"
	"strings"
	"testing"
)

// An empty log rendered as an empty log reads as "the app ran and said
// nothing". That is a different statement from "nothing on this box can run
// apps yet", and only one of them is true.
func TestLogsSayWhatTheyCannotShow(t *testing.T) {
	in, st := installer(t, "registry", &recorder{answer: true})
	if _, err := in.Install(context.Background(), "dev.alexis.standup-notes"); err != nil {
		t.Fatal(err)
	}

	view, err := st.View("standup-notes", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Events) == 0 {
		t.Fatal("the lifecycle events are missing")
	}
	if view.Note == "" {
		t.Fatal("a log with no invocations in it must say why")
	}
	if !strings.Contains(view.Note, "No app runtime is attached") {
		t.Errorf("note = %q", view.Note)
	}

	// Once the runtime writes something of its own, the note goes away: it
	// describes an absence, and the absence is over.
	if err := st.Append(view.App.ID(), "trigger.fired", "phrase %q matched", "wrap up the standup"); err != nil {
		t.Fatal(err)
	}
	view, err = st.View("standup-notes", 0)
	if err != nil {
		t.Fatal(err)
	}
	if view.Note != "" {
		t.Errorf("note = %q, want none once there are invocation records", view.Note)
	}
}

// A provisioned app that has simply not been triggered yet is a third state,
// and saying "no runtime" about it would be wrong.
func TestAProvisionedAppThatHasNotRunSaysThat(t *testing.T) {
	in, st := installer(t, "registry", &recorder{answer: true})
	in.Provisioner = &fakeRuntime{}
	if _, err := in.Install(context.Background(), "dev.alexis.standup-notes"); err != nil {
		t.Fatal(err)
	}
	view, err := st.View("standup-notes", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Note, "has not run yet") {
		t.Errorf("note = %q", view.Note)
	}
	if strings.Contains(view.Note, "No app runtime is attached") {
		t.Errorf("note = %q; there is a runtime here", view.Note)
	}
}

func TestViewOfAnAppThatIsNotInstalled(t *testing.T) {
	st := newStore(t)
	if _, err := st.View("nothing", 0); err == nil ||
		!strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v", err)
	}
}
