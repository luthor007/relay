package install

import (
	"context"
	"strings"
	"testing"
)

// `relay setup` writes the relay section
//
// It did not, for the whole life of the installer. Every other section of the
// config was written and `[relay]` was not, so SYSTEM.md §7's whole feature —
// reaching your machine from outside the house — could only be turned on by
// editing a TOML file the user had never opened, and the daemon's health screen
// reported "reachable only on its own network" about a decision nobody had been
// offered.

func TestTheInstallerWritesTheRelaySection(t *testing.T) {
	answers := baseAnswers()
	answers["relay"] = "hosted"
	opts, _, fs := newOpts(t, answers, nil)

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Relay.URL != DefaultRelay {
		t.Fatalf("the config says relay = %q", res.Config.Relay.URL)
	}

	// And it reached the file, which is the thing that actually matters: a
	// Result the CLI prints and never persists would look identical here.
	written := fs.Files[opts.ConfigPath]
	if !strings.Contains(written, "[relay]") || !strings.Contains(written, DefaultRelay) {
		t.Fatalf("the written config has no relay section:\n%s", written)
	}
}

func TestDecliningTheRelayIsARealAnswer(t *testing.T) {
	// A box on a LAN its owner never leaves genuinely does not need this, and an
	// installer that talks somebody out of "no" is an installer that turned a
	// question into a formality.
	answers := baseAnswers()
	answers["relay"] = "off"
	opts, _, _ := newOpts(t, answers, nil)

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Relay.URL != "" {
		t.Fatalf("declining still configured %q", res.Config.Relay.URL)
	}
	if res.Relay.Enabled() {
		t.Error("the outcome reports the relay on")
	}
	if !strings.Contains(res.Relay.Line(), "own network") {
		t.Errorf("the summary line is %q", res.Relay.Line())
	}
}

func TestTheInstallerSaysWhatTheRelayCanAndCannotSee(t *testing.T) {
	// This is the one part of a self-hosted install where traffic leaves the
	// house. §7's claim that the relay cannot read it is worth nothing if the
	// person it protects was never told it applied to them — and overstating it
	// would be the same mistake in the other direction.
	answers := baseAnswers()
	answers["relay"] = "off"
	opts, script, _ := newOpts(t, answers, nil)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	out := script.Output()
	for _, phrase := range []string{
		"can see",          // that a box id is online
		"cannot see",       // what was said
		"end-to-end",       // why
		"do not need this", // that no is a real answer
	} {
		if !strings.Contains(out, phrase) {
			t.Errorf("the relay copy never says %q", phrase)
		}
	}
}

func TestAnAlreadyConfiguredRelayIsLeftAlone(t *testing.T) {
	// Re-running the installer on a working box is usually about something
	// else. A menu whose default silently differs from the current state is how
	// a re-install turns a feature off that nobody was thinking about.
	answers := baseAnswers()
	delete(answers, "relay") // asking at all would fail the scripted run
	opts, script, _ := newOpts(t, answers, func(o *Options) {
		o.Config.Relay.URL = "wss://relay.example"
	})

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Relay.URL != "wss://relay.example" {
		t.Fatalf("a configured relay became %q", res.Config.Relay.URL)
	}
	if !strings.Contains(script.Output(), "Already configured") {
		t.Error("it changed the setting without saying so")
	}
}

func TestATypedRelayUrlIsCheckedWhileThePersonIsStillThere(t *testing.T) {
	// The failure this prevents: a URL that fails config.validate makes the
	// daemon refuse to load the *whole file*, so a typo typed at this prompt
	// presents as a box that no longer starts, with an error naming a config
	// file the user has never opened.
	for _, tc := range []struct {
		name, typed, want string
	}{
		{"https", "https://relay.example", "wss://"},
		{"a path", "wss://relay.example/rz/v1", "path"},
		{"plaintext to a public host", "ws://relay.example", "wss://"},
		{"not a url", "relay.example", "ws://"},
		{"nothing", "   ", "no URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, problem := normaliseRelayURL(tc.typed)
			if problem == "" {
				t.Fatalf("%q was accepted as %q", tc.typed, got)
			}
			if !strings.Contains(problem, tc.want) {
				t.Errorf("the message does not mention %q: %s", tc.want, problem)
			}
		})
	}

	// And the two forgiven shapes stay forgiven.
	for _, typed := range []string{"wss://relay.example/", " wss://relay.example ", "ws://localhost:8080"} {
		if got, problem := normaliseRelayURL(typed); problem != "" {
			t.Errorf("%q was refused: %s (%q)", typed, problem, got)
		}
	}
}

func TestABadTypedUrlLeavesTheRelayOffRatherThanBreakingTheInstall(t *testing.T) {
	// An installer that aborts half way is worse than one that finishes and
	// says what it could not do.
	answers := baseAnswers()
	answers["relay"] = "own"
	answers["relay-url"] = "https://relay.example"
	opts, _, _ := newOpts(t, answers, nil)

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("a typo aborted the install: %v", err)
	}
	if res.Config.Relay.URL != "" {
		t.Fatalf("a refused URL was written: %q", res.Config.Relay.URL)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "relay") {
			found = true
		}
	}
	if !found {
		t.Errorf("nothing warned about it: %v", res.Warnings)
	}
}
