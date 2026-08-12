package mcpbridge_test

import (
	"context"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/apps"
	"github.com/luthor007/relay/relayd/internal/apps/mcpbridge"
	"github.com/luthor007/relay/relayd/internal/mcp"
)

// Wiring the bridge's grants must never widen what a connector may do. Every
// connector that is not an app goes straight through to internal/connector's
// table, unchanged.
func TestGrantsDelegatesEveryConnectorThatIsNotAnApp(t *testing.T) {
	ctx := context.Background()
	asked := 0
	g := mcpbridge.Grants{
		Catalogue: &catalogue{list: []apps.Installed{install(t, standupManifest)}},
		Next: mcp.GrantsFunc(func(_ context.Context, connector string, a mcp.Access) (bool, string) {
			asked++
			return connector == "gmail" && a == mcp.AccessRead, "gmail is not connected"
		}),
	}
	if ok, _ := g.Allowed(ctx, "gmail", mcp.AccessRead); !ok {
		t.Fatal("a granted connector was denied by the app bridge")
	}
	if ok, reason := g.Allowed(ctx, "gmail", mcp.AccessWrite); ok || reason == "" {
		t.Fatalf("the delegate's answer was not passed through: %v %q", ok, reason)
	}
	if asked != 2 {
		t.Fatalf("the delegate was consulted %d times, want 2", asked)
	}
}

// The zero value has to be the safe one — internal/mcp's DenyAll argument
// applied here.
func TestGrantsWithNoDelegateDeniesConnectors(t *testing.T) {
	ctx := context.Background()
	g := mcpbridge.Grants{Catalogue: &catalogue{}}
	ok, reason := g.Allowed(ctx, "gmail", mcp.AccessRead)
	if ok {
		t.Fatal("a gateway with no connector table granted a connector")
	}
	if reason == "" {
		t.Fatal("a refusal must say why in words a user would recognise")
	}
}

func TestGrantsWithNoCatalogueGrantsNoApp(t *testing.T) {
	ctx := context.Background()
	g := mcpbridge.Grants{}
	ok, reason := g.Allowed(ctx, mcpbridge.Connector("dev.alexis.standup-notes"), mcp.AccessWrite)
	if ok || !strings.Contains(reason, "no installed-app list") {
		t.Fatalf("got %v %q", ok, reason)
	}
}

// The human decision is the install. There is no other path from "no grant" to
// "grant" for an app, and an app that is not installed has none.
func TestAnAppThatIsNotInstalledIsNotGranted(t *testing.T) {
	ctx := context.Background()
	g := mcpbridge.Grants{Catalogue: &catalogue{list: []apps.Installed{install(t, standupManifest)}}}
	ok, reason := g.Allowed(ctx, mcpbridge.Connector("dev.someone.else"), mcp.AccessWrite)
	if ok {
		t.Fatal("an app nobody installed was granted")
	}
	if !strings.Contains(reason, "no app installed on this box answers to") {
		t.Fatalf("the refusal says %q", reason)
	}
	// The same sentence for "removed since the agent last looked". An agent that
	// could tell the two apart could enumerate what the user has uninstalled.
	gone := mcpbridge.Grants{Catalogue: &catalogue{}}
	_, removed := gone.Allowed(ctx, mcpbridge.Connector("dev.alexis.standup-notes"), mcp.AccessWrite)
	if !strings.Contains(removed, "no app installed on this box answers to") {
		t.Fatalf("a removed app is refused differently from one that never existed: %q", removed)
	}
}

func TestAnAppWithoutAToolTriggerIsNotGranted(t *testing.T) {
	ctx := context.Background()
	g := mcpbridge.Grants{Catalogue: &catalogue{list: []apps.Installed{install(t, quietManifest)}}}
	ok, reason := g.Allowed(ctx, mcpbridge.Connector("dev.alexis.photo-log"), mcp.AccessRead)
	if ok {
		t.Fatal("an app that never asked to be callable by the agent was granted")
	}
	if !strings.Contains(reason, "did not ask to be callable") {
		t.Fatalf("the refusal says %q", reason)
	}
}

func TestTheTwoHalvesAreSeparateForAppsToo(t *testing.T) {
	ctx := context.Background()
	g := mcpbridge.Grants{Catalogue: &catalogue{list: []apps.Installed{
		install(t, standupManifest), install(t, readerManifest),
	}}}

	writer := mcpbridge.Connector("dev.alexis.standup-notes")
	if ok, _ := g.Allowed(ctx, writer, mcp.AccessWrite); !ok {
		t.Fatal("an app that writes was denied the write half")
	}
	if ok, reason := g.Allowed(ctx, writer, mcp.AccessRead); ok {
		t.Fatalf("answering yes to both halves makes the half meaningless: %q", reason)
	}

	reader := mcpbridge.Connector("dev.alexis.day-reader")
	if ok, _ := g.Allowed(ctx, reader, mcp.AccessRead); !ok {
		t.Fatal("a read-only app was denied the read half")
	}
	if ok, _ := g.Allowed(ctx, reader, mcp.AccessWrite); ok {
		t.Fatal("a read-only app was granted the write half")
	}
}

// An app's grant identity is a connector name in internal/mcp's vocabulary, so
// it has to survive ParseScope and stay inside the character set the strictest
// runtime client accepts.
func TestAnAppConnectorIsAWellFormedConnector(t *testing.T) {
	connector := mcpbridge.Connector("dev.alexis.standup-notes")
	scope := mcp.AccessWrite.Scope(connector)
	back, access, ok := mcp.ParseScope(scope)
	if !ok || back != connector || access != mcp.AccessWrite {
		t.Fatalf("the scope %q does not round-trip: %q %q %v", scope, back, access, ok)
	}
	slug, isApp := mcpbridge.AppIDFromConnector(connector)
	if !isApp || slug != "dev_alexis_standup_notes" {
		t.Fatalf("AppIDFromConnector gave %q %v", slug, isApp)
	}
	if _, isApp := mcpbridge.AppIDFromConnector("gmail"); isApp {
		t.Fatal("a connector was mistaken for an app")
	}
	if _, isApp := mcpbridge.AppIDFromConnector(mcpbridge.ConnectorPrefix); isApp {
		t.Fatal("a bare prefix is not an app")
	}
}

// The gateway hides ungranted tools rather than listing and refusing them, so
// an app the bridge will not grant must be invisible on the bus as well.
func TestAnUngrantedAppIsInvisibleRatherThanRefusing(t *testing.T) {
	ctx := context.Background()
	cat := &catalogue{list: []apps.Installed{install(t, standupManifest)}}
	p := mcpbridge.New(mcpbridge.Options{Catalogue: cat, Invoke: &recorder{}})
	gw := mcp.NewGateway(mcp.Options{
		// A grant table that knows nothing about apps: the state of a box where
		// the bridge's Grants were never wired in.
		Grants: mcp.DenyAll{},
	})
	gw.Register(ctx, p)

	if got := gw.All(ctx); len(got) != 1 {
		t.Fatalf("the app should exist on the bus for the console: %v", toolNames(got))
	}
	if got := gw.Tools(ctx); len(got) != 0 {
		t.Fatalf("an ungranted app was offered to an agent: %v", toolNames(got))
	}
}
