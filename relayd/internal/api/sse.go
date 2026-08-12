package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// KeepAlive is how often an idle SSE stream writes a comment, so a proxy that
// reaps quiet connections does not silently sever the console's live view.
const KeepAlive = 25 * time.Second

// handleSSE is DASHBOARD.md §7.1's live update channel.
//
// SSE rather than a WebSocket, deliberately: the console watches and does not
// talk back on this channel, and one-directional streaming over plain HTTP
// reconnects itself, survives a proxy that only speaks HTTP/1.1, and needs no
// framing library. The phone's socket is bidirectional and is a different
// problem — that one is a WebSocket for the reason SYSTEM.md §6.1 gives.
//
// Three event names from §7.1: `session` when a row moves, `incident` when
// something goes wrong, `ping` when the user is about to hear from us. The
// console's own mutations add `credential`, `connector`, `fact` and `probe`,
// which is what makes DASHBOARD.md §5's optimistic UI reconcilable — a row that
// appeared the instant the button was pressed learns from this stream whether it
// actually landed, and rolls back rather than sitting there looking applied.
//
// A client that wants only some of them filters by name; there is no
// server-side subscription to get wrong.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, CodeFailed, "this server cannot stream")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// Proxies that buffer turn a live view into a batch view.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	changes := s.reg.Watch("sse")
	defer changes.Close()
	incidents := s.reg.WatchIncidents("sse")
	defer incidents.Close()
	pings := s.pings.Subscribe("sse")
	defer pings.Close()
	console := s.console.Subscribe("sse")
	defer console.Close()

	ctx := r.Context()

	// Open with the current list so a console that connects mid-flight renders
	// something immediately rather than waiting for the next change. It is the
	// same union GET /v1/sessions returns, assembled by the same function, so
	// the opening frame and the first fetch cannot disagree.
	if list, err := s.consoleSessions(ctx, SessionQuery{Source: "all"}); err == nil {
		writeSSE(w, flusher, "sessions", list)
	}

	keep := time.NewTicker(KeepAlive)
	defer keep.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case c, ok := <-changes.C():
			if !ok {
				return
			}
			writeSSE(w, flusher, "session", c)

		case i, ok := <-incidents.C():
			if !ok {
				return
			}
			writeSSE(w, flusher, "incident", i)

		case p, ok := <-pings.C():
			if !ok {
				return
			}
			writeSSE(w, flusher, "ping", ssePing(p))

		case c, ok := <-console.C():
			if !ok {
				return
			}
			writeSSE(w, flusher, c.Kind, c)

		case <-keep.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// ssePing is the console's view of a ping. It carries no reply path — the
// console answers through POST /v1/sessions/{id}/answer, which goes through the
// same authorization check as everything else.
type ssePingView struct {
	ID       string           `json:"id"`
	Class    string           `json:"class"`
	At       int64            `json:"at"`
	Repeat   int              `json:"repeat,omitempty"`
	Sessions []string         `json:"sessions,omitempty"`
	Line     string           `json:"line,omitempty"`
	Speak    bool             `json:"speak"`
	Silent   bool             `json:"silent,omitempty"`
	Confirm  *ConfirmRequest  `json:"confirm,omitempty"`
	Resolved *ConfirmResolved `json:"resolved,omitempty"`
}

func ssePing(p Ping) ssePingView {
	if p.Resolved != nil {
		return ssePingView{Class: "resolved", Resolved: p.Resolved}
	}
	return ssePingView{
		ID:       p.Ping.ID,
		Class:    p.Ping.Class.String(),
		At:       p.Ping.At.UnixMilli(),
		Repeat:   p.Ping.Repeat,
		Sessions: p.Ping.Sessions,
		Line:     p.Ping.Line,
		// Read off the frames that were actually rendered, not off the policy:
		// this is the console's render view, so what it reports is what the
		// voice backend did, not what bus.Ping would have permitted.
		Speak:   p.Speak != nil,
		Silent:  p.Notify != nil && p.Notify.Silent,
		Confirm: p.Confirm,
	}
}

func writeSSE(w http.ResponseWriter, f http.Flusher, name string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	f.Flush()
}
