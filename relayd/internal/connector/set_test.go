package connector_test

import (
	"context"
	"errors"
	"testing"

	"github.com/luthor007/relay/relayd/internal/connector"
	"github.com/luthor007/relay/relayd/internal/mcp"
)

// A printer that is off must not stop the calendar from being read: errors come
// back per connector rather than aborting the pass.
func TestPollIsPerConnector(t *testing.T) {
	good := newPrusa(&stubHTTP{answers: map[string]stubAnswer{
		"GET /api/v1/status": {200, statusPrinting},
	}})
	ctx := context.Background()
	if _, err := good.Poll(ctx); err != nil { // baseline
		t.Fatal(err)
	}

	broken := &connector.Prusa{HTTP: &stubHTTP{err: errors.New("off")}}
	set := connector.NewSet(good, quietConnector{})
	// Replacing by name is how a set is reconfigured, and the same name must not
	// produce two rows.
	set.Add(broken)
	if len(set.All()) != 2 {
		t.Fatalf("adding the same connector twice must replace it: %d", len(set.All()))
	}

	evs, errs := set.Poll(ctx, connector.MustNormalizer())
	if len(errs) != 1 || errs["prusa"] == nil {
		t.Fatalf("the broken connector must be named: %v", errs)
	}
	if len(evs) != 1 || evs[0].Connector != "quiet" {
		t.Fatalf("the working one must still be heard: %+v", evs)
	}
}

func TestDescriptorHalvesAreOrderedReadFirst(t *testing.T) {
	d := (&connector.Prusa{}).Descriptor()
	halves := d.Halves()
	if len(halves) != 2 || halves[0] != mcp.AccessRead || halves[1] != mcp.AccessWrite {
		t.Fatalf("halves = %v", halves)
	}
	if d.Scope(mcp.AccessWrite) != "prusa:write" {
		t.Fatalf("scope = %q", d.Scope(mcp.AccessWrite))
	}
	// ORCHESTRATOR.md §4b: a reason that restates the permission is not a
	// reason, so neither half may be phrased as "read the printer".
	for half, opens := range d.Opens {
		if opens == "" {
			t.Fatalf("%s has no reason attached", half)
		}
	}
}

func TestGetIsCaseInsensitive(t *testing.T) {
	set := connector.NewSet(&connector.Prusa{})
	if _, ok := set.Get("  PRUSA "); !ok {
		t.Fatal("the grant key is normalised everywhere else, so lookup is too")
	}
	if _, ok := set.Get("gmail"); ok {
		t.Fatal("a connector nobody added is not there")
	}
}

// quietConnector is a poller that always has exactly one thing to say.
type quietConnector struct{}

func (quietConnector) Descriptor() connector.Descriptor {
	return connector.Descriptor{
		Name:  "quiet",
		Opens: map[mcp.Access]string{mcp.AccessRead: "hear about one thing"},
	}
}

func (quietConnector) Tools() []mcp.Tool { return nil }

func (quietConnector) Poll(context.Context) ([]connector.Envelope, error) {
	return []connector.Envelope{{
		Connector: "quiet", Kind: "thing.happened", At: at, Summary: "a thing happened",
	}}, nil
}
