package install

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/voice"
)

// Step 3a — choosing a voice. ORCHESTRATOR.md §2a.
//
// Speech is the output channel, so this is a step with a recommendation rather
// than an advanced setting. The two things the copy has to get right are in
// internal/voice, where the catalog lives; what this file guarantees is the
// behaviour behind the promise: **whatever happens here, the device talks.**
// A declined key, a typo'd reference, a provider outage at 2am — all of them
// end at the keyless fallback, and none of them end in silence.

// VoiceOutcome is what the voice step decided.
type VoiceOutcome struct {
	Option   voice.Option
	Fallback voice.Option
	Config   config.Voice
	Plan     voice.Plan
	// Checks is the primary and the fallback, both really called.
	Checks   []voice.Check
	Warnings []string
}

// OK reports whether the device will speak. That is deliberately not "the
// primary worked": a machine whose Simba key is wrong but whose fallback
// answers is a machine that talks, and reporting it as a failed install would
// be wrong about the thing that matters.
func (v VoiceOutcome) OK() bool {
	for _, c := range v.Checks {
		if c.OK() {
			return true
		}
	}
	return false
}

// PrimaryOK reports whether the chosen voice itself works.
func (v VoiceOutcome) PrimaryOK() bool {
	for _, c := range v.Checks {
		if c.Option == v.Option.ID {
			return c.OK()
		}
	}
	return false
}

// Line is the summary row.
func (v VoiceOutcome) Line() string {
	switch {
	case v.PrimaryOK():
		return v.Option.Label + " — verified"
	case v.OK():
		return v.Option.Label + " (not working yet) → falling back to " + v.Fallback.Label + ", so it still speaks"
	default:
		return v.Option.Label + " — could not be verified from here"
	}
}

func chooseVoice(ctx context.Context, opts Options) (VoiceOutcome, error) {
	p := opts.Prompt
	out := VoiceOutcome{Fallback: voice.Fallback()}

	choices := make([]Choice, 0, len(voice.Catalog()))
	for _, o := range voice.Catalog() {
		hint := strings.Join([]string{o.Quality, o.Latency, o.Cost}, " · ")
		if o.Hint != "" {
			hint += "\n      " + o.Hint
		}
		choices = append(choices, Choice{
			ID: o.ID, Label: o.Label, Hint: hint, Recommended: o.Recommended,
		})
	}

	id, err := p.Select(Question{
		ID:    "voice",
		Title: "Choose a voice",
		Body: "This is how Relay talks to you, so it is worth thirty seconds. Simba 3.2 is the " +
			"recommendation: it streams, so the first audio arrives before the sentence is " +
			"finished, and a typical user spends about $1.47 a month.\n\n" +
			"Whatever you pick — including picking nothing — the keyless voice sits underneath " +
			"it as an automatic fallback. A Relay device is never mute.",
		Choices: choices,
		Default: voice.Recommended().ID,
	})
	if err != nil {
		return out, err
	}
	opt, ok := voice.Get(id)
	if !ok {
		return out, fmt.Errorf("install: unknown voice %q", id)
	}
	out.Option = opt
	if opt.Note != "" {
		p.Say("  %s", wrapIndent(opt.Note, 2, 76))
	}

	cfg := voice.Config{
		Option:     opt.ID,
		Model:      opt.DefaultModel,
		Voice:      opt.DefaultVoice,
		HTTPClient: opts.HTTPClient,
		Lookup:     opts.Lookup(),
	}

	if opt.API == voice.APILocal {
		url, err := p.Input(Input{
			ID:     "voice.base_url",
			Prompt: "Local endpoint",
			Body: "The OpenAI-shaped speech endpoint your Piper or Kokoro server exposes, " +
				"e.g. http://127.0.0.1:8080/v1",
			Optional: true,
		})
		if err != nil {
			return out, err
		}
		cfg.BaseURL = url
	}

	if opt.NeedsCredential {
		ref, err := askCredential(ctx, opts, CredentialAsk{
			ID:        "voice.cred",
			Service:   "voice",
			Label:     opt.Label,
			EnvHint:   strings.ToUpper(opt.Vendor) + "_API_KEY",
			Optional:  true,
			SkipLabel: "Skip — use the keyless voice for now",
		})
		switch {
		case errors.Is(err, errCredentialSkipped):
			p.Say("  No key for %s, so Relay will use %s. That is not a failure state: the "+
				"device talks, merely not as well. Add a key later with `relay voice`.",
				opt.Label, out.Fallback.Label)
			out.Option = out.Fallback
			opt = out.Fallback
			cfg = voice.Config{Option: opt.ID, HTTPClient: opts.HTTPClient, Lookup: opts.Lookup()}
		case err != nil:
			return out, err
		default:
			cfg.Credential = ref
		}
	}

	// Every credential is tested with one real call before the installer exits.
	fbCfg := voice.Config{
		Option:     out.Fallback.ID,
		HTTPClient: opts.HTTPClient,
		Lookup:     opts.Lookup(),
	}
	out.Checks = voice.ProbePlan(ctx, cfg, fbCfg)
	for _, c := range out.Checks {
		p.Say("  %s", c.String())
		if c.Probed && !c.OK() {
			if advice := reasonAdvice(c.Reason); advice != "" {
				p.Say("      %s", advice)
			}
		}
	}

	out.Plan = voice.Plan{Primary: opt.ID, Fallback: out.Fallback.ID}
	if err := out.Plan.Validate(); err != nil {
		// Unreachable with the catalog as it stands, and asserted in a test: a
		// configuration that could go mute is a bug, not a warning.
		return out, err
	}

	out.Config = config.Voice{
		Provider:   opt.ID,
		Model:      cfg.Model,
		Credential: cfg.Credential.String(),
		Fallback:   out.Fallback.ID,
	}
	if cfg.Credential.Kind == llm.RefInline {
		return out, checkNoInline(map[string]string{"voice.credential": out.Config.Credential})
	}

	switch {
	case out.PrimaryOK():
	case !out.OK():
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"Neither %s nor the keyless fallback answered from this machine. Check this box's "+
				"outbound network before pairing — a device that cannot synthesise is a device "+
				"that says nothing.", opt.Label))
	case !opt.Probeable:
		// Not a failure. The phone is the synthesiser, so there is nothing here
		// to call, and saying "could not be verified" would read as broken.
		p.Say("  %s cannot be tested from this machine, and that is expected — %s",
			opt.Label, opt.ProbeNote)
	default:
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"%s could not be verified, so speech falls back to %s until it is fixed. "+
				"Run `relay voice` to try again.", opt.Label, out.Fallback.Label))
	}
	return out, nil
}
