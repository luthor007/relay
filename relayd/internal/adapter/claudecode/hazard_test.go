package claudecode

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/logx"
)

const settingsDir = "../../../testdata/claudecode/settings"

func settingsFile(scope, name string) SettingsFile {
	return SettingsFile{Scope: scope, Path: filepath.Join(settingsDir, name)}
}

func TestClassifyMode(t *testing.T) {
	for mode, want := range map[string]ModeClass{
		"":                             ModeAsks,
		"default":                      ModeAsks,
		"plan":                         ModeAsks,
		"acceptEdits":                  ModePartial,
		"auto":                         ModeSilent,
		"bypassPermissions":            ModeSilent,
		"dangerously-skip-permissions": ModeSilent,
		"wandering":                    ModeUnknown,
	} {
		if got := ClassifyMode(mode); got != want {
			t.Errorf("ClassifyMode(%q) = %v, want %v", mode, got, want)
		}
	}
	if !ModeAsks.SafeMode() || ModePartial.SafeMode() || ModeSilent.SafeMode() || ModeUnknown.SafeMode() {
		t.Error("only a mode that asks every time is safe for the needs-input path")
	}
}

// The detector, against the corpus. This is the check ADAPTERS.md §2 calls for:
// the failure presents as "the glasses never ask me anything", which reads as a
// feature until something destructive runs unattended.
func TestScanSettings(t *testing.T) {
	for _, tc := range []struct {
		file    string
		safe    bool
		class   ModeClass
		hazards int
	}{
		{"default.json", true, ModeAsks, 0},
		{"no-permissions.json", true, ModeAsks, 0},
		{"auto.json", false, ModeSilent, 1},
		{"bypass.json", false, ModeSilent, 1},
		{"accept-edits.json", false, ModePartial, 1},
		{"unknown-mode.json", false, ModeUnknown, 1},
		{"broken.json", false, ModeAsks, 1}, // unreadable: we cannot say
	} {
		t.Run(tc.file, func(t *testing.T) {
			scan := ScanSettings(ScanOptions{Paths: []SettingsFile{settingsFile("user", tc.file)}})
			if len(scan.Hazards) != tc.hazards {
				t.Fatalf("hazards = %+v, want %d", scan.Hazards, tc.hazards)
			}
			if scan.Safe() != tc.safe {
				t.Errorf("Safe() = %v, want %v", scan.Safe(), tc.safe)
			}
			if tc.file != "broken.json" && scan.Class != tc.class {
				t.Errorf("class = %v, want %v", scan.Class, tc.class)
			}
			for _, h := range scan.Hazards {
				if h.Detail == "" || h.Remedy == "" {
					t.Errorf("a hazard has to explain itself and say what to do: %+v", h)
				}
				// The risk runs one way: our needs-input path requires
				// permission checks to be ON, so nothing here may nudge a user
				// toward turning them off.
				low := strings.ToLower(h.Remedy)
				for _, bad := range []string{"bypass", "auto", "skip-permissions", "dangerous"} {
					if strings.Contains(low, bad) && !strings.Contains(low, `"default"`) {
						t.Errorf("remedy points at a more permissive mode: %q", h.Remedy)
					}
				}
			}
		})
	}
}

func TestScanSettingsMissingFileIsNotAFinding(t *testing.T) {
	scan := ScanSettings(ScanOptions{Paths: []SettingsFile{
		{Scope: "user", Path: filepath.Join(settingsDir, "nope.json")},
	}})
	if len(scan.Hazards) != 0 || !scan.Safe() {
		t.Errorf("a settings file that does not exist is the ordinary case: %+v", scan.Hazards)
	}
	if scan.Files[0].Exists {
		t.Error("Exists should be false")
	}
}

// Precedence: the last file to set a mode wins, matching the order
// SettingsPaths returns them in.
func TestScanSettingsPrecedence(t *testing.T) {
	scan := ScanSettings(ScanOptions{Paths: []SettingsFile{
		settingsFile("user", "auto.json"),
		settingsFile("project", "default.json"),
	}})
	if scan.Effective != "default" {
		t.Errorf("effective mode = %q, want the later file's", scan.Effective)
	}
	if len(scan.Hazards) != 1 {
		t.Errorf("the earlier auto is still worth reporting: %+v", scan.Hazards)
	}
}

func TestSettingsPathsCoverEveryScope(t *testing.T) {
	got := SettingsPaths(ScanOptions{Home: "/home/u", Workspace: "/w", GOOS: "linux"})
	want := []string{
		"/etc/claude-code/managed-settings.json",
		"/home/u/.claude/settings.json",
		"/w/.claude/settings.json",
		"/w/.claude/settings.local.json",
	}
	if len(got) != len(want) {
		t.Fatalf("paths = %+v", got)
	}
	for i := range want {
		if got[i].Path != want[i] {
			t.Errorf("path %d = %q, want %q", i, got[i].Path, want[i])
		}
	}
	if d := SettingsPaths(ScanOptions{Home: "/h", GOOS: "darwin"}); d[0].Path != "/Library/Application Support/ClaudeCode/managed-settings.json" {
		t.Errorf("darwin managed path = %q", d[0].Path)
	}
}

// A caller asking for a mode that silences approvals is refused by default. The
// escape hatch exists so an unattended run is possible, not so it is easy.
func TestStartRefusesASilentMode(t *testing.T) {
	proc := newScriptedProcess(scriptOptions{KeepOpen: true})
	a := New(Options{
		Launcher:          &scriptLauncher{proc: proc},
		Log:               logx.Discard(),
		PermissionBaseURL: "http://127.0.0.1:1",
		ConfigDir:         t.TempDir(),
		Home:              t.TempDir(),
	})
	defer func() { _ = a.Close(context.Background()) }()

	_, err := a.Start(context.Background(), adapter.SessionOptions{
		ID: "11111111-1111-1111-1111-111111111111", Workspace: "/work", PermissionMode: "bypassPermissions",
	})
	if !errors.Is(err, ErrUnsafePermissionMode) {
		t.Fatalf("Start = %v, want ErrUnsafePermissionMode", err)
	}

	proc2 := newScriptedProcess(scriptOptions{KeepOpen: true})
	b := New(Options{
		Launcher:          &scriptLauncher{proc: proc2},
		Log:               logx.Discard(),
		PermissionBaseURL: "http://127.0.0.1:1",
		ConfigDir:         t.TempDir(),
		Home:              t.TempDir(),
		AllowSilentMode:   true,
	})
	defer func() { _ = b.Close(context.Background()) }()

	s, err := b.Start(context.Background(), adapter.SessionOptions{
		ID: "22222222-2222-2222-2222-222222222222", Workspace: "/work", PermissionMode: "bypassPermissions",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Allowed, but the capability is off: an adapter must never report a
	// capability it cannot observe.
	if got := s.Capabilities().Get(adapter.CapNeedsInput); got != adapter.SupportNo {
		t.Errorf("CapNeedsInput = %v, want no", got)
	}
	if len(s.(*Session).Hazards()) == 0 {
		t.Error("an unattended run must still be reported as one")
	}
}
