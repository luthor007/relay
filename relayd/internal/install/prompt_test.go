package install

import (
	"bytes"
	"strings"
	"testing"
)

func termWith(input string) (*Terminal, *bytes.Buffer) {
	out := &bytes.Buffer{}
	// SecretFD -1 is never a terminal, so the secret path takes its fallback.
	return &Terminal{In: strings.NewReader(input), Out: out, SecretFD: -1}, out
}

func menu() Question {
	return Question{
		ID: "vendor", Title: "Vendor", Body: "pick one",
		Choices: []Choice{
			{ID: "openrouter", Label: "OpenRouter", Hint: "API key", Recommended: true},
			{ID: "custom", Label: "Custom Provider", Hint: "any endpoint", Last: true},
			{ID: "google", Label: "Google", Hint: "Gemini API key + OAuth",
				Risk: "Unofficial flow; review the account-risk warning before use."},
		},
		Default: "openrouter",
	}
}

func TestSelectRendersHintsRiskAndTheRecommendation(t *testing.T) {
	term, out := termWith("\n")
	got, err := term.Select(menu())
	if err != nil {
		t.Fatal(err)
	}
	if got != "openrouter" {
		t.Errorf("empty answer should take the default, got %q", got)
	}
	s := out.String()
	if !strings.Contains(s, "(recommended)") {
		t.Error("the recommendation must be visible")
	}
	// Risk is a hint on the row, not a wall in front of it: it renders under
	// the option, and the option is still numbered and selectable.
	if !strings.Contains(s, "account-risk warning") {
		t.Error("the risk hint must render")
	}
	if strings.Contains(s, "not available") || strings.Contains(s, "disabled") {
		t.Error("a risky row must not be presented as unavailable")
	}
	// Custom Provider is always the last row.
	if strings.Index(s, "Custom Provider") < strings.Index(s, "Google") {
		t.Errorf("Custom Provider must sort last:\n%s", s)
	}
}

func TestSelectAcceptsANumberOrAnID(t *testing.T) {
	term, _ := termWith("3\n")
	got, err := term.Select(menu())
	if err != nil {
		t.Fatal(err)
	}
	// Ordered: openrouter, google, custom — Custom Provider having moved last.
	if got != "custom" {
		t.Errorf("chose %q by number", got)
	}

	term, _ = termWith("google\n")
	got, err = term.Select(menu())
	if err != nil {
		t.Fatal(err)
	}
	if got != "google" {
		t.Errorf("chose %q by id", got)
	}
}

func TestSelectRejectsNonsenseAndAsksAgain(t *testing.T) {
	term, out := termWith("99\nzzz\n2\n")
	got, err := term.Select(menu())
	if err != nil {
		t.Fatal(err)
	}
	if got != "google" {
		t.Errorf("got %q", got)
	}
	if strings.Count(out.String(), "not one of the options") != 2 {
		t.Errorf("both bad answers should have been rejected:\n%s", out.String())
	}
}

func TestConfirmDefaults(t *testing.T) {
	term, _ := termWith("\n")
	yes, err := term.Confirm(Confirm{Prompt: "ok?", Default: true})
	if err != nil || !yes {
		t.Errorf("yes=%v err=%v", yes, err)
	}

	term, _ = termWith("\n")
	yes, _ = term.Confirm(Confirm{Prompt: "ok?", Default: false})
	if yes {
		t.Error("an empty answer must take the default, and the default here is no")
	}

	term, _ = termWith("Y\n")
	yes, _ = term.Confirm(Confirm{Prompt: "ok?", Default: false})
	if !yes {
		t.Error("Y is yes")
	}
}

// A secret read on something that is not a terminal cannot turn echo off, and
// the user is told rather than having a key silently land in their scrollback.
func TestSecretInputWarnsWhenItCannotHideTheKey(t *testing.T) {
	term, out := termWith("sk-secret\n")
	got, err := term.Input(Input{Prompt: "key", Secret: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-secret" {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(out.String(), "will be echoed") {
		t.Errorf("the user has to be told:\n%s", out.String())
	}
}

// A scripted run fails on a question it was not told about, so adding a
// question to the flow is a failing test rather than a silent default.
func TestScriptFailsOnAnUnknownQuestion(t *testing.T) {
	s := NewScript(map[string]string{})
	if _, err := s.Select(menu()); err == nil {
		t.Fatal("want an error for an unanswered question")
	}
	s = NewScript(map[string]string{"vendor": "nope"})
	if _, err := s.Select(menu()); err == nil {
		t.Fatal("want an error for an answer that is not one of the choices")
	}
}

// An unattended run may not opt into anything that defaults to off.
func TestAutoTakesDefaultsAndNeverInvents(t *testing.T) {
	a := &Auto{}
	got, err := a.Select(menu())
	if err != nil || got != "openrouter" {
		t.Errorf("got %q err=%v", got, err)
	}
	yes, _ := a.Confirm(Confirm{Prompt: "install something?", Default: false})
	if yes {
		t.Error("an unattended run said yes to something that defaults to no")
	}
	if _, err := a.Input(Input{ID: "key", Prompt: "key"}); err == nil {
		t.Error("an unattended run cannot invent an answer with no default")
	}
}

func TestWrapKeepsParagraphs(t *testing.T) {
	got := wrap("one two three four five", 9)
	if got != "one two\nthree\nfour five" {
		t.Errorf("wrap = %q", got)
	}
	if wrap("a\n\nb", 20) != "a\n\nb" {
		t.Errorf("blank lines should survive: %q", wrap("a\n\nb", 20))
	}
}
