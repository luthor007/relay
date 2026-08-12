package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The cloud console's host
//
// It serves files, and the two things worth asserting are both about what it
// does *not* do: it has no route that touches customer data, and its content
// security policy names exactly the two origins the page reaches. The second is
// the one that breaks in production rather than in review — a policy that is
// slightly too narrow produces a console that loads and then reports the
// customer's machine as unreachable.

func bundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte("<!doctype html><title>Relay</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "console.js"),
		[]byte("export const x = 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func start(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	addrs := make(chan net.Addr, 1)
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, args, func(a net.Addr) { addrs <- a }) }()

	select {
	case addr := <-addrs:
		return "http://" + addr.String()
	case err := <-errc:
		t.Fatalf("console-host exited before serving: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("console-host never came up")
	}
	return ""
}

func TestItServesTheBundleWithTheOriginsThePageActuallyReaches(t *testing.T) {
	base := start(t,
		"-dir", bundle(t), "-listen", "127.0.0.1:0",
		"-relay", "wss://relay.example", "-accounts", "https://p.supabase.co")

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "<!doctype html>") {
		t.Fatalf("GET / = %d: %s", resp.StatusCode, body)
	}

	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{
		"connect-src 'self' wss://relay.example https://p.supabase.co",
		"script-src 'self'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("the policy is missing %q: %s", want, csp)
		}
	}
	// The rest of the policy is unchanged from the self-hosted one. A cloud
	// deployment that quietly relaxed `script-src` would be the console's own
	// supply chain widening for the tier that pays us.
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "script-src 'self' ") {
		t.Errorf("the cloud policy relaxed something other than connect-src: %s", csp)
	}
}

func TestItRefusesToStartWithoutTheOrigins(t *testing.T) {
	// Defaulting would produce a console that loads and then cannot open a
	// socket, and the symptom a customer reports is "my machine is offline".
	// Failing at start puts the error in front of whoever deployed it.
	dir := bundle(t)
	for _, args := range [][]string{
		{"-dir", dir, "-listen", "127.0.0.1:0", "-accounts", "https://p.supabase.co"},
		{"-dir", dir, "-listen", "127.0.0.1:0", "-relay", "wss://relay.example"},
		{"-dir", dir, "-listen", "127.0.0.1:0"},
	} {
		if err := run(context.Background(), args, nil); err == nil {
			t.Errorf("it started with %v", args)
		}
	}
}

func TestItSaysSoWhenTheBundleWasNeverBuilt(t *testing.T) {
	// The failure mode this replaces is a 404 on `/`, which looks like a routing
	// bug rather than a missing build step.
	err := run(context.Background(), []string{
		"-dir", t.TempDir(), "-listen", "127.0.0.1:0",
		"-relay", "wss://r", "-accounts", "https://a",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "build:cloud") {
		t.Fatalf("an unbuilt bundle produced %v", err)
	}
}

func TestItHasNoRouteThatTouchesCustomerData(t *testing.T) {
	// The claim `CONTROL-PLANE.md` §3 rests on: a breach of this process
	// exposes a static bundle. Every API path falls through to the console's SPA
	// fallback — the document, not data — because there is nothing else here.
	base := start(t,
		"-dir", bundle(t), "-listen", "127.0.0.1:0",
		"-relay", "wss://relay.example", "-accounts", "https://p.supabase.co")

	for _, path := range []string{"/v1/sessions", "/v1/credentials", "/v1/health", "/v1/events"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s answered %s, which is not the console document", path, ct)
		}
		if strings.Contains(string(body), "\"sessions\"") {
			t.Errorf("GET %s returned something that looks like data", path)
		}
	}

	// And it will not accept a write at all, on any path.
	resp, err := http.Post(base+"/v1/credentials", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST to the console host answered %d", resp.StatusCode)
	}
}

func TestHealthzSaysNothingAboutAnybody(t *testing.T) {
	base := start(t,
		"-dir", bundle(t), "-listen", "127.0.0.1:0",
		"-relay", "wss://relay.example", "-accounts", "https://p.supabase.co")

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(body)); got != `{"ok":true}` {
		t.Fatalf("/healthz says %q", got)
	}
}
