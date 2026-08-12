package api

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/luthor007/relay/relayd/internal/store"
)

// DASHBOARD.md §3.1 — the sessions screen, and the default view.
//
// "Every session across all five runtimes, live and historical, from the
// registry and the index." Those are two different tables with two different
// lifetimes (MEMORY.md §2): `session` is the live tier the orchestrator drives,
// `session_index` is one row for every session that has ever existed. The
// console shows one list, so the union happens here rather than in the browser
// — a client that has to reconcile two paginated lists will get it wrong, and
// it would get it wrong differently in the phone app.
//
// The two are joined on (runtime, native id), which is what a registry row
// carries for exactly this purpose.

// SessionQuery is the console's filter.
type SessionQuery struct {
	Runtime   string
	State     string
	Workspace string
	// Source is all | live | index. Default all.
	Source string
	// BlockedOnly narrows to sessions waiting on a human.
	BlockedOnly bool
	// Text matches the subject or title, case-insensitively.
	Text  string
	Limit int
}

func sessionQuery(r *http.Request) (SessionQuery, error) {
	q := r.URL.Query()
	out := SessionQuery{
		Runtime:     q.Get("runtime"),
		State:       q.Get("state"),
		Workspace:   q.Get("workspace"),
		Source:      q.Get("source"),
		BlockedOnly: truthy(q.Get("blocked")),
		Text:        strings.TrimSpace(q.Get("q")),
	}
	if out.Source == "" {
		out.Source = "all"
	}
	switch out.Source {
	case "all", "live", "index":
	default:
		return out, errBadSource
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return out, errBadLimit
		}
		out.Limit = n
	}
	return out, nil
}

type queryError string

func (e queryError) Error() string { return string(e) }

const (
	errBadSource = queryError("source must be all, live or index")
	errBadLimit  = queryError("limit must be a non-negative integer")
)

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	q, err := sessionQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadPayload, err.Error())
		return
	}
	list, err := s.consoleSessions(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleBlockedSessions is the pinned list, separately queryable.
//
// DASHBOARD.md §3.1 puts blocked sessions at the top, unmissable, because a
// blocked session is the one failure mode that silently stops all work. The
// list endpoint already hoists them; this exists so the console can poll the
// small thing rather than the big one, and so a notification badge does not
// have to fetch every historical session to count to one.
func (s *Server) handleBlockedSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.consoleSessions(r.Context(), SessionQuery{Source: "live", BlockedOnly: true})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": list.Sessions,
		"count":    len(list.Sessions),
		"at":       list.At,
	})
}

// consoleSessions assembles the union.
func (s *Server) consoleSessions(ctx context.Context, q SessionQuery) (SessionList, error) {
	out := SessionList{At: s.now().UnixMilli(), Sessions: []SessionSummary{}}

	if q.Source != "index" {
		live, err := s.sessionList(ctx, store.SessionFilter{
			Runtime:   q.Runtime,
			State:     store.SessionState(q.State),
			Workspace: q.Workspace,
		})
		if err != nil {
			return SessionList{}, err
		}
		out.Sessions = live.Sessions
	}
	for i := range out.Sessions {
		out.Sessions[i].Source = SourceRegistry
	}

	// The index tier has no live state: every row in it is archived. Asking for
	// a live state therefore excludes it, rather than the filter silently not
	// applying to half the list.
	indexWanted := q.Source != "live" && (q.State == "" || q.State == "archived")
	if indexWanted && s.db != nil {
		// A registry row and an index row for the same conversation are one
		// session. The registry's native id is the runtime's own, which is what
		// the index keys on.
		seen := map[string]int{}
		for i, v := range out.Sessions {
			if v.NativeID != "" {
				seen[v.Runtime+"/"+v.NativeID] = i
			}
			seen[v.Runtime+"/"+v.ID] = i
		}
		rows, err := s.indexSessions(ctx, q)
		if err != nil {
			return SessionList{}, err
		}
		for _, row := range rows {
			if i, ok := seen[row.Runtime+"/"+row.NativeID]; ok {
				// Merge rather than duplicate: the index knows the transcript
				// pointer and the title, the registry knows the live state.
				merge(&out.Sessions[i], row)
				continue
			}
			out.Sessions = append(out.Sessions, row)
		}
	}

	filtered := out.Sessions[:0]
	for _, v := range out.Sessions {
		if q.BlockedOnly && !v.Blocked {
			continue
		}
		if q.Text != "" && !matchesText(v, q.Text) {
			continue
		}
		filtered = append(filtered, v)
	}
	out.Sessions = filtered

	// Blocked first — unmissable — then most recent. Sorting after the merge so
	// an index row that turned out to be a live blocked session is hoisted too.
	sort.SliceStable(out.Sessions, func(i, j int) bool {
		a, b := out.Sessions[i], out.Sessions[j]
		if a.Blocked != b.Blocked {
			return a.Blocked
		}
		return a.LastActive > b.LastActive
	})
	if q.Limit > 0 && len(out.Sessions) > q.Limit {
		out.Sessions = out.Sessions[:q.Limit]
	}
	return out, nil
}

func merge(into *SessionSummary, from SessionSummary) {
	into.Source = SourceBoth
	if into.Title == "" {
		into.Title = from.Title
	}
	if into.Model == "" {
		into.Model = from.Model
	}
	if into.Messages == 0 {
		into.Messages = from.Messages
	}
	if into.ToolCalls == 0 {
		into.ToolCalls = from.ToolCalls
	}
	if into.Transcript == nil {
		into.Transcript = from.Transcript
	}
	if into.CostUSD == nil {
		into.CostUSD = from.CostUSD
	}
	if into.Tokens == nil {
		into.Tokens = from.Tokens
	}
}

func matchesText(v SessionSummary, text string) bool {
	needle := strings.ToLower(text)
	for _, hay := range []string{v.Subject, v.Title, v.Workspace, v.Runtime} {
		if strings.Contains(strings.ToLower(hay), needle) {
			return true
		}
	}
	return false
}

// indexSessions reads the historical tier.
func (s *Server) indexSessions(ctx context.Context, q SessionQuery) ([]SessionSummary, error) {
	sqlq := `SELECT runtime, session_id, path, byte_offset, title, workspace, model,
	                started_at, ended_at, message_count, tool_call_count, cost_usd, tokens_total
	         FROM session_index WHERE 1 = 1`
	var args []any
	if q.Runtime != "" {
		sqlq += ` AND runtime = ?`
		args = append(args, q.Runtime)
	}
	if q.Workspace != "" {
		sqlq += ` AND workspace = ?`
		args = append(args, q.Workspace)
	}
	sqlq += ` ORDER BY COALESCE(ended_at, started_at, 0) DESC LIMIT ?`
	// The index holds every session that has ever existed — MEMORY.md §3
	// measured ~22,000 on one machine — so this is always bounded.
	limit := q.Limit
	if limit <= 0 || limit > maxIndexRows {
		limit = maxIndexRows
	}
	args = append(args, limit)

	rows, err := s.db.SQL().QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionSummary
	for rows.Next() {
		var (
			runtime, sessionID, path, title, workspace, model string
			byteOffset                                        int64
			started, ended                                    *int64
			messages, tools                                   int64
			cost                                              *float64
			tokens                                            *int64
		)
		if err := rows.Scan(&runtime, &sessionID, &path, &byteOffset, &title, &workspace,
			&model, &started, &ended, &messages, &tools, &cost, &tokens); err != nil {
			return nil, err
		}
		v := SessionSummary{
			ID:        runtime + "/" + sessionID,
			NativeID:  sessionID,
			Runtime:   runtime,
			Subject:   title,
			Title:     title,
			Workspace: workspace,
			Model:     model,
			// Historical sessions are not driven by anything, so the state is not
			// "closed" — that would claim we saw it end. "archived" says only what
			// we know: it is in the index and nothing is running it.
			State:     "archived",
			Messages:  messages,
			ToolCalls: tools,
			CostUSD:   cost,
			Tokens:    tokens,
			Source:    SourceIndex,
		}
		if started != nil {
			v.CreatedAt = *started
		}
		switch {
		case ended != nil:
			v.LastActive = *ended
		case started != nil:
			v.LastActive = *started
		}
		if path != "" {
			v.Transcript = &TranscriptRef{
				Runtime: runtime, Session: sessionID, Path: path, ByteOffset: byteOffset,
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// maxIndexRows bounds one read of the historical tier.
const maxIndexRows = 2000

// Session sources.
const (
	SourceRegistry = "registry"
	SourceIndex    = "index"
	SourceBoth     = "both"
)

// sessionKey is the session this request is about, in either of the two forms
// the console addresses one by: Relay's own id, or the index's
// "<runtime>/<session>" pair as two path segments.
func sessionKey(r *http.Request) string {
	if rt := r.PathValue("runtime"); rt != "" {
		return rt + "/" + r.PathValue("session")
	}
	return r.PathValue("id")
}

// transcriptFor resolves a session id to its pointer into the original file.
//
// The id is either Relay's own — in which case the registry row carries the
// runtime's native id, which is what the index keys on — or the index's
// "<runtime>/<session id>" form, which is what a historical row is listed under.
func (s *Server) transcriptFor(ctx context.Context, id string) (TranscriptRef, bool, error) {
	if s.db == nil {
		return TranscriptRef{}, false, nil
	}

	runtime, native := "", ""
	if row, err := s.reg.Session(ctx, id); err == nil {
		runtime = row.Runtime
		native = row.NativeID
		if native == "" {
			native = row.ID
		}
	} else if rt, sid, ok := strings.Cut(id, "/"); ok {
		runtime, native = rt, sid
	} else {
		return TranscriptRef{}, false, nil
	}

	var ref TranscriptRef
	err := s.db.SQL().QueryRowContext(ctx,
		`SELECT runtime, session_id, path, byte_offset, source_size
		 FROM session_index WHERE runtime = ? AND session_id = ?`, runtime, native).
		Scan(&ref.Runtime, &ref.Session, &ref.Path, &ref.ByteOffset, &ref.Size)
	if err == sql.ErrNoRows {
		return TranscriptRef{}, false, nil
	}
	if err != nil {
		return TranscriptRef{}, false, err
	}
	return ref, ref.Path != "", nil
}
