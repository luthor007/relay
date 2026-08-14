package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/llm"
)

// Probing a voice credential, ORCHESTRATOR.md §2: every credential is tested
// with one real call before the installer exits — synthesise a word, complete a
// token. A failure is reported at setup with the provider's own error.
//
// The alternative is a pairing code that works, glasses that pair, and silence
// the first time someone speaks. That is the worst possible place to discover a
// bad key, and it is the exact failure this package exists to move forward in
// time.
//
// The request shapes below come from vendor documentation rather than from a
// call against a live account — which is precisely why the reply is reported
// verbatim rather than interpreted. If a shape is wrong, the provider says so
// in its own words at install time and the user sees the truth. Reporting our
// guess about their error would be worse than reporting their error.

// ProbeWord is what gets synthesised. One short word: enough to prove the
// credential and the model id, cheap enough to be free in every plan.
const ProbeWord = "relay"

// Timeout caps a probe. The installer is already the slow part of setup.
const Timeout = 30 * time.Second

// Config is one configured voice.
type Config struct {
	// Option is a catalog id.
	Option string
	// Credential is a reference, never a secret (ORCHESTRATOR.md §2).
	Credential llm.CredentialRef
	Model      string
	Voice      string
	// BaseURL overrides the catalog entry; required for the self-hosted row.
	BaseURL string

	// HTTPClient is the injection point. Nothing here makes a network call in a
	// test.
	HTTPClient *http.Client
	// Lookup resolves a "vault:<id>" reference.
	Lookup llm.SecretLookup
	// Timeout defaults to Timeout.
	Timeout time.Duration
}

// Check is what one real call found out.
//
// Probed is separate from Reason because some rows cannot be tested from this
// machine at all — phone-native synthesis happens on the handset. Reporting
// that as "ok" would be claiming a verification that never happened, which is
// the same mistake as an adapter emitting an event it did not observe. So an
// untestable row is reported as untested, with the reason.
type Check struct {
	Option string
	Label  string

	// Probed is whether a real call was made.
	Probed bool
	// Reason is meaningful only when Probed. It reuses ORCHESTRATOR.md §2's
	// stable codes so the console and the installer speak one vocabulary.
	Reason llm.Reason
	// Detail is the provider's own message, verbatim where we have it.
	Detail string

	// Bytes is how much audio came back, which is the only proof that matters.
	Bytes   int
	Latency time.Duration
	At      time.Time
	// Ref is the credential reference tried, never the secret.
	Ref string
}

// OK reports a verified working voice.
func (c Check) OK() bool { return c.Probed && c.Reason == llm.ReasonOK }

// Verdict is the word the installer prints in front of the detail.
func (c Check) Verdict() string {
	if !c.Probed {
		return "not tested here"
	}
	if c.Reason == llm.ReasonOK {
		return "ok"
	}
	return string(c.Reason)
}

// String is the whole line.
func (c Check) String() string {
	label := c.Label
	if label == "" {
		label = c.Option
	}
	s := fmt.Sprintf("%s: %s", label, c.Verdict())
	if c.OK() && c.Bytes > 0 {
		s += fmt.Sprintf(" (%d bytes of audio in %s)", c.Bytes, c.Latency.Round(time.Millisecond))
	}
	if c.Detail != "" {
		s += " — " + c.Detail
	}
	return s
}

// Probe makes one real call. Like llm.Probe it never returns an error: a failed
// probe is a result the installer prints, not a reason to abort setup half way
// through.
func Probe(ctx context.Context, cfg Config) Check {
	opt, ok := Get(cfg.Option)
	c := Check{Option: cfg.Option, At: time.Now(), Ref: cfg.Credential.String()}
	if !ok {
		c.Probed = false
		c.Detail = "no such voice option " + cfg.Option
		return c
	}
	c.Label = opt.Label

	if !opt.Probeable {
		c.Probed = false
		c.Detail = opt.ProbeNote
		return c
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = opt.BaseURL
	}
	if opt.API == APILocal && baseURL == "" {
		c.Probed = false
		c.Detail = "no local endpoint configured yet — set the base URL of your Piper or Kokoro server"
		return c
	}

	var secret string
	if opt.NeedsCredential {
		var err error
		secret, err = cfg.Credential.Resolve(ctx, cfg.Lookup)
		if err != nil {
			c.Probed = false
			switch {
			case errors.Is(err, llm.ErrMissingCredential):
				c.Reason = llm.ReasonMissingCredential
			case errors.Is(err, llm.ErrUnresolvedRef):
				c.Reason = llm.ReasonUnresolvedRef
			default:
				c.Reason = llm.ReasonUnavailable
			}
			c.Detail = err.Error()
			// A resolution failure is still a determination, so the reason is
			// meaningful even though no call went out.
			c.Probed = true
			return c
		}
	} else if !cfg.Credential.IsZero() {
		// A key on a keyless row is harmless and occasionally deliberate.
		secret, _ = cfg.Credential.Resolve(ctx, cfg.Lookup)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := buildRequest(ctx, opt, cfg, baseURL, secret)
	if err != nil {
		c.Probed = true
		c.Reason = llm.ReasonUnavailable
		c.Detail = err.Error()
		return c
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}

	start := time.Now()
	resp, err := client.Do(req)
	c.Latency = time.Since(start)
	c.Probed = true
	if err != nil {
		c.Reason = llm.ReasonUnavailable
		c.Detail = err.Error()
		return c
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	c.Bytes = len(body)

	// The keyless row is the one place where any answer at all is the answer we
	// are after, because synthesis there is a websocket we do not open here.
	if opt.API == APIEdge {
		c.Reason = llm.ReasonOK
		c.Detail = fmt.Sprintf("the keyless voice service answered (HTTP %d). %s", resp.StatusCode, opt.ProbeNote)
		return c
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden,
		resp.StatusCode == http.StatusPaymentRequired:
		// The credential's problem. 402 belongs here: an unpaid account is a
		// credential that no longer buys anything.
		c.Reason = llm.ReasonExpired
		c.Detail = providerMessage(resp, body)
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if len(body) == 0 {
			// A 200 with no audio is not a working voice. Calling it ok is how
			// a device ends up silent with a green tick next to it.
			c.Reason = llm.ReasonUnavailable
			c.Detail = "the provider accepted the request and returned no audio"
			return c
		}
		if looksLikeJSONError(body) {
			c.Reason = llm.ReasonUnavailable
			c.Detail = providerMessage(resp, body)
			return c
		}
		c.Reason = llm.ReasonOK
	default:
		// Everything else the provider says is the provider's problem — a wrong
		// model id, a rate limit, an outage. Reporting any of those as expired
		// sends the user to rotate a key that is fine, which is the one failure
		// mode a probe exists to prevent (ORCHESTRATOR.md §2).
		c.Reason = llm.ReasonUnavailable
		c.Detail = providerMessage(resp, body)
	}
	return c
}

// ProbePlan probes a primary and its fallback. The fallback is probed too,
// because a fallback nobody has tested is a promise rather than a safety net.
func ProbePlan(ctx context.Context, primary Config, fallback Config) []Check {
	var out []Check
	if primary.Option != "" {
		out = append(out, Probe(ctx, primary))
	}
	if fallback.Option != "" && fallback.Option != primary.Option {
		out = append(out, Probe(ctx, fallback))
	}
	return out
}

func buildRequest(ctx context.Context, opt Option, cfg Config, baseURL, secret string) (*http.Request, error) {
	model := cfg.Model
	if model == "" {
		model = opt.DefaultModel
	}
	voice := cfg.Voice
	if voice == "" {
		voice = opt.DefaultVoice
	}
	base := strings.TrimRight(baseURL, "/")

	jsonReq := func(url string, payload any) (*http.Request, error) {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		return r, nil
	}

	switch opt.API {
	case APISpeechify:
		// POST /v1/audio/stream, and the body names the model and the voice as
		// `model` and `voice_id`. The old shape here — /speech, with `voice`
		// and a `format` — was aimed at a host that has never existed
		// (api.simba.audio, NXDOMAIN), so nothing about it was ever confirmed
		// against a server. Simba 3.2 is Speechify's model, not a vendor.
		r, err := jsonReq(base+"/v1/audio/stream", map[string]any{
			"model": model, "voice_id": voice, "input": ProbeWord,
		})
		if err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", "Bearer "+secret)
		return r, nil

	case APIOpenAI, APILocal:
		payload := map[string]any{"model": model, "input": ProbeWord}
		if voice != "" {
			payload["voice"] = voice
		}
		r, err := jsonReq(base+"/audio/speech", payload)
		if err != nil {
			return nil, err
		}
		if secret != "" {
			r.Header.Set("Authorization", "Bearer "+secret)
		}
		return r, nil

	case APIElevenLabs:
		if voice == "" {
			return nil, errors.New("elevenlabs needs a voice id")
		}
		r, err := jsonReq(base+"/text-to-speech/"+voice, map[string]any{
			"text": ProbeWord, "model_id": model,
		})
		if err != nil {
			return nil, err
		}
		r.Header.Set("xi-api-key", secret)
		return r, nil

	case APICartesia:
		payload := map[string]any{
			"model_id": model, "transcript": ProbeWord,
			"output_format": map[string]any{
				"container": "mp3", "sample_rate": 44100, "bit_rate": 128000,
			},
		}
		if voice != "" {
			payload["voice"] = map[string]any{"mode": "id", "id": voice}
		}
		r, err := jsonReq(base+"/tts/bytes", payload)
		if err != nil {
			return nil, err
		}
		r.Header.Set("X-API-Key", secret)
		r.Header.Set("Cartesia-Version", "2024-11-13")
		return r, nil

	case APIDeepgram:
		r, err := jsonReq(base+"/speak?model="+model, map[string]any{"text": ProbeWord})
		if err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", "Token "+secret)
		return r, nil

	case APIEdge:
		// No credential and no synthesis: this asks the keyless service whether
		// it is there at all, and the Check says exactly that much.
		return http.NewRequestWithContext(ctx, http.MethodGet, base+"/voices/list", nil)
	}
	return nil, fmt.Errorf("voice: no probe defined for %s", opt.API)
}

// providerMessage extracts something a human can act on, without ever echoing
// the request back.
func providerMessage(resp *http.Response, body []byte) string {
	msg := strings.TrimSpace(string(body))
	var doc map[string]any
	if json.Unmarshal(body, &doc) == nil {
		if m := digMessage(doc); m != "" {
			msg = m
		}
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	if msg == "" {
		return fmt.Sprintf("HTTP %d, and the provider said nothing", resp.StatusCode)
	}
	return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, msg)
}

func digMessage(doc map[string]any) string {
	for _, k := range []string{"message", "error", "detail", "error_message"} {
		switch v := doc[k].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return v
			}
		case map[string]any:
			if m := digMessage(v); m != "" {
				return m
			}
		}
	}
	return ""
}

// looksLikeJSONError catches a provider that answers 200 with an error body
// instead of audio. Audio never starts with '{'.
func looksLikeJSONError(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var doc map[string]any
	if json.Unmarshal(trimmed, &doc) != nil {
		return false
	}
	_, hasErr := doc["error"]
	_, hasMsg := doc["message"]
	return hasErr || hasMsg
}
