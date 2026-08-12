package mcp

import (
	"context"
	"sort"
	"strings"
)

// Access is the half of a connector a tool needs.
//
// ORCHESTRATOR.md §4b rule 2: read and write are separate grants, because
// reading a calendar is not sending invitations and reading Gmail is not
// sending mail as you. It is part of a tool's identity rather than a property
// of a grant, so the check cannot be forgotten at the call site — there is no
// way to describe a tool without saying which half it needs.
type Access string

const (
	AccessRead  Access = "read"
	AccessWrite Access = "write"
)

// Valid reports whether this is one of the two halves.
func (a Access) Valid() bool { return a == AccessRead || a == AccessWrite }

// Scope is the grant string for one half of one connector: "gmail:read".
// internal/api's phraseScope already renders this suffix in the user's words.
func (a Access) Scope(connector string) string { return connector + ":" + string(a) }

// ParseScope splits a scope back into its connector and half.
func ParseScope(s string) (connector string, access Access, ok bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return "", "", false
	}
	a := Access(strings.ToLower(strings.TrimSpace(s[i+1:])))
	if !a.Valid() {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(s[:i])), a, true
}

// ToolName is the name agents see. The connector's name is a prefix on purpose:
// DASHBOARD.md §3.4's "last used, and for what" attributes a call to a
// connector by matching the tool name against it, and every one of the five
// runtimes decorates the name differently on top of this
// (`mcp__relay__prusa_status`, `relay.prusa_status`, `relay:prusa_status`).
// Underscore rather than dot because the strictest of the five client
// validators accepts only [A-Za-z0-9_-].
func ToolName(connector, verb string) string {
	return strings.ToLower(connector) + "_" + verb
}

// Tool is one callable thing on the shared bus. Memory and installed apps are
// tools of this exact type; ORCHESTRATOR.md §4b is explicit that they are not
// special cases.
type Tool struct {
	// Name is the wire name. Use [ToolName] to build it.
	Name        string
	Title       string
	Description string

	// InputSchema is the JSON Schema for Arguments. A tool with no schema is
	// still callable — some clients tolerate it — but every tool here has one,
	// because a client that cannot validate arguments sends whatever the model
	// produced.
	InputSchema map[string]any

	// Connector is which grant this tool belongs to. It is never empty: a tool
	// with no connector would be a tool with no grant, and that is the hole
	// rule 1 exists to close.
	Connector string

	// Access is which half of the grant it needs.
	Access Access

	// Consequence is what happens outside this machine when the tool runs, in
	// one plain sentence — "sends mail as you", "starts a print on your Prusa".
	//
	// Non-empty means ORCHESTRATOR.md §4b rule 3 applies: confirm at the
	// glasses, every time, not suppressible. Empty means the effects stop at
	// the machine boundary. There is no third state and no setting.
	Consequence string

	// Handler runs the tool.
	Handler Handler
}

// Handler runs one tool call.
type Handler func(ctx context.Context, c Call) (Result, error)

// Consequential reports whether this tool needs a spoken confirmation.
func (t Tool) Consequential() bool { return strings.TrimSpace(t.Consequence) != "" }

// Scope is the grant string this tool needs.
func (t Tool) Scope() string { return t.Access.Scope(t.Connector) }

// descriptor renders the tool for tools/list.
func (t Tool) descriptor() map[string]any {
	schema := t.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	d := map[string]any{
		"name":        t.Name,
		"description": t.describe(),
		"inputSchema": schema,
	}
	if t.Title != "" {
		d["title"] = t.Title
	}
	// _meta is where MCP puts anything not in the base schema. The two facts an
	// orchestrator needs about a Relay tool — which grant it spends and whether
	// it will stop for a spoken confirmation — go here rather than only into
	// the prose, so a client can act on them without parsing English.
	d["_meta"] = map[string]any{
		"relay/connector":     t.Connector,
		"relay/access":        string(t.Access),
		"relay/consequential": t.Consequential(),
	}
	if t.Consequential() {
		d["annotations"] = map[string]any{
			"destructiveHint": true,
			"readOnlyHint":    false,
			"openWorldHint":   true,
		}
	} else if t.Access == AccessRead {
		d["annotations"] = map[string]any{"readOnlyHint": true}
	}
	return d
}

// describe appends the consequence to the description. An agent choosing
// between tools should be able to see, without calling one, that this is the
// one that will stop and ask.
func (t Tool) describe() string {
	if !t.Consequential() {
		return t.Description
	}
	s := strings.TrimSpace(t.Description)
	if s != "" {
		s += " "
	}
	return s + "This " + strings.TrimSpace(t.Consequence) +
		", so Relay confirms it out loud before it runs, every time."
}

// Call is one tool invocation, with everything the gateway knows about who is
// asking. Every field but Tool and Arguments may be empty: a runtime that
// connects to the shared endpoint without naming itself is still allowed to
// call, it just cannot be attributed, and [Gateway] says so rather than
// guessing.
type Call struct {
	Tool      string
	Arguments map[string]any

	// Session is Relay's session id, from the per-session endpoint path or the
	// call's _meta. Empty when the caller did not say.
	Session string
	// Turn is the runtime's turn id when it sent one.
	Turn string
	// Runtime is which of the five is asking, resolved from clientInfo.
	Runtime string
	// Client is the raw clientInfo.name, kept verbatim for the audit line.
	Client string
}

// Result is a tool's answer.
type Result struct {
	// Text is what the agent reads.
	Text string
	// Structured is the machine-readable half — for a connector, the normalized
	// envelope from SYSTEM.md §3.4.
	Structured any
	// IsError marks a tool that ran and failed, as opposed to one that could
	// not be called at all. MCP keeps those apart deliberately: the first is
	// something the model can react to, the second is a protocol error.
	IsError bool
	// Target is the thing acted on — a file name, a message id — for the
	// "last used, and for what" column. It is redacted before it is stored.
	Target string
}

// Provider contributes tools to the bus. Connectors are providers; so are
// memory and the installed-app host.
type Provider interface {
	// ProviderName is a stable identifier for the audit line. It is not the
	// connector name: one provider may expose tools for several connectors.
	ProviderName() string

	// Tools is everything this provider can do right now. It is called on every
	// tools/list, so it must be cheap and must not block on a network.
	Tools(ctx context.Context) []Tool
}

// ProviderFunc adapts a function to [Provider].
type ProviderFunc struct {
	Name string
	Fn   func(ctx context.Context) []Tool
}

func (p ProviderFunc) ProviderName() string { return p.Name }
func (p ProviderFunc) Tools(ctx context.Context) []Tool {
	if p.Fn == nil {
		return nil
	}
	return p.Fn(ctx)
}

// sortTools puts the tool list in a stable order so two tools/list calls that
// saw the same state produce byte-identical output. A client that diffs the
// list to decide whether anything changed depends on it.
func sortTools(list []Tool) {
	sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
}
