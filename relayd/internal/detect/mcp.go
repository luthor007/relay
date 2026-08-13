package detect

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/luthor007/relay/relayd/internal/adapter"
	"gopkg.in/yaml.v3"
)

// MCP reconciliation, MEMORY.md §7.
//
// The runtimes arrive with servers already connected — claude mcp, codex mcp,
// hermes mcp, opencode mcp and OpenClaw's config each hold their own list. So
// the installer enumerates and reconciles rather than starting empty:
//
//  1. Read every runtime's MCP config.
//  2. De-duplicate by command and args — the same server configured three times
//     is one server.
//  3. Present the union: "you have 7 MCP servers across 3 tools. Manage them in
//     one place?"
//  4. On accept, point every runtime at the Relay registry and keep the
//     originals as a rollback. That half is internal/install.
//
// One rule shapes the readers: **only structured input**. Every source here is
// either a `--json` flag or a config file in JSON, TOML or YAML. A runtime that
// offers neither is reported as unreadable with the reason attached — never as
// an empty list, because "no MCP servers" and "we could not tell" lead to
// opposite decisions and only one of them is recoverable.

// MCPServer is one entry in some runtime's MCP configuration.
type MCPServer struct {
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// URL is set instead of Command for HTTP and SSE servers.
	URL string `json:"url,omitempty"`
	// Transport is "stdio", "http" or "sse" as the source described it.
	Transport string `json:"transport,omitempty"`
}

// Key is the de-duplication key from MEMORY.md §7: command and args, or the URL
// for a remote server. The name is deliberately not part of it — the same
// server configured three times under three names is still one server.
func (s MCPServer) Key() string {
	if s.Command == "" && s.URL != "" {
		return "url\x00" + strings.TrimRight(strings.TrimSpace(s.URL), "/")
	}
	parts := append([]string{strings.TrimSpace(s.Command)}, s.Args...)
	return "cmd\x00" + strings.Join(parts, "\x00")
}

// Display is the one-line rendering for the union list.
func (s MCPServer) Display() string {
	if s.Command == "" && s.URL != "" {
		return s.URL
	}
	if len(s.Args) == 0 {
		return s.Command
	}
	return s.Command + " " + strings.Join(s.Args, " ")
}

// ToAdapter converts to the registry's shape, which is what a session is
// launched with.
func (s MCPServer) ToAdapter() adapter.MCPServer {
	out := adapter.MCPServer{Name: s.Name, Command: s.Command, Args: s.Args, URL: s.URL}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.Env = append(out.Env, k+"="+s.Env[k])
	}
	return out
}

// MCPOrigin is where one runtime's list came from.
type MCPOrigin struct {
	Runtime adapter.Runtime
	// Origin is the command or file, verbatim, so a user can check it.
	Origin string
	// FromFile is set when Origin is a path, which is what rollback backs up.
	FromFile string
	Servers  []MCPServer
	// Readable is false when neither the command nor any config file answered.
	Readable bool
	// Reason says why, when Readable is false. "No MCP servers" and "we could
	// not read this runtime's config" are different answers and the installer
	// prints them differently.
	Reason string
}

// MCPEntry is one server in the reconciled union.
type MCPEntry struct {
	MCPServer
	// Runtimes are the runtimes that already have this server.
	Runtimes []adapter.Runtime
	// Names is what each of them calls it. The same server is frequently named
	// three different things, and the console has to show all three or the user
	// cannot tell which row is theirs.
	Names map[adapter.Runtime]string
}

// Shared reports whether more than one runtime already has this server.
func (e MCPEntry) Shared() bool { return len(e.Runtimes) > 1 }

// MCPInventory is the whole reconciliation.
type MCPInventory struct {
	Origins []MCPOrigin
	Servers []MCPEntry
}

// Unreadable lists the runtimes whose configuration could not be read.
func (inv MCPInventory) Unreadable() []MCPOrigin {
	var out []MCPOrigin
	for _, o := range inv.Origins {
		if !o.Readable {
			out = append(out, o)
		}
	}
	return out
}

// ToolCount is how many runtimes contributed at least one server.
func (inv MCPInventory) ToolCount() int {
	n := 0
	for _, o := range inv.Origins {
		if o.Readable && len(o.Servers) > 0 {
			n++
		}
	}
	return n
}

// Headline is MEMORY.md §7 step 3, verbatim in shape: "you have 7 MCP servers
// across 3 tools. Manage them in one place?"
func (inv MCPInventory) Headline() string {
	if len(inv.Servers) == 0 {
		return "No MCP servers are configured in any runtime yet."
	}
	return fmt.Sprintf("You have %s across %s. Manage them in one place?",
		plural(len(inv.Servers), "MCP server"), plural(inv.ToolCount(), "tool"))
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// mcpSource describes one place a runtime's MCP list might be.
type mcpSource struct {
	// Command is a `--json` invocation. Empty when the runtime has none.
	Command []string
	// Files are config files, tried in order.
	Files []string
	// Format is how to parse the files: "json", "toml" or "yaml".
	Format string
}

// ReadMCP reconciles every runtime's MCP configuration into one union.
func ReadMCP(ctx context.Context, env Env, rep Report) MCPInventory {
	var inv MCPInventory
	index := map[string]int{}

	for _, f := range rep.Findings {
		o := readRuntimeMCP(ctx, env, f)
		inv.Origins = append(inv.Origins, o)
		for _, s := range o.Servers {
			k := s.Key()
			if k == "cmd\x00" {
				continue // neither a command nor a URL: not a server
			}
			i, ok := index[k]
			if !ok {
				entry := MCPEntry{MCPServer: s, Names: map[adapter.Runtime]string{}}
				inv.Servers = append(inv.Servers, entry)
				i = len(inv.Servers) - 1
				index[k] = i
			}
			e := &inv.Servers[i]
			if !containsRuntime(e.Runtimes, f.Runtime) {
				e.Runtimes = append(e.Runtimes, f.Runtime)
			}
			e.Names[f.Runtime] = s.Name
			if e.Name == "" {
				e.Name = s.Name
			}
		}
	}

	sort.SliceStable(inv.Servers, func(i, j int) bool {
		if len(inv.Servers[i].Runtimes) != len(inv.Servers[j].Runtimes) {
			return len(inv.Servers[i].Runtimes) > len(inv.Servers[j].Runtimes)
		}
		return inv.Servers[i].Name < inv.Servers[j].Name
	})
	return inv
}

func containsRuntime(list []adapter.Runtime, rt adapter.Runtime) bool {
	for _, r := range list {
		if r == rt {
			return true
		}
	}
	return false
}

// mcpSourcesFor is where each runtime keeps its MCP list. The command comes
// first because it is the runtime's own answer; the files are the fallback for
// a runtime that is installed but whose CLI will not answer.
func mcpSourcesFor(env Env, f Finding) []mcpSource {
	switch f.Runtime {
	case adapter.ClaudeCode:
		return []mcpSource{
			{Command: []string{"mcp", "list", "--json"}},
			{Files: []string{
				joinPath(env.Home, ".claude.json"),
				joinPath(f.StateDir, "settings.json"),
			}, Format: "json"},
		}
	case adapter.Codex:
		return []mcpSource{
			{Command: []string{"mcp", "list", "--json"}},
			{Files: []string{joinPath(f.StateDir, "config.toml")}, Format: "toml"},
		}
	case adapter.Hermes:
		return []mcpSource{
			{Command: []string{"mcp", "list", "--json"}},
			{Files: []string{
				joinPath(f.StateDir, "config.json"),
				joinPath(f.StateDir, "mcp.json"),
			}, Format: "json"},
		}
	case adapter.OpenCode:
		return []mcpSource{
			{Command: []string{"mcp", "list", "--json"}},
			{Files: []string{
				joinPath(env.Home, ".config", "opencode", "opencode.json"),
				joinPath(f.StateDir, "opencode.json"),
			}, Format: "json"},
		}
	case adapter.OpenClaw:
		// OpenClaw has no probed `mcp list --json`, so its config is the source,
		// and the config path is whatever ResolveOpenClawState found.
		return []mcpSource{
			{Files: []string{
				joinPath(f.StateDir, "openclaw.json"),
				joinPath(f.StateDir, "config.json"),
				joinPath(f.StateDir, "openclaw.yaml"),
				joinPath(f.StateDir, "config.yaml"),
			}, Format: "json"},
		}
	}
	return nil
}

func readRuntimeMCP(ctx context.Context, env Env, f Finding) MCPOrigin {
	o := MCPOrigin{Runtime: f.Runtime}
	sources := mcpSourcesFor(env, f)
	var tried []string

	for _, src := range sources {
		if len(src.Command) > 0 {
			if !f.Installed || env.Exec == nil {
				continue
			}
			label := f.BinaryName + " " + strings.Join(src.Command, " ")
			tried = append(tried, label)
			res, err := env.Exec.Run(ctx, Cmd{Name: f.BinaryName, Args: src.Command, Timeout: 2 * CommandTimeout})
			if err != nil || res.Code != 0 {
				continue
			}
			servers, ok := parseMCPJSON(res.Stdout)
			if !ok {
				continue
			}
			o.Origin, o.Servers, o.Readable = label, servers, true
			return o
		}
		for _, p := range src.Files {
			if p == "" || strings.HasSuffix(p, "/") {
				continue
			}
			tried = append(tried, p)
			b, err := env.FS.ReadFile(p)
			if err != nil {
				continue
			}
			servers, ok := parseMCPFile(b, src.Format)
			if !ok {
				continue
			}
			o.Origin, o.FromFile, o.Servers, o.Readable = p, p, servers, true
			return o
		}
	}

	switch {
	case !f.Installed && f.Status() == StatusAbsent:
		o.Reason = "not installed"
	case len(tried) == 0:
		o.Reason = "no known MCP configuration location for this runtime"
	default:
		o.Reason = "none of these answered: " + strings.Join(tried, ", ")
	}
	return o
}

// parseMCPFile parses one config file.
func parseMCPFile(b []byte, format string) ([]MCPServer, bool) {
	switch format {
	case "toml":
		return parseMCPTOML(b)
	case "yaml":
		return parseMCPYAML(b)
	default:
		if servers, ok := parseMCPJSON(b); ok {
			return servers, true
		}
		// A .json path holding YAML is not worth failing over.
		return parseMCPYAML(b)
	}
}

// mcpContainerKeys are the keys a server map hides under, across five products.
var mcpContainerKeys = []string{"mcpServers", "mcp_servers", "servers", "mcp"}

func parseMCPJSON(b []byte) ([]MCPServer, bool) {
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, false
	}
	return serversFromDoc(doc)
}

func parseMCPYAML(b []byte) ([]MCPServer, bool) {
	var doc any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, false
	}
	return serversFromDoc(normalizeYAML(doc))
}

// normalizeYAML turns yaml.v3's map keys into strings so one walker handles
// both formats.
func normalizeYAML(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeYAML(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = normalizeYAML(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalizeYAML(val)
		}
		return out
	}
	return v
}

func serversFromDoc(doc any) ([]MCPServer, bool) {
	return serversFromDocAt(doc, 0)
}

// maxMCPNesting is how deep one container key may sit inside another. OpenClaw's
// {"mcp":{"servers":…}} is the deepest of the five, and the bound stops a
// hand-written config from walking us into some unrelated map that happens to
// share a name.
const maxMCPNesting = 1

func serversFromDocAt(doc any, depth int) ([]MCPServer, bool) {
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, false
	}
	for _, key := range mcpContainerKeys {
		raw, present := m[key]
		if !present {
			continue
		}
		if list, isList := raw.([]any); isList {
			return serversFromList(list), true
		}
		inner, isMap := raw.(map[string]any)
		if !isMap {
			continue
		}
		// A container holding another container is OpenClaw's shape: the servers
		// are at mcp.servers, and reading the outer half as the list yields
		// "readable, and none" — the one answer this file exists to never give,
		// because it is indistinguishable from a machine with no servers and
		// leads the installer to the opposite decision. The server-map test comes
		// first so a server actually named "servers" is still read as a server.
		if !looksLikeServerMap(inner) && depth < maxMCPNesting {
			if servers, found := serversFromDocAt(inner, depth+1); found {
				return servers, true
			}
		}
		return serversFromMap(inner), true
	}
	// A file that is nothing but a server map, which is what a `mcp list --json`
	// could reasonably print.
	if looksLikeServerMap(m) {
		return serversFromMap(m), true
	}
	return nil, false
}

func looksLikeServerMap(m map[string]any) bool {
	if len(m) == 0 {
		return false
	}
	for _, v := range m {
		sm, ok := v.(map[string]any)
		if !ok {
			return false
		}
		if !namesATransport(sm) {
			return false
		}
	}
	return true
}

// namesATransport reports whether a map is a server entry rather than another
// layer of container. The types are load-bearing: a server says command as a
// string or an argv and url as a string, so a container whose single key happens
// to be "command" or "url" — pointing at a map — is not mistaken for one.
func namesATransport(sm map[string]any) bool {
	switch sm["command"].(type) {
	case string, []any:
		return true
	}
	_, remote := sm["url"].(string)
	return remote
}

func serversFromMap(m map[string]any) []MCPServer {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []MCPServer
	for _, n := range names {
		sm, ok := m[n].(map[string]any)
		if !ok {
			continue
		}
		s := serverFromMap(sm)
		if s.Name == "" {
			s.Name = n
		}
		if s.Command == "" && s.URL == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func serversFromList(list []any) []MCPServer {
	var out []MCPServer
	for _, item := range list {
		sm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		s := serverFromMap(sm)
		if s.Command == "" && s.URL == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func serverFromMap(sm map[string]any) MCPServer {
	var s MCPServer
	s.Name = str(sm["name"])
	s.URL = str(sm["url"])
	s.Transport = str(sm["type"])
	if s.Transport == "" {
		s.Transport = str(sm["transport"])
	}

	switch cmd := sm["command"].(type) {
	case string:
		s.Command = cmd
		s.Args = strs(sm["args"])
	case []any:
		// OpenCode's shape: command is the whole argv.
		all := toStrings(cmd)
		if len(all) > 0 {
			s.Command = all[0]
			s.Args = all[1:]
		}
	}

	for _, key := range []string{"env", "environment"} {
		if em, ok := sm[key].(map[string]any); ok && len(em) > 0 {
			s.Env = map[string]string{}
			for k, v := range em {
				s.Env[k] = str(v)
			}
			break
		}
	}
	if s.Transport == "" {
		if s.URL != "" {
			s.Transport = "http"
		} else if s.Command != "" {
			s.Transport = "stdio"
		}
	}
	return s
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func strs(v any) []string {
	list, _ := v.([]any)
	return toStrings(list)
}

func toStrings(list []any) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		} else {
			out = append(out, fmt.Sprint(v))
		}
	}
	return out
}

// codexMCPConfig is the shape of Codex's config.toml MCP section.
type codexMCPConfig struct {
	MCPServers map[string]struct {
		Command string            `toml:"command"`
		Args    []string          `toml:"args"`
		Env     map[string]string `toml:"env"`
		URL     string            `toml:"url"`
	} `toml:"mcp_servers"`
}

func parseMCPTOML(b []byte) ([]MCPServer, bool) {
	var cfg codexMCPConfig
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return nil, false
	}
	names := make([]string, 0, len(cfg.MCPServers))
	for k := range cfg.MCPServers {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []MCPServer
	for _, n := range names {
		v := cfg.MCPServers[n]
		if v.Command == "" && v.URL == "" {
			continue
		}
		s := MCPServer{Name: n, Command: v.Command, Args: v.Args, Env: v.Env, URL: v.URL}
		if s.URL != "" && s.Command == "" {
			s.Transport = "http"
		} else {
			s.Transport = "stdio"
		}
		out = append(out, s)
	}
	// An empty [mcp_servers] table is a readable answer of "none", which is not
	// the same as an unparseable file. Only a file with no such table at all is
	// a miss.
	return out, cfg.MCPServers != nil
}
