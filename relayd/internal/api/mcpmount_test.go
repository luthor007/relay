package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheGatewayRefusesOffMachineCallers.
//
// The MCP endpoint is the one route on this server that cannot carry the API
// token: the mcp.json entry Relay writes into five runtimes has no header field
// on any of them. So the network boundary is half the authentication, and
// --lan — a decision about the console, where a token still stands between a
// stranger and the machine — must not quietly become a decision to publish the
// whole tool bus.
func TestTheGatewayRefusesOffMachineCallers(t *testing.T) {
	var reached bool
	s := &Server{gateway: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})}
	mux := http.NewServeMux()
	s.mountGateway(mux)

	for _, tc := range []struct {
		name  string
		addr  string
		allow bool
	}{
		{"ipv4 loopback", "127.0.0.1:52344", true},
		{"ipv6 loopback", "[::1]:52344", true},
		{"another loopback address", "127.0.0.53:52344", true},
		{"a machine on the LAN", "192.168.1.40:52344", false},
		{"the open internet", "203.0.113.9:52344", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			r := httptest.NewRequest("POST", GatewayPrefix, strings.NewReader("{}"))
			r.RemoteAddr = tc.addr
			// A forwarded-for header is a caller telling us who to think it is.
			// It must not move the answer either way.
			r.Header.Set("X-Forwarded-For", "127.0.0.1")

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)

			if tc.allow {
				if !reached {
					t.Fatalf("a caller on this machine was refused: %d %s", w.Code, w.Body)
				}
				return
			}
			if reached {
				t.Fatal("an off-machine caller reached the tool bus")
			}
			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", w.Code)
			}
			if !strings.Contains(w.Body.String(), "loopback-only") {
				t.Errorf("the refusal does not say why: %s", w.Body)
			}
		})
	}
}

// TestNoGatewayLeavesThePathUnmounted: a daemon with no bus should not answer
// on a path five runtimes have been told is real, because a 404 and a 403 are
// different problems and the user has to be able to tell them apart.
func TestNoGatewayLeavesThePathUnmounted(t *testing.T) {
	mux := http.NewServeMux()
	(&Server{}).mountGateway(mux)

	r := httptest.NewRequest("POST", GatewayPrefix, strings.NewReader("{}"))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 from an unmounted path", w.Code)
	}
}
