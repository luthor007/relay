package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/audit"
	"github.com/luthor007/relay/relayd/internal/bus"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/store"
	"github.com/luthor007/relay/relayd/internal/web"
)

// UtteranceHandler is where a recognised sentence goes.
//
// Routing is M1 step 3 and lives in internal/routing, deliberately outside this
// package: the API's job is to carry the utterance, not to decide what it means.
// Until a router is wired the phone gets an explicit "not implemented" naming
// the milestone rather than silence, because a device that hears you and says
// nothing is indistinguishable from a broken one.
type UtteranceHandler interface {
	Utterance(ctx context.Context, u Utterance) error
}

// UtteranceFunc adapts a function to [UtteranceHandler].
type UtteranceFunc func(ctx context.Context, u Utterance) error

func (f UtteranceFunc) Utterance(ctx context.Context, u Utterance) error { return f(ctx, u) }

// DeviceHandler receives the glasses' non-speech inputs.
type DeviceHandler interface {
	Touch(ctx context.Context, t Touch) error
	Wear(ctx context.Context, w Wear) error
}

// Options configures the server.
type Options struct {
	Registry *registry.Registry
	Pinger   *bus.Pinger
	Gate     *bus.SpeechGate

	// DB is the main database, for the index tier the console reads: historical
	// sessions, facts, connector grants, secret markers. Nil serves those
	// screens empty rather than 404 — DASHBOARD.md's screens have to render on a
	// box where backfill has not run.
	DB *store.DB

	// Token authenticates every request. Empty generates one, which is what
	// relayd prints on start — the same pattern as the pairing code.
	Token string

	// Authenticator overrides the token check. This is the seam Relay Cloud
	// uses: it authenticates accounts and hands on an [Identity], and every
	// authorization decision still happens in this package. Nil uses the
	// printed token.
	Authenticator Authenticator

	// VaultReauth is DASHBOARD.md §4's cloud rule — every vault write
	// re-authenticated regardless of session age. Zero switches the check off,
	// which is correct for the token authenticator: the token is presented on
	// every request, so it is always fresh.
	VaultReauth time.Duration

	// TrustForwardedFor reads X-Forwarded-For for the audit log's "from where".
	// Off by default: on a loopback bind anyone who can reach the port can set
	// that header, and a log that records an attacker's chosen address is worse
	// than one that records none.
	TrustForwardedFor bool

	// Cloud marks the hosted deployment. It turns on the billing route and
	// nothing else — the screens are identical by design (DASHBOARD.md §2).
	Cloud bool

	// Listen is the bind address, for the health endpoint to report and for
	// [CheckBind] to refuse.
	Listen string

	// LAN is the deliberate flag that allows a non-loopback bind. DASHBOARD.md
	// §4: "Exposing it on a LAN is a deliberate flag with a warning, not a
	// config default someone flips without reading."
	LAN bool

	// Audit records every credential and connector mutation. Nil gets an
	// in-memory log rather than none, because a mutation path with nowhere to
	// record it is refused, and refusing every credential write on a box with no
	// writable data directory would be worse than saying the trail is not
	// durable. Health reports which it is.
	Audit audit.Log

	// Credentials is the vault, minus any path to a plaintext secret. See
	// [CredentialStore]: the narrowing is the mechanism, not a convention.
	Credentials CredentialStore
	// Validator makes MEMORY.md §6's one real call. Nil reports "not validated
	// here" rather than pretending.
	Validator CredentialValidator
	// Proposals is MEMORY.md §6's queue of "I found what looks like a Twilio
	// token". Nil falls back to the index's secret markers for the listing.
	Proposals ProposalStore

	// Connectors revokes a grant across all five runtimes (ORCHESTRATOR.md §4b).
	// Nil records the grant as revoked and says plainly which runtimes were not
	// reached, rather than claiming a revoke that did not happen.
	Connectors ConnectorRevoker
	// ConnectorProposals is §4b's evidence-grounded suggestion queue — a
	// different thing from [Options.Proposals], which is MEMORY.md §6's
	// credential queue. Nil serves the list with Available false and a sentence
	// saying nothing on this machine can propose anything.
	ConnectorProposals ConnectorProposals
	// MCP is MEMORY.md §7's reconciled union. It is a function rather than a
	// value because detection shells out to five runtimes and must not run on
	// every request.
	MCP MCPSource

	// Gateway is the shared MCP tool bus. Nil leaves /mcp/ unmounted, which is
	// what a daemon with no gateway should do rather than answering 404 from a
	// path five runtimes have been told is real.
	Gateway http.Handler

	// Machine is the detection pass behind DASHBOARD.md §3.5's "installed" and
	// "running".
	Machine MachineSource
	// RuntimeAuth answers §3.5's "authenticated". Nil leaves every runtime's
	// login state reported as unknown, which is the honest answer until
	// something can observe it.
	RuntimeAuth RuntimeAuthSource
	// Prober is the re-probe button. Nil answers 503 with the reason.
	Prober Prober
	// Setup is the configured voice, orchestrator models and embedding, for the
	// health screen to name.
	Setup *Setup
	// Host reports cloud machine health: uptime, disk, last backup.
	Host HostHealthSource

	// Billing returns a Stripe customer portal URL. Cloud only, one endpoint,
	// and nothing about billing is rebuilt here (DASHBOARD.md §3.6).
	Billing BillingPortal

	Utterances UtteranceHandler
	Devices    DeviceHandler

	Version   string
	StartedAt time.Time
	Now       func() time.Time
	NewID     func() string
	Log       *slog.Logger
}

// Server is relayd's HTTP and WebSocket surface. It is also a [bus.Delivery]:
// a ping becomes a confirm.request or a speak on every connected phone.
type Server struct {
	reg    *registry.Registry
	pinger *bus.Pinger
	gate   *bus.SpeechGate
	db     *store.DB

	token     string
	listen    string
	lan       bool
	version   string
	startedAt time.Time
	now       func() time.Time
	newID     func() string
	log       *slog.Logger

	authn          Authenticator
	vaultReauth    time.Duration
	trustForwarded bool
	cloud          bool

	audit       audit.Log
	consoleUI   *web.Console
	credentials CredentialStore
	validator   CredentialValidator
	proposals   ProposalStore
	connectors  ConnectorRevoker

	connectorProposals ConnectorProposals

	mcp          MCPSource
	gateway      http.Handler
	subsystems   map[string]string
	machine      MachineSource
	runtimeAuthn RuntimeAuthSource
	prober       Prober
	setup        *Setup
	host         HostHealthSource
	billing      BillingPortal

	utterances UtteranceHandler
	devices    DeviceHandler

	mux http.Handler

	// pings is the fan-out to every transport. A ping reaches the phone over the
	// WebSocket and the console over SSE, from one delivery.
	pings *bus.Topic[Ping]
	// console is the second fan-out: credential, connector, fact and audit
	// changes, so an optimistic UI can reconcile against what actually landed.
	console *bus.Topic[ConsoleEvent]

	mu        sync.Mutex
	pending   map[string]*event.NeedsInput // ping id → the question it is about
	clients   int
	lastProbe *ProbeReport
}

// Ping is a delivered ping in the shape the transports send it.
type Ping struct {
	Ping    bus.Ping
	Confirm *ConfirmRequest
	Speak   *Speak
	Notify  *Notify
	// Render is a mini-app's view. It rides this topic because the topic is the
	// fan-out to every transport, not because a view is a ping: [Ping.Ping] is
	// zero for one, it is never batched, and it is never held for quiet hours.
	// An app draws in reply to something the user just did, and holding that for
	// a gap in the conversation would be the turn-taking policy applied to
	// something it was not written about.
	Render *UIRender
	// Resolved is set instead of the rest when a ping is retracted.
	Resolved *ConfirmResolved
}

var _ bus.Delivery = (*Server)(nil)

// New builds the server.
func New(o Options) (*Server, error) {
	if o.Registry == nil {
		return nil, errors.New("api: a registry is required")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewID == nil {
		o.NewID = uuid.NewString
	}
	if o.StartedAt.IsZero() {
		o.StartedAt = o.Now()
	}
	if o.Listen == "" {
		o.Listen = "127.0.0.1:8787"
	}
	if o.Token == "" {
		t, err := GenerateToken()
		if err != nil {
			return nil, err
		}
		o.Token = t
	}
	// A mutation with nowhere to record it is refused, so there is always
	// somewhere. The memory log reports Durable() false and health prints it.
	if o.Audit == nil {
		o.Audit = audit.NewMemory()
	}

	s := &Server{
		reg:            o.Registry,
		pinger:         o.Pinger,
		gate:           o.Gate,
		db:             o.DB,
		token:          o.Token,
		listen:         o.Listen,
		lan:            o.LAN,
		version:        o.Version,
		startedAt:      o.StartedAt,
		now:            o.Now,
		newID:          o.NewID,
		log:            o.Log,
		vaultReauth:    o.VaultReauth,
		trustForwarded: o.TrustForwardedFor,
		cloud:          o.Cloud,
		audit:          o.Audit,
		credentials:    o.Credentials,
		validator:      o.Validator,
		proposals:      o.Proposals,
		connectors:     o.Connectors,

		connectorProposals: o.ConnectorProposals,

		mcp:          o.MCP,
		gateway:      o.Gateway,
		machine:      o.Machine,
		runtimeAuthn: o.RuntimeAuth,
		prober:       o.Prober,
		setup:        o.Setup,
		host:         o.Host,
		billing:      o.Billing,
		utterances:   o.Utterances,
		devices:      o.Devices,
		pings:        bus.NewTopic(bus.TopicOptions[Ping]{Buffer: 64, Log: o.Log}),
		console:      bus.NewTopic(bus.TopicOptions[ConsoleEvent]{Buffer: 64, Log: o.Log}),
		pending:      map[string]*event.NeedsInput{},
	}
	s.authn = o.Authenticator
	if s.authn == nil {
		s.authn = tokenAuth{token: o.Token, now: s.now}
	}
	s.mux = s.routes()
	return s, nil
}

// Token is the bearer token every request needs.
func (s *Server) Token() string { return s.token }

// SetPinger attaches the ping policy after construction.
//
// The two are mutually dependent by design and neither dependency is
// accidental: the Pinger needs the Server as its [bus.Delivery], and the Server
// needs the Pinger so that answering a question counts as having heard the ping
// and cancels the two-minute repeat. One of the two edges has to be set after
// the fact, and this is the one that is optional — a Server with no Pinger
// still serves, it simply never marks a ping heard.
// SetSubsystem records that a subsystem was wired, or why it was not.
//
// Called from the composition root as each one is built. status is "on" or a
// sentence a user would understand.
func (s *Server) SetSubsystem(name, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subsystems == nil {
		s.subsystems = map[string]string{}
	}
	s.subsystems[name] = status
}

func (s *Server) subsystemReport() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.subsystems) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.subsystems))
	for k, v := range s.subsystems {
		out[k] = v
	}
	return out
}

func (s *Server) SetPinger(p *bus.Pinger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pinger = p
}

func (s *Server) currentPinger() *bus.Pinger {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pinger
}

// ServeHTTP makes the server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated, and deliberately says nothing: a supervisor needs to know
	// the process is alive without holding a credential.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// DASHBOARD.md §3.5 — runtimes and health.
	mux.Handle("GET /v1/health", s.guard(ScopeRead, s.handleHealth))
	mux.Handle("GET /v1/runtimes", s.guard(ScopeRead, s.handleRuntimes))
	mux.Handle("POST /v1/health/probe", s.guard(ScopeWrite, s.handleProbe))

	// DASHBOARD.md §3.1 — sessions.
	mux.Handle("GET /v1/sessions", s.guard(ScopeRead, s.handleSessions))
	// A literal segment beats a wildcard, so this wins over /v1/sessions/{id}.
	// Session ids are uuids and index keys, so nothing real is shadowed.
	mux.Handle("GET /v1/sessions/blocked", s.guard(ScopeRead, s.handleBlockedSessions))
	mux.Handle("GET /v1/sessions/{id}", s.guard(ScopeRead, s.handleSession))
	mux.Handle("GET /v1/sessions/{id}/transcript", s.guard(ScopeRead, s.handleTranscript))
	// A historical session is keyed by the runtime and the runtime's own id —
	// index.Session.ID() is "<runtime>/<session>" — so it arrives as two path
	// segments rather than one. Making the console percent-encode a slash to
	// reach its own default screen would be a trap, so the two-segment form is a
	// route of its own. [sessionKey] rejoins them.
	mux.Handle("GET /v1/sessions/{runtime}/{session}", s.guard(ScopeRead, s.handleSession))
	mux.Handle("GET /v1/sessions/{runtime}/{session}/transcript", s.guard(ScopeRead, s.handleTranscript))
	mux.Handle("POST /v1/sessions/{id}/turns", s.guard(ScopeWrite, s.handleSendTurn))
	mux.Handle("POST /v1/sessions/{id}/cancel", s.guard(ScopeWrite, s.handleCancel))
	mux.Handle("POST /v1/sessions/{id}/answer", s.guard(ScopeWrite, s.handleAnswer))

	// DASHBOARD.md §3.2 — credentials. Every mutation here is ScopeVault, and
	// every one of them is audited before it runs.
	mux.Handle("GET /v1/credentials", s.guard(ScopeRead, s.handleCredentials))
	mux.Handle("POST /v1/credentials", s.guard(ScopeVault, s.handleAddCredential))
	mux.Handle("POST /v1/credentials/{id}/validate", s.guard(ScopeVault, s.handleValidateCredential))
	mux.Handle("POST /v1/credentials/{id}/rotate", s.guard(ScopeVault, s.handleRotateCredential))
	mux.Handle("POST /v1/credentials/{id}/revoke", s.guard(ScopeVault, s.handleRevokeCredential))
	mux.Handle("GET /v1/credentials/proposals", s.guard(ScopeRead, s.handleProposals))
	mux.Handle("POST /v1/credentials/proposals/{id}/accept", s.guard(ScopeVault, s.handleAcceptProposal))
	mux.Handle("POST /v1/credentials/proposals/{id}/dismiss", s.guard(ScopeVault, s.handleDismissProposal))

	// DASHBOARD.md §3.3 — facts. Editable, not just deletable.
	mux.Handle("GET /v1/facts", s.guard(ScopeRead, s.handleFacts))
	mux.Handle("PATCH /v1/facts/{id}", s.guard(ScopeWrite, s.handleEditFact))
	mux.Handle("DELETE /v1/facts/{id}", s.guard(ScopeWrite, s.handleDeleteFact))

	// DASHBOARD.md §3.4 — connectors and MCP.
	mux.Handle("GET /v1/connectors", s.guard(ScopeRead, s.handleConnectors))
	// ORCHESTRATOR.md §4b's proposals. A literal segment beats a wildcard in
	// Go's ServeMux, so these three sit alongside /v1/connectors/{id}/revoke
	// without shadowing it — and a connector named "proposals" is not possible,
	// because the name is the grant key and the tool prefix.
	//
	// The list is ScopeRead and both answers are ScopeVault. A proposal is a
	// question and reading it changes nothing; accepting one grants access to a
	// service, and declining one silences a suggestion for a month. Neither
	// answer belongs at the same scope as reading the session list.
	mux.Handle("GET /v1/connectors/proposals", s.guard(ScopeRead, s.handleConnectorProposals))
	mux.Handle("POST /v1/connectors/proposals/{connector}/accept",
		s.guard(ScopeVault, s.handleAcceptConnectorProposal))
	mux.Handle("POST /v1/connectors/proposals/{connector}/dismiss",
		s.guard(ScopeVault, s.handleDismissConnectorProposal))
	mux.Handle("POST /v1/connectors/{id}/revoke", s.guard(ScopeVault, s.handleRevokeConnector))
	mux.Handle("GET /v1/mcp", s.guard(ScopeRead, s.handleMCP))

	// ORCHESTRATOR.md §4b's shared bus: grant once, works in all five, revoke
	// once. Loopback only — see mountGateway.
	s.mountGateway(mux)

	// DASHBOARD.md §4 — the audit log the console itself displays.
	mux.Handle("GET /v1/audit", s.guard(ScopeRead, s.handleAudit))

	// DASHBOARD.md §3.6 — billing is a link, cloud only, and nothing more.
	mux.Handle("GET /v1/billing/portal", s.guard(ScopeRead, s.handleBillingPortal))

	mux.Handle("GET /v1/events", s.guard(ScopeRead, s.handleSSE))
	// The phone's socket, not the console's — the console watches over SSE. It
	// carries session.command and consent.decision, so it is guarded at the
	// highest scope any frame on it can exercise rather than at the lowest.
	// Per-frame checks would put authorization in a second place, which is the
	// one thing this design does not do.
	mux.Handle("GET /v1/ws", s.guard(ScopeWrite, s.handleWS))

	// DASHBOARD.md §2: the same web app, served by relayd on loopback or by us.
	// relayd embeds the built assets, so there is no second thing to install and
	// no static host to run — which is also why the console is plain TypeScript
	// rather than a framework needing a Node process at serve time.
	//
	// web.Mount registers "/" only, the lowest-priority pattern in Go's
	// ServeMux, so every route above still wins regardless of order. That is what
	// lets one listener serve both without either package knowing the other:
	// internal/web never imports internal/api.
	//
	// Before this, relayd printed the console's URL at startup and answered it
	// with 404 — advertising an address it did not serve.
	ui, err := web.Mount(mux, web.OptionsFromEnv())
	if err != nil {
		s.log.Warn("api: console not mounted", "error", err)
	} else {
		s.consoleUI = ui
	}

	return mux
}

// ------------------------------------------------------------------ auth --

// GenerateToken makes a 256-bit bearer token.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("api: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// The token is also accepted as ?token= because a browser cannot set headers on
// EventSource or WebSocket, and both of those are how the console watches live
// updates. That is safe here in a way a cookie would not be: nothing is sent
// automatically by the browser, so a cross-site request cannot authenticate
// itself. See auth.go for the authentication seam and the one authorization
// chokepoint every route goes through.

// ------------------------------------------------------------------ bind --

// ErrExposed is a non-loopback bind without the flag that admits to it.
var ErrExposed = errors.New("api: refusing to bind to a non-loopback address without --lan")

// CheckBind refuses to expose the API on a network by accident.
//
// DASHBOARD.md §4: the console can write to the vault, which makes it the
// highest-value target in the system, above the glasses and above relayd's own
// API. So loopback is the default, exposure is a flag, and a config file that
// quietly says 0.0.0.0 does not count as consent.
func CheckBind(listen string, lan bool) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("api: listen address %q: %w", listen, err)
	}
	if IsLoopback(host) {
		return nil
	}
	if lan {
		return nil
	}
	return fmt.Errorf(
		"%w: %s would let anyone who can reach this machine read your sessions and write to the credential vault. "+
			"Pass --lan if that is what you want", ErrExposed, listen)
}

// IsLoopback reports whether a host part binds only to this machine.
func IsLoopback(host string) bool {
	switch host {
	case "localhost":
		return true
	case "":
		// An empty host in "bind:port" means every interface.
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LANWarning is the sentence relayd prints when exposure is chosen on purpose.
func LANWarning(listen string) string {
	return fmt.Sprintf(
		"WARNING: relayd is listening on %s, not just this machine. "+
			"Anyone on this network who has the token can read every session and write to the credential vault.",
		listen)
}

// ---------------------------------------------------------------- serving --

// Serve runs the HTTP server until ctx is cancelled, then shuts it down.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE and WebSocket connections are long-lived by
		// design, and a write deadline would sever them on a schedule.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
		s.pings.Close()
		s.console.Close()
		return nil
	}
}

// Clients is how many phone sockets are attached.
func (s *Server) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clients
}

func (s *Server) addClient(d int) {
	s.mu.Lock()
	s.clients += d
	s.mu.Unlock()
}

// ------------------------------------------------------------- delivery --

// speaks is the voice backend's rule, and it lives here rather than in
// internal/bus on purpose.
//
// PRODUCT.md §6b: nothing in relayd decides *how* an event surfaces; rendering
// is a separate layer that today has one backend (voice) and tomorrow has two.
// bus.Ping therefore carries the two facts the ping policy established — did
// the moment it waited for arrive (Gap), do quiet hours apply (Quiet) — and
// this function is where one backend turns them into a decision. A v2 display
// backend reads the same two facts and reaches a different one: it shows the
// ping either way and uses Quiet only to withhold the chime.
//
// ADAPTERS.md §7 is preserved exactly. A blocking ping speaks whatever the
// clock says, because a session blocked at 3 a.m. is still blocked at 8 a.m. A
// completion speaks only if the gap it waited for arrived and quiet hours are
// not on — which is §7's "past the gap timeout the speech is dropped and the
// notification is not", and its "quiet hours hold the speech, not the
// notification", falling out of one expression instead of three booleans.
func speaks(p bus.Ping) bool {
	if p.Class == bus.ClassBlocking {
		return true
	}
	return p.Gap && !p.Quiet
}

// Deliver turns a ping into frames and fans them out to every transport.
//
// Nothing about ADAPTERS.md §7's policy is re-decided here — the Pinger already
// applied it and this function only renders. Notify is always set because a
// ping that reaches nobody is not a ping, Speak is set when [speaks] — the
// voice backend's rule, not the policy's — says this one is said out loud, and
// Confirm carries the options so the answer can come back by voice.
func (s *Server) Deliver(_ context.Context, p bus.Ping) error {
	out := Ping{Ping: p}

	out.Notify = &Notify{
		Title:    notifyTitle(p),
		Body:     p.Line,
		Sessions: p.Sessions,
		// Quiet hours produce a notification that arrives without a sound,
		// because holding it entirely would lose it.
		Silent: p.Quiet,
		Ping:   p.ID,
	}
	if speaks(p) && p.Line != "" {
		out.Speak = &Speak{
			Text: p.Line,
			// A blocked session may interrupt; a completion never does.
			Interrupt: p.Class == bus.ClassBlocking,
			Ping:      p.ID,
		}
		if len(p.Sessions) == 1 {
			out.Speak.Session = p.Sessions[0]
		}
	}

	if p.Class == bus.ClassBlocking && p.Ask != nil {
		s.mu.Lock()
		s.pending[p.ID] = p.Ask
		s.mu.Unlock()
		out.Confirm = confirmRequest(p)
	}

	s.pings.Publish(out)
	return nil
}

// Draw sends a mini-app's view to the phone.
//
// It returns when the frame has been published, not when a human has looked at
// it. There is no signal for the latter and inventing one would be an event we
// cannot observe.
//
// A box with no phone connected is [ErrNoScreen] rather than a silent success:
// the app is entitled to know its card went nowhere, because the alternative is
// an app that reports having shown you something it did not.
func (s *Server) Draw(_ context.Context, r UIRender) error {
	if s.Clients() == 0 {
		return ErrNoScreen
	}
	s.pings.Publish(Ping{Render: &r})
	return nil
}

// ErrNoScreen is a view with no phone to draw it on.
var ErrNoScreen = errors.New("api: no phone is connected, so there is nowhere to draw")

// DrawAndAsk sends a view containing a question and waits for the answer.
//
// The correlation lives here rather than in the caller because this is where
// [Server.answer] already looks: the phone replies with the same
// `consent.decision` frame it uses for a runtime's approval, naming the action
// id this hands it, and ws.go needs no case for mini-apps at all. An app's
// question participates in the same bookkeeping as every other question,
// including being cleared from the pending map when it is answered.
//
// False and nil is "no". False and [context.DeadlineExceeded] is nobody
// answering, which the caller converts to a no — this returns the difference
// because the *transport* did observe it, and flattening it here would leave the
// audit trail unable to say whether a question was declined or ignored.
func (s *Server) DrawAndAsk(ctx context.Context, r UIRender, deadline time.Time) (bool, error) {
	if s.Clients() == 0 {
		return false, ErrNoScreen
	}

	id := r.ActionID
	if id == "" {
		id = s.newID()
		r.ActionID = id
	}
	if !deadline.IsZero() {
		r.Deadline = deadline.UnixMilli()
	}

	answered := make(chan event.Reply, 1)
	ask := event.NewNeedsInput(
		event.Meta{At: s.now()},
		event.InputSpec{
			Ask: event.InputPermission,
			// The prompt is what something with no screen would read out. The
			// phone draws the view instead; this is for the audit log and for
			// anything that has to describe the question in words.
			Prompt:   "A mini-app is asking you something on your phone.",
			Deadline: deadline,
		},
		func(_ context.Context, reply event.Reply) error {
			select {
			case answered <- reply:
			default:
			}
			return nil
		})

	s.mu.Lock()
	s.pending[id] = ask
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	s.pings.Publish(Ping{Render: &r})

	select {
	case reply := <-answered:
		return reply.Decision == event.DecisionAllow, nil
	case <-ctx.Done():
		// The question outlived its welcome. Retract it, or the phone keeps a
		// button that no longer does anything and the user presses it believing
		// they answered.
		s.pings.Publish(Ping{Resolved: &ConfirmResolved{
			ActionID: id,
			Reason:   "the app stopped waiting for an answer",
		}})
		return false, ctx.Err()
	}
}

// Retract withdraws a confirm.request whose question is gone.
func (s *Server) Retract(_ context.Context, id, reason string) error {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()

	s.pings.Publish(Ping{Resolved: &ConfirmResolved{ActionID: id, Reason: reason}})
	return nil
}

func notifyTitle(p bus.Ping) string {
	if p.Class == bus.ClassBlocking {
		return "Waiting on you"
	}
	return "Relay"
}

func confirmRequest(p bus.Ping) *ConfirmRequest {
	ask := p.Ask
	m := ask.Envelope()
	c := &ConfirmRequest{
		ActionID:      p.ID,
		Session:       m.Session,
		Runtime:       m.Runtime,
		Ask:           string(ask.Ask),
		Prompt:        ask.Prompt,
		Consequential: p.Consequential,
		Repeat:        p.Repeat,
	}
	if !ask.Deadline.IsZero() {
		c.Deadline = ask.Deadline.UnixMilli()
	}
	if ask.Tool != nil {
		c.Tool = ask.Tool.Name
		c.Target = ask.Tool.Title
	}
	for _, o := range ask.Options {
		c.Options = append(c.Options, ConfirmOption{
			ID: o.ID, Name: o.Name, Kind: string(o.Kind), Standing: o.Kind.Standing(),
		})
	}
	return c
}

// answer resolves a pending confirm.request.
func (s *Server) answer(ctx context.Context, actionID string, r event.Reply) error {
	s.mu.Lock()
	ask, ok := s.pending[actionID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("api: no open question with id %s", actionID)
	}
	if err := ask.Reply(ctx, r); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.pending, actionID)
	s.mu.Unlock()
	if p := s.currentPinger(); p != nil {
		p.Heard(actionID)
	}
	return nil
}

// ---------------------------------------------------------------- helpers --

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, c, msg string) {
	writeJSON(w, code, ErrorPayload{Code: c, Message: msg})
}
