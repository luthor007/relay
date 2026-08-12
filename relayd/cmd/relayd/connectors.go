package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/luthor007/relay/relayd/internal/api"
	"github.com/luthor007/relay/relayd/internal/config"
	"github.com/luthor007/relay/relayd/internal/connector"
	"github.com/luthor007/relay/relayd/internal/llm"
	"github.com/luthor007/relay/relayd/internal/mcp"
	"github.com/luthor007/relay/relayd/internal/store"
)

// SubsystemConnectorProposals is ORCHESTRATOR.md §4b on the health screen.
const SubsystemConnectorProposals = "connector_proposals"

// buildConnectors turns the [connectors] config section into the set the
// proposer proposes from and the gateway serves tools from.
//
// It returns the set and a sentence. The sentence is the product on a machine
// with nothing configured — "no connectors are configured, so nothing can be
// proposed" is a complete answer, and it is what stops this subsystem being a
// blank on the health screen that nobody can act on.
//
// The narrow half of §4b, said out loud: this only ever proposes something the
// user already told Relay about. §4b's own example is Relay noticing a printer
// it had never heard of, which needs a catalogue of proposable descriptors and
// a setup flow on accept. That is a bigger change and it would mint grants for
// connectors with no tools behind them, so the config version ships first.
func buildConnectors(cfg config.Config, lookups credentialLookups, log *slog.Logger) (*connector.Set, string) {
	set := connector.NewSet()

	p := cfg.Connectors.Prusa
	switch {
	case p.Address == "" && p.Credential == "":
		return set, "no connectors are configured, so nothing can be proposed"
	case !p.Configured():
		// config.Validate already refuses an address with no credential, so this
		// is the reverse: a credential with no printer to spend it on.
		return set, "connectors.prusa has a credential and no address, so there is " +
			"nothing on the network to connect to"
	}

	ref, err := llm.ParseRef(p.Credential)
	if err != nil {
		log.Warn("relayd: the Prusa credential is not a usable reference",
			"error", err,
			"detail", "credentials are references — env:, file:, exec: or vault: — never pasted secrets")
		return set, "connectors.prusa.credential is not a usable reference, so the printer " +
			"would be offered and then refuse every call"
	}

	// The key is fetched per call rather than resolved once and held: that is
	// what connector.Prusa's APIKey being a function is for, and it means a
	// rotated vault entry takes effect without a restart.
	resolve := lookups.resolver(usedBy("connector/" + connector.PrusaName))
	set.Add(&connector.Prusa{
		Base:    p.Address,
		Storage: p.Storage,
		APIKey: func(ctx context.Context) (string, error) {
			return ref.Resolve(ctx, resolve)
		},
	})
	return set, "on"
}

// newProposer builds §4b's proposer over the set, or nil when there is nothing
// to propose.
//
// Note what it is given and what it is not. It holds an [mcp.Grants] — whose
// only method is a read, so an already-connected service is not proposed again
// — and it does NOT hold the *connector.Grants that could record one. The
// composition root holds both halves and the proposer holds the harmless one,
// which is the same split busCapabilities already uses for the orchestrator.
func newProposer(cfg config.Config, set *connector.Set, tools *toolBus, db *store.DB, log *slog.Logger) *connector.Proposer {
	if set == nil || len(set.All()) == 0 || tools == nil {
		return nil
	}
	p := &connector.Proposer{
		Set:         set,
		Granted:     tools.grants,
		Window:      cfg.Connectors.WindowDuration(),
		MinEpisodes: cfg.Connectors.MinEpisodes,
		Cooldown:    cfg.Connectors.CooldownDuration(),
		Log:         log,
	}
	if db != nil {
		// Without this the evidence resets on every restart — so a daemon
		// restarted daily never reaches three episodes inside a seven-day window
		// — and, worse, a dismissal is forgotten, so the connector the user
		// declined is proposed again tomorrow. §4b names repeated asking as how
		// blind-accept is trained.
		p.Memory = connector.NewSQLMemory(db)
	}
	return p
}

// connectorProposalStatus is the health screen's line for §4b.
//
// It reads the constructed proposer rather than the config, so deleting the
// join in main.go turns this off. A status built from cfg.Connectors would
// claim the subsystem was on with nothing wired behind it — this codebase's own
// defect, reproduced inside the report that exists to catch it.
func connectorProposalStatus(p *connector.Proposer, tools *toolBus, why string) string {
	if p != nil && p.Set != nil && len(p.Set.All()) > 0 {
		return "on"
	}
	if why != "" && why != "on" {
		return why
	}
	if tools == nil {
		return "no tool bus on this machine, so a connector could be granted and " +
			"then reach none of the five runtimes"
	}
	return "no connectors are configured, so nothing can be proposed"
}

// connectorProposals adapts §4b's proposer and grant store to the console's
// HTTP surface.
//
// It lives here rather than in internal/api for the same reason vaultqueue.go
// does: api must not import internal/connector, or the console's package graph
// grows an edge to the grant machinery it is deliberately downstream of.
type connectorProposals struct {
	proposer *connector.Proposer
	grants   *connector.Grants
	set      *connector.Set
}

var _ api.ConnectorProposals = (*connectorProposals)(nil)

func (c *connectorProposals) Proposals(ctx context.Context) ([]api.ConnectorProposal, error) {
	list := c.proposer.Proposals(ctx)
	out := make([]api.ConnectorProposal, 0, len(list))
	for _, p := range list {
		out = append(out, api.ConnectorProposal{
			Connector: p.Connector,
			Title:     p.Title,
			Access:    string(p.Access),
			Evidence:  p.Evidence,
			Opens:     p.Opens,
			// Line is built by the proposer from counts and from the connector's
			// own words. Rebuilding the sentence here would be a second place for
			// §4b's prompt to drift.
			Line:     p.Line(),
			Scopes:   p.Scopes,
			Episodes: p.Episodes,
			Mentions: p.Mentions,
			FirstAt:  msOrZero(p.FirstAt),
			LastAt:   msOrZero(p.LastAt),
		})
	}
	return out, nil
}

// Accept grants the read half, and only ever the read half.
//
// Decided is true because a human presented a vault-scope token to a loopback
// endpoint — the same standard handleRevokeConnector already applies to a
// revoke. Access is hard-coded rather than taken from the request: §4b rule 2
// says the write half costs a second decision, and an endpoint that accepts the
// half as an argument is the same click wearing a parameter. The second
// decision is the connectors screen.
func (c *connectorProposals) Accept(ctx context.Context, name, by string) (api.ConnectorGrantResult, error) {
	var out api.ConnectorGrantResult

	var proposed bool
	for _, p := range c.proposer.Proposals(ctx) {
		if p.Connector == name {
			proposed = true
			break
		}
	}
	// The door rule 1 would otherwise leak through: without this check the
	// accept route would grant any connector in the set, proposed or not,
	// turning a suggestion endpoint into a general grant endpoint with a longer
	// path.
	if !proposed {
		return out, fmt.Errorf("%w, so there is nothing here to accept", api.ErrNotProposed)
	}

	d, ok := c.set.Get(name)
	if !ok {
		return out, fmt.Errorf("%w: it is not configured", api.ErrNotProposed)
	}
	desc := d.Descriptor()

	g, refresh, err := c.grants.Grant(ctx, connector.GrantRequest{
		Connector: name,
		Access:    mcp.AccessRead,
		Decided:   true,
		By:        by,
		// What the person was told this half lets the agent do, recorded with
		// the grant so the trail says what was agreed to rather than what
		// today's copy in the code happens to say.
		Opens: desc.Opens[mcp.AccessRead],
		From:  "proposal",
	})
	if err != nil {
		return out, err
	}

	out = api.ConnectorGrantResult{
		ID:        g.ID,
		Connector: g.Connector,
		Scopes:    g.Scopes,
		GrantedAt: msOrZero(g.GrantedAt),
		Note:      refresh.Note,
	}
	for _, s := range refresh.Sessions {
		out.Sessions = append(out.Sessions, s.Session)
	}
	return out, nil
}

func (c *connectorProposals) Dismiss(ctx context.Context, name, reason string) error {
	return c.proposer.DismissWithReason(ctx, name, reason)
}

// connectorProposalHandler converts a possibly-nil proposer into the interface.
//
// A typed nil in an interface is not nil, and api.New checks the field against
// nil to decide whether the screen can answer at all. Without this a daemon
// with no connectors would report the surface as available and then dereference
// a nil pointer the first time anybody opened it.
func connectorProposalHandler(p *connector.Proposer, tools *toolBus, set *connector.Set) api.ConnectorProposals {
	if p == nil || tools == nil || tools.grants == nil || set == nil {
		return nil
	}
	return &connectorProposals{proposer: p, grants: tools.grants, set: set}
}

func msOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
