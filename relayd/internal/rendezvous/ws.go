package rendezvous

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// The HTTP surface.
//
// Four routes and no others. Each is a dial-out, because that is the whole point
// — SYSTEM.md §7: "Both sides dial out; the relay pipes end-to-end encrypted
// bytes and cannot read them."
//
//	GET /rz/v1/host/{slot}     a box holding a pairing slot
//	GET /rz/v1/join/{slot}     a phone joining one
//	GET /rz/v1/box/{id}        a box's control connection
//	GET /rz/v1/connect/{id}    a phone or console reaching that box
//	                           ?p=<label> picks which protocol the box speaks;
//	                           absent is the phone's, which is what makes a
//	                           phone built before the console existed still work
//	GET /rz/v1/stream/{id}     the box dialling back for one offered stream
//
// There is no route that lists anything. A relay that can answer "which boxes
// are online" is holding a presence list for every customer, and the only thing
// /healthz reports is counts.

// Handler serves the relay.
type Handler struct {
	hub *Hub
	log *slog.Logger
	// Origins, when non-empty, is the allowlist for browser connections.
	//
	// The console is a browser and the phone is not, so this exists only for the
	// console: a WebSocket from a page is not blocked by the same-origin policy,
	// so without a check any site a user visits could open a connection to the
	// relay in their name. The phone sends no Origin and is unaffected.
	Origins []string
}

// NewHandler wires the routes.
func NewHandler(h *Hub, log *slog.Logger, origins []string) *Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Handler{hub: h, log: log, Origins: origins}
}

// Routes returns the mux.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /rz/v1/host/{slot}", h.host)
	mux.HandleFunc("GET /rz/v1/join/{slot}", h.join)
	mux.HandleFunc("GET /rz/v1/box/{id}", h.box)
	mux.HandleFunc("GET /rz/v1/connect/{id}", h.connect)
	mux.HandleFunc("GET /rz/v1/stream/{id}", h.stream)
	mux.HandleFunc("GET /healthz", h.healthz)
	return mux
}

func (h *Handler) accept(w http.ResponseWriter, r *http.Request) (*wsConn, bool) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: h.Origins,
		// Compression off: everything crossing this relay is either a PAKE
		// message or a sealed frame, and both are incompressible. Leaving it on
		// would spend CPU on every byte of every customer's traffic for nothing.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		// Logged without the reason's detail at info: a rejected upgrade is
		// usually a health checker or a scanner, and this line exists to show
		// the relay is reachable rather than to describe who reached it.
		h.log.Debug("rendezvous: upgrade refused", "error", err)
		return nil, false
	}
	return &wsConn{c: c, max: h.hub.limits.MaxMessageBytes, idle: h.hub.limits.IdleTimeout}, true
}

func (h *Handler) host(w http.ResponseWriter, r *http.Request) {
	c, ok := h.accept(w, r)
	if !ok {
		return
	}
	// The request context is cancelled when the socket drops, which is what
	// HostSlot waits on rather than reading the connection. See its comment.
	err := h.hub.HostSlot(r.Context(), r.PathValue("slot"), c)
	h.finish(c, err)
}

func (h *Handler) join(w http.ResponseWriter, r *http.Request) {
	c, ok := h.accept(w, r)
	if !ok {
		return
	}
	if err := h.hub.JoinSlot(r.PathValue("slot"), c); err != nil {
		h.finish(c, err)
		return
	}
	// JoinSlot hands the connection to the waiting host, which pipes it. This
	// handler must not return until that pipe is done, or the HTTP server
	// cancels the request context and closes the socket underneath it.
	<-r.Context().Done()
}

func (h *Handler) box(w http.ResponseWriter, r *http.Request) {
	c, ok := h.accept(w, r)
	if !ok {
		return
	}
	h.finish(c, h.hub.Register(r.PathValue("id"), c))
}

func (h *Handler) connect(w http.ResponseWriter, r *http.Request) {
	c, ok := h.accept(w, r)
	if !ok {
		return
	}
	h.finish(c, h.hub.Connect(r.Context(), r.PathValue("id"), r.URL.Query().Get("p"), c))
}

func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	c, ok := h.accept(w, r)
	if !ok {
		return
	}
	if err := h.hub.Accept(r.PathValue("id"), c); err != nil {
		h.finish(c, err)
		return
	}
	<-r.Context().Done()
}

// healthz reports counts and nothing else.
func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	boxes, slots, streams := h.hub.Counts()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// Written by hand rather than marshalled, so that adding a field is a
	// deliberate act. The thing that must never appear here is an identifier.
	_, _ = w.Write([]byte(`{"ok":true,"boxes":` + itoa(boxes) +
		`,"slots":` + itoa(slots) + `,"streams":` + itoa(streams) + `}`))
}

// finish closes a connection with the reason, if the failure has one a person
// should read.
//
// The refusals are deliberately quotable: a phone that cannot pair should be
// able to show the sentence rather than "connection closed".
func (h *Handler) finish(c *wsConn, err error) {
	switch {
	case err == nil:
		_ = c.Close("")
	case errors.Is(err, ErrSlotTaken), errors.Is(err, ErrNoSlot), errors.Is(err, ErrSlotBusy),
		errors.Is(err, ErrNoBox), errors.Is(err, ErrTooManyGuests), errors.Is(err, ErrFull),
		errors.Is(err, ErrNoStream), errors.Is(err, ErrBadProto):
		_ = c.Close(err.Error())
	default:
		// Anything else is the pipe ending, which is normal, or a transport
		// error, which the peer already knows about.
		_ = c.Close("")
	}
}

// wsConn adapts a WebSocket to [Conn].
type wsConn struct {
	c    *websocket.Conn
	max  int64
	idle time.Duration
}

var _ Conn = (*wsConn)(nil)

func (w *wsConn) Read() (Message, error) {
	// The idle timeout is applied per read rather than by a sweeper: a pipe that
	// has carried nothing in either direction for this long is a socket TCP has
	// not noticed is dead, and holding it costs a file descriptor per customer.
	ctx := context.Background()
	if w.idle > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, w.idle)
		defer cancel()
	}
	w.c.SetReadLimit(w.max)
	typ, data, err := w.c.Read(ctx)
	if err != nil {
		return Message{}, err
	}
	return Message{Binary: typ == websocket.MessageBinary, Data: data}, nil
}

func (w *wsConn) Write(m Message) error {
	typ := websocket.MessageText
	if m.Binary {
		typ = websocket.MessageBinary
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return w.c.Write(ctx, typ, m.Data)
}

func (w *wsConn) Close(reason string) error {
	if reason == "" {
		return w.c.Close(websocket.StatusNormalClosure, "")
	}
	// A close reason is capped at 123 bytes by the protocol, and a truncated
	// sentence is worse than a short one.
	if len(reason) > 120 {
		reason = reason[:117] + "..."
	}
	return w.c.Close(websocket.StatusPolicyViolation, reason)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TrimSlot is exported for the box: it prints a code and dials the slot half.
func TrimSlot(code string) string { return normaliseSlot(strings.TrimSpace(code)) }
