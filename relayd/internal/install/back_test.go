package install

import (
	"context"
	"strings"
	"testing"
)

// Going back.
//
// "I selected ChatGPT but wanted OpenRouter" used to cost the whole step: the
// only way out of a wrong turn three questions ago was to finish the model with
// answers nobody wanted and run `relay models` again. A question can now hand
// control back to the one before it.

// countAsked is how many times a question id came up.
func countAsked(script *Script, id string) int {
	n := 0
	for _, got := range script.Asked {
		if got == id {
			n++
		}
	}
	return n
}

// The case from the install itself: the vendor menu was answered with OpenAI,
// the sign-in menu appeared, and OpenRouter was what was meant.
func TestBackFromTheSignInMenuReturnsToTheVendor(t *testing.T) {
	answers := baseAnswers()
	// The sign-in menu is answered with "back", and the vendor question is
	// answered differently the second time — which is the entire point of being
	// asked it again, and which a flat id->answer map cannot express.
	answers["models.small.auth"] = "back"

	opts, _, _ := newOpts(t, answers, nil)
	sp := &replayScript{Script: NewScript(answers), answers: answers, onVendor: func(n int) string {
		if n == 1 {
			return "openai"
		}
		return "openrouter"
	}}
	opts.Prompt = sp

	choice, err := chooseModel(context.Background(), opts, "small", "why", "m", nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, sp.Output())
	}
	if countAsked(sp.Script, "models.small.vendor") != 2 {
		t.Fatalf("the vendor question was asked %d times, want it asked again after back",
			countAsked(sp.Script, "models.small.vendor"))
	}
	if choice.Vendor.ID != "openrouter" {
		t.Errorf("vendor = %q, want the one chosen on the way back", choice.Vendor.ID)
	}
	// And the sign-in menu that was left behind does not haunt the result.
	if strings.HasPrefix(choice.Model.BaseURL, "https://chatgpt.com") {
		t.Errorf("base URL = %q, still pointing at the abandoned choice", choice.Model.BaseURL)
	}
}

// A vendor with one sign-in method never shows that menu, so back from the
// model id has to skip it rather than land on a question nobody was asked.
func TestBackSkipsTheQuestionsThatWereNeverAsked(t *testing.T) {
	answers := baseAnswers()
	answers["models.small.vendor"] = "openrouter" // one auth method: no menu
	answers["models.small.model"] = "back"

	opts, _, _ := newOpts(t, answers, nil)
	sp := &replayScript{Script: NewScript(answers), answers: answers}
	sp.onModel = func(n int) string {
		if n == 1 {
			return "back"
		}
		return "some-model"
	}
	opts.Prompt = sp

	if _, err := chooseModel(context.Background(), opts, "small", "why", "m", nil); err != nil {
		t.Fatalf("%v\n%s", err, sp.Output())
	}
	if got := countAsked(sp.Script, "models.small.auth"); got != 0 {
		t.Errorf("the sign-in menu was asked %d times for a vendor that has one", got)
	}
	if got := countAsked(sp.Script, "models.small.vendor"); got != 2 {
		t.Errorf("vendor asked %d times, want back from the model id to land there", got)
	}
}

// Back is only offered where somebody can act on it. An unhandled ErrBack would
// end the run at the moment the user asked for a much smaller correction.
func TestBackIsNotOfferedOnTheFirstQuestionOfTheStep(t *testing.T) {
	answers := baseAnswers()
	opts, _, _ := newOpts(t, answers, nil)
	sp := &replayScript{Script: NewScript(answers), answers: answers}
	opts.Prompt = sp

	if _, err := chooseModel(context.Background(), opts, "small", "why", "m", nil); err != nil {
		t.Fatalf("%v\n%s", err, sp.Output())
	}
	if sp.backOffered["models.small.vendor"] {
		t.Error("the first question of the first model offered a way back to nothing")
	}
	if !sp.backOffered["models.small.cred.kind"] {
		t.Error("the credential menu did not offer a way back")
	}
}

// replayScript is a Script that can answer the same question differently the
// second time, which is what a flat id->answer map cannot express and what
// every back does.
type replayScript struct {
	*Script
	answers  map[string]string
	onVendor func(n int) string
	onModel  func(n int) string

	backOffered map[string]bool
}

func (r *replayScript) note(id string, back bool) {
	if r.backOffered == nil {
		r.backOffered = map[string]bool{}
	}
	r.backOffered[id] = back
}

func (r *replayScript) Select(q Question) (string, error) {
	r.note(q.ID, q.Back)
	if q.ID == "models.small.vendor" && r.onVendor != nil {
		r.Script.Asked = append(r.Script.Asked, q.ID)
		return r.onVendor(countAsked(r.Script, q.ID)), nil
	}
	return r.Script.Select(q)
}

func (r *replayScript) Input(in Input) (string, error) {
	r.note(in.ID, in.Back)
	if in.ID == "models.small.model" && r.onModel != nil {
		r.Script.Asked = append(r.Script.Asked, in.ID)
		v := r.onModel(countAsked(r.Script, in.ID))
		if isBack(v) && in.Back {
			return "", ErrBack
		}
		return v, nil
	}
	return r.Script.Input(in)
}

func (r *replayScript) Confirm(c Confirm) (bool, error) {
	r.note(c.ID, c.Back)
	return r.Script.Confirm(c)
}
