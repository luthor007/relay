package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/luthor007/relay/relayd/internal/audit"
)

// DASHBOARD.md §4: "Every credential and connector mutation is written to an
// audit log the console itself displays. If something reads keys it should not,
// the evidence exists in a place the user can see without our help."
//
// The second half of that sentence is why this endpoint reads and never writes.
// Entries are appended by the mutation handlers, before the mutation runs; the
// console can only look.

// AuditList is the audit screen.
type AuditList struct {
	Entries []AuditEntry `json:"entries"`
	// Durable is false on a machine with no writable data directory. The console
	// shows it, because an empty list from a memory log after a restart looks
	// exactly like "nothing has happened".
	Durable bool   `json:"durable"`
	Path    string `json:"path,omitempty"`
	// Intact is the hash chain verifying. A false here means a line was edited
	// or removed, which is worth more than every entry in the list.
	Intact bool   `json:"intact"`
	Broken string `json:"broken,omitempty"`
	// Pending is the attempts with no outcome — a mutation that started and did
	// not finish, which is the shape of an interrupted or crashed write.
	Pending int   `json:"pending"`
	At      int64 `json:"at"`
}

// AuditEntry is one line, on the wire.
type AuditEntry struct {
	ID     string `json:"id"`
	At     int64  `json:"at"`
	Seq    int64  `json:"seq"`
	Action string `json:"action"`
	// Who, and from where.
	Actor   string `json:"actor"`
	ActorID string `json:"actor_id,omitempty"`
	From    string `json:"from,omitempty"`
	Agent   string `json:"agent,omitempty"`

	Target  string `json:"target,omitempty"`
	Service string `json:"service,omitempty"`

	Outcome string            `json:"outcome"`
	Reason  string            `json:"reason,omitempty"`
	Detail  map[string]string `json:"detail,omitempty"`
	// Attempt links an outcome back to its attempt so the console can pair them
	// into one row and show how long the write took.
	Attempt string `json:"attempt,omitempty"`
}

func auditEntry(e audit.Entry) AuditEntry {
	return AuditEntry{
		ID: e.ID, At: msOrZero(e.At), Seq: e.Seq,
		Action:  string(e.Action),
		Actor:   e.Actor.Kind,
		ActorID: e.Actor.ID,
		From:    e.Actor.From,
		Agent:   e.Actor.Agent,
		Target:  e.Target, Service: e.Service,
		Outcome: string(e.Outcome), Reason: e.Reason,
		Detail: e.Detail, Attempt: e.Attempt,
	}
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	out := AuditList{Entries: []AuditEntry{}, At: s.now().UnixMilli()}
	if s.audit == nil {
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable,
			"there is no audit log on this machine, which means credential mutations are refused")
		return
	}
	out.Durable = s.audit.Durable()
	out.Path = s.audit.Path()

	f := audit.Filter{
		Action: audit.Action(r.URL.Query().Get("action")),
		Target: r.URL.Query().Get("target"),
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, CodeBadPayload, "limit must be a non-negative integer")
			return
		}
		f.Limit = n
	}
	if v := r.URL.Query().Get("since"); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, CodeBadPayload, "since is unix milliseconds")
			return
		}
		f.Since = time.UnixMilli(ms)
	}
	if v := r.URL.Query().Get("outcome"); v != "" {
		f.Outcomes = []audit.Outcome{audit.Outcome(v)}
	}

	entries, err := s.audit.List(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}

	answered := map[string]bool{}
	for _, e := range entries {
		if e.Attempt != "" {
			answered[e.Attempt] = true
		}
	}
	for _, e := range entries {
		if e.Pending() && !answered[e.ID] {
			out.Pending++
		}
		out.Entries = append(out.Entries, auditEntry(e))
	}

	// Verification runs over the contiguous window that was returned. A filtered
	// read skips entries, so its result would be meaningless and is not
	// attempted; an unfiltered one is verified against the chain's own start
	// when it reaches back that far, and against its first link otherwise.
	if f.Action == "" && f.Target == "" && f.Since.IsZero() && len(f.Outcomes) == 0 {
		var err error
		switch {
		case len(entries) == 0:
			out.Intact = true
		case entries[0].Seq <= 1:
			err = audit.Verify(entries)
		default:
			err = audit.VerifyFrom(entries[0].Prev, entries)
		}
		if err != nil {
			out.Broken = err.Error()
		} else {
			out.Intact = true
		}
	}
	writeJSON(w, http.StatusOK, out)
}
