package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// The silent trap, as a first-class check.
//
// ADAPTERS.md §2: if the user's ~/.claude/settings.json sets
// permissions.defaultMode to "auto" — or the run uses an auto or bypass
// permission mode — the tool named by --permission-prompt-tool is never called.
// The tool simply runs. No warning, no stderr, exit 0. The probe machine had
// defaultMode "auto" set globally and it leaked into every headless run, which
// is why the first attempt produced no output at all, and the vendored fixture
// was recorded in exactly that state ("permissionMode": "auto").
//
// Relay must both defend against it and report it, because to a user the
// failure presents as "the glasses never ask me anything", which reads as a
// feature until something destructive runs unattended.
//
// The defence is in args.go: --setting-sources "" and an explicit non-auto
// --permission-mode on every spawn. The report is here, plus the authoritative
// per-turn check in session.go, which reads permissionMode straight back out of
// system/init. That last one is the one to trust: it is what the runtime
// actually did rather than what we asked for.

// HazardKind names a way approvals can be silenced.
type HazardKind string

const (
	// HazardSettingsDefaultMode is permissions.defaultMode in a settings file.
	HazardSettingsDefaultMode HazardKind = "settings_default_mode"
	// HazardRunPermissionMode is the --permission-mode the run was given.
	HazardRunPermissionMode HazardKind = "run_permission_mode"
	// HazardRuntimeReported is what system/init said, which outranks both of
	// the above because it is the runtime's own answer.
	HazardRuntimeReported HazardKind = "runtime_reported_mode"
	// HazardUnreadableSettings is a settings file we could not read or parse,
	// so we cannot say whether the trap is set.
	HazardUnreadableSettings HazardKind = "unreadable_settings"
)

// Hazard is one finding: a place approvals are, or may be, silenced.
type Hazard struct {
	Kind   HazardKind
	Source string // a file path, "--permission-mode", or "system/init"
	Value  string // the mode that was found
	Class  ModeClass
	// Detail is a sentence a human reads.
	Detail string
	// Remedy is the fix. It never suggests a more permissive mode: every
	// remedy here moves toward asking, because the whole needs-input path
	// depends on approvals being on.
	Remedy string
}

func (h Hazard) String() string {
	return fmt.Sprintf("%s at %s: %s", h.Kind, h.Source, h.Detail)
}

// Blocking reports whether this hazard means needs-input cannot be observed at
// all, as opposed to only partly.
func (h Hazard) Blocking() bool { return h.Class == ModeSilent || h.Class == ModeUnknown }

// SettingsFile is one file the scan looked at.
type SettingsFile struct {
	// Scope is "managed", "user", "project" or "local", in Claude Code's own
	// precedence order (managed wins).
	Scope string
	Path  string
	// Exists is false when the file is simply not there, which is the ordinary
	// case and not a finding.
	Exists bool
	// DefaultMode is permissions.defaultMode, empty when unset.
	DefaultMode string
	Err         error
}

// SettingsScan is the result of inspecting the user's settings.
type SettingsScan struct {
	Files   []SettingsFile
	Hazards []Hazard
	// Effective is the highest-precedence defaultMode that was actually set.
	Effective string
	// EffectiveFrom is the file it came from.
	EffectiveFrom string
	// Class is the class of Effective. With no setting anywhere this is
	// ModeAsks, because Claude Code's own default asks.
	Class ModeClass
}

// Safe reports whether nothing found would silence approvals.
func (s SettingsScan) Safe() bool {
	for _, h := range s.Hazards {
		if h.Class != ModeAsks {
			return false
		}
	}
	return true
}

// ScanOptions locates the settings files. Everything is injectable so the
// detector is testable without touching a real home directory.
type ScanOptions struct {
	// Home defaults to the user's home directory.
	Home string
	// Workspace is the session's cwd; project and local settings live under it.
	Workspace string
	// GOOS defaults to runtime.GOOS and only picks the managed-settings path.
	GOOS string
	// Paths, when set, replaces the whole computed list.
	Paths []SettingsFile
}

// SettingsPaths returns the files Claude Code reads, lowest precedence first.
//
// The three per-user and per-project paths are the ones the probe exercised.
// The managed-settings path is from Claude Code's documented enterprise layout
// and has *not* been probed here — it is included because missing a
// managed-settings bypass would be the worst possible miss, and a file that
// does not exist costs a stat.
func SettingsPaths(o ScanOptions) []SettingsFile {
	goos := o.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	var out []SettingsFile

	managed := ""
	switch goos {
	case "darwin":
		managed = "/Library/Application Support/ClaudeCode/managed-settings.json"
	case "windows":
		managed = filepath.Join(os.Getenv("PROGRAMDATA"), "ClaudeCode", "managed-settings.json")
	default:
		managed = "/etc/claude-code/managed-settings.json"
	}
	if managed != "" {
		out = append(out, SettingsFile{Scope: "managed", Path: managed})
	}
	if o.Home != "" {
		out = append(out, SettingsFile{Scope: "user", Path: filepath.Join(o.Home, ".claude", "settings.json")})
	}
	if o.Workspace != "" {
		out = append(out,
			SettingsFile{Scope: "project", Path: filepath.Join(o.Workspace, ".claude", "settings.json")},
			SettingsFile{Scope: "local", Path: filepath.Join(o.Workspace, ".claude", "settings.local.json")},
		)
	}
	return out
}

type settingsDoc struct {
	Permissions struct {
		DefaultMode string `json:"defaultMode"`
	} `json:"permissions"`
}

// ScanSettings inspects the user's settings files for the trap.
//
// Note what this does and does not mean for a Relay-launched session. Because
// every spawn passes --setting-sources "", a hazard found here should not reach
// *our* runs — but "should not" is a claim about a flag whose interaction with
// permissionMode has not been probed, and the fixture was recorded with
// permissionMode "auto" despite being a headless run. So this scan is the
// pre-flight warning and the explanation for why the user's own terminal never
// asks; the per-turn system/init check is the authority.
func ScanSettings(o ScanOptions) SettingsScan {
	files := o.Paths
	if files == nil {
		files = SettingsPaths(o)
	}
	scan := SettingsScan{Class: ModeAsks}

	for _, f := range files {
		b, err := os.ReadFile(f.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			f.Exists = false
		case err != nil:
			f.Exists = true
			f.Err = err
			scan.Hazards = append(scan.Hazards, Hazard{
				Kind:   HazardUnreadableSettings,
				Source: f.Path,
				Class:  ModeUnknown,
				Detail: "this settings file could not be read, so whether it disables the approval prompt is unknown: " + err.Error(),
				Remedy: "make the file readable, or pass an explicit --permission-mode when starting the session",
			})
		default:
			f.Exists = true
			var doc settingsDoc
			if err := json.Unmarshal(b, &doc); err != nil {
				f.Err = err
				scan.Hazards = append(scan.Hazards, Hazard{
					Kind:   HazardUnreadableSettings,
					Source: f.Path,
					Class:  ModeUnknown,
					Detail: "this settings file is not valid JSON, so whether it disables the approval prompt is unknown: " + err.Error(),
					Remedy: "fix the JSON; Claude Code may also be ignoring the whole file",
				})
				break
			}
			f.DefaultMode = doc.Permissions.DefaultMode
			if f.DefaultMode != "" {
				scan.Effective = f.DefaultMode
				scan.EffectiveFrom = f.Path
				scan.Class = ClassifyMode(f.DefaultMode)
				if h, bad := modeHazard(HazardSettingsDefaultMode, f.Path, f.DefaultMode); bad {
					scan.Hazards = append(scan.Hazards, h)
				}
			}
		}
		scan.Files = append(scan.Files, f)
	}
	return scan
}

// modeHazard builds the finding for a mode, and reports false when the mode is
// fine.
func modeHazard(kind HazardKind, source, mode string) (Hazard, bool) {
	class := ClassifyMode(mode)
	if class == ModeAsks {
		return Hazard{}, false
	}
	h := Hazard{Kind: kind, Source: source, Value: mode, Class: class}
	switch class {
	case ModeSilent:
		h.Detail = fmt.Sprintf(
			"permission mode %q silences the approval prompt: the tool runs, there is no warning and the process exits 0, "+
				"so Relay can never ask you to approve anything and a destructive command runs unattended", mode)
		h.Remedy = `start the session with --permission-mode default (Relay does this itself) and remove permissions.defaultMode, or set it to "default"`
	case ModePartial:
		h.Detail = fmt.Sprintf(
			"permission mode %q approves some actions without asking, so Relay sees only the approvals it did not auto-grant", mode)
		h.Remedy = `set permissions.defaultMode to "default" if you want every action confirmed by voice`
	default:
		h.Detail = fmt.Sprintf(
			"permission mode %q is not one Relay recognises, so whether approvals reach it is unknown", mode)
		h.Remedy = `set permissions.defaultMode to "default", or tell Relay what this mode does`
	}
	return h, true
}
