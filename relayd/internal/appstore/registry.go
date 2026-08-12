package appstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The registry, and why it is an interface.
//
// APP-PLATFORM.md §6: the registry is `github.com/luthor007/relay-apps`, a
// directory of manifests pointing at source repositories — no central build
// service, no proprietary publishing pipeline, and forking it is supported.
//
// So resolution is [Source], and every implementation is a dumb file fetch. A
// fork is a spec string: `github:you/your-apps@main`, an HTTPS base URL, or a
// directory on disk. There is no API only we could run, nothing signs anything
// with a key only we hold, and a registry that goes away leaves the boxes that
// already resolved from it working.
//
// The layout is one JSON file per app:
//
//	index.json                          {"apps": ["dev.alexis.standup-notes", ...]}
//	apps/dev.alexis.standup-notes.json  an Entry
//
// One file per app is what makes review a pull request: adding an app is one
// added file, and the diff *is* the permission sheet.

// DefaultRegistry is the registry a box resolves from when nobody has said
// otherwise.
const DefaultRegistry = "github:luthor007/relay-apps@main"

// EnvRegistry overrides the registry for one process. The precedence is
// [ResolveSpec]: flag, environment, file, default.
const EnvRegistry = "RELAY_APP_REGISTRY"

// RegistryFile is read from the config directory. It holds a spec string and
// nothing else — one line, so a fork is `echo` and not an edit to a TOML file
// another command rewrites.
const RegistryFile = "app-registry"

// ErrNotListed means the registry answered and does not carry this app.
var ErrNotListed = errors.New("appstore: not listed in this registry")

// EntrySource points at where the app's code actually lives. The registry
// carries manifests, not tarballs: there is no build service, so the box
// fetches from the author's repository itself.
type EntrySource struct {
	// Git is an https:// clone URL. Not ssh (the box has no key of the user's
	// to offer) and not http (the code runs on their machine).
	Git string `json:"git"`
	// Rev is a tag or a commit. A branch name is accepted and is worse: it
	// means "whatever that branch says the next time this is fetched".
	Rev string `json:"rev"`
	// Subdir is where relay.json lives inside the repo, for monorepos.
	Subdir string `json:"subdir,omitempty"`
}

// Entry is one file in the registry.
type Entry struct {
	Manifest Manifest    `json:"manifest"`
	Source   EntrySource `json:"source"`
	// Review is the pull request that listed the app. Launch review posture is
	// a human reading a diff (§5); recording which diff is the difference
	// between a claim and a link.
	Review string `json:"review,omitempty"`
	// ListedAt is when the entry was merged, as the registry recorded it.
	ListedAt time.Time `json:"listedAt,omitempty"`
}

// Index is the optional index.json. A registry without one still resolves by
// id; it just cannot be listed.
type Index struct {
	Apps []string `json:"apps"`
}

// Source is where manifests come from. Two methods, both dumb on purpose.
type Source interface {
	// Describe is the spec string, shown on the permission sheet and recorded
	// with the install. The user is agreeing to an app *from somewhere*, and
	// somewhere is part of the decision.
	Describe() string
	// Fetch returns the bytes at a slash-separated path relative to the
	// registry root, or a wrapped [fs.ErrNotExist] when there is no such file.
	Fetch(ctx context.Context, name string) ([]byte, error)
}

// Registry reads apps out of a Source.
type Registry struct{ src Source }

// New wraps a Source.
func New(src Source) *Registry { return &Registry{src: src} }

// Describe is the registry's spec string.
func (r *Registry) Describe() string { return r.src.Describe() }

// Resolve fetches and validates one app's entry.
//
// Everything is checked here rather than at install time, because the sheet the
// user reads is built from this and a half-valid entry makes a half-true sheet.
func (r *Registry) Resolve(ctx context.Context, id string) (Entry, error) {
	if !idPattern.MatchString(id) {
		return Entry{}, fmt.Errorf("appstore: %q is not an app id — they look like dev.you.app-name", id)
	}
	b, err := r.src.Fetch(ctx, "apps/"+id+".json")
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, fmt.Errorf("appstore: %s is %w (%s)", id, ErrNotListed, r.src.Describe())
	}
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return Entry{}, fmt.Errorf("appstore: %s: registry entry is not valid JSON: %w", id, err)
	}
	if err := e.Manifest.Validate(); err != nil {
		return Entry{}, err
	}
	if e.Manifest.TimeoutMS == 0 {
		e.Manifest.TimeoutMS = DefaultTimeoutMS
	}
	// The file name and the manifest id must agree. When they do not, one of
	// them is what the reviewer read and the other is what would be installed.
	if e.Manifest.ID != id {
		return Entry{}, fmt.Errorf("appstore: registry file apps/%s.json carries a manifest for %q; "+
			"the reviewed file name and the installed app must be the same app", id, e.Manifest.ID)
	}
	if err := validateEntrySource(e.Source); err != nil {
		return Entry{}, fmt.Errorf("appstore: %s: %w", id, err)
	}
	return e, nil
}

// List reads index.json. A registry that has none is not broken — it just
// cannot enumerate, and says so rather than reporting zero apps.
func (r *Registry) List(ctx context.Context) ([]string, error) {
	b, err := r.src.Fetch(ctx, "index.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("appstore: %s has no index.json, so it cannot be listed; "+
			"apps in it still resolve by id", r.src.Describe())
	}
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("appstore: %s: index.json is not valid JSON: %w", r.src.Describe(), err)
	}
	out := append([]string(nil), idx.Apps...)
	sort.Strings(out)
	return out, nil
}

func validateEntrySource(s EntrySource) error {
	if s.Git == "" {
		return errors.New("registry entry has no source.git — a manifest with no code behind it")
	}
	// Checked as a prefix before parsing, because `git@github.com:you/app.git`
	// is not a URL at all and "not a URL" is a worse answer than the true one.
	if !strings.HasPrefix(s.Git, "https://") {
		return fmt.Errorf("source.git must be https, got %q — this code will run on the user's machine", s.Git)
	}
	u, err := url.Parse(s.Git)
	if err != nil {
		return fmt.Errorf("source.git %q is not a URL: %w", s.Git, err)
	}
	if u.Host == "" {
		return fmt.Errorf("source.git %q has no host", s.Git)
	}
	if s.Rev == "" {
		return errors.New("registry entry has no source.rev — nothing pins which commit was reviewed")
	}
	if strings.HasPrefix(s.Subdir, "/") || strings.Contains(s.Subdir, "..") {
		return fmt.Errorf("source.subdir %q must be a relative path inside the repository", s.Subdir)
	}
	return nil
}

// ---------------------------------------------------------------- sources

// DirSource reads a registry from a directory — a clone, a fork someone is
// editing, or a test fixture. It is the implementation everything else is
// tested against, because the network is not a thing a test may need.
type DirSource struct {
	FS fs.FS
	// Name is what [Source.Describe] returns.
	Name string
}

// NewDirSource opens a registry checked out at a path.
func NewDirSource(dir string) (*DirSource, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("appstore: registry %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("appstore: registry %s is not a directory", dir)
	}
	return &DirSource{FS: os.DirFS(abs), Name: "dir:" + abs}, nil
}

func (d *DirSource) Describe() string {
	if d.Name != "" {
		return d.Name
	}
	return "dir:."
}

func (d *DirSource) Fetch(_ context.Context, name string) ([]byte, error) {
	if err := checkPath(name); err != nil {
		return nil, err
	}
	b, err := fs.ReadFile(d.FS, name)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// HTTPSource reads a registry over HTTPS from any base URL. GitHub is one case
// of this and not a special one — which is the point: a fork can be a directory
// on a static host with no GitHub account anywhere in the picture.
type HTTPSource struct {
	// Base ends in a slash.
	Base string
	// Name is the spec string this was built from, so the sheet shows what the
	// user configured rather than the URL it expanded to.
	Name   string
	Client *http.Client
	// MaxBytes caps a response. A registry is small text; anything large is
	// either a mistake or an attempt to make the box chew on something.
	MaxBytes int64
}

// DefaultMaxEntryBytes caps one registry file at 256 KiB.
const DefaultMaxEntryBytes = 256 << 10

func (h *HTTPSource) Describe() string {
	if h.Name != "" {
		return h.Name
	}
	return h.Base
}

func (h *HTTPSource) Fetch(ctx context.Context, name string) ([]byte, error) {
	if err := checkPath(name); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.Base+name, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	c := h.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("appstore: %s: %w", h.Describe(), err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("appstore: %s%s: %w", h.Base, name, fs.ErrNotExist)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("appstore: %s%s: HTTP %d", h.Base, name, resp.StatusCode)
	}
	max := h.MaxBytes
	if max <= 0 {
		max = DefaultMaxEntryBytes
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("appstore: %s%s is larger than %d bytes; a registry entry is a manifest",
			h.Base, name, max)
	}
	return b, nil
}

// checkPath refuses anything that is not a plain relative path. Registry names
// are built by this package, but a Source is an interface anyone can hand a
// path to, and `..` on a DirSource reads the user's disk.
func checkPath(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || !fs.ValidPath(path.Clean(name)) ||
		strings.Contains(name, "..") {
		return fmt.Errorf("appstore: %q is not a registry path", name)
	}
	return nil
}

// ---------------------------------------------------------------- specs

// ParseSpec turns a registry spec into a Source.
//
//	github:luthor007/relay-apps@main    a GitHub repository at a ref
//	https://apps.example.com/       any static host serving the same layout
//	/srv/relay-apps                 a directory: a clone, or a fork being edited
//	file:///srv/relay-apps          the same, spelled as a URL
//	""                              DefaultRegistry
//
// Forking is a spec, which is a config change. Nothing here needs a patch, a
// key, or an account.
func ParseSpec(spec string) (Source, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = DefaultRegistry
	}
	switch {
	case strings.HasPrefix(spec, "github:"):
		return gitHubSource(spec)
	case strings.HasPrefix(spec, "https://"):
		return &HTTPSource{Base: ensureSlash(spec), Name: spec}, nil
	case strings.HasPrefix(spec, "http://"):
		// Loopback is a fork someone is serving locally while they edit it.
		// Anything else is a manifest describing what an app may do, fetched
		// over a channel that lets somebody else write it.
		if isLoopbackURL(spec) {
			return &HTTPSource{Base: ensureSlash(spec), Name: spec}, nil
		}
		return nil, fmt.Errorf("appstore: %s is plain HTTP. The permission sheet is built from "+
			"what this fetches, so it is https:// or a path on disk", spec)
	case strings.HasPrefix(spec, "file://"):
		u, err := url.Parse(spec)
		if err != nil {
			return nil, fmt.Errorf("appstore: %q is not a file URL: %w", spec, err)
		}
		src, err := NewDirSource(u.Path)
		if err != nil {
			return nil, err
		}
		src.Name = spec
		return src, nil
	case strings.Contains(spec, "://"):
		return nil, fmt.Errorf("appstore: %q is not a registry this box knows how to read "+
			"(github:owner/repo@ref, an https:// base URL, or a directory)", spec)
	default:
		src, err := NewDirSource(spec)
		if err != nil {
			return nil, err
		}
		src.Name = spec
		return src, nil
	}
}

// gitHubSource expands github:owner/repo@ref to the raw file host.
//
// It is deliberately the same HTTPSource as everything else: GitHub is where
// our registry happens to live, not a capability the design depends on.
func gitHubSource(spec string) (Source, error) {
	rest := strings.TrimPrefix(spec, "github:")
	ref := "main"
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		ref, rest = rest[i+1:], rest[:i]
	}
	owner, repo, ok := strings.Cut(rest, "/")
	if !ok || owner == "" || repo == "" || ref == "" || strings.Contains(rest, "//") {
		return nil, fmt.Errorf("appstore: %q should look like github:owner/repo@ref", spec)
	}
	for _, part := range []string{owner, repo, ref} {
		if strings.ContainsAny(part, " \t?#") {
			return nil, fmt.Errorf("appstore: %q has a character that cannot appear in a GitHub ref", spec)
		}
	}
	return &HTTPSource{
		Base: fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/",
			url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref)),
		Name: spec,
	}, nil
}

func ensureSlash(s string) string {
	if strings.HasSuffix(s, "/") {
		return s
	}
	return s + "/"
}

func isLoopbackURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// ResolveSpec is the precedence, in one place so the CLI and the API cannot
// disagree about which registry a box is using:
//
//  1. an explicit flag        --registry
//  2. the environment         RELAY_APP_REGISTRY
//  3. <config dir>/app-registry, one line
//  4. DefaultRegistry
//
// configDir may be empty, in which case step 3 is skipped.
func ResolveSpec(flag, configDir string) (string, error) {
	if s := strings.TrimSpace(flag); s != "" {
		return s, nil
	}
	if s := strings.TrimSpace(os.Getenv(EnvRegistry)); s != "" {
		return s, nil
	}
	if configDir != "" {
		b, err := os.ReadFile(filepath.Join(configDir, RegistryFile))
		switch {
		case err == nil:
			if s := firstLine(string(b)); s != "" {
				return s, nil
			}
		case errors.Is(err, fs.ErrNotExist):
			// The normal case on a box nobody has forked anything on.
		default:
			return "", fmt.Errorf("appstore: reading %s: %w", RegistryFile, err)
		}
	}
	return DefaultRegistry, nil
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}
