package llm

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Reason is a stable reason code for a credential probe.
//
// ORCHESTRATOR.md §2 names four, taken from OpenClaw's models status --probe:
// ok, missing_credential, expired, unresolved_ref. [ReasonUnavailable] is a
// fifth, added here because those four cannot express "the credential resolved
// and the provider answered, but not with a completion" — a wrong model id, a
// rate limit, a provider outage. Reporting one of those as expired would send
// the user to rotate a key that is fine. The addition is recorded in
// ORCHESTRATOR.md §2 alongside the original four.
type Reason string

const (
	ReasonOK                Reason = "ok"
	ReasonMissingCredential Reason = "missing_credential"
	ReasonExpired           Reason = "expired"
	ReasonUnresolvedRef     Reason = "unresolved_ref"
	ReasonUnavailable       Reason = "unavailable"
)

// Reasons lists every reason code.
func Reasons() []Reason {
	return []Reason{ReasonOK, ReasonMissingCredential, ReasonExpired, ReasonUnresolvedRef, ReasonUnavailable}
}

// ProbeTimeout caps a probe. The installer is already the slow part of setup
// and a hanging provider must not make it worse.
const ProbeTimeout = 30 * time.Second

// ProbeResult is what one real call found out.
type ProbeResult struct {
	Vendor string
	Model  string
	Reason Reason
	// Detail is the provider's own error, verbatim where we have it.
	// ORCHESTRATOR.md §2: report what the provider actually says rather than
	// shipping a table of which subscriptions currently permit what. Empirical
	// beats maintained.
	Detail  string
	Latency time.Duration
	At      time.Time
	// Ref is the credential reference that was tried, never the secret.
	Ref string
}

// OK reports whether the credential works.
func (r ProbeResult) OK() bool { return r.Reason == ReasonOK }

// probeRequest is the smallest call that proves a credential and a model id
// are both good.
func probeRequest() Request {
	return Request{
		Messages:  []Message{{Role: RoleUser, Text: "ping"}},
		MaxTokens: 1,
	}
}

// classify maps a resolution or transport failure onto a reason code.
func classify(err error) (Reason, string) {
	switch {
	case err == nil:
		return ReasonOK, ""
	case errors.Is(err, ErrMissingCredential):
		return ReasonMissingCredential, err.Error()
	case errors.Is(err, ErrUnresolvedRef):
		return ReasonUnresolvedRef, err.Error()
	}

	var he *HTTPError
	if errors.As(err, &he) {
		return classifyStatus(he.Status), he.Error()
	}
	return ReasonUnavailable, err.Error()
}

// classifyStatus maps an HTTP status onto a reason code.
//
// 401 and 403 are the credential's problem; everything else the provider says
// is the provider's problem, and calling a 404 "expired" would send someone to
// rotate a working key.
func classifyStatus(status int) Reason {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden,
		status == http.StatusPaymentRequired:
		return ReasonExpired
	case status >= 200 && status < 300:
		return ReasonOK
	default:
		return ReasonUnavailable
	}
}

// runProbe is the shared body of every provider's Probe.
func runProbe(ctx context.Context, p Provider, cfg Config) ProbeResult {
	res := ProbeResult{
		Vendor: cfg.Vendor,
		Model:  cfg.Model,
		At:     time.Now(),
		Ref:    cfg.Credential.String(),
	}

	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	start := time.Now()
	_, err := p.Complete(ctx, probeRequest())
	res.Latency = time.Since(start)
	res.Reason, res.Detail = classify(err)
	return res
}
