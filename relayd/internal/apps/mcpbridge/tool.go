package mcpbridge

import (
	"strings"

	"github.com/luthor007/relay/relayd/internal/apps"
	"github.com/luthor007/relay/relayd/internal/mcp"
)

// How an installed app becomes a row in tools/list.
//
// APP-PLATFORM.md §4: "An installed app is automatically exposed to the user's
// agent as an MCP tool, so 'wrap up the standup' works without a wake phrase —
// the agent just calls it. Apps get to be both a thing you invoke and a
// capability your agent has."
//
// Automatic, but not unconditional, and the two conditions are both in the
// manifest the user was shown at install:
//
//   - the app declared a `tool` trigger, which is the author saying "my agent
//     may call this"; and
//   - the trigger carries a description, which internal/apps already refuses a
//     manifest without, because it is what the agent reads to decide whether to
//     call it.

// ConnectorPrefix distinguishes an app's grant identity from a connector's.
//
// The grant string an app tool spends is `app_<slug>:write`, which is a
// connector name in internal/mcp's vocabulary and an app id in this one. The
// prefix is what lets [Grants] answer for its own and delegate the rest, and it
// is what makes the console's "which connector was this" answerable by looking
// at the name — the property internal/mcp's ToolName comment relies on.
const ConnectorPrefix = "app_"

// Connector is the grant identity for one installed app.
//
// [apps.Slug] rather than the id: the strictest of the five runtime MCP client
// validators accepts only [A-Za-z0-9_-], and an app id is reverse-DNS.
func Connector(appID string) string { return ConnectorPrefix + apps.Slug(appID) }

// AppIDFromConnector is the reverse, as far as it goes: the slug, which is what
// the catalogue is matched on. It answers false for anything that is not an
// app connector.
func AppIDFromConnector(connector string) (string, bool) {
	if !strings.HasPrefix(connector, ConnectorPrefix) {
		return "", false
	}
	slug := strings.TrimPrefix(connector, ConnectorPrefix)
	if slug == "" {
		return "", false
	}
	return slug, true
}

// ToolVerb is the one verb an app tool has.
//
// One tool per app, and the verb is always the same, because the manifest has
// exactly one place to describe what the agent may ask for. An app that wants
// several verbs is describing several tools, and there is nowhere in
// `relay.json` to say what each of them takes — inventing a schema the author
// never wrote would put an unreviewed contract in front of the model.
const ToolVerb = "run"

// ToolName is the wire name for one app's tool.
func ToolName(appID string) string { return mcp.ToolName(Connector(appID), ToolVerb) }

// AccessFor is which half of the grant running this app spends.
//
// ORCHESTRATOR.md §4b rule 2 is that read and write are separate grants because
// reading a calendar is not sending invitations. For an app, the question is
// whether running it changes anything: an app that can only read is a read, and
// an app that can write a note, speak, capture or fetch is a write. The half is
// part of the tool's identity, so a console showing "this app's tool writes" is
// reading the same value the grant check reads.
func AccessFor(a apps.Installed) mcp.Access {
	for _, s := range a.Granted {
		switch s {
		case apps.ScopeMemoryWrite, apps.ScopeNetFetch, apps.ScopeGlassesSpeaker, apps.ScopeGlassesCamera:
			return mcp.AccessWrite
		}
	}
	return mcp.AccessRead
}

// ConsequenceFor is what happens outside this machine when the app runs, in one
// plain sentence, or empty when the effects stop at the machine boundary.
//
// Only `net.fetch` crosses that boundary. A note written to the user's own
// memory and a sentence spoken into the user's own ear both stay on the box and
// on the person; a request to `api.linear.app` does not, and APP-PLATFORM.md §3
// is blunt about what that combination is: *an app with `memory.read` and
// unrestricted network access is an exfiltration tool.*
//
// The allowlist is named in the sentence because the confirmation is spoken and
// "this app talks to the internet" is not a thing anyone can consent to. The
// hosts come from [apps.Installed.AllowedHosts], which is empty unless the
// scope was actually granted — so a manifest that declared hosts and had
// `net.fetch` declined produces no consequence, because it has no egress.
func ConsequenceFor(a apps.Installed) string {
	hosts := a.AllowedHosts()
	if len(hosts) == 0 {
		return ""
	}
	return "sends what it produces to " + strings.Join(hosts, ", ")
}

// inputSchema is what the agent is told this tool takes.
//
// One optional string, and nothing else, because `relay.json` has nowhere to
// declare a schema. Inventing one — guessing at fields from the description, or
// accepting an open object and hoping — would put a contract in front of the
// model that no reviewer ever read, and the whole distribution story is that an
// app is reviewed as a manifest.
//
// The handler forwards every argument it receives regardless. The gateway is
// not a validator: a client that sends more than this describes gets its extra
// arguments delivered to the app, which is better than dropping them silently.
func inputSchema(t apps.Trigger) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"request": map[string]any{
				"type": "string",
				"description": "What you want the app to do, in the user's own words. " +
					"Optional: the app knows what it is for — " + t.Description,
			},
		},
		"required": []any{},
	}
}

// describe is what the agent reads to decide whether to call this app.
//
// The author's own sentence first, verbatim, because it is the one the reviewer
// read and the one the install sheet showed. Then who wrote it, because an
// agent choosing between two apps that both claim to summarise meetings should
// be able to see that one of them is third-party code on this machine.
func describe(a apps.Installed, t apps.Trigger) string {
	s := strings.TrimSpace(t.Description)
	name := strings.TrimSpace(a.Manifest.Name)
	if name == "" {
		name = a.Manifest.ID
	}
	author := strings.TrimSpace(a.Manifest.Author.Name)
	tail := "Runs the " + name + " app, installed on this machine"
	if author != "" {
		tail += " and written by " + author
	}
	return s + " " + tail + "."
}

// InstallNote is the sentence an install sheet owes the user for an app that
// declares a `tool` trigger.
//
// APP-PLATFORM.md §3 makes every permission carry a reason shown verbatim, for
// a reason that applies here too: "your agent can call this app on its own" is
// a thing the user is agreeing to and it is not any of the nine scopes. It is
// not a scope because it grants the app nothing — the app's reach is exactly
// what its permissions say either way — but it changes *when* the app runs from
// "when I ask" to "when my agent decides", and that is worth a sentence.
//
// Returns false for an app that did not declare one, so a caller cannot render
// a blank row.
func InstallNote(m apps.Manifest) (string, bool) {
	t, ok := m.ToolTrigger()
	if !ok {
		return "", false
	}
	return "Your agent can call this app on its own, without a wake phrase: " +
		strings.TrimSpace(t.Description), true
}
