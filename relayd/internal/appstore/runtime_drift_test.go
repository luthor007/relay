package appstore_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/appstore"
)

// internal/apps is the app runtime — APP-PLATFORM.md §8 step 2 — and it landed
// while this package was being written. It carries its own manifest parser,
// because it is the thing that has to *enforce* a scope, and this package
// carries one because it is the thing that has to *resolve and describe* one
// before any code arrives on the box.
//
// Two parsers of one file in one binary is a duplication to remove, not to
// enshrine; see the note in this package's doc comment and the handover. Until
// a pass reconciles them, the closed list of scopes must not drift, so it is
// pinned here — by reading the source rather than by importing it, so that a
// package still being written cannot break this one's build, and so that a
// rename produces a clear failure instead of a compile error somewhere else.
//
// Only the *ids* are pinned. The sentences are each package's own today (this
// one addresses the user in the second person, the runtime quotes §3's table),
// which is exactly the drift the reconciliation has to settle.
func TestScopeVocabularyMatchesTheRuntime(t *testing.T) {
	path := filepath.Join("..", "apps", "manifest.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("internal/apps is not present: %v", err)
	}

	re := regexp.MustCompile(`(?m)^\s*Scope\w+\s+Scope\s*=\s*"([a-z.]+)"`)
	var runtime []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		runtime = append(runtime, m[1])
	}
	if len(runtime) == 0 {
		t.Skip("internal/apps no longer declares its scopes the way this guard reads them")
	}

	var here []string
	for _, s := range appstore.Scopes() {
		here = append(here, string(s))
	}
	sort.Strings(runtime)
	sort.Strings(here)
	if strings.Join(runtime, ",") != strings.Join(here, ",") {
		t.Errorf("the runtime enforces %v and the install sheet describes %v.\n"+
			"A scope the sheet omits is one the user was never asked about; a scope the "+
			"sheet shows and the runtime does not know is a promise nothing keeps.",
			runtime, here)
	}
}
