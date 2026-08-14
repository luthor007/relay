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

// verifyVoice picks a voice and offers to pick again when the chosen one did
// not answer. See repair.go.
//
// What counts as needing repair is narrower here than for a model, and getting
// that right is the whole difficulty of this step. The device talks either way —
// that is what the keyless fallback is for — so the loop must not open on:
//
//   - the phone, which cannot be tested from this machine at all and is not
//     broken for failing a test that was never possible;
//   - a deliberately skipped key, which chose the fallback on purpose and is a
//     real answer rather than a failure;
//   - a fallback that also failed, which is a network problem this box has and
//     re-picking a voice will not fix.
//
// What is left is exactly the case worth re-asking about: you chose a voice,
// you gave it a credential, and the credential did not work.
func verifyVoice(ctx context.Context, opts Options) (VoiceOutcome, error) {
	return verify(ctx, opts, repair[VoiceOutcome]{
		ID:     "voice.repair",
		Title:  "That voice is not working yet",
		Choose: func() (VoiceOutcome, error) { return chooseVoice(ctx, opts) },
		OK: func(v VoiceOutcome) bool {
			return v.PrimaryOK() || !v.Option.Probeable || v.Option.ID == v.Fallback.ID || !v.OK()
		},
		Trouble: func(v VoiceOutcome) string { return voiceTrouble(v) },
		Facts: func(v VoiceOutcome) DiagnoseFacts {
			f := DiagnoseFacts{
				What:   v.Option.Label + ", the voice",
				Vendor: v.Option.Vendor,
				Model:  v.Config.Model,
				Ref:    v.Config.Credential,
			}
			for _, c := range v.Checks {
				if c.Option == v.Option.ID {
					f.Reason = string(c.Reason)
					f.Detail = c.Detail
				}
			}
			return f
		},
		FixLabel:      "Choose again — a different voice, or a different key",
		ContinueLabel: "Leave it, and speak with the keyless voice for now",
		GiveUp: "Leaving speech on the keyless voice. The device still talks — it is the one " +
			"thing this step guarantees — and `relay voice` re-runs it whenever the key is ready.",
	})
}

func voiceTrouble(v VoiceOutcome) string {
	for _, c := range v.Checks {
		if c.Option == v.Option.ID && c.Probed && !c.OK() {
			s := fmt.Sprintf("%s did not answer: %s", v.Option.Label, c.Reason)
			if advice := reasonAdvice(c.Reason); advice != "" {
				s += "\n\n" + advice
			}
			s += fmt.Sprintf("\n\nSpeech falls back to %s, so the device is not mute either way.",
				v.Fallback.Label)
			return s
		}
	}
	return fmt.Sprintf("%s could not be verified from this machine.", v.Option.Label)
}

func chooseVoice(ctx context.Context, opts Options) (VoiceOutcome, error) {
	p := opts.Prompt
	out := VoiceOutcome{Fallback: voice.Fallback()}

	// The credential question can hand control back to the voice menu, which is
	// where somebody who picked the wrong row finds out — the row is a name and
	// the credential question is where it becomes a key they do not have.
restart:

	// One line per row, not two.
	//
	// Every row used to print quality · latency · cost and then a sentence
	// underneath, and the sentence usually said the same thing again —
	// "good to very good · streams · per-character" over "Very good voices,
	// priced per character." Eight options that way is the longest block in the
	// installer. The catalog's own hint is now one authored line carrying the
	// same three facts, and the triple is the fallback for a row without one.
	choices := make([]Choice, 0, len(voice.Catalog()))
	for _, o := range voice.Catalog() {
		hint := o.Hint
		if hint == "" {
			hint = strings.Join([]string{o.Quality, o.Latency, o.Cost}, " · ")
		}
		choices = append(choices, Choice{
			ID: o.ID, Label: o.Label, Hint: hint, Recommended: o.Recommended,
		})
	}

	id, err := p.Select(Question{
		ID:      "voice",
		Title:   "Choose a voice",
		Body:    "The keyless voice sits under whatever you pick, so it always speaks.",
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
			ID:       "voice.base_url",
			Prompt:   "Local endpoint",
			Body:     "e.g. http://127.0.0.1:8080/v1",
			Optional: true,
			Back:     true,
		})
		if errors.Is(err, ErrBack) {
			goto restart
		}
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
			Back:      true,
		})
		switch {
		case errors.Is(err, ErrBack):
			goto restart
		case errors.Is(err, errCredentialSkipped):
			p.Say("  Using %s instead. `relay voice` adds a key later.", out.Fallback.Label)
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
