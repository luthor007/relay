package install

import (
	"context"
	"fmt"
	"strings"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/voice"
)

// Not asking again for an answer that is already on disk and still works.
//
// Setup is run more than once — after a failure, after a new release, after
// somebody changes their mind about one step — and every run used to start from
// nothing: choose a voice, hand over a key, choose a vendor, sign in to ChatGPT
// again. The config file sitting next to the installer already held all of it,
// verified, and was ignored.
//
// So each step that can be resumed asks one yes-or-no first, and only after the
// stored credential has been tested with the same real call the step ends with.
// Never a claim from a file: a key revoked since the last run has to look like a
// broken key, not like a working one nobody checked.

// keep asks whether to keep something already configured and verified. The
// question is one keystroke, and the alternative is the whole step.
func keep(p Prompter, id, what string) (bool, error) {
	return p.Confirm(Confirm{
		ID:      id,
		Prompt:  fmt.Sprintf("Keep %s?", what),
		Default: true,
	})
}

// keptVoice returns the configured voice when it still works, so a rerun does
// not ask for a key it already has.
func keptVoice(ctx context.Context, opts Options) (VoiceOutcome, bool, error) {
	cur := opts.Config.Voice
	if cur.Provider == "" {
		return VoiceOutcome{}, false, nil
	}
	opt, ok := voice.Get(cur.Provider)
	if !ok {
		// A provider this build no longer has. The menu is the honest answer.
		return VoiceOutcome{}, false, nil
	}
	out := VoiceOutcome{Option: opt, Fallback: voice.Fallback()}

	cfg := voice.Config{
		Option: opt.ID, Model: cur.Model, Voice: opt.DefaultVoice,
		HTTPClient: opts.HTTPClient, Lookup: opts.Lookup(),
	}
	if cur.Credential != "" {
		ref, err := llm.ParseRef(cur.Credential)
		if err != nil {
			return VoiceOutcome{}, false, nil
		}
		cfg.Credential = ref
	}
	fb := voice.Config{Option: out.Fallback.ID, HTTPClient: opts.HTTPClient, Lookup: opts.Lookup()}

	// The real call, before the offer. A stored key that no longer works must
	// not be kept quietly.
	checks := voice.ProbePlan(ctx, cfg, fb)
	var primary voice.Check
	for _, c := range checks {
		if c.Option == opt.ID {
			primary = c
		}
	}
	if !primary.Probed || !primary.OK() {
		say := opt.Label + " is already configured here, and did not answer just now"
		if d := strings.TrimSpace(primary.Detail); d != "" {
			say += ": " + d
		}
		opts.Prompt.Say("  %s.", wrapIndent(say, 2, 76))
		return VoiceOutcome{}, false, nil
	}

	p := opts.Prompt
	p.Say("  %s", primary.String())
	yes, err := keep(p, "voice.keep", opt.Label)
	if err != nil || !yes {
		return VoiceOutcome{}, false, err
	}

	out.Checks = checks
	out.Config = cur
	out.Plan = voice.Plan{Primary: opt.ID, Fallback: out.Fallback.ID}
	if err := out.Plan.Validate(); err != nil {
		return VoiceOutcome{}, false, err
	}
	return out, true, nil
}

// keptModel returns a configured model when its credential still answers.
//
// This is the ChatGPT case in particular: a subscription sign-in is the slowest
// thing in the install, the refresh token is already in Relay's vault, and the
// reference in the config resolves it without asking anybody for anything.
func keptModel(ctx context.Context, opts Options, role string) (ModelChoice, bool, error) {
	var cur config.Model
	switch role {
	case "small":
		cur = opts.Config.Models.Small
	case "big":
		cur = opts.Config.Models.Big
	}
	if cur.Model == "" || cur.Credential == "" {
		return ModelChoice{}, false, nil
	}
	vendor, ok := llm.Vendor(cur.Vendor)
	if !ok {
		return ModelChoice{}, false, nil
	}

	choice := ModelChoice{Role: role, Vendor: vendor, Model: cur}
	choice.Probe, choice.Probed = probeModel(ctx, opts, cur)
	if !choice.OK() {
		// Broken, or unreachable from here. Say which, because the alternative
		// is what this looked like from the outside: a second run asking the
		// whole model question again with no explanation, on a machine where
		// the answer was already on disk. A silent fall-through reads as the
		// installer having forgotten.
		say := cur.Model + " on " + vendor.Label + " is already configured here, and did not " +
			"answer just now"
		if d := strings.TrimSpace(choice.Probe.Detail); d != "" {
			say += ": " + d
		}
		opts.Prompt.Say("  %s.", wrapIndent(say, 2, 76))
		return ModelChoice{}, false, nil
	}

	p := opts.Prompt
	p.Say("  %s %s: %s", vendor.Label, cur.Model, describeProbe(choice.Probe))
	what := cur.Model
	if vendor.Label != "" {
		what += " on " + vendor.Label
	}
	yes, err := keep(p, "models."+role+".keep", what)
	if err != nil || !yes {
		return ModelChoice{}, false, err
	}
	// The auth method is not recorded in the config — only where the credential
	// lives — so a kept model carries the reference and nothing about how it was
	// obtained. That is enough for everything downstream except offering it for
	// reuse, which asks for an auth id it will not find, and correctly declines.
	return choice, true, nil
}
