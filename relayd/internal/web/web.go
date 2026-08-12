// Package web serves the console out of the relayd binary.
//
// DASHBOARD.md §2: one web app, served from two places. `relayd` embeds the
// built assets with Go's `embed`, so there is no second thing to install and no
// static host to run — which is also the constraint that decided the console's
// stack, since anything needing a Node process at serve time cannot be embedded
// in a static binary at all.
//
// # What is in the binary, and what is not
//
// The embedded tree is `dist/`, written by `npm run build` in `console/`. It is
// build output, so it is not committed; the only file checked in under `dist/`
// is its own `.gitignore`, which exists so that `//go:embed` has something to
// match and `go build ./...` works on a clean clone. A binary built without
// running the console's build therefore compiles, starts, serves the API — and
// serves [NotBuiltPage] at the console's address, which says in words that the
// assets are missing and how to get them.
//
// That was chosen over a build tag on purpose. A tag would make the same
// binary compile with the console silently absent, put two serving paths in the
// tree with only one of them covered by `go vet ./...`, and move the discovery
// of the mistake from the person building to the person using. A page that
// admits what happened fails in the right place.
//
// # Dev mode
//
// [Options.Dev] proxies to a running Vite server instead, so changing a
// stylesheet does not mean rebuilding the binary. It is off unless asked for,
// by flag or by `RELAY_CONSOLE_DEV`, and it is refused for any target that is
// not loopback — a daemon that will proxy to an arbitrary host on request is an
// SSRF hole wearing a developer-convenience hat.
//
// # Auth
//
// This handler is deliberately unauthenticated, and that is not an oversight.
// A browser has to load the HTML before it can hold a token, so gating the app
// shell would leave nowhere to type the token in. Nothing served here is
// secret: it is the same public bundle on every install. Every byte of user
// data arrives later, over `relayd`'s API, behind the bearer token in
// `internal/api`.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

//go:generate go run ./typegen/gen

//go:embed all:dist
var dist embed.FS

// IndexFile is the document every route falls back to. The console routes on
// the fragment (DASHBOARD.md §2 puts the same app behind two very different
// hosts, and a fragment needs no rewrite rule on either), so this fallback is
// belt-and-braces for a deep link someone typed or bookmarked.
const IndexFile = "index.html"

// EnvDev is the environment variable that turns on the Vite proxy. It exists as
// well as a flag because the flag lives in `cmd/relayd`, and a console
// developer should not need to touch that file to reload a stylesheet.
const EnvDev = "RELAY_CONSOLE_DEV"

// Options configures the handler.
type Options struct {
	// Dev is the origin of a running Vite dev server, e.g.
	// http://127.0.0.1:5173. Empty serves the embedded assets.
	Dev string

	// FS overrides the embedded tree.
	//
	// Two callers: the tests, and `cmd/console-host`, which serves the cloud
	// bundle from a directory because that bundle is built with the account
	// backend's address in it and therefore cannot be the one compiled into
	// relayd. Everything else about how it is served — the policy below, the
	// ETags, the fallback — is the same code, which is the whole reason the
	// cloud host is forty lines rather than a second web server.
	FS fs.FS

	// ConnectSrc widens `connect-src` beyond `'self'`.
	//
	// Empty on a self-hosted box and that is the point: everything the console
	// needs there is same-origin, so the policy says so, and any future
	// dependency that breaks it is a decision somebody has to make on purpose.
	//
	// The cloud host is the one deployment where that is genuinely untrue — the
	// console is served from our origin and talks to the relay and to the
	// account service, neither of which is us. Naming those two origins is a
	// much narrower statement than the alternative, which is dropping the
	// directive; a page that may connect anywhere is a page where an injected
	// script can send the vault somewhere.
	ConnectSrc []string

	Log *slog.Logger
}

// OptionsFromEnv reads [EnvDev].
func OptionsFromEnv() Options { return Options{Dev: os.Getenv(EnvDev)} }

// ErrDevTargetNotLocal is a dev proxy pointed somewhere it should not go.
var ErrDevTargetNotLocal = errors.New("web: the console dev proxy only targets loopback")

// Console serves the console.
type Console struct {
	files map[string]*asset
	built bool
	proxy http.Handler
	dev   string
	csp   string
	log   *slog.Logger
}

// policy builds the Content-Security-Policy header.
//
// The console can write to the vault, which DASHBOARD.md §4 calls the
// highest-value target in the system. Everything it needs on a self-hosted box
// is same-origin — no CDN, no font host, no analytics — so the policy says
// exactly that. `extra` is the cloud host naming the two origins it genuinely
// reaches, and nothing else in this string moves for it.
func policy(extra []string) string {
	connect := "'self'"
	for _, origin := range extra {
		if origin = strings.TrimSpace(origin); origin != "" {
			connect += " " + origin
		}
	}
	return "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"font-src 'self'; connect-src " + connect +
		"; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}

type asset struct {
	body []byte
	etag string
	typ  string
}

// Handler builds the console handler.
func Handler(o Options) (*Console, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	c := &Console{log: o.Log, files: map[string]*asset{}, csp: policy(o.ConnectSrc)}

	if o.Dev != "" {
		p, err := devProxy(o.Dev)
		if err != nil {
			return nil, err
		}
		c.proxy, c.dev = p, o.Dev
		return c, nil
	}

	tree := o.FS
	if tree == nil {
		sub, err := fs.Sub(dist, "dist")
		if err != nil {
			return nil, fmt.Errorf("web: %w", err)
		}
		tree = sub
	}

	err := fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || hidden(p) {
			return nil
		}
		b, err := fs.ReadFile(tree, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		c.files[p] = &asset{
			body: b,
			etag: `"` + hex.EncodeToString(sum[:8]) + `"`,
			typ:  contentType(p),
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("web: reading embedded assets: %w", err)
	}
	_, c.built = c.files[IndexFile]
	return c, nil
}

// Mount attaches the console to a mux at the origin root.
//
// It registers "/" only, which in Go's ServeMux is the lowest-priority pattern:
// every "/v1/..." route the API declares still wins, whether it is registered
// before or after this call. That ordering is what lets one listener serve both
// the API and the console without either package knowing about the other —
// internal/web never imports internal/api, and internal/api is free to import
// this.
//
//	web.Mount(mux, web.OptionsFromEnv())
func Mount(mux *http.ServeMux, o Options) (*Console, error) {
	c, err := Handler(o)
	if err != nil {
		return nil, err
	}
	mux.Handle("/", c)
	return c, nil
}

// StartupLine is the one sentence relayd should print about the console, so a
// binary built without it says so at start rather than at first use.
func (c *Console) StartupLine(listen string) string {
	switch {
	case c.dev != "":
		return fmt.Sprintf("console: proxying to the Vite dev server at %s", c.dev)
	case !c.built:
		return "console: not built into this binary — run `npm run build` in console/, " +
			"or set " + EnvDev + " to a running Vite server"
	default:
		return fmt.Sprintf("console: http://%s/ (%d files embedded)", listen, len(c.files))
	}
}

// Built reports whether real console assets are in this binary. A caller that
// wants to log one line at startup — "console: not built into this binary" —
// asks this rather than parsing a 503 later.
func (c *Console) Built() bool { return c.built || c.proxy != nil }

// Dev reports the dev-proxy target, empty when serving embedded assets.
func (c *Console) Dev() string { return c.dev }

// Assets is the embedded file list, for tests and for a startup log line.
func (c *Console) Assets() []string {
	out := make([]string, 0, len(c.files))
	for p := range c.files {
		out = append(out, p)
	}
	return out
}

func (c *Console) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "the console is read-only over HTTP; the API is at /v1", http.StatusMethodNotAllowed)
		return
	}

	if c.proxy != nil {
		// No CSP in dev: Vite's client injects inline script and styles, and a
		// policy that has to be relaxed enough to allow them would not be the
		// policy that ships.
		c.proxy.ServeHTTP(w, r)
		return
	}

	h := w.Header()
	// The console can write to the vault, which DASHBOARD.md §4 calls the
	// highest-value target in the system. Everything it needs is same-origin —
	// no CDN, no font host, no analytics — so the policy says exactly that and
	// any future dependency that breaks it is a decision someone has to make on
	// purpose.
	h.Set("Content-Security-Policy", c.csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")

	if !c.built {
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(NotBuiltPage))
		}
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = IndexFile
	}

	a, ok := c.files[name]
	if !ok {
		// A path with no extension is a route someone typed. Anything else is a
		// missing file, and answering it with HTML would only produce a confusing
		// parse error in the browser.
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		a = c.files[IndexFile]
		name = IndexFile
	}

	h.Set("Content-Type", a.typ)
	h.Set("ETag", a.etag)
	// Asset names are stable rather than content-hashed (see vite.config.ts), so
	// caching is revalidate-always with a strong ETag: a reload is one 304, and
	// a new build is never served from a stale cache.
	h.Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(a.body))
}

func hidden(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

func contentType(p string) string {
	switch path.Ext(p) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".woff2":
		return "font/woff2"
	case ".ico":
		return "image/vnd.microsoft.icon"
	case ".map":
		return "application/json"
	}
	return "application/octet-stream"
}

// devProxy builds the reverse proxy to Vite, refusing anything non-loopback.
func devProxy(target string) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("web: %s=%q: %w", EnvDev, target, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("web: %s=%q: want an http(s) origin", EnvDev, target)
	}
	host := u.Hostname()
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("%w: %s", ErrDevTargetNotLocal, target)
		}
	}

	p := httputil.NewSingleHostReverseProxy(u)
	// Vite serves its own HTML; letting it also set caching headers keeps the
	// dev path honest about being a different path.
	p.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Set("X-Relay-Console", "dev")
		return nil
	}
	p.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "the console dev server at %s is not answering (%v).\n\n"+
			"Start it with:  cd console && npm run dev\n"+
			"Or unset %s to serve the assets embedded in this binary.\n", target, err, EnvDev)
	}
	return p, nil
}

// NotBuiltPage is what a binary built without the console says when someone
// opens it. It is a whole page rather than a status code because the person
// reading it is in a browser, and the fix is two commands they should not have
// to go and find.
const NotBuiltPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Relay console — not built</title>
</head><body>
<h1>The console is not in this binary</h1>
<p>relayd is running and its API is serving normally. What is missing is the
console's built assets, which are produced by a separate build step and embedded
at compile time.</p>
<h2>Build them</h2>
<pre>cd console
npm install
npm run build     # writes relayd/internal/web/dist
cd ../relayd
go build ./cmd/relayd</pre>
<h2>Or run the dev server instead</h2>
<pre>cd console &amp;&amp; npm run dev            # Vite on 127.0.0.1:5173
RELAY_CONSOLE_DEV=http://127.0.0.1:5173 relayd</pre>
<p>Released binaries always carry the console. This page means a local build
skipped the console build, not that anything is broken.</p>
</body></html>
`
