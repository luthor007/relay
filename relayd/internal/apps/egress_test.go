package apps

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTheGuardIsDefaultDeny(t *testing.T) {
	var zero *Guard
	if d := zero.Check("example.com"); d.Allowed {
		t.Error("a nil guard must allow nothing")
	}
	empty := NewGuard(nil)
	d := empty.Check("example.com")
	if d.Allowed {
		t.Error("an empty allowlist is not an absent allowlist")
	}
	if !strings.Contains(d.Reason, "can reach nothing") {
		t.Errorf("the reason should be readable: %q", d.Reason)
	}
}

func TestGuardMatching(t *testing.T) {
	g := NewGuard([]string{"api.example.com", "*.internal.example.com"})
	cases := []struct {
		host  string
		allow bool
		why   string
	}{
		{"api.example.com", true, "exact"},
		{"API.Example.com", true, "hosts are case-insensitive"},
		{"api.example.com:443", true, "a port is not part of the host"},
		{"api.example.com.", true, "a trailing dot is the same name"},
		{"evil.com", false, "not on the list"},
		{"api.example.com.evil.com", false, "a suffix attack"},
		{"notapi.example.com", false, "not a subdomain of anything listed"},
		{"a.internal.example.com", true, "the wildcard"},
		{"a.b.internal.example.com", true, "the wildcard covers deeper labels"},
		{"internal.example.com", false, "a wildcard does not cover the bare domain the author did not write"},
		{"", false, "no host"},
	}
	for _, tc := range cases {
		if got := g.Check(tc.host); got.Allowed != tc.allow {
			t.Errorf("%q: allowed=%v, want %v (%s) — %s", tc.host, got.Allowed, tc.allow, tc.why, got.Reason)
		}
	}
}

func TestFetchRechecksTheAllowlistOnEveryRedirect(t *testing.T) {
	// The exfiltration path an allowlist checked once would leave open: the
	// allowed host answers 302 to somewhere else, carrying the payload.
	var elsewhere *httptest.Server
	elsewhere = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("this should never be reached"))
	}))
	defer elsewhere.Close()

	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://elsewhere.example/collect", http.StatusFound)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer allowed.Close()

	log := &MemoryEgressLog{}
	f, err := NewFetcher(FetchOptions{
		Guard:  NewGuard([]string{"allowed.example"}),
		Log:    log,
		AppID:  "dev.test.fetch",
		Client: &http.Client{Transport: rewriteTo(allowed.Listener.Addr().String())},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Do(context.Background(), FetchRequest{URL: "http://allowed.example/redirect"})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("a 302 to a host the manifest did not declare must be refused: %v", err)
	}

	attempts := log.All()
	if len(attempts) != 2 {
		t.Fatalf("both hops are attempts: %+v", attempts)
	}
	if !attempts[0].Allowed || attempts[0].Status != http.StatusFound {
		t.Errorf("the first hop was allowed and answered 302: %+v", attempts[0])
	}
	if attempts[1].Allowed || !strings.Contains(attempts[1].URL, "elsewhere.example") {
		t.Errorf("the second hop is the denial, and it names where it was going: %+v", attempts[1])
	}
}

func TestFetchRefusesNonHTTPSchemes(t *testing.T) {
	f, _ := NewFetcher(FetchOptions{Guard: NewGuard([]string{"example.com"}), Log: &MemoryEgressLog{}})
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/x", "data:text/plain,hi"} {
		if _, err := f.Do(context.Background(), FetchRequest{URL: u}); !errors.Is(err, ErrDenied) {
			t.Errorf("%s should be refused, got %v", u, err)
		}
	}
}

func TestFetchTruncatesRatherThanLying(t *testing.T) {
	big := strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()

	f, _ := NewFetcher(FetchOptions{
		Guard: NewGuard([]string{"allowed.example"}), Log: &MemoryEgressLog{}, MaxBody: 100,
		Client: &http.Client{Transport: rewriteTo(srv.Listener.Addr().String())},
	})
	res, err := f.Do(context.Background(), FetchRequest{URL: "http://allowed.example/big"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Body) != 100 {
		t.Errorf("body = %d bytes", len(res.Body))
	}
	if !res.Truncated {
		t.Error("an app handed a silently truncated body will parse it and get something wrong")
	}
}

func TestFetchStripsHopByHopHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	f, _ := NewFetcher(FetchOptions{
		Guard: NewGuard([]string{"allowed.example"}), Log: &MemoryEgressLog{}, AppID: "dev.test.fetch",
		Client: &http.Client{Transport: rewriteTo(srv.Listener.Addr().String())},
	})
	if _, err := f.Do(context.Background(), FetchRequest{
		URL: "http://allowed.example/x",
		Headers: map[string]string{
			"X-Thing": "kept", "Proxy-Authorization": "Bearer stolen", "Host": "someone.else",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got.Get("X-Thing") != "kept" {
		t.Error("an app's own headers should reach the host it was allowed to talk to")
	}
	if got.Get("Proxy-Authorization") != "" {
		t.Error("the app must not be able to set the proxy credential")
	}
	if !strings.Contains(got.Get("User-Agent"), "dev.test.fetch") {
		t.Errorf("the request should say which app made it: %q", got.Get("User-Agent"))
	}
}

func TestTheProxyRefusesWithoutItsTokenAndOffTheAllowlist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("upstream"))
	}))
	defer upstream.Close()

	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	log := &MemoryEgressLog{}
	p, err := NewProxy(ProxyOptions{
		Guard: NewGuard([]string{"allowed.example"}), Log: log, AppID: "dev.test.proxy", Token: token,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	defer front.Close()

	do := func(target, tok string) *http.Response {
		req, err := http.NewRequest("GET", target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tok != "" {
			req.Header.Set("Proxy-Authorization", "Bearer "+tok)
		}
		req.URL.Scheme = "http"
		req.URL.Host = front.Listener.Addr().String()
		req.Host = hostOf(target)
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if resp := do("http://allowed.example/x", ""); resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("no token = %d, want 407", resp.StatusCode)
	}
	if resp := do("http://allowed.example/x", "wrong"); resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("wrong token = %d, want 407", resp.StatusCode)
	}
	if resp := do("http://elsewhere.example/x", token); resp.StatusCode != http.StatusForbidden {
		t.Errorf("off-allowlist = %d, want 403", resp.StatusCode)
	}
	if resp := do("http://allowed.example/x", token); resp.StatusCode != http.StatusOK {
		t.Errorf("allowed = %d, want 200", resp.StatusCode)
	}

	var denied int
	for _, a := range log.All() {
		if !a.Allowed {
			denied++
		}
	}
	if denied != 1 {
		t.Errorf("the proxy should have recorded exactly one allowlist denial, got %d of %d attempts",
			denied, len(log.All()))
	}
}

func hostOf(rawURL string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(rawURL, "http://"), "https://")
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i]
	}
	return s
}

func TestTheEgressLogNeverCarriesTheQueryString(t *testing.T) {
	log := &MemoryEgressLog{}
	f, _ := NewFetcher(FetchOptions{Guard: NewGuard([]string{"allowed.example"}), Log: log})
	// Denied before it goes anywhere, which is the case that matters: the log of
	// an exfiltration attempt must not be a second copy of the exfiltrated data.
	_, _ = f.Do(context.Background(), FetchRequest{
		URL: "https://elsewhere.example/collect?transcript=the+whole+meeting",
	})
	all := log.All()
	if len(all) != 1 {
		t.Fatalf("attempts = %+v", all)
	}
	if strings.Contains(all[0].URL, "transcript") {
		t.Errorf("the log carried the payload: %q", all[0].URL)
	}
	if all[0].URL != "https://elsewhere.example/collect" {
		t.Errorf("url = %q", all[0].URL)
	}
}
