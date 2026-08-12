package mcpbridge

import (
	"context"
	"strings"

	"github.com/luthor007/relay/relayd/internal/apps"
	"github.com/luthor007/relay/relayd/internal/mcp"
)

// Where an app tool's grant comes from.
//
// internal/mcp's rule is that nothing is auto-granted, and its [mcp.Grants] doc
// says why the interface exists at all: it is implemented in internal/connector
// "so the gateway has no way to grant anything to itself. The only path from
// 'no grant' to 'grant' is a human decision recorded in internal/connector."
//
// An app is the same rule with a different record. The human decision is the
// install: APP-PLATFORM.md §6's flow "shows the permission sheet with each
// reason, waits for consent, then provisions the container", and the manifest
// that sheet was built from is where the `tool` trigger is declared. So
// [Grants] does not invent a grant — it reads the one the install already made,
// out of the [Catalogue], and it cannot mint one for an app that is not
// installed or did not ask.
//
// It answers only for its own connectors and delegates everything else, so
// wiring it never widens what a connector may do:
//
//	gw := mcp.NewGateway(mcp.Options{
//	    Grants: mcpbridge.Grants{Catalogue: cat, Next: connectors},
//	    …
//	})
//	gw.Register(ctx, mcpbridge.New(mcpbridge.Options{Catalogue: cat, Invoke: rt}))

// Grants answers "may this caller run this app?" for app connectors, and
// delegates the rest.
type Grants struct {
	// Catalogue is what is installed. Nil grants no app anything.
	Catalogue Catalogue
	// Next answers for every connector that is not an app. Nil denies them,
	// matching [mcp.DenyAll]: the zero value has to be the safe one.
	Next mcp.Grants
}

// Allowed implements [mcp.Grants].
func (g Grants) Allowed(ctx context.Context, connector string, access mcp.Access) (bool, string) {
	slug, isApp := AppIDFromConnector(connector)
	if !isApp {
		if g.Next == nil {
			return mcp.DenyAll{}.Allowed(ctx, connector, access)
		}
		return g.Next.Allowed(ctx, connector, access)
	}
	if g.Catalogue == nil {
		return false, "this box has no installed-app list, so no app can be called"
	}

	var found *apps.Installed
	for _, a := range g.Catalogue.Apps(ctx) {
		if apps.Slug(a.Manifest.ID) == slug {
			app := a
			found = &app
			break
		}
	}
	if found == nil {
		// The same sentence for "never installed" and "removed since the agent
		// last looked", on purpose: an agent that could tell them apart could
		// enumerate what the user has uninstalled. It quotes the connector
		// rather than reconstructing an app id, because a slug is lossy — the
		// dots and the dashes of a reverse-DNS id both become underscores, and
		// guessing which was which puts a name in front of the user that no app
		// has.
		return false, "no app installed on this box answers to " + connector
	}

	t, ok := found.Manifest.ToolTrigger()
	if !ok || strings.TrimSpace(t.Description) == "" {
		return false, name(*found) + " did not ask to be callable by your agent — " +
			"its manifest declares no tool trigger"
	}
	if want := AccessFor(*found); access != want {
		// Read and write are separate grants, and an app's tool is one or the
		// other. Answering yes to both halves would make the half meaningless in
		// the console and in the audit trail.
		return false, name(*found) + " is a " + string(want) + " tool"
	}
	return true, ""
}
