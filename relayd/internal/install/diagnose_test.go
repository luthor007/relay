package install

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/detect"
)

// Reading a failed probe out loud with a model.
//
// The feature is three sentences of prose on a screen. The tests are about the
// three ways it must not misbehave: it must not call anything unless it was
// given a key, it must not put a credential on the wire, and it must not be
// able to break the install it is trying to explain.

// diagnoseOpts wires the real environment, because a reference is resolved
// against the real environment at the moment of use — testing presence through
// one accessor and the value through another would prove nothing about a run.
func diagnoseOpts(t *testing.T, key string, h *http.Client) Options {
	t.Helper()
	// Neither variable may leak in from the developer's own shell, or the
	// off-without-a-key case passes for the wrong reason.
	t.Setenv(DiagnoseEnv, "")
	t.Setenv("OPENROUTER_API_KEY", "")
	if key != "" {
		t.Setenv(DiagnoseEnv, key)
	}
	return Options{Prompt: NewScript(map[string]string{}), HTTPClient: h, Env: detect.Env{}}
}

// Off unless a key exists. That absence is the opt-in: no key, no call, nothing
// sent anywhere, and an installer that behaves exactly as it did before.
func TestDiagnosisIsOffWithoutAKey(t *testing.T) {
	var calls int
	h := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{}`, r), nil
	})}

	got := diagnose(context.Background(), diagnoseOpts(t, "", h), DiagnoseFacts{
		What: "the small orchestrator model", Reason: "expired",
	})
	if got != "" {
		t.Errorf("diagnosis = %q with no key configured", got)
	}
	if calls != 0 {
		t.Errorf("made %d calls with no key configured; the absence of a key is the opt-in", calls)
	}
}

// The one this repo has already been bitten by: a second consumer of a digest
// that posts it to a model, with no redaction on it. The credential itself is
// never in the facts — a reference is a name — and everything the installer did
// not write goes through the measured detector first.
func TestDiagnosisNeverPutsACredentialOnTheWire(t *testing.T) {
	// Split so the literal never appears: GitHub push protection blocks a file
	// carrying an OpenRouter-shaped key, and it cannot tell this all-zeros fake
	// from a live one. It should not try.
	secret := "sk-or-" + "v1-" + strings.Repeat("0", 64)
	const leaked = "ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	var sent string
	h := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		return jsonResp(200,
			`{"model":"m","choices":[{"message":{"role":"assistant","content":"the key was rejected"}}]}`,
			r), nil
	})}

	opts := diagnoseOpts(t, secret, h)
	got := diagnose(context.Background(), opts, DiagnoseFacts{
		What:   "the small orchestrator model",
		Vendor: "OpenRouter",
		Reason: "expired",
		// A provider that echoes the request back has put a bearer in an error
		// body before. This is the field the installer did not write.
		Detail: "http 401: rejected token " + leaked,
		Ref:    "env:" + DiagnoseEnv,
	})
	if got == "" {
		t.Fatal("no diagnosis came back")
	}
	if sent == "" {
		t.Fatal("nothing was sent")
	}
	if strings.Contains(sent, secret) {
		t.Error("the resolved credential was sent to the provider in the request body")
	}
	if strings.Contains(sent, leaked) {
		t.Error("a credential in the provider's own error text was forwarded unredacted")
	}
	// The name is the whole point: it is what lets the model say "that variable
	// is set but the provider rejected it" instead of guessing.
	if !strings.Contains(sent, DiagnoseEnv) {
		t.Error("the reference name was not sent, so the model cannot say anything useful about it")
	}
}

// A diagnostic that breaks setup is worse than no diagnostic.
func TestDiagnosisFailureIsSilentRatherThanFatal(t *testing.T) {
	h := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return jsonResp(500, `{"error":"upstream is down"}`, r), nil
	})}
	opts := diagnoseOpts(t, "sk-or-"+"v1-whatever", h)

	if got := diagnose(context.Background(), opts, DiagnoseFacts{Reason: "expired"}); got != "" {
		t.Errorf("diagnosis = %q, want silence when the diagnoser itself fails", got)
	}
}

// The read is labelled as a model's read. An installer that otherwise only says
// what it measured must not present generated prose as measurement.
func TestDiagnosisIsLabelledAsAModelsRead(t *testing.T) {
	script := NewScript(map[string]string{})
	sayDiagnosis(script, "the key resolved and OpenRouter rejected it")

	out := script.Output()
	if !strings.Contains(out, DiagnoseModel) {
		t.Errorf("the diagnosis does not name the model that wrote it:\n%s", out)
	}
	if !strings.Contains(out, "the key resolved and OpenRouter rejected it") {
		t.Errorf("the diagnosis itself was not printed:\n%s", out)
	}
}

// A host cannot carry a token; a path can.
func TestOnlyTheHostOfAnEndpointIsSent(t *testing.T) {
	for base, want := range map[string]string{
		"https://openrouter.ai/api/v1":             "openrouter.ai",
		"https://chatgpt.com/backend-api/codex":    "chatgpt.com",
		"http://127.0.0.1:8080/v1/token/abcdef123": "127.0.0.1:8080",
		"":          "",
		"not a url": "",
	} {
		if got := hostOf(base); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", base, got, want)
		}
	}
}
