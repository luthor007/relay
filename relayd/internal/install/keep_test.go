package install

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/config"
)

// The second run.
//
// From the user, after a day of them: "since I am already logged in in codex and
// claude, could it just pick up from there instead of restarting all the time,
// same thing for my Simba api key?" Every run started from nothing — choose a
// voice, hand over a key, choose a vendor, sign in to ChatGPT again — while the
// config file sitting next to the installer already held all of it.

// configured is a config.toml as a finished run leaves it.
func configured() config.Config {
	c := config.Default()
	c.Voice = config.Voice{
		Provider: "speechify", Model: "simba-3.2",
		Credential: "env:SPEECHIFY_API_KEY", Fallback: "edge",
	}
	c.Models = config.Models{
		Small: config.Model{
			Vendor: "openrouter", Model: "deepseek/deepseek-v4-pro-0813",
			Credential: "env:OPENROUTER_API_KEY",
		},
		Big: config.Model{
			Vendor: "openrouter", Model: "x-ai/grok-4.6",
			Credential: "env:OPENROUTER_API_KEY",
		},
	}
	return c
}

// keepAnswers is a run where the user presses return at every keep question.
func keepAnswers() map[string]string {
	a := baseAnswers()
	for _, id := range []string{"voice.keep", "models.small.keep", "models.big.keep"} {
		a[id] = "yes"
	}
	// And none of the questions those replace are answered, so reaching one is
	// a failed run rather than a silent default.
	for _, id := range []string{
		"voice", "voice.cred.kind", "voice.cred.env",
		"models.small.vendor", "models.small.model", "models.small.cred.kind",
		"models.small.cred.env", "models.big.vendor", "models.big.model", "models.big.reuse",
	} {
		delete(a, id)
	}
	return a
}

func TestASecondRunKeepsWhatTheFirstOneVerified(t *testing.T) {
	answers := keepAnswers()
	opts, script, _ := newOpts(t, answers, func(o *Options) { o.Config = configured() })

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}

	// What was configured is what came out.
	if got := res.Config.Voice.Provider; got != "speechify" {
		t.Errorf("voice = %q, want the one already configured", got)
	}
	if got := res.Config.Models.Big.Model; got != "x-ai/grok-4.6" {
		t.Errorf("big model = %q, want the one already configured", got)
	}
	if !res.Models.Big.OK() || !res.Voice.OK() {
		t.Errorf("kept without verifying: voice=%v big=%+v", res.Voice.OK(), res.Models.Big.Probe)
	}
	// And it was verified rather than believed: a kept credential is probed
	// with the same real call the step would have ended with.
	if !strings.Contains(script.Output(), "x-ai/grok-4.6: ok") {
		t.Errorf("no real call stands behind the kept model:\n%s", script.Output())
	}
}

// Kept means kept, not re-asked with a different default: none of the questions
// a first run asks may appear at all.
func TestKeepingAsksOneQuestionPerStepAndNoMore(t *testing.T) {
	answers := keepAnswers()
	opts, script, _ := newOpts(t, answers, func(o *Options) { o.Config = configured() })

	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	for _, id := range script.Asked {
		switch {
		case id == "voice", strings.HasPrefix(id, "voice.cred"):
			t.Errorf("asked %q about a voice that already works", id)
		case strings.HasPrefix(id, "models.") && !strings.HasSuffix(id, ".keep"):
			t.Errorf("asked %q about a model that already works", id)
		}
	}
}

// A stored credential that no longer works is not kept quietly. The whole point
// of testing it first is that a revoked key looks like a revoked key.
func TestABrokenStoredCredentialIsNotKept(t *testing.T) {
	answers := baseAnswers()
	// The first-run questions are answered, because this run has to fall
	// through to them.
	answers["voice.keep"] = "yes"
	answers["models.small.keep"] = "yes"
	answers["models.big.keep"] = "yes"
	// Falling through means meeting the repair loop, which is the point: a
	// credential that stopped working is a problem to explain, not one to keep.
	answers["voice.repair"] = "continue"
	answers["models.small.repair"] = "continue"
	answers["models.big.repair"] = "continue"

	opts, script, _ := newOpts(t, answers, func(o *Options) {
		o.Config = configured()
		// Nothing this config points at answers: every provider is 401, which
		// is what a revoked key looks like from here.
		o.HTTPClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			return jsonResp(401, `{"error":{"message":"revoked"}}`, r), nil
		})}
	})
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("%v\n%s", err, script.Output())
	}
	for _, id := range script.Asked {
		if strings.HasSuffix(id, ".keep") {
			t.Errorf("offered to keep %q without a call that proved it", id)
		}
	}
	// And it says why it is asking again. A second run that re-asks the whole
	// model question with no explanation reads as an installer that forgot —
	// which is exactly how this looked from the outside.
	out := script.Output()
	for _, want := range []string{"already configured here, and did not answer"} {
		if !strings.Contains(out, want) {
			t.Errorf("re-asked silently, without saying why:\n%s", out)
		}
	}
}
