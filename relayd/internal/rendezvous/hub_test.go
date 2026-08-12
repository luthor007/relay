package rendezvous_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/rendezvous"
)

// The relay's rules, tested without a network
//
// Every property here is one the relay is solely responsible for — SYSTEM.md §7
// leg 3: isolation and availability. Confidentiality is not tested here because
// it is not this package's to provide: pairing is a PAKE and the link is sealed,
// both above this layer, which is what lets the relay be a dumb pipe.

// fakeConn is a Conn backed by two channels.
type fakeConn struct {
	name string
	in   chan rendezvous.Message
	out  chan rendezvous.Message

	mu     sync.Mutex
	closed bool
	reason string
}

func newConn(name string) *fakeConn {
	return &fakeConn{
		name: name,
		in:   make(chan rendezvous.Message, 16),
		out:  make(chan rendezvous.Message, 16),
	}
}

func (c *fakeConn) Read() (rendezvous.Message, error) {
	m, ok := <-c.in
	if !ok {
		return rendezvous.Message{}, errors.New(c.name + ": closed")
	}
	return m, nil
}

func (c *fakeConn) Write(m rendezvous.Message) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return errors.New(c.name + ": closed")
	}
	select {
	case c.out <- m:
		return nil
	default:
		return errors.New(c.name + ": not draining")
	}
}

func (c *fakeConn) Close(reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.reason = reason
		close(c.in)
	}
	return nil
}

func (c *fakeConn) send(t *testing.T, text string) {
	t.Helper()
	select {
	case c.in <- rendezvous.Message{Data: []byte(text)}:
	case <-time.After(time.Second):
		t.Fatalf("%s: nothing read the message", c.name)
	}
}

func (c *fakeConn) recv(t *testing.T) rendezvous.Message {
	t.Helper()
	select {
	case m := <-c.out:
		return m
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: no message arrived", c.name)
		return rendezvous.Message{}
	}
}

func (c *fakeConn) closedWith() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed, c.reason
}

func hub(t *testing.T, tune ...func(*rendezvous.Limits)) *rendezvous.Hub {
	t.Helper()
	l := rendezvous.DefaultLimits()
	for _, f := range tune {
		f(&l)
	}
	return rendezvous.NewHub(rendezvous.HubOptions{Limits: l})
}

// hostSlot starts a host and returns a func that waits for it to finish.
func hostSlot(t *testing.T, h *rendezvous.Hub, ctx context.Context, name string, c *fakeConn) func() error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- h.HostSlot(ctx, name, c) }()
	// Give the registration a moment to land, so a JoinSlot that follows is not
	// racing it. The hub is not a queue by design, so this is the test's job.
	waitFor(t, func() bool { return slotsOf(h) > 0 })
	return func() error {
		select {
		case err := <-done:
			return err
		case <-time.After(3 * time.Second):
			t.Fatal("HostSlot never returned")
			return nil
		}
	}
}

func slotsOf(h *rendezvous.Hub) int {
	_, slots, _ := h.Counts()
	return slots
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// --------------------------------------------------------------- pairing --

func TestPairingBytesCrossVerbatimInBothDirections(t *testing.T) {
	h := hub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	box, phone := newConn("box"), newConn("phone")
	wait := hostSlot(t, h, ctx, "7K", box)

	if err := h.JoinSlot("7K", phone); err != nil {
		t.Fatal(err)
	}

	// The four pairing messages, as pairing.ts writes them. The relay must not
	// understand any of them.
	phone.send(t, `{"v":1,"kind":"pair.hello","slot":"7K","pake":"..."}`)
	if got := string(box.recv(t).Data); !strings.Contains(got, "pair.hello") {
		t.Fatalf("the box did not receive the hello: %q", got)
	}
	box.send(t, `{"v":1,"kind":"pair.accept","pake":"...","confirm":"..."}`)
	if got := string(phone.recv(t).Data); !strings.Contains(got, "pair.accept") {
		t.Fatalf("the phone did not receive the accept: %q", got)
	}

	// Binary frames survive as binary: a sealed channel's are binary and
	// relayd's envelopes are text, so a pipe that normalised them would corrupt
	// one of the two.
	phone.in <- rendezvous.Message{Binary: true, Data: []byte{0x00, 0xff, 0x10}}
	got := box.recv(t)
	if !got.Binary || len(got.Data) != 3 || got.Data[1] != 0xff {
		t.Errorf("a binary frame did not survive the pipe: %+v", got)
	}

	_ = box.Close("")
	wait()
}

func TestASecondBoxOnAnOccupiedSlotIsRefused(t *testing.T) {
	// 1024 slots means collisions happen. Two boxes sharing a rendezvous would
	// mean a phone pairing with whichever registered second, having read the
	// other's code.
	h := hub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := newConn("first")
	wait := hostSlot(t, h, ctx, "7K", first)

	err := h.HostSlot(ctx, "7K", newConn("second"))
	if !errors.Is(err, rendezvous.ErrSlotTaken) {
		t.Fatalf("a second box took an occupied slot: %v", err)
	}

	// Cancelling, not closing. A host waiting for a guest is deliberately not
	// being read from — see HostSlot — so the context is the only thing that
	// ends it, exactly as the transport arranges when its socket drops.
	cancel()
	wait()
	_ = first.Close("")
}

func TestASecondPhoneOnAPairingSlotIsRefused(t *testing.T) {
	// "A code that pairs a second phone is a code that pairs an attacker's."
	h := hub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	box := newConn("box")
	wait := hostSlot(t, h, ctx, "7K", box)
	if err := h.JoinSlot("7K", newConn("phone")); err != nil {
		t.Fatal(err)
	}
	if err := h.JoinSlot("7K", newConn("attacker")); !errors.Is(err, rendezvous.ErrSlotBusy) {
		t.Fatalf("a second phone joined: %v", err)
	}
	_ = box.Close("")
	wait()
}

func TestJoiningASlotNobodyIsHoldingSaysSo(t *testing.T) {
	h := hub(t)
	// A mistyped code that happens to checksum, or a box that gave up. Either
	// way the user needs a sentence, not a timeout.
	if err := h.JoinSlot("ZZ", newConn("phone")); !errors.Is(err, rendezvous.ErrNoSlot) {
		t.Fatalf("joining an empty slot returned %v", err)
	}
}

func TestASlotIsFoundHoweverTheCodeWasTyped(t *testing.T) {
	// parsePairingCode maps O to 0 and I/L to 1 before it ever reaches here, but
	// the box printed the canonical form and a client that skipped that step
	// must not silently fail to find a slot that exists.
	h := hub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	box := newConn("box")
	wait := hostSlot(t, h, ctx, "01", box)
	if err := h.JoinSlot(" oi ", newConn("phone")); err != nil {
		t.Fatalf("a code typed with O and I did not find slot 01: %v", err)
	}
	_ = box.Close("")
	wait()
}

func TestAnUnjoinedSlotIsReleased(t *testing.T) {
	h := hub(t, func(l *rendezvous.Limits) { l.SlotTTL = 40 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	box := newConn("box")
	done := make(chan error, 1)
	go func() { done <- h.HostSlot(ctx, "7K", box) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an unjoined slot was held forever")
	}
	if closed, reason := box.closedWith(); !closed || !strings.Contains(reason, "new code") {
		t.Errorf("the box was not told why: closed=%v reason=%q", closed, reason)
	}
	if n := slotsOf(h); n != 0 {
		t.Errorf("%d slots still held", n)
	}
}

// --------------------------------------------------------------- linking --

// register starts a box registration and returns a waiter.
func register(t *testing.T, h *rendezvous.Hub, id string, c *fakeConn) func() {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Register(id, c) }()
	waitFor(t, func() bool { return h.Connected(id) })
	return func() {
		_ = c.Close("")
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Register never returned")
		}
	}
}

func TestAGuestReachesABoxThroughAStreamTheBoxDialsBack(t *testing.T) {
	h := hub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	control := newConn("control")
	stop := register(t, h, "box-1", control)
	defer stop()

	phone := newConn("phone")
	connected := make(chan error, 1)
	go func() { connected <- h.Connect(ctx, "box-1", "", phone) }()

	// The one message the relay ever composes: a stream id and nothing else.
	offer := string(control.recv(t).Data)
	if !strings.Contains(offer, `"kind":"rz.connect"`) {
		t.Fatalf("the box was not offered a stream: %q", offer)
	}
	id := between(t, offer, `"stream":"`, `"`)
	// Nothing about the guest travels with the offer — not an address, not a
	// count. The relay does not tell a box about its users.
	for _, leak := range []string{"phone", "addr", "ip"} {
		if strings.Contains(strings.ToLower(offer), leak) {
			t.Errorf("the offer carries %q: %s", leak, offer)
		}
	}

	boxSide := newConn("box-side")
	if err := h.Accept(id, boxSide); err != nil {
		t.Fatal(err)
	}

	phone.send(t, `{"v":1,"type":"utterance"}`)
	if got := string(boxSide.recv(t).Data); !strings.Contains(got, "utterance") {
		t.Fatalf("the box did not receive the frame: %q", got)
	}
	boxSide.send(t, `{"v":1,"type":"speak"}`)
	if got := string(phone.recv(t).Data); !strings.Contains(got, "speak") {
		t.Fatalf("the phone did not receive the reply: %q", got)
	}

	_ = phone.Close("")
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("Connect never returned after the guest hung up")
	}
}

func TestReachingAMachineThatIsNotConnectedSaysWhich(t *testing.T) {
	h := hub(t)
	err := h.Connect(context.Background(), "box-nope", "", newConn("phone"))
	// "Off, asleep, or no network" is a true thing to say. A timeout is not.
	if !errors.Is(err, rendezvous.ErrNoBox) {
		t.Fatalf("connecting to an absent box returned %v", err)
	}
}

func TestABoxThatRestartsReplacesItsOldRegistration(t *testing.T) {
	// The common case, not an attack. Refusing the new registration would leave
	// the machine unreachable until a timeout the user cannot see.
	h := hub(t)
	first := newConn("first")
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Register("box-1", first) }()
	waitFor(t, func() bool { return h.Connected("box-1") })

	second := newConn("second")
	stop := register(t, h, "box-1", second)
	defer stop()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the displaced registration was not released")
	}
	if closed, reason := first.closedWith(); !closed || !strings.Contains(reason, "registered again") {
		t.Errorf("the displaced box was not told why: closed=%v reason=%q", closed, reason)
	}
	if !h.Connected("box-1") {
		t.Error("the box is unreachable after restarting")
	}
}

func TestAStreamIdThatWasNeverOfferedIsRefused(t *testing.T) {
	h := hub(t)
	if err := h.Accept("made-up", newConn("box")); !errors.Is(err, rendezvous.ErrNoStream) {
		t.Fatalf("an invented stream id was accepted: %v", err)
	}
}

func TestOneBoxCannotBeSwampedWithGuests(t *testing.T) {
	h := hub(t, func(l *rendezvous.Limits) {
		l.MaxGuestsPerBox = 1
		l.StreamTTL = 30 * time.Second
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	control := newConn("control")
	stop := register(t, h, "box-1", control)
	defer stop()

	first := newConn("first")
	go func() { _ = h.Connect(ctx, "box-1", "", first) }()
	control.recv(t) // the offer for the first guest

	if err := h.Connect(ctx, "box-1", "", newConn("second")); !errors.Is(err, rendezvous.ErrTooManyGuests) {
		t.Fatalf("a second guest was let in past the cap: %v", err)
	}
}

func TestTheRelayReportsCountsAndNeverWho(t *testing.T) {
	// /healthz has to say something, and what it must not say is which machines
	// are online — that is a presence list for every customer.
	h := hub(t)
	control := newConn("control")
	stop := register(t, h, "box-1", control)
	defer stop()

	boxes, _, _ := h.Counts()
	if boxes != 1 {
		t.Errorf("boxes = %d, want 1", boxes)
	}
}

func TestAFrameOverTheCapEndsThePipeRatherThanBeingForwarded(t *testing.T) {
	h := hub(t, func(l *rendezvous.Limits) { l.MaxMessageBytes = 32 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	box := newConn("box")
	wait := hostSlot(t, h, ctx, "7K", box)
	phone := newConn("phone")
	if err := h.JoinSlot("7K", phone); err != nil {
		t.Fatal(err)
	}

	phone.in <- rendezvous.Message{Data: []byte(strings.Repeat("x", 64))}
	err := wait()
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("an oversized frame did not end the pipe: %v", err)
	}
	if closed, _ := box.closedWith(); !closed {
		t.Error("the far side was left open")
	}
}

func between(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("%q not in %q", start, s)
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("no %q after %q", end, start)
	}
	return rest[:j]
}

// The protocol label
//
// A box serves two protocols on relayed streams — the phone's session protocol
// and the console's HTTP tunnel — and the relay is the only thing that knows
// which one a guest asked for. It carries the label and does not read it.

func TestTheGuestsProtocolLabelReachesTheBox(t *testing.T) {
	h := hub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	control := newConn("control")
	stop := register(t, h, "box-1", control)
	defer stop()

	go func() { _ = h.Connect(ctx, "box-1", "console", newConn("console")) }()

	offer := string(control.recv(t).Data)
	if !strings.Contains(offer, `"proto":"console"`) {
		t.Fatalf("the box was not told which protocol to speak: %s", offer)
	}
	// And still nothing about the guest. The label is the guest's request, not a
	// description of the guest.
	for _, leak := range []string{"addr", "ip", "origin"} {
		if strings.Contains(strings.ToLower(offer), leak) {
			t.Errorf("the offer carries %q: %s", leak, offer)
		}
	}
}

func TestAPhoneThatNamesNoProtocolStillGetsTheOldFrame(t *testing.T) {
	// Every phone already built sends no label. If their offers grew a field,
	// the tolerant parse in relaylink would carry them — but a box running an
	// older daemon than the relay is the ordinary case in a fleet, and the frame
	// it was written against must not change under it.
	h := hub(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	control := newConn("control")
	stop := register(t, h, "box-1", control)
	defer stop()

	go func() { _ = h.Connect(ctx, "box-1", "", newConn("phone")) }()

	offer := string(control.recv(t).Data)
	if strings.Contains(offer, "proto") {
		t.Fatalf("an unlabelled guest produced a labelled offer: %s", offer)
	}
}

func TestALabelThatCouldBreakTheFrameIsRefused(t *testing.T) {
	// The offer is composed by string concatenation, so the character set is
	// what keeps a guest from writing their own JSON into the one message the
	// relay composes. Deleting validProto makes this test produce a frame with
	// an extra key in it.
	h := hub(t)
	control := newConn("control")
	stop := register(t, h, "box-1", control)
	defer stop()

	for _, bad := range []string{
		`console","evil":"`,
		`console"}`,
		"Console",
		"con sole",
		strings.Repeat("c", 33),
	} {
		err := h.Connect(context.Background(), "box-1", bad, newConn("guest"))
		if !errors.Is(err, rendezvous.ErrBadProto) {
			t.Errorf("label %q was accepted: %v", bad, err)
		}
	}
}
