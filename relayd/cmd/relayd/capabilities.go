package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/luthor007/relay/relayd/internal/connector"
	"github.com/luthor007/relay/relayd/internal/mcp"
	"github.com/luthor007/relay/relayd/internal/orchestrator"
)

// busCapabilities is what the big model can see and ask for.
//
// The list is built from the tools actually on the bus rather than from a
// catalogue of what Relay could in principle connect to. That is the difference
// between "here is what an agent on this machine can be given" and "here is a
// brochure": a capability in this list is one some provider has registered, so
// granting it does something, and a connector nobody implemented never appears
// and cannot be promised to the user out loud.
type busCapabilities struct{ bus *toolBus }

var _ orchestrator.Capabilities = (*busCapabilities)(nil)

func (c *busCapabilities) List(ctx context.Context) ([]orchestrator.Capability, error) {
	tools := c.bus.gateway.All(ctx)

	// One row per (connector, half) — which is the unit a grant is in.
	// ORCHESTRATOR.md §4b rule 2 keeps the halves separate, and a list that
	// merged them would invite the model to ask for write when read would do.
	type key struct {
		name string
		half mcp.Access
	}
	seen := map[key][]string{}
	for _, t := range tools {
		if t.Connector == "" {
			continue
		}
		k := key{t.Connector, t.Access}
		seen[k] = append(seen[k], summaryOf(t))
	}

	out := make([]orchestrator.Capability, 0, len(seen))
	for k, verbs := range seen {
		granted, _ := c.bus.grants.Allowed(ctx, k.name, k.half)
		out = append(out, orchestrator.Capability{
			Name:    k.name,
			Half:    string(k.half),
			Granted: granted,
			Summary: summarise(verbs),
			Opens:   opensFor(k.name, k.half, verbs),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Half < out[j].Half
	})
	return out, nil
}

// Grant records a decision a human has already made.
//
// By is "glasses" because that is the surface the decision came through — the
// orchestrator only reaches this after [llm.Loop] has raised a blocking
// event.NeedsInput and someone answered it. connector.Grants refuses a request
// whose Decided is false, so if that ordering were ever broken this would fail
// rather than quietly grant.
func (c *busCapabilities) Grant(ctx context.Context, name, half, by string) error {
	access := mcp.Access(strings.ToLower(strings.TrimSpace(half)))
	if !access.Valid() {
		return fmt.Errorf("connector: %q is not read or write", half)
	}
	if by == "" {
		by = "glasses"
	}

	caps, err := c.List(ctx)
	if err != nil {
		return err
	}
	var opens string
	var known bool
	for _, x := range caps {
		if x.Name == name && x.Half == string(access) {
			opens, known = x.Opens, true
			break
		}
	}
	if !known {
		// Granting a connector nothing implements would record a permission
		// against a capability that does not exist, and the console would show
		// a row the user cannot use or explain.
		return fmt.Errorf("connector: nothing on this machine provides %s:%s", name, access)
	}

	_, refresh, err := c.bus.grants.Grant(ctx, connector.GrantRequest{
		Connector: name,
		Access:    access,
		Decided:   true,
		By:        by,
		// Opens is recorded with the grant so the trail says what was agreed
		// to, rather than what today's copy in the code happens to say.
		Opens: opens,
	})
	if err != nil {
		return err
	}
	if refresh.NeedsUser() {
		// Not an error: the grant is real. But a session that will not see the
		// new tools until it restarts is something the user has to hear, or
		// they will conclude the grant did not work.
		return nil
	}
	return nil
}

// summaryOf is one tool in a few words, preferring what the tool itself says
// it does over anything inferred from its name.
func summaryOf(t mcp.Tool) string {
	if t.Title != "" {
		return strings.ToLower(t.Title)
	}
	if t.Description != "" {
		if i := strings.IndexAny(t.Description, ".\n"); i > 0 {
			return strings.ToLower(t.Description[:i])
		}
		return strings.ToLower(t.Description)
	}
	return t.Name
}

func summarise(verbs []string) string {
	sort.Strings(verbs)
	if len(verbs) > 3 {
		return fmt.Sprintf("%s, and %d more", strings.Join(verbs[:3], "; "), len(verbs)-3)
	}
	return strings.Join(verbs, "; ")
}

// opensFor is the sentence the user is shown when deciding. It leads with the
// half, because that is the decision: reading a calendar is not sending
// invitations, and the copy has to make that the obvious difference.
func opensFor(name string, half mcp.Access, verbs []string) string {
	lead := "Lets agents read " + name
	if half == mcp.AccessWrite {
		lead = "Lets agents act on " + name + ", as you"
	}
	if len(verbs) == 0 {
		return lead + "."
	}
	return lead + " — " + summarise(verbs) + "."
}
