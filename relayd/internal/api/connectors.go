package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/audit"
	"github.com/luthor007/relay/relayd/internal/detect"
)

// DASHBOARD.md §3.4 — connectors and MCP.
//
// Two things joined on one screen, because ORCHESTRATOR.md §4b and MEMORY.md §7
// are the same surface seen from two sides: §4b is the grant ("every connector
// states what it opens", "revocable in one place, and visibly"), §7 is the
// plumbing that makes one revoke reach all five runtimes.
//
// Reading the runtimes' MCP configs is internal/detect's job and it already
// does it — five products, three file formats, and the rule that "no MCP
// servers" and "we could not tell" are different answers. This package consumes
// detect.MCPInventory and duplicates none of it.

// MCPSource returns MEMORY.md §7's reconciled union.
//
// A function, not a value, because reading it shells out to five runtimes and
// must not happen on every request. The caller decides how often to refresh and
// what to cache.
type MCPSource func(ctx context.Context) (detect.MCPInventory, error)

// ConnectorRevoker turns a connector off across all five runtimes.
//
// It returns what it reached rather than an error alone, because ORCHESTRATOR.md
// §4b's catch is that agents differ in how they re-read tool lists: a running
// session may not notice, and the orchestrator has to say which it did rather
// than leaving the user wondering why the thing they just revoked is still
// there.
type ConnectorRevoker interface {
	Revoke(ctx context.Context, connector string) (RevokeResult, error)
}

// RevokeResult is what a revoke actually reached.
type RevokeResult struct {
	// Runtimes is the per-runtime outcome, in adapter.Runtimes() order.
	Runtimes []RuntimeRevoke `json:"runtimes"`
	// Sessions is the live sessions that were restarted or re-announced, because
	// a grant change mid-session may not reach one that already enumerated its
	// tools.
	Sessions []string `json:"sessions,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// RuntimeRevoke is one runtime's answer.
type RuntimeRevoke struct {
	Runtime string `json:"runtime"`
	// Reached is false when this runtime's config could not be written. A revoke
	// that silently missed a runtime is worse than one that failed loudly.
	Reached bool   `json:"reached"`
	Reason  string `json:"reason,omitempty"`
}

// ConnectorView is one connector on the revocation screen.
type ConnectorView struct {
	ID        string `json:"id"`
	Connector string `json:"connector"`

	// Scopes are the raw scope strings and Opens is ORCHESTRATOR.md §4b's "what
	// it opens" — scope in the user's words, plus what it lets the agent do that
	// it could not before. A reason that restates the permission is not a reason.
	Scopes []string `json:"scopes"`
	Opens  []string `json:"opens"`

	GrantedAt  int64 `json:"granted_at"`
	LastUsedAt int64 `json:"last_used_at,omitempty"`
	// LastUsedFor is the most recent tool call attributable to this connector.
	// Derived by matching tool names against the connector, which is a heuristic
	// and is labelled as one rather than presented as an audit trail.
	LastUsedFor string `json:"last_used_for,omitempty"`

	Revoked   bool  `json:"revoked"`
	RevokedAt int64 `json:"revoked_at,omitempty"`

	// Unused is access nobody has touched. DASHBOARD.md §3.4: that is the kind
	// that gets forgotten and then exploited, so the row says so itself rather
	// than leaving the user to compare dates.
	Unused     bool   `json:"unused"`
	UnusedFor  string `json:"unused_for,omitempty"`
	UnusedDays int    `json:"unused_days,omitempty"`

	// Runtimes are the runtimes that can currently reach this connector, from
	// the MCP union.
	Runtimes []string `json:"runtimes,omitempty"`
}

// UnusedAfter is how long untouched access has to sit before the screen calls
// it out. DASHBOARD.md §3.4 says "a month" in as many words.
const UnusedAfter = 30 * 24 * time.Hour

// ConnectorList is the connectors screen.
type ConnectorList struct {
	Connectors []ConnectorView `json:"connectors"`
	MCP        MCPView         `json:"mcp"`
	Available  bool            `json:"available"`
	Note       string          `json:"note,omitempty"`
	At         int64           `json:"at"`
}

func (s *Server) handleConnectors(w http.ResponseWriter, r *http.Request) {
	out := ConnectorList{Connectors: []ConnectorView{}, At: s.now().UnixMilli()}

	inv, mcpNote := s.mcpView(r.Context())
	out.MCP = inv

	if s.db == nil {
		out.Note = "no store on this machine yet, so no connector grants have been recorded"
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Available = true
	if mcpNote != "" {
		out.Note = mcpNote
	}

	grants, err := s.listGrants(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}
	byConnector := map[string][]string{}
	for _, srv := range out.MCP.Servers {
		byConnector[strings.ToLower(srv.Name)] = srv.Runtimes
	}
	now := s.now()
	for i := range grants {
		g := &grants[i]
		g.Runtimes = byConnector[strings.ToLower(g.Connector)]
		s.markUnused(g, now)
	}
	out.Connectors = grants
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listGrants(ctx context.Context) ([]ConnectorView, error) {
	rows, err := s.db.SQL().QueryContext(ctx,
		`SELECT id, connector, scopes, granted_at, last_used_at, revoked_at
		 FROM "grant" ORDER BY revoked_at IS NOT NULL, granted_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConnectorView
	for rows.Next() {
		var v ConnectorView
		var scopes string
		var used, revoked *int64
		if err := rows.Scan(&v.ID, &v.Connector, &scopes, &v.GrantedAt, &used, &revoked); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(scopes), &v.Scopes); err != nil {
			v.Scopes = []string{}
		}
		if v.Scopes == nil {
			v.Scopes = []string{}
		}
		v.Opens = plainScopes(v.Connector, v.Scopes)
		if used != nil {
			v.LastUsedAt = *used
		}
		if revoked != nil {
			v.RevokedAt, v.Revoked = *revoked, true
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		if tool, at, ok := s.lastToolFor(ctx, out[i].Connector); ok {
			out[i].LastUsedFor = tool
			if out[i].LastUsedAt == 0 {
				out[i].LastUsedAt = at
			}
		}
	}
	return out, nil
}

// lastToolFor finds the most recent tool call attributable to a connector.
//
// The match is on the tool name containing the connector's, which is how MCP
// tools are actually named across the five runtimes (`mcp__gmail__send`,
// `gmail.send`, `Gmail:send`). It is a heuristic and the field it fills is
// documented as one; the alternative — showing nothing — makes "last used and
// for what" unanswerable, which is the whole point of the screen.
func (s *Server) lastToolFor(ctx context.Context, connector string) (string, int64, bool) {
	if connector == "" {
		return "", 0, false
	}
	var tool, target string
	var at int64
	err := s.db.SQL().QueryRowContext(ctx,
		`SELECT tool, target, at FROM tool_call
		 WHERE lower(tool) LIKE ? ORDER BY at DESC LIMIT 1`,
		"%"+strings.ToLower(connector)+"%").Scan(&tool, &target, &at)
	if err != nil {
		return "", 0, false
	}
	if target != "" {
		return tool + " (" + target + ")", at, true
	}
	return tool, at, true
}

func (s *Server) markUnused(v *ConnectorView, now time.Time) {
	if v.Revoked {
		return
	}
	if v.LastUsedAt == 0 {
		v.Unused = true
		v.UnusedFor = "never used since it was granted"
		if v.GrantedAt > 0 {
			v.UnusedDays = int(now.Sub(time.UnixMilli(v.GrantedAt)) / (24 * time.Hour))
		}
		return
	}
	idle := now.Sub(time.UnixMilli(v.LastUsedAt))
	if idle >= UnusedAfter {
		v.Unused = true
		v.UnusedDays = int(idle / (24 * time.Hour))
		v.UnusedFor = fmt.Sprintf("not used for %d days", v.UnusedDays)
	}
}

// plainScopes renders a scope list in the user's words.
//
// ORCHESTRATOR.md §4b: every connector states what it opens — scope, plus what
// it lets the agent do that it could not before — and read and write are
// separate grants because reading a calendar is not sending invitations. A
// scope we have no phrasing for is passed through verbatim rather than dressed
// up, because inventing a friendly sentence for a permission we do not
// understand is how a consent sheet becomes a lie.
func plainScopes(connector string, scopes []string) []string {
	out := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		out = append(out, phraseScope(connector, sc))
	}
	return out
}

func phraseScope(connector, scope string) string {
	s := strings.ToLower(scope)
	noun := connector
	if noun == "" {
		noun = "this connector"
	}
	switch {
	case strings.HasSuffix(s, ".readonly"), strings.HasSuffix(s, ":read"),
		strings.HasSuffix(s, ".read"), s == "read":
		return "Read " + noun + ". It can see what is there and cannot change it."
	case strings.HasSuffix(s, ".send"), strings.Contains(s, "send"):
		return "Send from " + noun + " as you. This is the half that leaves the machine."
	case strings.HasSuffix(s, ":write"), strings.HasSuffix(s, ".write"), s == "write",
		strings.Contains(s, "modify"):
		return "Change things in " + noun + ", not only read them."
	case strings.Contains(s, "admin"), strings.Contains(s, "full"):
		return "Full control of " + noun + ", including anything added later."
	}
	return scope
}

// ------------------------------------------------- connector proposals --

// ConnectorProposals is ORCHESTRATOR.md §4b's suggestion queue.
//
// It is NOT [ProposalStore], which is MEMORY.md §6's credential queue. The two
// read alike and are entirely different things: that one asks whether a key
// found in a transcript should be saved, this one asks whether a service the
// user keeps talking about should be connected.
//
// Three methods and no fourth, and the missing one is the point. There is no
// "propose" here: a proposal comes from observed evidence inside the daemon,
// and an HTTP surface that could manufacture one would make §4b's "grounded in
// something observed, not guessed" a convention rather than a mechanism.
type ConnectorProposals interface {
	Proposals(ctx context.Context) ([]ConnectorProposal, error)
	// Accept grants the READ half and nothing else. The implementation, not the
	// caller, chooses the half — see [Server.handleAcceptConnectorProposal].
	Accept(ctx context.Context, connector, by string) (ConnectorGrantResult, error)
	Dismiss(ctx context.Context, connector, reason string) error
}

// ErrNotProposed is an accept for something that was never suggested.
//
// It is exported because the implementation lives in the composition root and
// this is the door §4b rule 1 would otherwise leak through: without it, POST
// /v1/connectors/proposals/gmail/accept would be a general grant endpoint with
// a longer path. It maps to 404 rather than 500 — "there is no such offer" is
// the truth, not a fault.
var ErrNotProposed = errors.New("that connector has not been proposed on this machine")

// ConnectorGrantResult is what saying yes actually did.
type ConnectorGrantResult struct {
	ID        string   `json:"id"`
	Connector string   `json:"connector"`
	Scopes    []string `json:"scopes"`
	GrantedAt int64    `json:"granted_at"`

	// Sessions and Note are §4b's catch, carried rather than hidden: some
	// runtimes enumerate their tools once per session, so a grant made now may
	// not reach an agent that is already running. Saying which sessions were
	// told beats leaving the user wondering why the thing they just connected
	// is invisible.
	Sessions []string `json:"sessions,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// ConnectorProposalList is the proposals half of the connectors screen.
type ConnectorProposalList struct {
	Proposals []ConnectorProposal `json:"proposals"`
	// Available is false when nothing on this machine can propose anything —
	// no connectors are configured, or the daemon has no proposer. Note then
	// says which, because an empty list with no explanation reads as "you have
	// nothing to connect" when it means "Relay cannot tell".
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
	At        int64  `json:"at"`
}

const noConnectorProposalsNote = "no connectors are configured on this machine, so there " +
	"is nothing to propose however often you mention one"

func (s *Server) handleConnectorProposals(w http.ResponseWriter, r *http.Request) {
	out := ConnectorProposalList{Proposals: []ConnectorProposal{}, At: s.now().UnixMilli()}
	if s.connectorProposals == nil {
		out.Note = noConnectorProposalsNote
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Available = true
	list, err := s.connectorProposals.Proposals(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}
	if list != nil {
		out.Proposals = list
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAcceptConnectorProposal is §4b rule 1 and rule 2 at the same door.
//
// Rule 1 — nothing is auto-granted — holds because this is the only path from a
// proposal to a grant and it is a POST from a human holding the vault scope.
// The proposer itself has no reference to anything that can grant.
//
// Rule 2 — read and write are separate grants, and the write half costs a
// second decision — holds because THE HALF IS NOT AN INPUT. There is no access
// field in the body and no query parameter; the implementation hard-codes read.
// An endpoint that took the half as an argument would put the write grant one
// keystroke from the read grant, which is the same click wearing a parameter.
// The second decision is the connectors screen, which grants deliberately.
func (s *Server) handleAcceptConnectorProposal(w http.ResponseWriter, r *http.Request) {
	// The body is read and discarded. Reading it keeps the surface uniform with
	// the credential proposals; discarding it is the guarantee above.
	_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, maxBodyBytes))

	id, _ := IdentityFrom(r.Context())
	name := strings.ToLower(strings.TrimSpace(r.PathValue("connector")))

	if s.connectorProposals == nil {
		s.recordRefusal(r.Context(), id, audit.ActionConnectorProposalAccept, name,
			"no connectors are configured on this machine, so there is nothing to accept")
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, noConnectorProposalsNote)
		return
	}

	var res ConnectorGrantResult
	err := s.audited(r.Context(), auditRequest{
		kind: ConsoleConnector, action: audit.ActionConnectorProposalAccept,
		identity: id, target: name, service: name,
	}, func() (map[string]string, error) {
		var err error
		res, err = s.connectorProposals.Accept(r.Context(), name, "console")
		if err != nil {
			return nil, err
		}
		return map[string]string{
			"connector": res.Connector,
			"scopes":    strings.Join(res.Scopes, ","),
			"grant":     res.ID,
		}, nil
	})
	if err != nil {
		if errors.Is(err, ErrNotProposed) {
			writeErr(w, http.StatusNotFound, CodeNotFound, err.Error())
			return
		}
		s.writeVaultErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"grant": res})
}

func (s *Server) handleDismissConnectorProposal(w http.ResponseWriter, r *http.Request) {
	var req proposalRequest
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req)

	id, _ := IdentityFrom(r.Context())
	name := strings.ToLower(strings.TrimSpace(r.PathValue("connector")))

	if s.connectorProposals == nil {
		s.recordRefusal(r.Context(), id, audit.ActionConnectorProposalDismiss, name,
			"no connectors are configured on this machine, so there is nothing to dismiss")
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, noConnectorProposalsNote)
		return
	}
	err := s.audited(r.Context(), auditRequest{
		kind: ConsoleConnector, action: audit.ActionConnectorProposalDismiss,
		identity: id, target: name, service: name,
	}, func() (map[string]string, error) {
		if err := s.connectorProposals.Dismiss(r.Context(), name, req.Reason); err != nil {
			return nil, err
		}
		return map[string]string{"connector": name, "reason": req.Reason}, nil
	})
	if err != nil {
		s.writeVaultErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ----------------------------------------------------------------- revoke --

func (s *Server) handleRevokeConnector(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	grantID := r.PathValue("id")

	if s.db == nil {
		s.recordRefusal(r.Context(), id, audit.ActionConnectorRevoke, grantID,
			"no store on this machine, so there is no grant to revoke")
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable,
			"no store on this machine, so there is no grant to revoke")
		return
	}

	var connector string
	var result RevokeResult
	err := s.audited(r.Context(), auditRequest{
		kind: ConsoleConnector, action: audit.ActionConnectorRevoke,
		identity: id, target: grantID,
	}, func() (map[string]string, error) {
		row := s.db.SQL().QueryRowContext(r.Context(),
			`SELECT connector FROM "grant" WHERE id = ?`, grantID)
		if err := row.Scan(&connector); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, errNoSuchGrant
			}
			return nil, err
		}

		// The grant row goes first. If reaching the runtimes fails half way, the
		// state of record already says revoked — the failure mode to avoid is a
		// connector that our own table still calls live.
		if _, err := s.db.SQL().ExecContext(r.Context(),
			`UPDATE "grant" SET revoked_at = ? WHERE id = ?`,
			s.now().UnixMilli(), grantID); err != nil {
			return nil, err
		}

		if s.connectors == nil {
			result = unreachedRevoke(
				"nothing is wired to rewrite the runtimes' MCP configuration on this " +
					"machine, so this grant is marked revoked here and the runtimes have " +
					"not been told")
			return map[string]string{"connector": connector, "reached": "none"}, nil
		}
		var err error
		result, err = s.connectors.Revoke(r.Context(), connector)
		if err != nil {
			return nil, err
		}
		return map[string]string{
			"connector": connector,
			"reached":   strings.Join(reachedRuntimes(result), ","),
		}, nil
	})
	if err != nil {
		if errors.Is(err, errNoSuchGrant) {
			writeErr(w, http.StatusNotFound, CodeNotFound, "no such connector grant")
			return
		}
		s.writeVaultErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connector": connector,
		"revoke":    result,
	})
}

var errNoSuchGrant = fmt.Errorf("api: no such connector grant")

func unreachedRevoke(note string) RevokeResult {
	res := RevokeResult{Note: note}
	for _, rt := range adapter.Runtimes() {
		res.Runtimes = append(res.Runtimes, RuntimeRevoke{
			Runtime: string(rt), Reached: false, Reason: note,
		})
	}
	return res
}

func reachedRuntimes(r RevokeResult) []string {
	var out []string
	for _, rt := range r.Runtimes {
		if rt.Reached {
			out = append(out, rt.Runtime)
		}
	}
	return out
}

// -------------------------------------------------------------------- MCP --

// MCPView is MEMORY.md §7's union, on the wire.
type MCPView struct {
	// Headline is §7 step 3, verbatim in shape: "you have 7 MCP servers across
	// 3 tools. Manage them in one place?"
	Headline string         `json:"headline"`
	Servers  []MCPServerRow `json:"servers"`
	// Origins says where each runtime's list came from, and Unreadable is the
	// distinction §7 insists on: "no MCP servers" and "we could not read this
	// runtime's config" lead to opposite decisions and only one is recoverable.
	Origins    []MCPOriginRow `json:"origins"`
	Unreadable []string       `json:"unreadable,omitempty"`
	// Probed is false when no reconciliation has run. The screen then says so
	// rather than rendering an empty registry as "you have none".
	Probed bool `json:"probed"`
}

// MCPServerRow is one server in the union.
type MCPServerRow struct {
	Name      string   `json:"name"`
	Display   string   `json:"display"`
	Transport string   `json:"transport,omitempty"`
	URL       string   `json:"url,omitempty"`
	Runtimes  []string `json:"runtimes"`
	// Names is what each runtime calls it. The same server is frequently named
	// three different things and the console has to show all three, or the user
	// cannot tell which row is theirs.
	Names  map[string]string `json:"names,omitempty"`
	Shared bool              `json:"shared"`
}

// MCPOriginRow is one runtime's source.
type MCPOriginRow struct {
	Runtime  string `json:"runtime"`
	Origin   string `json:"origin,omitempty"`
	File     string `json:"file,omitempty"`
	Readable bool   `json:"readable"`
	Reason   string `json:"reason,omitempty"`
	Servers  int    `json:"servers"`
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	view, note := s.mcpView(r.Context())
	out := map[string]any{"mcp": view, "at": s.now().UnixMilli()}
	if note != "" {
		out["note"] = note
	}
	writeJSON(w, http.StatusOK, out)
}

const noMCPNote = "no MCP reconciliation has run on this machine yet, so this is not " +
	"a claim that there are none"

func (s *Server) mcpView(ctx context.Context) (MCPView, string) {
	if s.mcp == nil {
		return MCPView{Headline: noMCPNote, Servers: []MCPServerRow{}, Origins: []MCPOriginRow{}}, noMCPNote
	}
	inv, err := s.mcp(ctx)
	if err != nil {
		return MCPView{
			Headline: "MCP reconciliation failed: " + err.Error(),
			Servers:  []MCPServerRow{}, Origins: []MCPOriginRow{},
		}, err.Error()
	}

	view := MCPView{
		Probed:   true,
		Headline: inv.Headline(),
		Servers:  make([]MCPServerRow, 0, len(inv.Servers)),
		Origins:  make([]MCPOriginRow, 0, len(inv.Origins)),
	}
	for _, e := range inv.Servers {
		row := MCPServerRow{
			Name:      e.Name,
			Display:   e.Display(),
			Transport: e.Transport,
			URL:       e.URL,
			Shared:    e.Shared(),
			Names:     map[string]string{},
		}
		for _, rt := range e.Runtimes {
			row.Runtimes = append(row.Runtimes, string(rt))
		}
		sort.Strings(row.Runtimes)
		for rt, name := range e.Names {
			row.Names[string(rt)] = name
		}
		view.Servers = append(view.Servers, row)
	}
	for _, o := range inv.Origins {
		view.Origins = append(view.Origins, MCPOriginRow{
			Runtime:  string(o.Runtime),
			Origin:   o.Origin,
			File:     o.FromFile,
			Readable: o.Readable,
			Reason:   o.Reason,
			Servers:  len(o.Servers),
		})
	}
	for _, o := range inv.Unreadable() {
		view.Unreadable = append(view.Unreadable, string(o.Runtime)+": "+o.Reason)
	}
	return view, ""
}

// ------------------------------------------------------- marker proposals --

// markerProposals serves the credential proposal list from the index's secret
// markers when no proposal queue is wired.
//
// The markers are the raw material a proposal is made of — MEMORY.md §6's
// detection step writes one per finding, before indexing — so this is a real
// list rather than an empty one. Tier is carried through in the detector string
// because §12.2 measured a 26% false-positive rate on tier 2, and a console that
// presents both tiers as equally likely is misleading the person deciding.
func (s *Server) markerProposals(ctx context.Context) ([]Proposal, string, error) {
	out := []Proposal{}
	if s.db == nil {
		return out, "no index on this machine yet, so nothing has been scanned for credentials", nil
	}
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT id, runtime, session_id, path, byte_offset, detector, service, at
		FROM secret_marker WHERE vault_id = '' ORDER BY at DESC LIMIT 200`)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	for rows.Next() {
		var p Proposal
		var at int64
		if err := rows.Scan(&p.ID, &p.Runtime, &p.Session, &p.Path,
			&p.ByteOffset, &p.Detector, &p.Service, &at); err != nil {
			return nil, "", err
		}
		p.FoundAt = at
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	note := ""
	if len(out) > 0 {
		note = "these are detections from the index, not yet offered as proposals — " +
			"accepting one has to re-read the transcript at its byte offset"
	}
	return out, note, nil
}
