package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
)

func testTree() fs.FS {
	return fstest.MapFS{
		"index.html":         {Data: []byte("<!doctype html><title>Relay</title>")},
		"assets/console.js":  {Data: []byte("export const x = 1;")},
		"assets/console.css": {Data: []byte(".shell{}")},
		".gitignore":         {Data: []byte("*\n")},
	}
}

func newTest(t *testing.T, o Options) *Console {
	t.Helper()
	c, err := Handler(o)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return c
}

func get(t *testing.T, c *Console, path string, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	return rec.Result()
}

func TestServesIndexAndAssets(t *testing.T) {
	c := newTest(t, Options{FS: testTree()})
	if !c.Built() {
		t.Fatal("Built() is false with an index.html present")
	}

	res := get(t, c, "/", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / content-type = %q", ct)
	}

	res = get(t, c, "/assets/console.js", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /assets/console.js = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("js content-type = %q; a browser refuses a module served as octet-stream", ct)
	}
}

// The assets are served out of a binary with fixed filenames rather than
// content hashes, so caching correctness rests entirely on the ETag. If it ever
// stops being sent, every console in the world starts serving yesterday's
// JavaScript against today's API.
func TestETagRevalidates(t *testing.T) {
	c := newTest(t, Options{FS: testTree()})

	res := get(t, c, "/assets/console.js", nil)
	etag := res.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on an asset")
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache (names are stable, so nothing may be cached immutably)", cc)
	}

	res = get(t, c, "/assets/console.js", map[string]string{"If-None-Match": etag})
	if res.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", res.StatusCode)
	}
}

func TestETagChangesWithContent(t *testing.T) {
	a := newTest(t, Options{FS: testTree()})
	other := fstest.MapFS{
		"index.html":        {Data: []byte("<!doctype html><title>Relay</title>")},
		"assets/console.js": {Data: []byte("export const x = 2;")},
	}
	b := newTest(t, Options{FS: other})

	ea := get(t, a, "/assets/console.js", nil).Header.Get("ETag")
	eb := get(t, b, "/assets/console.js", nil).Header.Get("ETag")
	if ea == eb {
		t.Fatal("two different bundles produced the same ETag")
	}
}

// DASHBOARD.md §2 says routing is the console's business, but a deep link that
// someone bookmarked or typed should reach the app rather than a 404 page.
func TestSPAFallback(t *testing.T) {
	c := newTest(t, Options{FS: testTree()})

	res := get(t, c, "/credentials", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /credentials = %d, want the app", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("fallback content-type = %q", ct)
	}
}

// A missing *file* must 404. Answering /assets/missing.js with index.html would
// hand the browser HTML where it expects a module, and the console would fail
// with a syntax error that names the wrong file.
func TestMissingAssetIs404(t *testing.T) {
	c := newTest(t, Options{FS: testTree()})
	res := get(t, c, "/assets/missing.js", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /assets/missing.js = %d, want 404", res.StatusCode)
	}
}

func TestDotfilesAreNotServed(t *testing.T) {
	c := newTest(t, Options{FS: testTree()})
	res := get(t, c, "/.gitignore", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /.gitignore = %d, want 404", res.StatusCode)
	}
	for _, name := range c.Assets() {
		if strings.HasPrefix(name, ".") {
			t.Errorf("dotfile %q was loaded into the served set", name)
		}
	}
}

func TestPathTraversal(t *testing.T) {
	c := newTest(t, Options{FS: testTree()})
	for _, p := range []string{"/../go.mod", "/assets/../../secret.txt"} {
		res := get(t, c, p, nil)
		if res.StatusCode == http.StatusOK && res.Header.Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("GET %s escaped the asset tree (%d)", p, res.StatusCode)
		}
	}
}

// The console holds a token that can write to the credential vault
// (DASHBOARD.md §4), so the policy that stops an injected script from reaching
// anywhere else is part of the product, not a hardening pass.
func TestSecurityHeaders(t *testing.T) {
	c := newTest(t, Options{FS: testTree()})
	res := get(t, c, "/", nil)

	csp := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'none'",
		"script-src 'self'",
		"connect-src 'self'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP is missing %q; got %q", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP allows inline or eval: %q", csp)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
	if res.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Error("missing Referrer-Policy: no-referrer")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	c := newTest(t, Options{FS: testTree()})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / = %d, want 405", rec.Code)
	}
}

// A binary built without the console must say so, in words, at the address the
// console would have been. Silence here is the failure mode the placeholder
// exists to prevent.
func TestNotBuiltPage(t *testing.T) {
	c := newTest(t, Options{FS: fstest.MapFS{".gitignore": {Data: []byte("*\n")}}})
	if c.Built() {
		t.Fatal("Built() is true with no index.html")
	}
	res := get(t, c, "/", nil)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET / = %d, want 503", res.StatusCode)
	}
	body := make([]byte, len(NotBuiltPage))
	n, _ := res.Body.Read(body)
	page := string(body[:n])
	for _, want := range []string{"npm run build", "RELAY_CONSOLE_DEV"} {
		if !strings.Contains(page, want) {
			t.Errorf("the not-built page does not mention %q", want)
		}
	}
}

// The embedded tree ships in every relayd binary, so it has to compile against
// whatever is on disk. On a clean clone that is the committed .gitignore and
// nothing else, which must still produce a working handler.
func TestEmbeddedTreeLoads(t *testing.T) {
	c := newTest(t, Options{})
	for _, name := range c.Assets() {
		if strings.HasPrefix(name, ".") {
			t.Errorf("embedded dotfile %q is served", name)
		}
	}
	res := get(t, c, "/", nil)
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET / on the embedded tree = %d", res.StatusCode)
	}
}

// ------------------------------------------------------------------- dev --

func TestDevProxyForwards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("vite: " + r.URL.Path))
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	// httptest binds 127.0.0.1, which is what the loopback check requires.
	c := newTest(t, Options{Dev: "http://" + u.Host})
	if c.Dev() == "" {
		t.Fatal("Dev() is empty on a dev-proxy handler")
	}

	req := httptest.NewRequest(http.MethodGet, "/src/main.ts", nil)
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxied GET = %d", rec.Code)
	}
	if got := rec.Body.String(); got != "vite: /src/main.ts" {
		t.Fatalf("proxied body = %q", got)
	}
	if rec.Header().Get("X-Relay-Console") != "dev" {
		t.Error("a dev-proxied response should be identifiable as one")
	}
}

// A daemon that proxies to any host on request is an SSRF hole. The dev flag is
// for a Vite server on the same machine and nothing else.
func TestDevProxyRefusesNonLoopback(t *testing.T) {
	for _, target := range []string{
		"http://example.com:5173",
		"http://10.0.0.5:5173",
		"http://0.0.0.0:5173",
	} {
		if _, err := Handler(Options{Dev: target}); err == nil {
			t.Errorf("Handler accepted a dev proxy to %s", target)
		}
	}
	for _, target := range []string{"http://127.0.0.1:5173", "http://localhost:5173", "http://[::1]:5173"} {
		if _, err := Handler(Options{Dev: target}); err != nil {
			t.Errorf("Handler refused a loopback dev proxy to %s: %v", target, err)
		}
	}
}

func TestDevProxyBadGatewayExplains(t *testing.T) {
	// Port 1 on loopback: nothing is listening, so the error handler runs.
	c := newTest(t, Options{Dev: "http://127.0.0.1:1"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("dead dev server = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "npm run dev") {
		t.Errorf("the 502 does not say how to fix it: %q", rec.Body.String())
	}
}

func TestMountLosesToTheAPIRoutes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	if _, err := Mount(mux, Options{FS: testTree()}); err != nil {
		t.Fatal(err)
	}

	// The console is registered at "/" and must not shadow the API. Go's
	// ServeMux prefers the more specific pattern regardless of order, and this
	// is the assertion that keeps that true if either side is ever re-registered.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("the console swallowed /v1/health: %q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Relay") {
		t.Fatalf("GET / did not reach the console: %d %q", rec.Code, rec.Body.String())
	}
}

func TestStartupLineSaysWhichModeItIsIn(t *testing.T) {
	built := newTest(t, Options{FS: testTree()})
	if got := built.StartupLine("127.0.0.1:8787"); !strings.Contains(got, "127.0.0.1:8787") {
		t.Errorf("built line = %q", got)
	}
	missing := newTest(t, Options{FS: fstest.MapFS{".gitignore": {Data: []byte("*")}}})
	if got := missing.StartupLine("127.0.0.1:8787"); !strings.Contains(got, "npm run build") {
		t.Errorf("not-built line = %q", got)
	}
	dev := newTest(t, Options{Dev: "http://127.0.0.1:5173"})
	if got := dev.StartupLine("127.0.0.1:8787"); !strings.Contains(got, "5173") {
		t.Errorf("dev line = %q", got)
	}
}
