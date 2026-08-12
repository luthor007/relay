package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/luthor007/relay/relayd/internal/adapter"
)

// The MCP server and tool that --permission-prompt-tool points at. Claude Code
// exposes an MCP tool to the model as mcp__<server>__<tool>, so these two names
// decide the flag's value. The permission tool is *hidden from the model* and
// never appears in system/init's tools array, so the way to test it is to watch
// for the call, not to look for the tool.
const (
	MCPServerName = "relay_permission"
	MCPToolName   = "approve"
)

// PermissionToolName is the value of --permission-prompt-tool.
func PermissionToolName() string { return "mcp__" + MCPServerName + "__" + MCPToolName }

// DefaultBinary is the executable this adapter drives.
const DefaultBinary = "claude"

// ModeClass is what a Claude Code permission mode does to the approval path.
//
// This matters more than it looks. Our needs-input path *requires* permission
// checks to be on: in an auto or bypass mode the permission-prompt tool is
// never called, the tool simply runs, and there is no warning, no stderr and
// exit 0. The failure presents to a user as "the glasses never ask me
// anything", which reads as a feature until something destructive runs
// unattended. Nothing in this package may move a user toward such a mode.
type ModeClass uint8

const (
	// ModeAsks means approvals reach the permission-prompt tool.
	ModeAsks ModeClass = iota
	// ModePartial means some approvals are auto-granted and the rest still ask
	// — acceptEdits accepts file edits without asking but still prompts for
	// commands. Needs-input is real but incomplete, and an adapter must not
	// claim a capability it can only partly observe.
	ModePartial
	// ModeSilent means the prompt tool is never called.
	ModeSilent
	// ModeUnknown is a mode name nobody here recognises. Treated as unsafe to
	// depend on rather than assumed benign.
	ModeUnknown
)

func (m ModeClass) String() string {
	switch m {
	case ModeAsks:
		return "asks"
	case ModePartial:
		return "partial"
	case ModeSilent:
		return "silent"
	default:
		return "unknown"
	}
}

// SafeMode reports whether checks are on. Only ModeAsks qualifies.
func (m ModeClass) SafeMode() bool { return m == ModeAsks }

// DefaultPermissionMode is what this adapter passes when the caller says
// nothing. It is explicit rather than omitted because omitting it lets a
// user-level setting decide, and that setting is the trap.
const DefaultPermissionMode = "default"

// ClassifyMode maps a permission mode name onto what it does to approvals.
// An empty string is the flag we would pass ourselves, so it classifies as
// DefaultPermissionMode rather than as "unset".
func ClassifyMode(mode string) ModeClass {
	switch strings.TrimSpace(mode) {
	case "", "default", "plan":
		return ModeAsks
	case "acceptEdits":
		return ModePartial
	case "auto", "bypassPermissions", "dangerously-skip-permissions", "dangerouslySkipPermissions":
		return ModeSilent
	default:
		return ModeUnknown
	}
}

// ErrUnsafePermissionMode is returned when a caller asks for a mode that
// silences approvals *and* has not said it means it. It exists so the default
// path cannot drift into a bypass by accident; see Options.AllowSilentMode.
var ErrUnsafePermissionMode = errors.New("claudecode: this permission mode silences the approval prompt")

// argSpec is everything the command line depends on.
type argSpec struct {
	// SessionID is the UUID we generate and name the session with. Empty on a
	// plain resume, where the runtime already knows the name.
	SessionID string
	// Resume is the runtime's own session id to reattach to.
	Resume string
	// Fork branches the resumed session instead of continuing it.
	Fork bool

	Model          string
	PermissionMode string
	MCPConfigPath  string
	PermissionTool string

	// SettingSources is passed verbatim. Empty string is the value ADAPTERS.md
	// §2 calls mandatory: it stops the user's own settings.json — and its
	// permissions.defaultMode — from leaking into a headless run.
	SettingSources string

	Extra []string
}

// buildArgs renders the command line. Order is fixed so tests can assert on it
// and so a support ticket's copy of the line is comparable to anyone else's.
func buildArgs(s argSpec) ([]string, error) {
	if s.Resume == "" && s.SessionID == "" {
		return nil, errors.New("claudecode: a new session needs a --session-id")
	}
	if s.Fork && s.Resume == "" {
		return nil, errors.New("claudecode: --fork-session needs a session to fork from")
	}
	if s.MCPConfigPath == "" {
		return nil, errors.New("claudecode: --strict-mcp-config needs an --mcp-config file")
	}
	mode := s.PermissionMode
	if mode == "" {
		mode = DefaultPermissionMode
	}

	args := []string{
		// -p is required: --permission-prompt-tool is undocumented, does not
		// appear in claude --help, and works only in print mode.
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--replay-user-messages",
		"--setting-sources", s.SettingSources,
		"--permission-mode", mode,
		// Without this every one of the user's own MCP servers loads on each
		// spawn, which is slow, noisy and grants the session tools nobody asked
		// it to have. MEMORY.md §7's registry is injected through --mcp-config
		// instead, so the set is ours and revocable.
		"--strict-mcp-config",
		"--mcp-config", s.MCPConfigPath,
	}
	if s.PermissionTool != "" {
		args = append(args, "--permission-prompt-tool", s.PermissionTool)
	}
	if s.Resume != "" {
		args = append(args, "--resume", s.Resume)
		if s.Fork {
			args = append(args, "--fork-session")
		}
	}
	if s.SessionID != "" {
		args = append(args, "--session-id", s.SessionID)
	}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	return append(args, s.Extra...), nil
}

// mcpConfig is the file --mcp-config points at. With --strict-mcp-config this
// file is the complete set of MCP servers the session gets: ours, plus whatever
// MEMORY.md §7's registry granted this session, and nothing else.
type mcpConfig struct {
	Servers map[string]mcpServerConfig `json:"mcpServers"`
}

type mcpServerConfig struct {
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// buildMCPConfig renders the config file. endpoint is the URL Claude Code will
// call our permission server on; when it is empty the server is not wired up
// and the caller must not pass --permission-prompt-tool either.
func buildMCPConfig(endpoint string, servers []adapter.MCPServer) ([]byte, error) {
	cfg := mcpConfig{Servers: map[string]mcpServerConfig{}}
	for _, s := range servers {
		if s.Name == "" {
			return nil, errors.New("claudecode: an MCP server needs a name")
		}
		if s.Name == MCPServerName {
			return nil, fmt.Errorf("claudecode: MCP server name %q is reserved for the permission prompt", MCPServerName)
		}
		e := mcpServerConfig{Command: s.Command, Args: s.Args, URL: s.URL}
		if s.URL != "" {
			e.Type = "http"
		}
		if len(s.Env) > 0 {
			e.Env = map[string]string{}
			for _, kv := range s.Env {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return nil, fmt.Errorf("claudecode: MCP env %q is not K=V", kv)
				}
				e.Env[k] = v
			}
		}
		cfg.Servers[s.Name] = e
	}
	if endpoint != "" {
		cfg.Servers[MCPServerName] = mcpServerConfig{Type: "http", URL: endpoint}
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// mcpServerNames lists the configured servers in a stable order, for logging.
func mcpServerNames(b []byte) []string {
	var cfg mcpConfig
	if json.Unmarshal(b, &cfg) != nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Servers))
	for k := range cfg.Servers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
