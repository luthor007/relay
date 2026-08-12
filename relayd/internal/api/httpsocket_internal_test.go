package api

import (
	"net/http"
	"strings"
	"testing"
)

// The header allowlist, asserted where the decision lives
//
// This is internal rather than in `api_test` because the thing worth pinning is
// the allowlist itself, and routing around it end to end would mean asserting on
// an audit record two subsystems away — a test that fails for six reasons is a
// test nobody trusts when it goes red.

func TestATunnelledRequestCannotChooseWhatTheAuditLogRecords(t *testing.T) {
	// [Server.origin] reads X-Forwarded-For when the deployment says a proxy is
	// in front. A tunnel that carried arbitrary headers would therefore let
	// anyone who can open a stream write their own address into the audit log,
	// and a log that records an attacker's chosen origin is worse than one that
	// records none.
	tn := &tunnel{}
	r, err := tn.build(tunnelRequest{
		ID:     "1",
		Kind:   "req",
		Method: "GET",
		Path:   "/v1/health",
		Headers: map[string]string{
			"Authorization":     "Bearer t",
			"X-Forwarded-For":   "10.0.0.1",
			"X-Forwarded-Proto": "https",
			"X-Real-IP":         "10.0.0.1",
			"Cookie":            "session=stolen",
			"Host":              "example.com",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := r.Header.Get("Authorization"); got != "Bearer t" {
		t.Errorf("the credential did not survive: %q", got)
	}
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Real-IP", "Cookie"} {
		if got := r.Header.Get(name); got != "" {
			t.Errorf("%s was carried through as %q", name, got)
		}
	}

	// And the address it does record is the true one. "relay" is not a host:port,
	// so [Server.origin] returns it verbatim — the audit log says the request
	// arrived over the relay, which is the complete answer to where it came
	// from. A plausible-looking address here would be the log claiming to know
	// something it does not.
	s := &Server{}
	if got := s.origin(r); got != "relay" {
		t.Errorf("a tunnelled request is logged as coming from %q", got)
	}
}

func TestTheTunnelRefusesAMethodItShouldNotCarry(t *testing.T) {
	tn := &tunnel{}
	if _, err := tn.build(tunnelRequest{ID: "1", Kind: "req", Method: "CONNECT", Path: "/v1/health"}); err == nil {
		t.Error("CONNECT was accepted")
	}
	// And an absent method is a GET rather than a refusal, because that is the
	// overwhelmingly common frame and requiring it would be ceremony.
	r, err := tn.build(tunnelRequest{ID: "1", Kind: "req", Path: "/v1/health"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != http.MethodGet {
		t.Errorf("a frame with no method became %s", r.Method)
	}
}

func TestTheQueryStringSurvives(t *testing.T) {
	// Six of the console's calls are filters expressed as query parameters. A
	// tunnel that dropped them would show every screen unfiltered — which looks
	// like working software right up to the moment somebody trusts a count.
	tn := &tunnel{}
	r, err := tn.build(tunnelRequest{
		ID: "1", Kind: "req", Method: "GET",
		Path: "/v1/sessions?limit=10&state=blocked&q=a%20b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.URL.Query().Get("limit"); got != "10" {
		t.Errorf("limit is %q", got)
	}
	if got := r.URL.Query().Get("q"); got != "a b" {
		t.Errorf("an escaped parameter arrived as %q", got)
	}
	if !strings.HasPrefix(r.RequestURI, "/v1/sessions?") {
		t.Errorf("RequestURI is %q", r.RequestURI)
	}
}
