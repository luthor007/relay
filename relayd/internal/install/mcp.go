package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/detect"
)

// MCP reconciliation — MEMORY.md §7.
//
// The runtimes arrive with servers already connected, so the installer
// enumerates and reconciles rather than starting empty: read every runtime's
// config, de-duplicate by command and args, present the union, and on accept
// point every runtime at the Relay registry while keeping the originals as a
// rollback.
//
// **Ordering hazard, found while building this.** Steps 1–3 read; step 4
// writes, and what it writes has to point somewhere real. Relay's own MCP
// gateway is step 6 of ORCHESTRATOR.md §6's build order, three steps after this
// installer. Rewriting five runtime configs to point at a server that does not
// exist yet would take a machine with seven working MCP servers and leave it
// with none — the exact "connected means nothing" failure §4b is trying to
// prevent, caused by the fix for it.
//
// So [MCPGateway] is a required input to the write half. While it is zero the
// installer does all of the reading, presents the union, records the answer and
// changes nothing, and says so plainly. When the gateway ships, one field gets
// populated and the same code adopts for real. This is recorded in MEMORY.md §7.

// MCPGateway describes Relay's own MCP server — the one every runtime gets
// pointed at. Zero until the gateway exists.
type MCPGateway struct {
	Name    string
	Command string
	Args    []string
	URL     string
}

// Zero reports whether there is a gateway to point anything at.
func (g MCPGateway) Zero() bool { return g.Command == "" && g.URL == "" }

// MCPBackup is one original config, kept so adoption is reversible.
type MCPBackup struct {
	Runtime  adapter.Runtime `json:"runtime"`
	Original string          `json:"original"`
	Copy     string          `json:"copy"`
}

// MCPManifest is what `relay mcp rollback` reads.
type MCPManifest struct {
	At      time.Time   `json:"at"`
	Gateway MCPGateway  `json:"gateway"`
	Backups []MCPBackup `json:"backups"`
}

// MCPOutcome is what the MCP step did.
type MCPOutcome struct {
	Inventory detect.MCPInventory
	// Accepted is the user's answer to "manage them in one place?".
	Accepted bool
	// Adopted is whether any runtime config was actually rewritten. It is false
	// whenever the gateway is not yet real, and Note says so.
	Adopted bool
	// Registry is where Relay wrote the reconciled union.
	Registry string
	// ManifestPath is the rollback manifest, when anything was changed.
	ManifestPath string
	Backups      []MCPBackup
	// NeedRestart are the runtimes with a live process, which enumerate their
	// tool list once per session and therefore will not see a change until they
	// restart. MEMORY.md §7: the orchestrator says which it did.
	NeedRestart []adapter.Runtime
	Note        string
	Warnings    []string
}

// Line is the summary row.
func (m MCPOutcome) Line() string {
	if len(m.Inventory.Servers) == 0 {
		return ""
	}
	s := fmt.Sprintf("%d server(s) across %d tool(s)", len(m.Inventory.Servers), m.Inventory.ToolCount())
	switch {
	case m.Adopted:
		s += " — now managed by Relay, originals backed up"
	case m.Accepted:
		s += " — recorded, nothing changed yet"
	default:
		s += " — left alone"
	}
	return s
}

// registryFile is where the reconciled union is written.
type registryFile struct {
	At      time.Time       `json:"at"`
	Servers []registryEntry `json:"servers"`
}

type registryEntry struct {
	Name      string            `json:"name"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Transport string            `json:"transport,omitempty"`
	// From records which runtimes already had it, and what each called it —
	// the audit trail ORCHESTRATOR.md §4b's "last used, and for what" screen is
	// built on.
	From map[string]string `json:"from"`
}

func reconcileMCP(ctx context.Context, opts Options, rep detect.Report) (MCPOutcome, error) {
	p := opts.Prompt
	out := MCPOutcome{}
	out.Inventory = detect.ReadMCP(ctx, opts.Env, rep)

	unreadable := out.Inventory.Unreadable()
	if len(out.Inventory.Servers) == 0 {
		body := "No MCP servers are configured in any runtime yet. Relay will manage them from " +
			"here — grant a connector once and all five runtimes get it."
		if n := countInstalledUnreadable(rep, unreadable); n > 0 {
			body += fmt.Sprintf("\n\n%d installed runtime(s) would not tell us, so this may be "+
				"incomplete — that is a different answer from \"none\", and Relay will not "+
				"pretend otherwise.", n)
		}
		p.Section("Tools you already have", body)
		return out, nil
	}

	p.Section("Tools you already have", out.Inventory.Headline())
	if opts.Gateway.Zero() && opts.GatewayNote != "" {
		p.Say("  %s", wrapIndent(opts.GatewayNote, 2, 76))
	}
	for _, s := range out.Inventory.Servers {
		names := map[string]bool{}
		for _, n := range s.Names {
			names[n] = true
		}
		var alias []string
		for n := range names {
			alias = append(alias, n)
		}
		sort.Strings(alias)
		p.Say("  %-20s %s", strings.Join(alias, " / "), s.Display())
		p.Say("      in %s", joinRuntimes(s.Runtimes))
	}
	for _, o := range unreadable {
		if o.Reason == "not installed" {
			continue
		}
		p.Say("  %-20s could not be read: %s", o.Runtime, wrapIndent(o.Reason, 26, 76))
	}

	accepted, err := p.Confirm(Confirm{
		ID:      "mcp.adopt",
		Prompt:  "Manage them in one place?",
		Body:    "Grant once, every agent gets it. Your configs are kept.",
		Default: true,
	})
	if err != nil {
		return out, err
	}
	out.Accepted = accepted
	if !accepted {
		p.Say("  Left alone. Run `relay mcp` whenever you change your mind.")
		return out, nil
	}

	// The union is written whether or not anything is rewritten: it is Relay's
	// own registry, and it costs nothing to have it ready.
	registry := dirOf(opts.ConfigPath) + "/mcp.json"
	if err := writeRegistry(opts, registry, out.Inventory); err != nil {
		out.Warnings = append(out.Warnings, err.Error())
	} else {
		out.Registry = registry
	}

	if opts.Gateway.Zero() {
		out.Note = "Recorded. Your runtimes keep their own MCP configs for now: Relay's " +
			"gateway is a later step in the build, and pointing five tools at a server that " +
			"does not exist yet would leave you with no tools at all. Nothing on this machine " +
			"has been changed."
		p.Say("  %s", wrapIndent(out.Note, 2, 76))
		return out, nil
	}

	adopted, err := adopt(opts, rep, out.Inventory)
	out.Backups = adopted.Backups
	out.ManifestPath = adopted.ManifestPath
	out.NeedRestart = adopted.NeedRestart
	out.Warnings = append(out.Warnings, adopted.Warnings...)
	out.Adopted = len(adopted.Backups) > 0
	if err != nil {
		out.Warnings = append(out.Warnings, err.Error())
		return out, nil
	}

	if out.Adopted {
		p.Say("  Pointed %d runtime(s) at Relay. Originals saved; undo with `relay mcp rollback`.",
			len(adopted.Backups))
	}
	if len(out.NeedRestart) > 0 {
		// MEMORY.md §7's catch, said out loud rather than left for the user to
		// discover as "the thing I just connected is invisible".
		p.Say("  %s %s running right now and %s tools once per session, so the change reaches "+
			"%s on restart. Relay did not restart %s for you.",
			joinRuntimes(out.NeedRestart), plural2(len(out.NeedRestart), "is", "are"),
			plural2(len(out.NeedRestart), "enumerates", "enumerate"),
			plural2(len(out.NeedRestart), "it", "them"),
			plural2(len(out.NeedRestart), "it", "them"))
	}
	return out, nil
}

type adoptResult struct {
	Backups      []MCPBackup
	ManifestPath string
	NeedRestart  []adapter.Runtime
	Warnings     []string
}

// adopt rewrites each runtime's MCP config to point at the gateway, having
// first copied the original somewhere it can be restored from.
func adopt(opts Options, rep detect.Report, inv detect.MCPInventory) (adoptResult, error) {
	var res adoptResult
	stamp := opts.Now().UTC().Format("20060102-150405")
	backupDir := fmt.Sprintf("%s/mcp-rollback/%s", dirOf(opts.ConfigPath), stamp)
	if err := opts.FS.MkdirAll(backupDir, 0o700); err != nil {
		return res, err
	}

	for _, o := range inv.Origins {
		if !o.Readable {
			continue
		}
		if o.FromFile == "" {
			// The runtime only answered through its CLI, so there is no file to
			// rewrite or to restore. Say what to run rather than touching
			// something we cannot undo.
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%s answered through its CLI, not a config file, so Relay did not change it. "+
					"Add the registry yourself with the command that runtime documents, or "+
					"point it at %s.", o.Runtime, opts.Gateway.Name))
			continue
		}
		original, err := opts.FS.ReadFile(o.FromFile)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", o.Runtime, err))
			continue
		}
		copyPath := fmt.Sprintf("%s/%s%s", backupDir, o.Runtime, extOf(o.FromFile))
		if err := opts.FS.WriteFile(copyPath, original, 0o600); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: could not back up %s: %v", o.Runtime, o.FromFile, err))
			continue
		}
		rewritten, err := pointAtGateway(o.Runtime, original, opts.Gateway)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", o.Runtime, err))
			continue
		}
		if err := opts.FS.WriteFile(o.FromFile, rewritten, 0o600); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", o.Runtime, err))
			continue
		}
		res.Backups = append(res.Backups, MCPBackup{
			Runtime: o.Runtime, Original: o.FromFile, Copy: copyPath,
		})
		if f, ok := rep.Get(o.Runtime); ok && len(f.Running) > 0 {
			res.NeedRestart = append(res.NeedRestart, o.Runtime)
		}
	}

	if len(res.Backups) == 0 {
		return res, nil
	}
	manifest := MCPManifest{At: opts.Now().UTC(), Gateway: opts.Gateway, Backups: res.Backups}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return res, err
	}
	res.ManifestPath = backupDir + "/manifest.json"
	if err := opts.FS.WriteFile(res.ManifestPath, b, 0o600); err != nil {
		return res, err
	}
	return res, nil
}

// mcpKeyFor is the config key each runtime keeps its servers under. A key with
// a dot in it is a path into nested tables.
//
// OpenClaw is the one that nests: it reads mcp.servers (src/config/mcp-config.ts
// reads sourceConfig.mcp?.servers), and its root schema is a strict object. So a
// top-level mcpServers is not merely ignored — it makes the whole config
// invalid. Measured against the installed 2026.7.1-2: writing that key and
// running `openclaw mcp list` prints `OpenClaw config is invalid / <root>:
// Unrecognized key: "mcpServers"`, and every config-reading command refuses
// until it is removed. Adoption would have taken a working bus and stopped it.
func mcpKeyFor(rt adapter.Runtime) (key, format string) {
	switch rt {
	case adapter.Codex:
		return "mcp_servers", "toml"
	case adapter.OpenCode:
		return "mcp", "json"
	case adapter.OpenClaw:
		return "mcp.servers", "json"
	default:
		return "mcpServers", "json"
	}
}

// setServers writes the server map at a dotted key, creating the tables above it
// and leaving everything else inside them alone: OpenClaw keeps the rest of its
// mcp table beside mcp.servers, and a surgical edit stays surgical at depth.
func setServers(doc map[string]any, key string, servers map[string]any) error {
	parts := strings.Split(key, ".")
	cur := doc
	for i, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			if raw, present := cur[p]; present && raw != nil {
				// Refusing beats clobbering. The caller turns this into a warning
				// and leaves the file exactly as it found it.
				return fmt.Errorf("%s is not a table, so there is nowhere to write %s",
					strings.Join(parts[:i+1], "."), key)
			}
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = servers
	return nil
}

// pointAtGateway replaces a runtime's server list with the single Relay entry,
// leaving every other setting in the file alone. The original is already backed
// up by the caller, which is what makes a surgical edit safe enough to do.
func pointAtGateway(rt adapter.Runtime, original []byte, g MCPGateway) ([]byte, error) {
	key, format := mcpKeyFor(rt)
	entry := gatewayEntry(rt, g)

	if format == "toml" {
		var doc map[string]any
		if err := toml.Unmarshal(original, &doc); err != nil {
			return nil, fmt.Errorf("cannot parse config: %w", err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
		if err := setServers(doc, key, map[string]any{g.Name: entry}); err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	var doc map[string]any
	if err := json.Unmarshal(original, &doc); err != nil {
		return nil, fmt.Errorf("cannot parse config: %w", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	if err := setServers(doc, key, map[string]any{g.Name: entry}); err != nil {
		return nil, err
	}
	return json.MarshalIndent(doc, "", "  ")
}

// gatewayEntry is the server entry in each runtime's own vocabulary. OpenCode
// takes the whole argv as "command"; the others take a command plus args.
func gatewayEntry(rt adapter.Runtime, g MCPGateway) map[string]any {
	if g.URL != "" {
		if rt == adapter.OpenCode {
			return map[string]any{"type": "remote", "url": g.URL, "enabled": true}
		}
		return map[string]any{"type": "http", "url": g.URL}
	}
	if rt == adapter.OpenCode {
		argv := append([]string{g.Command}, g.Args...)
		return map[string]any{"type": "local", "command": argv, "enabled": true}
	}
	return map[string]any{"command": g.Command, "args": g.Args}
}

// RollbackMCP restores every config named in a manifest.
//
// Adoption is only defensible because this exists and works: a user who does
// not like what happened gets their seven servers back, in the files they were
// in, without hunting through five products.
func RollbackMCP(fsys detect.WriteFS, manifestPath string) (MCPManifest, error) {
	var m MCPManifest
	b, err := fsys.ReadFile(manifestPath)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("install: %s is not a rollback manifest: %w", manifestPath, err)
	}
	var failures []string
	for _, bk := range m.Backups {
		body, err := fsys.ReadFile(bk.Copy)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", bk.Runtime, err))
			continue
		}
		if err := fsys.WriteFile(bk.Original, body, 0o600); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", bk.Runtime, err))
		}
	}
	if len(failures) > 0 {
		return m, fmt.Errorf("install: rollback incomplete: %s", strings.Join(failures, "; "))
	}
	return m, nil
}

func writeRegistry(opts Options, path string, inv detect.MCPInventory) error {
	f := registryFile{At: opts.Now().UTC()}
	for _, s := range inv.Servers {
		e := registryEntry{
			Name: s.Name, Command: s.Command, Args: s.Args, Env: s.Env,
			URL: s.URL, Transport: s.Transport, From: map[string]string{},
		}
		for rt, name := range s.Names {
			e.From[string(rt)] = name
		}
		f.Servers = append(f.Servers, e)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := opts.FS.MkdirAll(dirOf(path), 0o700); err != nil {
		return err
	}
	return opts.FS.WriteFile(path, b, 0o600)
}

func countInstalledUnreadable(rep detect.Report, unreadable []detect.MCPOrigin) int {
	n := 0
	for _, o := range unreadable {
		if f, ok := rep.Get(o.Runtime); ok && f.Installed {
			n++
		}
	}
	return n
}

func joinRuntimes(list []adapter.Runtime) string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, string(r))
	}
	return strings.Join(out, ", ")
}

func plural2(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func extOf(p string) string {
	i := strings.LastIndex(p, ".")
	j := strings.LastIndex(p, "/")
	if i > j && i >= 0 {
		return p[i:]
	}
	return ""
}
