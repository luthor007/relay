package mcp

import (
	"context"
	"fmt"

	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/install"
)

// Adoption — MEMORY.md §7 step 4, and the reason this package exists now rather
// than at M1.
//
// The installer already does the whole read: it enumerates every runtime's MCP
// configuration, de-duplicates by command and args, presents the union and asks
// "manage them in one place?". Then it stops, on purpose, and says so in words.
// From internal/install/mcp.go:
//
//	Rewriting five runtime configs to point at a server that does not exist yet
//	would take a machine with seven working MCP servers and leave it with none —
//	the exact "connected means nothing" failure §4b is trying to prevent, caused
//	by the fix for it.
//
// The switch is one field, install.Options.Gateway, and [Descriptor.Install] is
// what fills it in. Nothing else in the installer changes: it already writes
// the backups, already writes the rollback manifest, already refuses to rewrite
// a runtime that answered only through its CLI because there is no file to
// restore, and already prints which running runtimes will not see the change
// until they restart.

// Descriptor is how a runtime reaches this gateway. Exactly one of Command or
// URL is set: a stdio server is launched per client, an HTTP one is dialled.
type Descriptor struct {
	Name    string
	Command string
	Args    []string
	URL     string
}

// Zero reports whether there is anything to point a runtime at. It mirrors
// install.MCPGateway.Zero, which is the value the installer tests before it
// writes anything.
func (d Descriptor) Zero() bool { return d.Command == "" && d.URL == "" }

// Install converts to the installer's shape, which is the one field that turns
// MEMORY.md §7 step 4 on.
func (d Descriptor) Install() install.MCPGateway {
	return install.MCPGateway{Name: d.Name, Command: d.Command, Args: d.Args, URL: d.URL}
}

// StdioDescriptor is the gateway as a stdio server, which is what four of the
// five runtimes reach for by default. exe is the relay binary.
//
// stdio is the better transport for adoption, because it is bidirectional: a
// connection opened this way can be pushed a tools/list_changed notification,
// which is the difference between a mid-session grant being visible and the
// user being told to restart (see refresh.go).
//
// **It names a subcommand — `relay mcp serve` — that the CLI does not expose
// yet.** Handing this to the installer before that exists would rewrite five
// configs to point at a command that fails on launch, which is MEMORY.md §7's
// hazard in a new costume. Until the subcommand lands, adopt with
// [HTTPDescriptor], whose address is relayd's own listener and therefore
// certainly real.
func StdioDescriptor(name, exe string) Descriptor {
	if name == "" {
		name = "relay"
	}
	return Descriptor{Name: name, Command: exe, Args: []string{"mcp", "serve"}}
}

// HTTPDescriptor is the gateway as an HTTP server. base is relayd's own listen
// address; the path is [HTTPPrefix].
func HTTPDescriptor(name, base string) Descriptor {
	if name == "" {
		name = "relay"
	}
	return Descriptor{Name: name, URL: trimSlash(base) + HTTPPrefix}
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// Descriptor returns how this gateway is reachable, as configured.
func (g *Gateway) Descriptor() Descriptor {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.opts.Endpoint
}

// SetEndpoint records how this gateway is reachable, once that is known.
//
// It exists because of an ordering the daemon cannot avoid: the endpoint is
// relayd's own listen address, and the address is not known until net.Listen
// returns — which is after the HTTP server that serves this gateway has been
// built. The alternative is to take the address from the config, which is a
// guess whenever the config says :0 or the port was already taken, and a
// guessed endpoint written into five runtimes' configs is MEMORY.md §7's hazard
// exactly.
func (g *Gateway) SetEndpoint(d Descriptor) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.opts.Endpoint = d
}

// RollbackResult is what a rollback restored, plus what it means for the
// sessions that are running right now.
type RollbackResult struct {
	// Manifest is the record that was replayed.
	Manifest install.MCPManifest
	// Restored names the runtimes whose original configuration is back.
	Restored []string
	// Refresh is the tool-list consequence. A rollback changes what every agent
	// can see just as much as an adoption does, and leaving a user to discover
	// that their tools moved is the same failure in the other direction.
	Refresh RefreshResult
}

// Rollback restores every configuration named in a manifest.
//
// Adoption is only defensible because this exists and works: MEMORY.md §7 step
// 4 says "keep the originals as a rollback", and a user who does not like what
// happened gets their seven servers back, in the files they were in, without
// hunting through five products. The restore itself is install.RollbackMCP —
// the installer wrote the backups, so the installer owns putting them back, and
// duplicating that here would be a second implementation to keep in step with
// the manifest format.
func Rollback(fsys detect.WriteFS, manifestPath string) (RollbackResult, error) {
	var out RollbackResult
	m, err := install.RollbackMCP(fsys, manifestPath)
	out.Manifest = m
	for _, b := range m.Backups {
		out.Restored = append(out.Restored, string(b.Runtime))
	}
	if err != nil {
		return out, err
	}
	return out, nil
}

// Rollback restores the originals and then tells every session it can reach
// that the tool list moved back.
func (g *Gateway) Rollback(ctx context.Context, fsys detect.WriteFS, manifestPath string) (RollbackResult, error) {
	out, err := Rollback(fsys, manifestPath)
	if err != nil {
		return out, err
	}
	reason := "Relay's MCP gateway was rolled back"
	if len(out.Restored) > 0 {
		reason = fmt.Sprintf("Relay's MCP gateway was rolled back for %v", out.Restored)
	}
	out.Refresh = g.Refresh(ctx, reason)
	return out, nil
}
