package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/index"
)

// Discovery is MEMORY.md §6's second arrival path: a key already sitting in a
// runtime's own config, enumerable at install with the user watching.
//
// It is second in §6's order of preference and it behaves like it: what it
// finds becomes a proposal, not a credential. The user is at the keyboard
// during the install, which is the best possible moment to ask, and the worst
// possible moment to have taken something without asking.
//
// # Why it does not know the file formats
//
// §4's table names `~/.local/share/opencode/auth.json`, and that is the whole
// of what is documented. No probe in this repository has ever seen its schema —
// the measurement machine had OpenCode installed and never run — so a reader
// that expected `{"provider": {"key": "..."}}` would be a guess wearing a
// parser's clothes, and the failure mode of a wrong guess is silence that looks
// like success.
//
// So the walk is structural instead: every string in the JSON, at any depth,
// run through internal/index's measured tier-1 ruleset. That reuses the
// detector rather than copying it, matches whatever shape the file actually has,
// and inherits §12.2's precision — one false positive in the whole corpus, and
// that one a documentation placeholder. It also means a file whose format
// changes tomorrow keeps working.

// FS is the read side of the filesystem, so discovery runs from fixtures in a
// test on a box with none of the five runtimes installed — which is the only
// box CI will ever have.
type FS interface {
	ReadFile(name string) ([]byte, error)
	Stat(name string) (fs.FileInfo, error)
}

// OSFS reads the real filesystem.
type OSFS struct{}

// ReadFile reads a file.
func (OSFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// Stat stats a file.
func (OSFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

// ConfigFile is one place a runtime keeps provider credentials.
type ConfigFile struct {
	Runtime string
	Path    string
	// Documented marks a path MEMORY.md names outright, as opposed to one we
	// are guessing at from a neighbouring convention. The distinction is
	// carried through to [Discovery] so "we looked in a place we invented and
	// found nothing" is not reported as "you have no keys".
	Documented bool
}

// DiscoverOptions configures a scan.
type DiscoverOptions struct {
	// Home is the user's home directory. Required.
	Home string
	// XDGData is $XDG_DATA_HOME, when set.
	XDGData string
	// XDGConfig is $XDG_CONFIG_HOME, when set.
	XDGConfig string
	// Files replaces the built-in list entirely, for an installer that has
	// already detected where the runtimes actually live.
	Files []ConfigFile
	// Extra adds to the built-in list.
	Extra []ConfigFile
	// FS defaults to the real filesystem.
	FS FS
	// Detector defaults to the measured ruleset.
	Detector *index.Detector
	// Now defaults to time.Now, and stamps provenance for files whose mtime is
	// unreadable.
	Now func() time.Time
}

// Discovery is what a scan found, and — just as importantly — what it could not
// read.
//
// MEMORY.md §7's rule, applied here: "the file is not there" and "we could not
// read the file" lead to opposite decisions and only one of them is
// recoverable, so they are different fields rather than the same empty list.
type Discovery struct {
	// Candidates are the proposals to make, one per detected credential.
	Candidates []Candidate
	// Read are the files that parsed.
	Read []string
	// Missing are the files that are simply not there — the normal case for a
	// runtime that is installed but never used, and not an error.
	Missing []string
	// Unreadable are files that exist and would not open or would not parse,
	// each with the reason. A key could be in any of them.
	Unreadable []string
}

// DefaultConfigFiles is where credentials are looked for when the caller does
// not say.
func DefaultConfigFiles(o DiscoverOptions) []ConfigFile {
	data := o.XDGData
	if data == "" {
		data = filepath.Join(o.Home, ".local", "share")
	}
	config := o.XDGConfig
	if config == "" {
		config = filepath.Join(o.Home, ".config")
	}
	return []ConfigFile{
		// The one MEMORY.md §6 names: "right there".
		{Runtime: "opencode", Path: filepath.Join(data, "opencode", "auth.json"), Documented: true},
		// The same file under the other half of the XDG convention. A guess,
		// and labelled as one.
		{Runtime: "opencode", Path: filepath.Join(config, "opencode", "auth.json")},
		{Runtime: "opencode", Path: filepath.Join(o.Home, ".opencode", "auth.json")},
	}
}

// Discover enumerates the config files and proposes what it finds. It makes no
// network calls and writes nothing: [Proposals.Propose] is a separate,
// deliberate step, and validation is a third.
func Discover(ctx context.Context, o DiscoverOptions) (Discovery, error) {
	var out Discovery
	if o.Home == "" && len(o.Files) == 0 {
		return out, errors.New("vault: discovery needs a home directory or an explicit file list")
	}
	rfs := o.FS
	if rfs == nil {
		rfs = OSFS{}
	}
	det := o.Detector
	if det == nil {
		det = index.MustDetector()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}

	files := o.Files
	if files == nil {
		files = DefaultConfigFiles(o)
	}
	files = append(append([]ConfigFile{}, files...), o.Extra...)

	seen := map[string]bool{}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if f.Path == "" || seen[f.Path] {
			continue
		}
		seen[f.Path] = true

		data, err := rfs.ReadFile(f.Path)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			out.Missing = append(out.Missing, f.Path)
			continue
		}
		if err != nil {
			out.Unreadable = append(out.Unreadable, f.Path+": "+err.Error())
			continue
		}

		at := now()
		if info, serr := rfs.Stat(f.Path); serr == nil {
			at = info.ModTime()
		}

		found, perr := scanConfig(det, f, data, at)
		if perr != nil {
			out.Unreadable = append(out.Unreadable, f.Path+": "+perr.Error())
			continue
		}
		out.Read = append(out.Read, f.Path)
		out.Candidates = append(out.Candidates, found...)
	}
	return out, nil
}

// scanConfig walks the JSON and turns every tier-1 match into a candidate.
func scanConfig(det *index.Detector, f ConfigFile, data []byte, at time.Time) ([]Candidate, error) {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}

	byFingerprint := map[string]Candidate{}
	walkJSON(doc, nil, func(path []string, value string) {
		for _, hit := range det.ScanTier(value, index.TierVendor) {
			c, ok := FromFinding(hit, Provenance{
				Kind:    SourceConfig,
				Runtime: f.Runtime,
				Path:    f.Path,
				At:      at,
			})
			if !ok {
				continue
			}
			// The JSON key is the best label the file offers — "anthropic.api"
			// beats "Anthropic API key" for telling two entries apart — so it
			// becomes the credential's label while the detector keeps naming
			// the kind.
			if len(path) > 0 {
				c.Label = f.Runtime + " " + strings.Join(path, ".")
			}
			byFingerprint[hit.Value] = c
		}
	})

	out := make([]Candidate, 0, len(byFingerprint))
	for _, c := range byFingerprint {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Label < out[j].Label
	})
	return out, nil
}

// walkJSON visits every string in a decoded document, carrying the key path.
func walkJSON(node any, path []string, visit func(path []string, value string)) {
	switch v := node.(type) {
	case string:
		visit(path, v)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkJSON(v[k], descend(path, k), visit)
		}
	case []any:
		for i, item := range v {
			walkJSON(item, descend(path, strconv.Itoa(i)), visit)
		}
	}
}

// descend copies rather than appending in place, so two branches of the
// document cannot share a backing array and hand each other the wrong key path.
func descend(path []string, key string) []string {
	next := make([]string, len(path), len(path)+1)
	copy(next, path)
	return append(next, key)
}
