package connector

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/luthor007/relay/relayd/internal/mcp"
)

// Descriptor is what a connector is, and — the part ORCHESTRATOR.md §4b cares
// about — what it opens.
//
//	Every connector states what it opens. Scope, in the user's words, plus what
//	it lets the agent do that it could not before. Same rule as APP-PLATFORM.md
//	§3: a reason that restates the permission is not a reason.
//
// So [Descriptor.Opens] is per half and is prose, not a scope string. "Read
// your printer" is a restatement; "see whether the print finished without
// walking to the garage" is a reason.
type Descriptor struct {
	// Name is the grant key and the tool-name prefix: "prusa", "gmail".
	Name string
	// Title is what the user is shown: "Prusa 3D printer".
	Title string
	// Vendor is whose service it is, when that differs from the title.
	Vendor string

	// Opens says, per half, what the agent could not do before. A half with no
	// entry is a half this connector does not have — a read-only connector says
	// so by omitting AccessWrite, and there is then no write grant to ask for.
	Opens map[mcp.Access]string

	// Mentions are the words that count as evidence for a proposal. They are
	// the connector's own claim about what it is called in speech, which is
	// what keeps [Proposer] from having a hard-coded table of brand names.
	Mentions []string
}

// Halves lists the access halves this connector actually has, read first.
func (d Descriptor) Halves() []mcp.Access {
	var out []mcp.Access
	for _, a := range []mcp.Access{mcp.AccessRead, mcp.AccessWrite} {
		if _, ok := d.Opens[a]; ok {
			out = append(out, a)
		}
	}
	return out
}

// Scope is the grant string for one half.
func (d Descriptor) Scope(a mcp.Access) string { return a.Scope(d.Name) }

// Connector is one service the agent can reach.
type Connector interface {
	Descriptor() Descriptor
	// Tools is what this connector can do. Every tool must carry
	// Connector == Descriptor().Name and a valid Access, or the gateway drops
	// it: a tool with no grant to spend has no place on the bus.
	Tools() []mcp.Tool
}

// Poller is a connector that also produces events. SYSTEM.md §3.4's envelope is
// the output; the orchestrator never sees the vendor's shape.
//
// It is a separate interface because most connectors are request/response only,
// and a connector that cannot observe anything must not pretend to — the same
// rule that keeps adapters from emitting events they cannot see.
type Poller interface {
	Connector
	// Poll returns everything observed since the last call. It must return an
	// error rather than an empty slice when it could not reach the service:
	// "nothing happened" and "we could not tell" are different answers.
	Poll(ctx context.Context) ([]Envelope, error)
}

// Set is a collection of connectors, and the [mcp.Provider] that puts them on
// the shared bus.
//
// It does no grant checking. That is deliberate: the gateway is the one place
// grants are enforced, and a second check here would be a second place to get
// it wrong — and, worse, a place someone could later add an exception to.
type Set struct {
	mu   sync.RWMutex
	list []Connector
}

// NewSet builds a set.
func NewSet(cs ...Connector) *Set {
	s := &Set{}
	for _, c := range cs {
		s.Add(c)
	}
	return s
}

// Add registers a connector, replacing one with the same name.
func (s *Set) Add(c Connector) {
	if c == nil {
		return
	}
	name := strings.ToLower(c.Descriptor().Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.list {
		if strings.ToLower(existing.Descriptor().Name) == name {
			s.list[i] = c
			return
		}
	}
	s.list = append(s.list, c)
	sort.SliceStable(s.list, func(i, j int) bool {
		return s.list[i].Descriptor().Name < s.list[j].Descriptor().Name
	})
}

// Get finds a connector by name.
func (s *Set) Get(name string) (Connector, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.list {
		if strings.ToLower(c.Descriptor().Name) == n {
			return c, true
		}
	}
	return nil, false
}

// All lists every connector.
func (s *Set) All() []Connector {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Connector(nil), s.list...)
}

// Descriptors lists what every connector is and what it opens.
func (s *Set) Descriptors() []Descriptor {
	all := s.All()
	out := make([]Descriptor, 0, len(all))
	for _, c := range all {
		out = append(out, c.Descriptor())
	}
	return out
}

// ProviderName implements [mcp.Provider].
func (s *Set) ProviderName() string { return "connectors" }

// Tools implements [mcp.Provider]. It normalises each tool's connector name and
// drops anything a connector declared inconsistently, because a tool whose
// Connector field does not match its owner would be a tool that spends somebody
// else's grant.
func (s *Set) Tools(context.Context) []mcp.Tool {
	var out []mcp.Tool
	for _, c := range s.All() {
		d := c.Descriptor()
		name := strings.ToLower(strings.TrimSpace(d.Name))
		for _, t := range c.Tools() {
			if strings.ToLower(strings.TrimSpace(t.Connector)) != name {
				continue
			}
			if _, declared := d.Opens[t.Access]; !declared {
				// The connector offered a tool for a half it never said it
				// opens, so the user was never told what granting it would
				// mean. §4b's "every connector states what it opens" is only
				// true if an undeclared half cannot slip through.
				continue
			}
			t.Connector = name
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Poll runs every poller in the set and returns everything they observed,
// normalized. Errors are returned per connector rather than aborting the pass:
// a printer that is off should not stop the calendar from being read.
func (s *Set) Poll(ctx context.Context, n *Normalizer) ([]Envelope, map[string]error) {
	var out []Envelope
	errs := map[string]error{}
	for _, c := range s.All() {
		p, ok := c.(Poller)
		if !ok {
			continue
		}
		name := c.Descriptor().Name
		evs, err := p.Poll(ctx)
		if err != nil {
			errs[name] = err
			continue
		}
		for _, e := range evs {
			norm, _, nerr := n.Normalize(e)
			if nerr != nil {
				errs[name] = nerr
				continue
			}
			out = append(out, norm)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, errs
}
