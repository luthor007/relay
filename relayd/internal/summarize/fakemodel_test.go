package summarize_test

import (
	"context"
	"errors"
	"sync"

	"github.com/luthor007/relay/relayd/internal/llm"
)

// fakeModel is a local llm.Provider. Nothing in this package's tests makes a
// network call; internal/llm's own rule is that the transport is injectable and
// this is the same rule one level up.
type fakeModel struct {
	mu sync.Mutex
	// Reply is returned for every completion when Replies is empty.
	Reply string
	// Replies is consumed in order, so a test can give the title call and the
	// summary call different answers.
	Replies []string
	Err     error

	Calls   int
	Prompts []string
	Systems []string
}

var _ llm.Provider = (*fakeModel)(nil)

func (f *fakeModel) Vendor() string { return "fake" }
func (f *fakeModel) Model() string  { return "fake-small" }
func (f *fakeModel) API() llm.API   { return llm.APIOpenAI }

func (f *fakeModel) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	f.Systems = append(f.Systems, req.System)
	if len(req.Messages) > 0 {
		f.Prompts = append(f.Prompts, req.Messages[0].Text)
	}
	if f.Err != nil {
		return llm.Response{}, f.Err
	}
	out := f.Reply
	if len(f.Replies) > 0 {
		out = f.Replies[0]
		f.Replies = f.Replies[1:]
	}
	return llm.Response{Model: f.Model(), Text: out}, nil
}

func (f *fakeModel) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return nil, errors.New("fake: no streaming")
}

func (f *fakeModel) Probe(context.Context) llm.ProbeResult {
	return llm.ProbeResult{Vendor: f.Vendor(), Model: f.Model(), Reason: llm.ReasonOK}
}

func (f *fakeModel) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls
}

func (f *fakeModel) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Prompts) == 0 {
		return ""
	}
	return f.Prompts[len(f.Prompts)-1]
}

func (f *fakeModel) allPrompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Prompts...)
}
