package apps

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Egress is default-deny — APP-PLATFORM.md §3 and §5.
//
// §3 states the reason in one sentence and it is the sentence this whole file
// exists for: *an app with `memory.read` and unrestricted network access is an
// exfiltration tool.* So the manifest declares its hosts at install time, the
// user sees them on the sheet, and nothing else is reachable.
//
// There are two enforcement points and one decision, which is the important
// shape here — **one** [Guard], consulted by both, so there is exactly one
// implementation of "may this app talk to this host":
//
//   - [Fetcher] runs the request inside relayd on the app's behalf. This is the
//     path that is actually used, because the strongest sandbox this package can
//     build puts the app in an empty network namespace where it has no route to
//     anything at all. `ctx.fetch` is then not "restricted" — it is the only
//     wire out of the box, and it goes through [Guard] before it goes anywhere.
//   - [Proxy] is an HTTP forward proxy for the degraded case: a platform where
//     network isolation could not be enforced, where the app process can open a
//     socket relayd does not see. The proxy still refuses a host the manifest
//     did not declare, but it is a lock on a door in a wall with a hole in it,
//     and [Runtime] refuses to run an app holding a scope that reads the user's
//     life on a sandbox in that state. See [ErrCannotContain].
//
// Redirects are re-checked at every hop. An allowlist that is enforced on the
// first request and not on the 302 it returns is not an allowlist; it is a
// suggestion the destination host gets to overrule.

// Decision is one allowlist answer.
type Decision struct {
	Allowed bool
	Host    string
	// Reason is why, in words a user would recognise, whichever way it went.
	Reason string
	// Matched is the allowlist entry that permitted it.
	Matched string
}

// Guard answers "may this app reach this host?" from the manifest's allowlist.
//
// The zero Guard allows nothing, which is the safe default and also the correct
// one: an app that was not granted `net.fetch` has an empty allowlist, and empty
// must never mean "unrestricted".
type Guard struct {
	hosts []string
}

// NewGuard builds a guard over an allowlist. Entries are hostnames or a single
// leading-label wildcard (`*.example.com`), already validated by
// [ParseManifest].
func NewGuard(allowed []string) *Guard {
	g := &Guard{}
	for _, h := range allowed {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			g.hosts = append(g.hosts, h)
		}
	}
	return g
}

// Hosts is the allowlist, for the console.
func (g *Guard) Hosts() []string {
	if g == nil {
		return nil
	}
	return append([]string(nil), g.hosts...)
}

// Check decides one host.
func (g *Guard) Check(host string) Decision {
	h := strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	h = strings.Trim(h, "[]")
	h = strings.TrimSuffix(h, ".")
	d := Decision{Host: h}
	if h == "" {
		d.Reason = "no host"
		return d
	}
	if g == nil || len(g.hosts) == 0 {
		d.Reason = "this app declared no hosts, so it can reach nothing"
		return d
	}
	for _, pat := range g.hosts {
		if match(pat, h) {
			return Decision{Allowed: true, Host: h, Matched: pat,
				Reason: fmt.Sprintf("%s is on this app's allowlist", pat)}
		}
	}
	d.Reason = fmt.Sprintf("%s is not on this app's allowlist (%s)", h, strings.Join(g.hosts, ", "))
	return d
}

func match(pattern, host string) bool {
	if pattern == host {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		// A wildcard covers subdomains and not the bare domain: `*.example.com`
		// is what the author wrote, and reading it as "and example.com too"
		// grants a host the sheet did not show.
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	return false
}

// Attempt is one recorded egress attempt.
type Attempt struct {
	At         time.Time `json:"at"`
	AppID      string    `json:"app"`
	Invocation string    `json:"invocation,omitempty"`
	Method     string    `json:"method"`
	// URL is scheme://host/path with the query dropped. A query string carries
	// the payload, and a log of exfiltration attempts that includes the
	// exfiltrated data is a second copy of the problem.
	URL     string `json:"url"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
	Status  int    `json:"status,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
}

// EgressLog records attempts, allowed and denied alike. Denied ones are the
// interesting half: an app repeatedly trying a host it was not granted is the
// signal the review process wants.
type EgressLog interface {
	Attempted(ctx context.Context, a Attempt) error
}

// MemoryEgressLog keeps attempts in memory.
type MemoryEgressLog struct {
	mu   sync.Mutex
	list []Attempt
}

func (l *MemoryEgressLog) Attempted(_ context.Context, a Attempt) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, a)
	return nil
}

// All returns everything recorded.
func (l *MemoryEgressLog) All() []Attempt {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Attempt(nil), l.list...)
}

// FetchOptions configures a [Fetcher].
type FetchOptions struct {
	Guard  *Guard
	Log    EgressLog
	Client *http.Client
	AppID  string
	// Invocation ties attempts to one run.
	Invocation string
	// MaxBody caps a response. Defaults to DefaultMaxBody.
	MaxBody int64
	// MaxRedirects caps the hop count. Defaults to DefaultMaxRedirects.
	MaxRedirects int
	Timeout      time.Duration
	Now          func() time.Time
}

// Defaults for [Fetcher].
const (
	DefaultMaxBody      = 8 << 20 // 8 MiB
	DefaultMaxRedirects = 5
	DefaultFetchTimeout = 20 * time.Second
)

// ErrDenied is a request to a host the manifest did not declare.
var ErrDenied = errors.New("apps: egress denied")

// DeniedError carries the reason a request was refused, in the user's words.
type DeniedError struct {
	Host   string
	Reason string
}

func (e *DeniedError) Error() string { return "apps: egress to " + e.Host + " denied: " + e.Reason }
func (e *DeniedError) Unwrap() error { return ErrDenied }

// Fetcher is the host side of `ctx.fetch`.
type Fetcher struct {
	guard     *Guard
	log       EgressLog
	client    *http.Client
	appID     string
	inv       string
	maxBody   int64
	maxHops   int
	now       func() time.Time
	hopByHop  map[string]bool
	userAgent string
}

// NewFetcher builds the fetch capability.
func NewFetcher(o FetchOptions) (*Fetcher, error) {
	if o.Guard == nil {
		return nil, errors.New("apps: fetch needs a guard; an empty allowlist is not the same as no allowlist")
	}
	if o.Log == nil {
		return nil, errors.New("apps: fetch needs an egress log")
	}
	if o.MaxBody <= 0 {
		o.MaxBody = DefaultMaxBody
	}
	if o.MaxRedirects <= 0 {
		o.MaxRedirects = DefaultMaxRedirects
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultFetchTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: o.Timeout}
	}
	// Redirects are followed by this package, one hop at a time, so each new
	// host can be checked. Handing that to http.Client would follow a 302 to
	// anywhere.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Fetcher{
		guard: o.Guard, log: o.Log, client: &c, appID: o.AppID, inv: o.Invocation,
		maxBody: o.MaxBody, maxHops: o.MaxRedirects, now: o.Now,
		hopByHop: map[string]bool{
			"connection": true, "keep-alive": true, "proxy-authenticate": true,
			"proxy-authorization": true, "te": true, "trailer": true,
			"transfer-encoding": true, "upgrade": true, "host": true,
		},
		userAgent: "relay-app/1 (" + o.AppID + ")",
	}, nil
}

// FetchRequest is one `ctx.fetch` call.
type FetchRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// FetchResponse is what the app gets back. It is a value rather than a stream:
// the SDK types it as a `Response`, and the runner builds one from this.
type FetchResponse struct {
	Status     int                 `json:"status"`
	StatusText string              `json:"statusText"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
	URL        string              `json:"url"`
	// Truncated says the body hit [FetchOptions.MaxBody]. An app that is handed
	// a silently truncated body will parse it and get something wrong; saying so
	// is the difference between a bug in the app and a bug in us.
	Truncated bool `json:"truncated,omitempty"`
}

// Do performs one request, checking the allowlist at every hop.
func (f *Fetcher) Do(ctx context.Context, r FetchRequest) (FetchResponse, error) {
	method := strings.ToUpper(strings.TrimSpace(r.Method))
	if method == "" {
		method = http.MethodGet
	}
	current := r.URL
	body := r.Body

	for hop := 0; hop <= f.maxHops; hop++ {
		u, err := url.Parse(current)
		if err != nil {
			return FetchResponse{}, fmt.Errorf("apps: fetch: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			d := Decision{Host: u.Host, Reason: "only http and https go through the egress proxy"}
			f.attempted(ctx, method, u, d, 0, 0)
			return FetchResponse{}, &DeniedError{Host: u.Host, Reason: d.Reason}
		}
		dec := f.guard.Check(u.Host)
		if !dec.Allowed {
			f.attempted(ctx, method, u, dec, 0, 0)
			return FetchResponse{}, &DeniedError{Host: dec.Host, Reason: dec.Reason}
		}

		req, err := http.NewRequestWithContext(ctx, method, current, strings.NewReader(body))
		if err != nil {
			return FetchResponse{}, fmt.Errorf("apps: fetch: %w", err)
		}
		for k, v := range r.Headers {
			if f.hopByHop[strings.ToLower(k)] {
				continue
			}
			req.Header.Set(k, v)
		}
		req.Header.Set("User-Agent", f.userAgent)

		resp, err := f.client.Do(req)
		if err != nil {
			f.attempted(ctx, method, u, dec, 0, 0)
			return FetchResponse{}, fmt.Errorf("apps: fetch %s: %w", dec.Host, err)
		}

		if loc := resp.Header.Get("Location"); isRedirect(resp.StatusCode) && loc != "" {
			resp.Body.Close()
			next, err := u.Parse(loc)
			if err != nil {
				return FetchResponse{}, fmt.Errorf("apps: fetch: bad redirect: %w", err)
			}
			f.attempted(ctx, method, u, dec, resp.StatusCode, 0)
			current = next.String()
			if resp.StatusCode == http.StatusSeeOther || (resp.StatusCode == http.StatusFound && method == http.MethodPost) {
				method, body = http.MethodGet, ""
			}
			continue
		}

		limited := io.LimitReader(resp.Body, f.maxBody+1)
		raw, err := io.ReadAll(limited)
		resp.Body.Close()
		if err != nil {
			return FetchResponse{}, fmt.Errorf("apps: fetch %s: %w", dec.Host, err)
		}
		truncated := int64(len(raw)) > f.maxBody
		if truncated {
			raw = raw[:f.maxBody]
		}
		f.attempted(ctx, method, u, dec, resp.StatusCode, int64(len(raw)))
		return FetchResponse{
			Status:     resp.StatusCode,
			StatusText: http.StatusText(resp.StatusCode),
			Headers:    resp.Header,
			Body:       string(raw),
			URL:        current,
			Truncated:  truncated,
		}, nil
	}
	return FetchResponse{}, fmt.Errorf("apps: fetch: more than %d redirects", f.maxHops)
}

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

func (f *Fetcher) attempted(ctx context.Context, method string, u *url.URL, d Decision, status int, n int64) {
	safe := *u
	safe.RawQuery = ""
	safe.Fragment = ""
	safe.User = nil
	_ = f.log.Attempted(ctx, Attempt{
		At: f.now(), AppID: f.appID, Invocation: f.inv, Method: method,
		URL: safe.String(), Allowed: d.Allowed, Reason: d.Reason, Status: status, Bytes: n,
	})
}

// ------------------------------------------------------------------- proxy --

// Proxy is an HTTP forward proxy that enforces one app's allowlist.
//
// It exists for the degraded sandbox — a platform where the app process has a
// network stack relayd could not take away. It is not the primary path, and it
// is not as strong as the primary path: it stops an app that uses the proxy, and
// an app that has a socket has not necessarily used the proxy. [Enforcement] is
// where that distinction is written down; this type only enforces the part it
// can.
type Proxy struct {
	guard *Guard
	log   EgressLog
	appID string
	inv   string
	token string
	dial  func(ctx context.Context, network, addr string) (net.Conn, error)
	now   func() time.Time
}

// ProxyOptions configures a [Proxy].
type ProxyOptions struct {
	Guard *Guard
	Log   EgressLog
	AppID string
	// Invocation ties attempts to one run.
	Invocation string
	// Token is the credential the app process presents. Minted per invocation,
	// so a token that leaks out of the sandbox is useless the moment the
	// invocation ends.
	Token string
	Dial  func(ctx context.Context, network, addr string) (net.Conn, error)
	Now   func() time.Time
}

// NewProxy builds the egress proxy.
func NewProxy(o ProxyOptions) (*Proxy, error) {
	if o.Guard == nil {
		return nil, errors.New("apps: proxy needs a guard")
	}
	if o.Log == nil {
		return nil, errors.New("apps: proxy needs an egress log")
	}
	if o.Token == "" {
		return nil, errors.New("apps: proxy needs a per-invocation token")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		o.Dial = d.DialContext
	}
	return &Proxy{guard: o.Guard, log: o.Log, appID: o.AppID, inv: o.Invocation,
		token: o.Token, dial: o.Dial, now: o.Now}, nil
}

// NewToken mints a per-invocation proxy credential.
func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (p *Proxy) authorised(r *http.Request) bool {
	const prefix = "Bearer "
	v := r.Header.Get("Proxy-Authorization")
	if !strings.HasPrefix(v, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(v[len(prefix):]), []byte(p.token)) == 1
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.authorised(r) {
		w.Header().Set("Proxy-Authenticate", `Bearer realm="relay"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	host := r.Host
	scheme := "http"
	if r.Method == http.MethodConnect {
		scheme = "https"
	} else if r.URL != nil && r.URL.Host != "" {
		host = r.URL.Host
		if r.URL.Scheme != "" {
			scheme = r.URL.Scheme
		}
	}
	dec := p.guard.Check(host)
	u := &url.URL{Scheme: scheme, Host: dec.Host, Path: "/"}
	if r.URL != nil {
		u.Path = r.URL.Path
	}
	if !dec.Allowed {
		p.record(r.Context(), r.Method, u, dec, http.StatusForbidden, 0)
		http.Error(w, dec.Reason, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		p.connect(w, r, dec)
		return
	}
	p.forward(w, r, dec)
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request, dec Decision) {
	addr := r.Host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}
	upstream, err := p.dial(r.Context(), "tcp", addr)
	if err != nil {
		p.record(r.Context(), r.Method, &url.URL{Scheme: "https", Host: dec.Host}, dec, http.StatusBadGateway, 0)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "proxy transport cannot tunnel", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, "proxy transport cannot tunnel", http.StatusInternalServerError)
		return
	}
	defer client.Close()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	p.record(r.Context(), r.Method, &url.URL{Scheme: "https", Host: dec.Host}, dec, http.StatusOK, 0)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(upstream, client) }()
	go func() { defer wg.Done(); _, _ = io.Copy(client, upstream) }()
	wg.Wait()
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, dec Decision) {
	outURL := *r.URL
	if outURL.Host == "" {
		outURL.Host = r.Host
		outURL.Scheme = "http"
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for k, vs := range r.Header {
		if strings.EqualFold(k, "Proxy-Authorization") || strings.EqualFold(k, "Proxy-Connection") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	client := &http.Client{
		Timeout:   DefaultFetchTimeout,
		Transport: &http.Transport{DialContext: p.dial},
		// The allowlist has to hold across redirects here too. The proxy sees
		// each hop as a separate request from the client, so refusing to follow
		// them is the correct answer: the client re-issues and gets checked.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		p.record(r.Context(), r.Method, &outURL, dec, http.StatusBadGateway, 0)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, io.LimitReader(resp.Body, DefaultMaxBody))
	p.record(r.Context(), r.Method, &outURL, dec, resp.StatusCode, n)
}

func (p *Proxy) record(ctx context.Context, method string, u *url.URL, d Decision, status int, n int64) {
	safe := *u
	safe.RawQuery = ""
	safe.Fragment = ""
	safe.User = nil
	_ = p.log.Attempted(ctx, Attempt{
		At: p.now(), AppID: p.appID, Invocation: p.inv, Method: method,
		URL: safe.String(), Allowed: d.Allowed, Reason: d.Reason, Status: status, Bytes: n,
	})
}
