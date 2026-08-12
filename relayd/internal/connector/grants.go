package connector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/audit"
	"github.com/luthor007/relay/relayd/internal/mcp"
	"github.com/luthor007/relay/relayd/internal/store"
)

// Grants is ORCHESTRATOR.md §4b rules 1 and 2, in code.
//
//	1. Nothing is auto-granted. Not on install, not on suggestion, not ever.
//	   A proposal is a proposal.
//	2. Read and write are separate grants. Reading a calendar is not sending
//	   invitations; reading Gmail is not sending mail as you. Most useful
//	   connectors only need the read half, and the write half should cost a
//	   second decision.
//
// Rule 1 is held by [GrantRequest.Decided]: [Grants.Grant] refuses a request
// that does not carry an explicit human decision, and there is no other way
// into the store from this package. [Proposer] has no reference to a Grants at
// all, so the suggestion path physically cannot grant.
//
// Rule 2 is held by the signature: Grant takes one [mcp.Access], not a set. Two
// halves is two calls, two audit entries and two decisions.

// Grant is one connector's access, as recorded.
type Grant struct {
	ID         string
	Connector  string
	Scopes     []string
	GrantedAt  time.Time
	LastUsedAt time.Time
	RevokedAt  time.Time
}

// Live reports whether this grant is still in force.
func (g Grant) Live() bool { return g.RevokedAt.IsZero() }

// Has reports whether this grant carries one half of its connector.
func (g Grant) Has(a mcp.Access) bool {
	want := a.Scope(strings.ToLower(g.Connector))
	for _, s := range g.Scopes {
		if strings.EqualFold(strings.TrimSpace(s), want) {
			return true
		}
	}
	return false
}

// Store is where grants live.
type Store interface {
	// List returns every grant, revoked ones included — the console shows them.
	List(ctx context.Context) ([]Grant, error)
	// Live returns the grants still in force for one connector.
	Live(ctx context.Context, connector string) ([]Grant, error)
	// Put writes one grant.
	Put(ctx context.Context, g Grant) error
	// Revoke marks every live grant for a connector revoked and returns their
	// ids. Revoking something already revoked is not an error.
	Revoke(ctx context.Context, connector string, at time.Time) ([]string, error)
}

// Errors from the grant path.
var (
	// ErrNotDecided is a grant nobody agreed to. It is the shape rule 1 takes
	// at the only door into the store.
	ErrNotDecided = errors.New("connector: a grant needs an explicit decision from the person using this machine")
	// ErrNoAudit is a grant with nowhere to record it. It is refused for the
	// same reason internal/api refuses an unrecordable vault write: an
	// unrecorded grant is exactly what the audit trail exists to make
	// impossible.
	ErrNoAudit = errors.New("connector: no audit log, so this grant cannot be recorded and will not be made")
	// ErrNoSuchHalf is a half the connector never said it opens.
	ErrNoSuchHalf = errors.New("connector: this connector does not open that half")
)

// GrantRequest is one decision.
type GrantRequest struct {
	Connector string
	Access    mcp.Access

	// Decided is the human decision. False is refused — not defaulted, not
	// inferred from context, not implied by having asked.
	Decided bool

	// By is the surface the person used: console | glasses | phone | installer.
	// It goes in the audit line, because "granted from the console" and
	// "granted by the installer" are different stories about the same row.
	By string

	// Opens is what the person was told this half lets the agent do. It is
	// recorded with the grant so the trail says what was agreed to, rather than
	// what today's copy in the code happens to say.
	Opens string

	// From names the proposal this decision answers, when there was one. It is
	// evidence, not authority: a proposal id does not make Decided true.
	From string
}

// Grants is the grant store plus its audit trail and its refresh path.
type Grants struct {
	Store Store
	// Audit is required. See ErrNoAudit.
	Audit audit.Log
	// Refresher tells running sessions the tool list moved. Nil means the
	// result says nobody was told, which is still better than implying they
	// were.
	Refresher Refresher

	Now   func() time.Time
	NewID func() string
	Log   *slog.Logger
}

// Refresher is the gateway's tool-list refresh, as this package needs it.
// *mcp.Gateway implements it.
type Refresher interface {
	Refresh(ctx context.Context, reason string) mcp.RefreshResult
}

// NewGrants builds a grant store with the usual defaults.
func NewGrants(s Store, a audit.Log) *Grants { return &Grants{Store: s, Audit: a} }

func (g *Grants) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Grants) newID() string {
	if g.NewID != nil {
		return g.NewID()
	}
	return uuid.NewString()
}

func (g *Grants) log() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.Default()
}

// Allowed implements [mcp.Grants]. It is the check every tool call goes
// through, and it reads the store rather than a cache on purpose: the console
// revokes by writing the row directly (internal/api's handleRevokeConnector),
// and a cache here would keep a revoked connector alive until something
// invalidated it. "One revoke, immediately" is worth a local SQLite read.
func (g *Grants) Allowed(ctx context.Context, connector string, access mcp.Access) (bool, string) {
	if g == nil || g.Store == nil {
		return false, "no grant store on this machine, so nothing is connected"
	}
	if !access.Valid() {
		return false, "unknown access half " + string(access)
	}
	live, err := g.Store.Live(ctx, connector)
	if err != nil {
		// A store that cannot answer is not a store that says yes.
		g.log().Warn("connector: could not read grants", "connector", connector, "err", err)
		return false, "Relay could not read its grant list, so it is refusing rather than guessing"
	}
	for _, gr := range live {
		if gr.Has(access) {
			return true, ""
		}
	}
	if len(live) > 0 {
		return false, fmt.Sprintf("%s is connected for %s only — the %s half is a separate decision",
			connector, otherHalf(access), access)
	}
	return false, connector + " is not connected"
}

func otherHalf(a mcp.Access) mcp.Access {
	if a == mcp.AccessRead {
		return mcp.AccessWrite
	}
	return mcp.AccessRead
}

// Grant records one half of one connector, having first refused everything that
// is not a decision.
func (g *Grants) Grant(ctx context.Context, req GrantRequest) (Grant, RefreshResult, error) {
	var out Grant
	var refresh RefreshResult

	name := strings.ToLower(strings.TrimSpace(req.Connector))
	if name == "" {
		return out, refresh, ErrNoConnector
	}
	if !req.Access.Valid() {
		return out, refresh, fmt.Errorf("%w: %q", ErrNoSuchHalf, req.Access)
	}
	if !req.Decided {
		return out, refresh, ErrNotDecided
	}
	if g.Store == nil {
		return out, refresh, errors.New("connector: no grant store")
	}
	if g.Audit == nil {
		return out, refresh, ErrNoAudit
	}

	att, err := audit.Begin(ctx, g.Audit, audit.Entry{
		Actor:   audit.Actor{Kind: actorKind(req.By)},
		Action:  audit.ActionConnectorGrant,
		Target:  name,
		Service: name,
		Detail: map[string]string{
			"access": string(req.Access),
			"scope":  req.Access.Scope(name),
			"opens":  req.Opens,
			"from":   req.From,
		},
	})
	if err != nil {
		return out, refresh, fmt.Errorf("%w: %w", ErrNoAudit, err)
	}

	now := g.now()
	// One live grant row per connector, whose scope list grows a half at a
	// time. internal/api's connectors screen renders one row per grant, and a
	// second row for the same connector would read as a second connection.
	live, err := g.Store.Live(ctx, name)
	if err != nil {
		_ = att.Fail(ctx, err)
		return out, refresh, err
	}
	if len(live) > 0 {
		out = live[0]
		if out.Has(req.Access) {
			// Already granted. Not an error, and not a second audit outcome
			// that says something changed.
			g.finish(ctx, att.OK(ctx, map[string]string{"already": "true"}))
			return out, refresh, nil
		}
		out.Scopes = append(out.Scopes, req.Access.Scope(name))
	} else {
		out = Grant{
			ID:        g.newID(),
			Connector: name,
			Scopes:    []string{req.Access.Scope(name)},
			GrantedAt: now,
		}
	}
	sort.Strings(out.Scopes)

	if err := g.Store.Put(ctx, out); err != nil {
		g.finish(ctx, att.Fail(ctx, err))
		return Grant{}, refresh, err
	}
	g.finish(ctx, att.OK(ctx, map[string]string{"scope": req.Access.Scope(name)}))

	refresh = g.refresh(ctx, fmt.Sprintf("%s is connected (%s)", name, req.Access))
	return out, refresh, nil
}

// RevokeResult is what a revoke reached.
//
// It mirrors internal/api's RevokeResult field for field so M3 can adapt it
// with a copy; this package does not import internal/api, because the console
// package importing this one is the direction that has to stay open.
type RevokeResult struct {
	Connector string          `json:"connector"`
	Runtimes  []RuntimeRevoke `json:"runtimes"`
	Sessions  []string        `json:"sessions,omitempty"`
	Note      string          `json:"note,omitempty"`
	// Refresh is the per-session detail behind Sessions.
	Refresh mcp.RefreshResult `json:"refresh"`
}

// RuntimeRevoke is one runtime's answer.
type RuntimeRevoke struct {
	Runtime string `json:"runtime"`
	Reached bool   `json:"reached"`
	Reason  string `json:"reason,omitempty"`
}

// RefreshResult is re-exported so callers of this package do not have to import
// internal/mcp to read a grant's return value.
type RefreshResult = mcp.RefreshResult

// Revoke turns a connector off everywhere.
//
// Every runtime is reported reached, and that is a claim this design can
// actually make: the runtimes are pointed at one gateway, so the enforcement
// point is [Grants.Allowed] rather than five configuration files.
// ORCHESTRATOR.md §4b's "one revoke — turning Gmail off turns it off for all
// five, immediately, without hunting through five config files" is a
// consequence of the shared bus, not a loop over runtimes.
//
// What is *not* immediate is a session that already enumerated its tools, and
// that is what Refresh reports rather than glossing.
func (g *Grants) Revoke(ctx context.Context, connector string) (RevokeResult, error) {
	name := strings.ToLower(strings.TrimSpace(connector))
	res := RevokeResult{Connector: name}
	if name == "" {
		return res, ErrNoConnector
	}
	if g.Store == nil {
		return res, errors.New("connector: no grant store")
	}
	if g.Audit == nil {
		return res, ErrNoAudit
	}

	att, err := audit.Begin(ctx, g.Audit, audit.Entry{
		Actor:   audit.Actor{Kind: "console"},
		Action:  audit.ActionConnectorRevoke,
		Target:  name,
		Service: name,
	})
	if err != nil {
		return res, fmt.Errorf("%w: %w", ErrNoAudit, err)
	}

	ids, err := g.Store.Revoke(ctx, name, g.now())
	if err != nil {
		g.finish(ctx, att.Fail(ctx, err))
		return res, err
	}
	g.finish(ctx, att.OK(ctx, map[string]string{"grants": fmt.Sprint(len(ids))}))

	for _, rt := range adapter.Runtimes() {
		res.Runtimes = append(res.Runtimes, RuntimeRevoke{
			Runtime: string(rt), Reached: true,
			Reason: "every runtime calls this connector through Relay's gateway, and the " +
				"gateway now refuses it",
		})
	}
	res.Refresh = g.refresh(ctx, name+" was disconnected")
	for _, s := range res.Refresh.Sessions {
		res.Sessions = append(res.Sessions, s.Session)
	}
	res.Note = res.Refresh.Note
	return res, nil
}

// finish reports an outcome that could not be written. The mutation already
// happened, so the trail is left holding an attempt with no outcome — which
// internal/audit documents as "a mutation that never finished" and the console
// renders as one. Swallowing the error silently would turn that visible gap
// into an invisible one.
func (g *Grants) finish(_ context.Context, err error) {
	if err != nil {
		g.log().Warn("connector: could not close an audit entry; the trail will show an unfinished mutation", "err", err)
	}
}

func (g *Grants) refresh(ctx context.Context, reason string) RefreshResult {
	if g.Refresher == nil {
		return RefreshResult{
			Reason: reason,
			Note: "No agent sessions were told: nothing is wired to notify them on this " +
				"machine, so a session that is already running will not see the change " +
				"until it restarts.",
		}
	}
	return g.Refresher.Refresh(ctx, reason)
}

// actorKind maps the surface a decision came from onto audit.Actor.Kind, which
// is a closed vocabulary in that package.
func actorKind(by string) string {
	switch strings.ToLower(strings.TrimSpace(by)) {
	case "console", "phone", "installer", "cloud", "runtime", "orchestrator":
		return strings.ToLower(strings.TrimSpace(by))
	case "glasses":
		// The glasses speak through the phone; there is no separate door.
		return "phone"
	default:
		return "console"
	}
}

// List returns every grant, for the console.
func (g *Grants) List(ctx context.Context) ([]Grant, error) {
	if g.Store == nil {
		return nil, nil
	}
	return g.Store.List(ctx)
}

var _ mcp.Grants = (*Grants)(nil)

// ------------------------------------------------------------ SQL backing --

// SQLStore is the `grant` table, which internal/api's connectors screen already
// reads. Writing anywhere else would give the console two sources of truth.
type SQLStore struct{ DB *store.DB }

// NewSQLStore builds a store on the main database.
func NewSQLStore(db *store.DB) *SQLStore { return &SQLStore{DB: db} }

func (s *SQLStore) List(ctx context.Context) ([]Grant, error) {
	rows, err := s.DB.SQL().QueryContext(ctx,
		`SELECT id, connector, scopes, granted_at, last_used_at, revoked_at
		 FROM "grant" ORDER BY granted_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGrants(rows)
}

func (s *SQLStore) Live(ctx context.Context, connector string) ([]Grant, error) {
	rows, err := s.DB.SQL().QueryContext(ctx,
		`SELECT id, connector, scopes, granted_at, last_used_at, revoked_at
		 FROM "grant" WHERE connector = ? AND revoked_at IS NULL
		 ORDER BY granted_at DESC`, strings.ToLower(connector))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGrants(rows)
}

func (s *SQLStore) Put(ctx context.Context, g Grant) error {
	scopes, err := json.Marshal(g.Scopes)
	if err != nil {
		return err
	}
	_, err = s.DB.SQL().ExecContext(ctx,
		`INSERT INTO "grant" (id, connector, scopes, granted_at, last_used_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET scopes = excluded.scopes, revoked_at = excluded.revoked_at`,
		g.ID, strings.ToLower(g.Connector), string(scopes),
		g.GrantedAt.UnixMilli(), nullMS(g.LastUsedAt), nullMS(g.RevokedAt))
	return err
}

func (s *SQLStore) Revoke(ctx context.Context, connector string, at time.Time) ([]string, error) {
	rows, err := s.DB.SQL().QueryContext(ctx,
		`SELECT id FROM "grant" WHERE connector = ? AND revoked_at IS NULL`,
		strings.ToLower(connector))
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if len(ids) == 0 {
		return nil, nil
	}
	_, err = s.DB.SQL().ExecContext(ctx,
		`UPDATE "grant" SET revoked_at = ? WHERE connector = ? AND revoked_at IS NULL`,
		at.UnixMilli(), strings.ToLower(connector))
	return ids, err
}

func scanGrants(rows *sql.Rows) ([]Grant, error) {
	var out []Grant
	for rows.Next() {
		var g Grant
		var scopes string
		var granted int64
		var used, revoked *int64
		if err := rows.Scan(&g.ID, &g.Connector, &scopes, &granted, &used, &revoked); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(scopes), &g.Scopes); err != nil {
			g.Scopes = nil
		}
		g.GrantedAt = time.UnixMilli(granted).UTC()
		if used != nil {
			g.LastUsedAt = time.UnixMilli(*used).UTC()
		}
		if revoked != nil {
			g.RevokedAt = time.UnixMilli(*revoked).UTC()
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func nullMS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// --------------------------------------------------------- memory backing --

// MemoryStore keeps grants in memory. Same reason audit.Memory exists: a box
// with no writable data directory still has to make the grant path work, and
// the console can say the grants are not durable rather than implying they are.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[string]Grant
}

// NewMemoryStore builds an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{rows: map[string]Grant{}} }

func (m *MemoryStore) List(context.Context) ([]Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Grant, 0, len(m.rows))
	for _, g := range m.rows {
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].GrantedAt.After(out[j].GrantedAt) })
	return out, nil
}

func (m *MemoryStore) Live(_ context.Context, connector string) ([]Grant, error) {
	name := strings.ToLower(strings.TrimSpace(connector))
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Grant
	for _, g := range m.rows {
		if strings.ToLower(g.Connector) == name && g.Live() {
			out = append(out, g)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].GrantedAt.After(out[j].GrantedAt) })
	return out, nil
}

func (m *MemoryStore) Put(_ context.Context, g Grant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]Grant{}
	}
	g.Connector = strings.ToLower(g.Connector)
	m.rows[g.ID] = g
	return nil
}

func (m *MemoryStore) Revoke(_ context.Context, connector string, at time.Time) ([]string, error) {
	name := strings.ToLower(strings.TrimSpace(connector))
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, g := range m.rows {
		if strings.ToLower(g.Connector) == name && g.Live() {
			g.RevokedAt = at
			m.rows[id] = g
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
