package rendezvous

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Limits bound what one relay will hold.
//
// Every one of these is a refusal a client can be told about, rather than a
// resource that quietly runs out. A relay that dies under load takes every
// self-hoster offline at once, and "we run it and it is free" is a promise about
// availability more than anything else.
type Limits struct {
	// SlotTTL is how long an unjoined pairing slot is held. Pairing codes
	// expire on the box anyway; this is the relay refusing to hold state for a
	// box that walked away.
	SlotTTL time.Duration
	// StreamTTL is how long a stream id is held between the relay offering it
	// and the box dialling back. Short: the box is already connected and
	// answering, so a slow dial-back means it is gone.
	StreamTTL time.Duration
	// IdleTimeout closes a joined pipe that has carried nothing in either
	// direction. The link sends pings, so silence means the socket is dead in a
	// way TCP has not noticed yet.
	IdleTimeout time.Duration
	// MaxMessageBytes caps one frame. This is the only number here the relay
	// enforces *on* traffic rather than around it, and it is deliberately
	// generous: the relay must not become a thing that has opinions about
	// message shape.
	MaxMessageBytes int64
	// MaxBoxes and MaxSlots bound the whole process.
	MaxBoxes int
	MaxSlots int
	// MaxGuestsPerBox is how many phones and consoles may hold a stream to one
	// box at once. One phone, one console, and room for a reconnect that has not
	// been reaped yet.
	MaxGuestsPerBox int
}

// DefaultLimits is what the binary runs with.
func DefaultLimits() Limits {
	return Limits{
		SlotTTL:         10 * time.Minute,
		StreamTTL:       30 * time.Second,
		IdleTimeout:     5 * time.Minute,
		MaxMessageBytes: 1 << 20,
		MaxBoxes:        10_000,
		MaxSlots:        512,
		MaxGuestsPerBox: 4,
	}
}

// Refusals. Each is a distinct answer because each leads the user somewhere
// different: retry with a new code, wait, check the box is on, or stop.
var (
	// ErrSlotTaken is a second host on a slot somebody is already pairing on.
	// With 1024 slots this happens, and the honest answer is a new code — the
	// alternative is two boxes sharing a rendezvous and one of them pairing a
	// phone that was reading the other's code.
	ErrSlotTaken = errors.New("rendezvous: that pairing slot is in use; print a new code")
	// ErrNoSlot is a guest joining a slot no box is holding. Almost always a
	// mistyped code that happened to checksum, or a box that gave up waiting.
	ErrNoSlot = errors.New("rendezvous: nothing is waiting on that pairing slot")
	// ErrSlotBusy is a second guest on a slot that already joined one. Pairing is
	// one phone; a code that pairs a second phone is a code that pairs an
	// attacker's.
	ErrSlotBusy = errors.New("rendezvous: that pairing slot already has a phone on it")
	// ErrNoBox is a guest asking for a box that is not connected. The user's
	// machine is off, asleep, or has no network — all of which are true things
	// to say rather than a timeout.
	ErrNoBox = errors.New("rendezvous: that machine is not connected to the relay")
	// ErrBoxTaken is a second registration for one box id. It is not an attack
	// so much as a box that restarted before the relay reaped the old
	// registration, and the new one wins — see Register.
	ErrBoxTaken = errors.New("rendezvous: that machine is already registered")
	// ErrTooManyGuests is MaxGuestsPerBox.
	ErrTooManyGuests = errors.New("rendezvous: too many connections to that machine")
	// ErrFull is the process cap.
	ErrFull = errors.New("rendezvous: the relay is at capacity")
	// ErrNoStream is a dial-back for a stream that expired or never existed.
	ErrNoStream = errors.New("rendezvous: no such stream")
	// ErrBadProto is a protocol label the relay will not put in a frame. It is
	// refused rather than sanitised: a guest that asked for something this relay
	// cannot carry should be told, not quietly connected to a different
	// protocol than it asked for.
	ErrBadProto = errors.New("rendezvous: that is not a protocol label")
)

// maxProtoLen bounds a label. Long enough for every name anyone would choose,
// short enough that it cannot be used to push data through the control frame.
const maxProtoLen = 32

// validProto is the relay's only opinion about a label, and it is about frame
// safety rather than meaning.
//
// The relay does not know what "console" is and must not start knowing. What it
// does know is that the label goes into a JSON string it composes by hand, so
// the character set is closed to the ones that cannot end one.
func validProto(p string) bool {
	if len(p) > maxProtoLen {
		return false
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// Conn is one side of a pipe.
//
// An interface rather than *websocket.Conn so the hub — where every rule lives —
// is testable without a network. The two implementations are the WebSocket one
// and the test double.
type Conn interface {
	// Read returns the next message. It blocks until one arrives or the
	// connection ends.
	Read() (Message, error)
	// Write sends one message.
	Write(Message) error
	// Close ends the connection, reporting why. The reason reaches the peer
	// where the transport can carry one.
	Close(reason string) error
}

// Message is one frame, carried verbatim.
//
// Binary carries whether the frame was text or binary because the relay must not
// change it: relayd's envelopes are text and a sealed channel's are binary, and a
// pipe that normalises them is a pipe that corrupts one of them.
type Message struct {
	Binary bool
	Data   []byte
}

// Hub is the relay's whole state: who is registered, who is pairing, and which
// stream ids are outstanding.
//
// Everything is in memory and dies with the process, deliberately — see the
// package comment. It is safe for concurrent use.
type Hub struct {
	limits Limits
	now    func() time.Time
	newID  func() string
	log    *slog.Logger

	mu      sync.Mutex
	slots   map[string]*slot
	boxes   map[string]*box
	streams map[string]*pendingStream
}

type slot struct {
	host      Conn
	createdAt time.Time
	joined    bool
	// arrived carries the guest to the waiting host. Buffered by one, so
	// JoinSlot never blocks on a host that has just given up.
	arrived chan Conn
}

type box struct {
	control Conn
	id      string
	// guests counts live streams, so MaxGuestsPerBox is enforced against the
	// pipes that exist rather than against how many were ever offered.
	guests int
	at     time.Time
}

type pendingStream struct {
	boxID  string
	guest  Conn
	ready  chan Conn // the box's dial-back lands here
	at     time.Time
	closed bool
}

// HubOptions configures a [Hub].
type HubOptions struct {
	Limits Limits
	Now    func() time.Time
	NewID  func() string
	Log    *slog.Logger
}

// NewHub builds a hub.
func NewHub(o HubOptions) *Hub {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.NewID == nil {
		o.NewID = randomID
	}
	l := o.Limits
	d := DefaultLimits()
	if l.SlotTTL <= 0 {
		l.SlotTTL = d.SlotTTL
	}
	if l.StreamTTL <= 0 {
		l.StreamTTL = d.StreamTTL
	}
	if l.IdleTimeout <= 0 {
		l.IdleTimeout = d.IdleTimeout
	}
	if l.MaxMessageBytes <= 0 {
		l.MaxMessageBytes = d.MaxMessageBytes
	}
	if l.MaxBoxes <= 0 {
		l.MaxBoxes = d.MaxBoxes
	}
	if l.MaxSlots <= 0 {
		l.MaxSlots = d.MaxSlots
	}
	if l.MaxGuestsPerBox <= 0 {
		l.MaxGuestsPerBox = d.MaxGuestsPerBox
	}
	return &Hub{
		limits:  l,
		now:     o.Now,
		newID:   o.NewID,
		log:     o.Log,
		slots:   map[string]*slot{},
		boxes:   map[string]*box{},
		streams: map[string]*pendingStream{},
	}
}

// --------------------------------------------------------------- pairing --

// HostSlot holds a pairing slot open for a box.
//
// It returns once a guest has joined and the two have finished, or the slot
// expired, or the host went away. The slot is released on every path.
//
// ctx is how the host going away is noticed, and it has to be: nothing here may
// read the host connection before a guest arrives. A watchdog goroutine reading
// it to detect a hangup would still be running when the pipe starts, and would
// steal the first frames of the pairing exchange from it. The transport already
// knows when its socket closed — that is what it cancels ctx for.
func (h *Hub) HostSlot(ctx context.Context, name string, host Conn) error {
	name = normaliseSlot(name)
	if name == "" {
		return fmt.Errorf("%w: empty slot", ErrNoSlot)
	}

	h.mu.Lock()
	if len(h.slots) >= h.limits.MaxSlots {
		h.mu.Unlock()
		return ErrFull
	}
	if existing, ok := h.slots[name]; ok {
		// Not replaced, unlike a box registration: a slot's other side is a
		// human reading a code aloud, and taking over would mean the phone
		// finishes pairing with whichever box registered second.
		if h.now().Sub(existing.createdAt) < h.limits.SlotTTL {
			h.mu.Unlock()
			return ErrSlotTaken
		}
		delete(h.slots, name)
	}
	s := &slot{host: host, createdAt: h.now(), arrived: make(chan Conn, 1)}
	h.slots[name] = s
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		if cur, ok := h.slots[name]; ok && cur == s {
			delete(h.slots, name)
		}
		h.mu.Unlock()
	}()

	// Wait for a guest by reading: the host sends nothing until one arrives, so
	// this doubles as noticing the host hanging up.
	select {
	case guest := <-s.arrived:
		return pipe(host, guest, h.limits)
	case <-ctx.Done():
		return nil
	case <-time.After(h.limits.SlotTTL):
		return host.Close("pairing timed out; print a new code")
	}
}

// JoinSlot connects a guest to a waiting host.
func (h *Hub) JoinSlot(name string, guest Conn) error {
	name = normaliseSlot(name)
	h.mu.Lock()
	s, ok := h.slots[name]
	switch {
	case !ok:
		h.mu.Unlock()
		return ErrNoSlot
	case s.joined:
		h.mu.Unlock()
		return ErrSlotBusy
	}
	s.joined = true
	arrived := s.arrived
	h.mu.Unlock()

	arrived <- guest
	return nil
}

// --------------------------------------------------------------- linking --

// Register holds a box's control connection open.
//
// A second registration for the same id **replaces** the first, unlike a slot.
// A box that restarts is the common case and the old registration is a socket
// nothing is on the other end of; refusing the new one would leave the machine
// unreachable until a timeout the user cannot see. The displaced connection is
// closed with a reason so its logs say what happened.
func (h *Hub) Register(id string, control Conn) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: empty id", ErrNoBox)
	}

	h.mu.Lock()
	if _, exists := h.boxes[id]; !exists && len(h.boxes) >= h.limits.MaxBoxes {
		h.mu.Unlock()
		return ErrFull
	}
	previous := h.boxes[id]
	b := &box{control: control, id: id, at: h.now()}
	h.boxes[id] = b
	h.mu.Unlock()

	if previous != nil {
		_ = previous.control.Close("this machine registered again from somewhere else")
	}

	defer func() {
		h.mu.Lock()
		if cur, ok := h.boxes[id]; ok && cur == b {
			delete(h.boxes, id)
		}
		h.mu.Unlock()
	}()

	// The control connection carries offers outbound and nothing inbound. Read
	// until it ends: that is how the relay learns the box is gone, and it is the
	// only thing it wants from this socket.
	for {
		if _, err := control.Read(); err != nil {
			return nil
		}
	}
}

// Connected reports whether a box is registered, for the health endpoint.
func (h *Hub) Connected(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.boxes[id]
	return ok
}

// Counts is what /healthz reports: how many, never who.
func (h *Hub) Counts() (boxes, slots, streams int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.boxes), len(h.slots), len(h.streams)
}

// Connect asks a box for a stream and pipes the guest into it.
//
// The sequence is the one the package comment argues for: mint a stream id, tell
// the box over its control connection, wait for the box to dial back, then pipe.
// The guest never learns anything about the box beyond the fact that it
// answered.
//
// proto is a label the guest chose and the relay carries verbatim, so the box
// knows which of its protocols to speak on the stream — the phone's session
// protocol or the console's HTTP tunnel. It is routing metadata, not
// permission: the relay does not know what any label means, and both protocols
// authenticate on the far side, so choosing one gets a stranger no further than
// choosing the other. Empty is the phone, which is why a phone built before
// this existed still works.
func (h *Hub) Connect(ctx context.Context, boxID string, proto string, guest Conn) error {
	if !validProto(proto) {
		return ErrBadProto
	}

	h.mu.Lock()
	b, ok := h.boxes[boxID]
	if !ok {
		h.mu.Unlock()
		return ErrNoBox
	}
	if b.guests >= h.limits.MaxGuestsPerBox {
		h.mu.Unlock()
		return ErrTooManyGuests
	}
	b.guests++
	id := h.newID()
	ps := &pendingStream{boxID: boxID, guest: guest, ready: make(chan Conn, 1), at: h.now()}
	h.streams[id] = ps
	control := b.control
	h.mu.Unlock()

	release := func() {
		h.mu.Lock()
		delete(h.streams, id)
		if cur, ok := h.boxes[boxID]; ok && cur == b && b.guests > 0 {
			b.guests--
		}
		h.mu.Unlock()
	}
	defer release()

	// The one message the relay ever composes. It carries a stream id and the
	// guest's protocol label and nothing else — not the guest's address, not a
	// count, not a timestamp — because anything more would be the relay telling
	// a box about its users.
	//
	// Built by concatenation rather than marshalled, and [validProto] is what
	// makes that safe: the label is `[a-z0-9._-]` only, so there is no input
	// here that can close a JSON string. Keeping it a literal is also how the
	// contents of this frame stay obvious to anyone auditing what the relay
	// knows.
	frame := `{"kind":"rz.connect","stream":"` + id + `"}`
	if proto != "" {
		frame = `{"kind":"rz.connect","stream":"` + id + `","proto":"` + proto + `"}`
	}
	if err := control.Write(Message{Data: []byte(frame)}); err != nil {
		return ErrNoBox
	}

	select {
	case boxSide := <-ps.ready:
		return pipe(guest, boxSide, h.limits)
	case <-ctx.Done():
		return nil
	case <-time.After(h.limits.StreamTTL):
		return ErrNoBox
	}
}

// Accept is the box dialling back for a stream it was offered.
func (h *Hub) Accept(streamID string, boxSide Conn) error {
	h.mu.Lock()
	ps, ok := h.streams[streamID]
	if ok && !ps.closed {
		ps.closed = true
	}
	h.mu.Unlock()
	if !ok {
		return ErrNoStream
	}
	ps.ready <- boxSide
	return nil
}

// --------------------------------------------------------------- the pipe --

// pipe copies until either side ends, then closes both.
//
// It is the whole of what the relay does to traffic: read a frame, write the
// same frame. It does not parse, does not reframe, does not log payloads, and
// does not look at [Message.Binary] beyond passing it on.
func pipe(a, b Conn, l Limits) error {
	errs := make(chan error, 2)
	go func() { errs <- copyFrames(a, b, l) }()
	go func() { errs <- copyFrames(b, a, l) }()

	err := <-errs
	// Closing both is what makes the second copier return, and it is also the
	// honest end state: half a pipe is a socket that looks alive and carries
	// nothing.
	_ = a.Close("")
	_ = b.Close("")
	<-errs
	return err
}

func copyFrames(from, to Conn, l Limits) error {
	for {
		m, err := from.Read()
		if err != nil {
			return err
		}
		if int64(len(m.Data)) > l.MaxMessageBytes {
			return fmt.Errorf("rendezvous: frame is %d bytes, over the %d cap",
				len(m.Data), l.MaxMessageBytes)
		}
		if err := to.Write(m); err != nil {
			return err
		}
	}
}

// normaliseSlot applies the same forgiveness `parsePairingCode` does.
//
// A user who typed O for 0 has their code corrected on the phone; the box printed
// the canonical form. Both arrive here and both have to name the same slot, or a
// correctly-typed code fails to find a correctly-printed one.
func normaliseSlot(s string) string {
	up := strings.ToUpper(strings.TrimSpace(s))
	var out strings.Builder
	for _, r := range up {
		switch r {
		case 'O':
			out.WriteRune('0')
		case 'I', 'L':
			out.WriteRune('1')
		default:
			if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

// randomID mints a stream id.
//
// 128 bits from crypto/rand, and it is guessing-resistant on purpose: a stream
// id is the only thing standing between a stranger and the box side of a pipe
// that is about to be offered. The window is StreamTTL and the pipe is
// authenticated end to end afterwards, but "hard to guess" costs nothing here.
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform this runs on, and a
		// predictable id would be worse than no relay at all.
		panic("rendezvous: no randomness available: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
