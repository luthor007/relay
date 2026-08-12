package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/audit"
)

// Grants is the answer to "may this caller use this half of this connector?".
//
// It is an interface here and implemented in internal/connector so the gateway
// has no way to grant anything to itself. The only path from "no grant" to
// "grant" is a human decision recorded in internal/connector, which is what
// makes ORCHESTRATOR.md §4b rule 1 — nothing is auto-granted, not on install,
// not on suggestion, not ever — a property of the code rather than a promise.
type Grants interface {
	// Allowed reports whether one half of one connector is currently granted.
	// reason explains a no in words a user would recognise, and is returned to
	// the agent so it can say why rather than retrying.
	Allowed(ctx context.Context, connector string, access Access) (bool, string)
}

// GrantsFunc adapts a function to [Grants].
type GrantsFunc func(ctx context.Context, connector string, access Access) (bool, string)

func (f GrantsFunc) Allowed(ctx context.Context, c string, a Access) (bool, string) {
	return f(ctx, c, a)
}

// DenyAll is the default: a gateway with no grant source grants nothing. The
// zero value has to be the safe one, because the unsafe version of this
// particular default is "every agent on the machine can send mail as you".
type DenyAll struct{}

func (DenyAll) Allowed(context.Context, string, Access) (bool, string) {
	return false, "no connectors have been granted on this machine yet"
}

// Options configures a [Gateway].
type Options struct {
	// Name and Version identify the gateway in the MCP handshake and in every
	// runtime's config file.
	Name    string
	Version string

	// Endpoint is how a runtime reaches this gateway. It is what
	// [Descriptor.Install] hands the installer to turn MEMORY.md §7 step 4 on,
	// and it is zero until relayd knows its own address — which is why the
	// installer's switch is a value it is given rather than a constant.
	Endpoint Descriptor

	// Grants decides every call. Nil is DenyAll.
	Grants Grants

	// Confirm is the spoken confirmation path for consequential tools. Nil
	// refuses them — see ErrNoConfirmer.
	Confirm Confirmer

	// Record is the per-call trail. Nil counts the calls and reports that they
	// are not recorded, rather than silently having no trail.
	Record Recorder

	// Audit records provider registration and removal. Nil skips it; the
	// gateway does not fail a registration over a missing audit log, because
	// unlike a credential write a provider registration is not a mutation of
	// the user's secrets.
	Audit audit.Log

	// Sessions is the live session list, for the tool-list refresh problem.
	Sessions SessionSource

	Now   func() time.Time
	NewID func() string
	Log   *slog.Logger
}

// Stats is what the health screen shows about the bus.
type Stats struct {
	Calls         uint64
	Denied        uint64
	Confirmed     uint64
	Refused       uint64
	Failed        uint64
	Unrecorded    uint64
	Connections   int
	Providers     int
	Tools         int
	RecordDurable bool
}

// Gateway is the one MCP registry every runtime is pointed at.
type Gateway struct {
	opts Options
	log  *slog.Logger

	mu        sync.RWMutex
	providers []Provider
	conns     map[string]*Server

	stats struct {
		calls, denied, confirmed, refused, failed, unrecorded atomic.Uint64
	}
}

// NewGateway builds a gateway.
func NewGateway(o Options) *Gateway {
	if o.Name == "" {
		o.Name = "relay"
	}
	if o.Version == "" {
		o.Version = "1"
	}
	if o.Grants == nil {
		o.Grants = DenyAll{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewID == nil {
		o.NewID = uuid.NewString
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Gateway{opts: o, log: o.Log, conns: map[string]*Server{}}
}

// Name is what the gateway calls itself.
func (g *Gateway) Name() string { return g.opts.Name }

// Register adds a provider and refreshes every session that can be reached.
//
// Adding a provider is exactly the mid-session grant SYSTEM.md §9 problem 6 is
// about, so it returns the refresh result rather than leaving the caller to
// remember. ctx is used for the audit write and the notifications.
func (g *Gateway) Register(ctx context.Context, p Provider) RefreshResult {
	g.mu.Lock()
	g.providers = append(g.providers, p)
	g.mu.Unlock()

	g.audited(ctx, audit.ActionMCPRegister, p.ProviderName())
	return g.Refresh(ctx, p.ProviderName()+" is now on the bus")
}

// Remove drops a provider by name and refreshes.
func (g *Gateway) Remove(ctx context.Context, name string) RefreshResult {
	g.mu.Lock()
	kept := g.providers[:0]
	found := false
	for _, p := range g.providers {
		if p.ProviderName() == name {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	g.providers = kept
	g.mu.Unlock()

	if !found {
		return RefreshResult{Reason: name + " was not on the bus", Note: "nothing changed"}
	}
	g.audited(ctx, audit.ActionMCPRemove, name)
	return g.Refresh(ctx, name+" is off the bus")
}

func (g *Gateway) audited(ctx context.Context, action audit.Action, target string) {
	if g.opts.Audit == nil {
		return
	}
	att, err := audit.Begin(ctx, g.opts.Audit, audit.Entry{
		Actor:  audit.Actor{Kind: "orchestrator"},
		Action: action,
		Target: target,
	})
	if err != nil {
		g.log.Warn("mcp: could not record provider change", "action", action, "target", target, "err", err)
		return
	}
	if err := att.OK(ctx, nil); err != nil {
		g.log.Warn("mcp: could not close provider change", "action", action, "target", target, "err", err)
	}
}

// All returns every tool on the bus, granted or not. It is the console's view:
// "these are the tools that exist, and this is the half you have not granted".
func (g *Gateway) All(ctx context.Context) []Tool {
	g.mu.RLock()
	providers := append([]Provider(nil), g.providers...)
	g.mu.RUnlock()

	var out []Tool
	seen := map[string]bool{}
	for _, p := range providers {
		for _, t := range p.Tools(ctx) {
			if t.Name == "" || t.Connector == "" || !t.Access.Valid() || t.Handler == nil {
				// A tool with no name, no connector, no access half or no
				// implementation cannot be granted or called, so it is not
				// offered. Dropping it loudly beats offering something that
				// fails at call time.
				g.log.Warn("mcp: provider offered an unusable tool",
					"provider", p.ProviderName(), "tool", t.Name)
				continue
			}
			if seen[t.Name] {
				g.log.Warn("mcp: two providers offer the same tool name, keeping the first",
					"tool", t.Name, "provider", p.ProviderName())
				continue
			}
			seen[t.Name] = true
			out = append(out, t)
		}
	}
	sortTools(out)
	return out
}

// Tools is what an agent may actually see: every tool whose half of its
// connector is granted right now.
//
// Ungranted tools are hidden rather than listed-and-refused. An agent that can
// see `gmail_send` will try it, narrate the refusal, and teach the user that
// Relay is broken; an agent that cannot see it asks the user for another way,
// which is the behaviour a proposal (ORCHESTRATOR.md §4b) is supposed to
// interrupt.
func (g *Gateway) Tools(ctx context.Context) []Tool {
	all := g.All(ctx)
	out := all[:0:0]
	for _, t := range all {
		if ok, _ := g.opts.Grants.Allowed(ctx, t.Connector, t.Access); ok {
			out = append(out, t)
		}
	}
	return out
}

// Lookup finds one tool by wire name, granted or not.
func (g *Gateway) Lookup(ctx context.Context, name string) (Tool, bool) {
	for _, t := range g.All(ctx) {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Errors from Call.
var (
	// ErrNoSuchTool is a name nothing on the bus answers to.
	ErrNoSuchTool = errors.New("mcp: no such tool")
	// ErrNotGranted is a tool whose connector half has not been granted.
	ErrNotGranted = errors.New("mcp: not granted")
)

// NotGrantedError carries the reason a call was refused, in the user's words.
type NotGrantedError struct {
	Connector string
	Access    Access
	Reason    string
}

func (e *NotGrantedError) Error() string {
	s := fmt.Sprintf("%s access to %s has not been granted", e.Access, e.Connector)
	if e.Reason != "" {
		s += ": " + e.Reason
	}
	return s
}

func (e *NotGrantedError) Unwrap() error { return ErrNotGranted }

// Call runs one tool, in the order the three rules require: grant first, then
// confirmation, then the work, then the trail.
//
// The order is load-bearing. Checking the grant after running the tool would
// make the refusal cosmetic; asking for confirmation after running it would
// confirm something that already happened.
func (g *Gateway) Call(ctx context.Context, c Call) (Result, error) {
	g.stats.calls.Add(1)

	tool, ok := g.Lookup(ctx, c.Tool)
	if !ok {
		g.stats.failed.Add(1)
		return Result{}, fmt.Errorf("%w: %s", ErrNoSuchTool, c.Tool)
	}

	// 1. Nothing is auto-granted.
	if allowed, reason := g.opts.Grants.Allowed(ctx, tool.Connector, tool.Access); !allowed {
		g.stats.denied.Add(1)
		err := &NotGrantedError{Connector: tool.Connector, Access: tool.Access, Reason: reason}
		g.record(ctx, tool, c, "", "denied")
		return Result{}, err
	}

	// 2. Consequences outside the machine confirm at the glasses, every time.
	if tool.Consequential() {
		if g.opts.Confirm == nil {
			g.stats.refused.Add(1)
			g.record(ctx, tool, c, "", "denied")
			return Result{}, fmt.Errorf("%w (%s %s)", ErrNoConfirmer, tool.Connector, tool.Consequence)
		}
		conf := Confirmation{
			Connector:   tool.Connector,
			Tool:        tool.Name,
			Consequence: tool.Consequence,
			Target:      confirmTarget(c.Arguments),
			Session:     c.Session,
			Runtime:     c.Runtime,
		}
		if err := g.opts.Confirm.Confirm(ctx, conf); err != nil {
			g.stats.refused.Add(1)
			g.record(ctx, tool, c, conf.Target, "denied")
			return Result{}, err
		}
		g.stats.confirmed.Add(1)
	}

	res, err := tool.Handler(ctx, c)
	status := "completed"
	if err != nil {
		status = "failed"
		g.stats.failed.Add(1)
	} else if res.IsError {
		status = "failed"
	}
	target := res.Target
	if target == "" {
		target = confirmTarget(c.Arguments)
	}
	g.record(ctx, tool, c, target, status)
	return res, err
}

// record writes the per-call trail, counting anything it could not record.
func (g *Gateway) record(ctx context.Context, t Tool, c Call, target, status string) {
	if g.opts.Record == nil {
		g.stats.unrecorded.Add(1)
		return
	}
	rec := Recorded{
		ID:         g.opts.NewID(),
		Session:    c.Session,
		Turn:       c.Turn,
		Connector:  t.Connector,
		Tool:       t.Name,
		Target:     target,
		ArgsDigest: Digest(c.Arguments),
		At:         g.opts.Now(),
		Status:     status,
	}
	if err := g.opts.Record.Record(ctx, rec); err != nil {
		g.stats.unrecorded.Add(1)
		// Unattributed is expected on a runtime Relay is not driving, so it is
		// info rather than a warning — but it is still counted, and Stats is
		// what stops "one audit trail" being a claim nobody checked.
		if errors.Is(err, ErrUnattributed) {
			g.log.Info("mcp: tool call not attributable to a session",
				"tool", t.Name, "session", c.Session)
			return
		}
		g.log.Warn("mcp: could not record tool call", "tool", t.Name, "err", err)
	}
}

// confirmTarget picks the argument worth reading aloud: the one that names what
// the call acts on. It never invents one — an argument map with nothing
// recognisable produces an empty target and the confirmation says only what the
// tool declared.
func confirmTarget(args map[string]any) string {
	for _, key := range []string{"path", "file", "filename", "name", "to", "target", "id", "url", "query"} {
		if v, ok := args[key].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// Stats is a snapshot.
func (g *Gateway) Stats() Stats {
	ctx := context.Background()
	g.mu.RLock()
	providers := len(g.providers)
	conns := len(g.conns)
	g.mu.RUnlock()

	_, durable := g.opts.Record.(*SQLRecorder)
	return Stats{
		Calls:         g.stats.calls.Load(),
		Denied:        g.stats.denied.Load(),
		Confirmed:     g.stats.confirmed.Load(),
		Refused:       g.stats.refused.Load(),
		Failed:        g.stats.failed.Load(),
		Unrecorded:    g.stats.unrecorded.Load(),
		Connections:   conns,
		Providers:     providers,
		Tools:         len(g.All(ctx)),
		RecordDurable: durable,
	}
}

// ------------------------------------------------------------ connections --

func (g *Gateway) attach(s *Server) {
	g.mu.Lock()
	g.conns[s.id] = s
	g.mu.Unlock()
}

// connection returns the connection registered under key, creating it when
// there is none. canPush says whether this transport has a server→client
// channel available at all.
func (g *Gateway) connection(key, session string, canPush bool) *Server {
	g.mu.Lock()
	s, ok := g.conns[key]
	if !ok {
		s = &Server{gw: g, id: key, canPush: canPush}
		g.conns[key] = s
	}
	g.mu.Unlock()
	if session != "" {
		s.bind(session, "")
	}
	return s
}

// Sweep drops connections belonging to sessions that are gone and that have no
// channel open. It is called before every refresh so the connection table
// cannot grow for the life of the process, and it is conservative on purpose: a
// connection with a live stream is kept whatever the session list says, because
// the stream is evidence and the list is a snapshot.
func (g *Gateway) Sweep(live func(session string) bool) int {
	if live == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	dropped := 0
	for id, s := range g.conns {
		sess := s.Session()
		if sess == "" || s.CanNotify() || live(sess) {
			continue
		}
		delete(g.conns, id)
		dropped++
	}
	return dropped
}

func (g *Gateway) detach(id string) {
	g.mu.Lock()
	delete(g.conns, id)
	g.mu.Unlock()
}

// Connections lists the open MCP connections, newest id order stable by id.
func (g *Gateway) Connections() []*Server {
	g.mu.RLock()
	out := make([]*Server, 0, len(g.conns))
	for _, s := range g.conns {
		out = append(out, s)
	}
	g.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}
