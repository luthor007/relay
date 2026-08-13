package routing_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/routing"
)

// The LLM tie-break. ORCHESTRATOR.md §4.
//
// These tests are about the three ways it must not misbehave, because the cost
// of each is asymmetric: a decline costs the user one spoken question, and a
// wrong pick drops their question into an unrelated session and poisons it.

type tieTransport struct {
	status int
	body   string
	seen   string
	delay  time.Duration
}

func (t *tieTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		t.seen = string(b)
	}
	if t.delay > 0 {
		select {
		case <-time.After(t.delay):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	status := t.status
	if status == 0 {
		status = 200
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(t.body)),
		Request:    r,
	}, nil
}

// reply wraps a tie-break answer in the chat-completions shape.
func reply(content string) string {
	b, _ := json.Marshal(content)
	return `{"model":"m","choices":[{"message":{"role":"assistant","content":` + string(b) + `}}]}`
}

func tieProvider(t *testing.T, tr *tieTransport) llm.Provider {
	t.Helper()
	p, err := llm.New(llm.Config{
		Vendor: "openrouter", API: llm.APIOpenAI, Model: "m",
		Credential: llm.CredentialRef{Kind: llm.RefInline, Value: "k"},
		HTTPClient: &http.Client{Transport: tr},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func twoCandidates() []routing.Candidate {
	now := time.Now()
	return []routing.Candidate{
		{Session: routing.SessionView{
			ID: "s1", Subject: "payments refactor", Entities: []string{"billing"},
			Files: []string{"pkg/billing/invoice.go"}, LastActive: now.Add(-10 * time.Minute),
		}, Score: 0.51},
		{Session: routing.SessionView{
			ID: "s2", Subject: "flaky search tests", Entities: []string{"search"},
			Files: []string{"internal/search/rank_test.go"}, LastActive: now.Add(-12 * time.Minute),
		}, Score: 0.49},
	}
}

func TestTieBreakPicksFromTheShortlist(t *testing.T) {
	tr := &tieTransport{body: reply(`{"choice":2,"why":"names the search tests"}`)}
	pick, ok := routing.LLMTieBreak(tieProvider(t, tr)).Break(
		context.Background(),
		routing.Request{Text: "why is that search one still failing", At: time.Now()},
		twoCandidates())

	if !ok {
		t.Fatal("a clear answer was declined")
	}
	if pick.Session.ID != "s2" {
		t.Errorf("picked %q, want the session the model named", pick.Session.ID)
	}
	// The model's evidence replaces the scorer's, because the scorer's reason
	// is why it was a tie, and the announcement should say what actually
	// decided it.
	if pick.Why != "names the search tests" {
		t.Errorf("why = %q, want the model's own reason", pick.Why)
	}
}

// Declining is a first-class answer. KindAsk costs the user a second; a wrong
// continue is not recoverable by the session that receives it.
func TestTieBreakMayDecline(t *testing.T) {
	tr := &tieTransport{body: reply(`{"choice":0,"why":"could be either"}`)}
	if _, ok := routing.LLMTieBreak(tieProvider(t, tr)).Break(
		context.Background(), routing.Request{Text: "how's it going", At: time.Now()},
		twoCandidates()); ok {
		t.Error("0 means unsure, and unsure must not become a pick")
	}
}

// An index outside the shortlist is the same as a decline. A model that invents
// a third option has not answered the question it was asked.
func TestTieBreakRefusesAnInventedChoice(t *testing.T) {
	for _, body := range []string{
		reply(`{"choice":7,"why":"the other one"}`),
		reply(`{"choice":-1,"why":"nope"}`),
		reply(`not json at all`),
		reply(``),
	} {
		tr := &tieTransport{body: body}
		if _, ok := routing.LLMTieBreak(tieProvider(t, tr)).Break(
			context.Background(), routing.Request{Text: "carry on", At: time.Now()},
			twoCandidates()); ok {
			t.Errorf("answer %q was treated as a pick", body)
		}
	}
}

// A provider that is down, slow or angry produces a question to the user, never
// an error the caller has to handle mid-utterance.
func TestTieBreakFailureIsADeclineNotAnError(t *testing.T) {
	for name, tr := range map[string]*tieTransport{
		"http 500": {status: 500, body: `{"error":"boom"}`},
		"timeout":  {body: reply(`{"choice":1,"why":"x"}`), delay: 3 * routing.TieBreakTimeout},
	} {
		if _, ok := routing.LLMTieBreak(tieProvider(t, tr)).Break(
			context.Background(), routing.Request{Text: "carry on", At: time.Now()},
			twoCandidates()); ok {
			t.Errorf("%s: was treated as a pick", name)
		}
	}
}

// It runs on every ambiguous utterance, so what it sends is a standing
// disclosure rather than a one-off: subjects, entities and paths — the same
// things the scorer already matched on.
//
// Transcripts are not sent, and that is guaranteed by the type rather than by
// this test: SessionView has no transcript field, so there is nothing here to
// leak even by accident. What this pins is the other half — that everything the
// model needs to tell two sessions apart is actually in the prompt, because a
// tie-break given nothing to distinguish them with declines every time and the
// feature quietly does nothing.
func TestTieBreakSendsWhatTheSessionIsAbout(t *testing.T) {
	tr := &tieTransport{body: reply(`{"choice":1,"why":"billing"}`)}
	if _, ok := routing.LLMTieBreak(tieProvider(t, tr)).Break(
		context.Background(),
		routing.Request{Text: "the billing one", At: time.Now()},
		twoCandidates()); !ok {
		t.Fatal("declined a clear answer")
	}
	for _, want := range []string{
		"payments refactor", "flaky search tests", // subjects
		"billing", "search", // entities
		"pkg/billing/invoice.go", // files
		"the billing one",        // the utterance itself
	} {
		if !strings.Contains(tr.seen, want) {
			t.Errorf("the prompt is missing %q, so the model cannot tell them apart", want)
		}
	}
}

// A nil provider is the configuration where no big model is set up yet, and it
// must behave exactly as the tie-break did before it existed: ask.
func TestNoProviderDeclines(t *testing.T) {
	if _, ok := routing.LLMTieBreak(nil).Break(
		context.Background(), routing.Request{Text: "x", At: time.Now()},
		twoCandidates()); ok {
		t.Error("a nil provider must decline rather than panic or pick")
	}
}
