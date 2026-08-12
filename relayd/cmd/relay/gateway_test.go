package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/config"
)

func TestListenURLIsSomethingARuntimeCanDial(t *testing.T) {
	for _, tc := range []struct {
		listen string
		want   string
	}{
		{"127.0.0.1:8787", "http://127.0.0.1:8787"},
		// A wildcard bind is reachable at loopback from this machine, and
		// loopback is the address to write: the gateway refuses off-machine
		// callers, so a config naming the LAN address points five runtimes at
		// something that answers 403.
		{"0.0.0.0:8787", "http://127.0.0.1:8787"},
		{":8787", "http://127.0.0.1:8787"},
		{"[::]:8787", "http://127.0.0.1:8787"},
		// Port 0 means "whatever the OS gives me", which is not something that
		// can be written into a config file in advance.
		{"127.0.0.1:0", ""},
		{"", ""},
		{"not-an-address", ""},
	} {
		if got := listenURL(tc.listen); got != tc.want {
			t.Errorf("listenURL(%q) = %q, want %q", tc.listen, got, tc.want)
		}
	}
}

// TestAdoptionNeedsALiveGateway is the guard the whole MCP write half sits
// behind.
//
// Adoption rewrites five runtimes' mcp.json to point at one server. If that
// server is not there the user does not get a degraded Relay, they get five
// agents with no tools at all — a worse machine than the one they started with,
// and a failure they will blame on whichever agent they open first.
func TestAdoptionNeedsALiveGateway(t *testing.T) {
	var out bytes.Buffer
	cfg := config.Config{Listen: "127.0.0.1:1"} // nothing is listening there

	g := gatewayIfLive(context.Background(), cfg, &out)
	if !g.Zero() {
		t.Fatalf("adopted a gateway that is not running: %+v", g)
	}
	// And says so, because "my tools did not appear" is otherwise silent.
	for _, want := range []string{"not reachable", "start relayd"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the output never says %q:\n%s", want, out.String())
		}
	}
}

// TestAdoptionTakesAServerThatSpeaksMCP. A TCP connect would prove something
// holds the port; it would not prove the thing holding it is our gateway.
func TestAdoptionTakesAServerThatSpeaksMCP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["method"] != "initialize" {
			t.Errorf("the probe sent %v, not an initialize", req["method"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{
			"protocolVersion":"2025-06-18","capabilities":{},
			"serverInfo":{"name":"relay","version":"test"}}}`))
	}))
	defer srv.Close()

	if err := probeGateway(context.Background(), srv.URL); err != nil {
		t.Fatalf("a real MCP server was rejected: %v", err)
	}
}

func TestAdoptionRefusesAServerThatIsNotOurs(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
	}{
		{"an unrelated web server", "<html>hello</html>", 200},
		{"an MCP server that refuses", `{"jsonrpc":"2.0","id":1,"error":{"message":"nope"}}`, 200},
		{"a server with no name", `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{}}}`, 200},
		{"something that is not up yet", "", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			if err := probeGateway(context.Background(), srv.URL); err == nil {
				t.Error("adopted a server that is not the Relay gateway")
			}
		})
	}
}
