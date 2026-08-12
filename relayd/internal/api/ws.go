package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/bus"
	"github.com/luthor007/relay/relayd/internal/event"
	"github.com/luthor007/relay/relayd/internal/registry"
	"github.com/luthor007/relay/relayd/internal/store"
)

// listDebounce collapses a burst of registry changes into one session.list
// frame. A turn produces hundreds of events and a phone does not want hundreds
// of lists.
const listDebounce = 250 * time.Millisecond

// outboundBuffer is how many frames may queue for one phone before the socket
// is closed. A phone that has stopped reading is gone; holding frames for it
// forever is how a daemon leaks.
const outboundBuffer = 64

// handleWS is SYSTEM.md §6.1: one authenticated WebSocket, JSON envelopes, both
// directions.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same-origin only, which is coder/websocket's default when no patterns
		// are given. The console is served from this origin and the phone is not
		// a browser, so nothing legitimate is cross-origin.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("api: websocket accept", "error", err)
		return
	}
	s.ServeSocket(r.Context(), c)
}

// ServeSocket runs the phone protocol over an already-established socket.
//
// Split out of [Server.handleWS] because a socket does not have to have arrived
// as an inbound request. SYSTEM.md §7's rendezvous relay works by both sides
// *dialling out*, so when a phone on cellular reaches a machine behind NAT, this
// daemon is the one that opened the TCP connection — and the protocol it then
// speaks is identical. Everything below this line is unchanged from the inbound
// path, which is the property that matters: there is no second implementation to
// keep in step, and no relay-only branch where an authorization check could be
// forgotten.
//
// It returns when the socket ends. The caller owns closing it.
func (s *Server) ServeSocket(parent context.Context, c *websocket.Conn) {
	defer c.CloseNow()

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	conn := &wsConn{srv: s, c: c, out: make(chan Envelope, outboundBuffer)}

	// Subscribe on this goroutine, before the opening frame goes out and before
	// anything else can run. Doing it inside pushLoop left a window between
	// accepting the socket and the subscription existing, and a ping published
	// in that window reached nobody: bus.Topic delivers to the subscribers that
	// exist at publish time, by design. A dropped informational ping is a
	// nuisance; a dropped blocking one is a session waiting on a human who was
	// never told, which is the single failure mode ADAPTERS.md §7 exists to
	// prevent.
	pings := s.pings.Subscribe("ws")
	defer pings.Close()
	changes := s.reg.Watch("ws")
	defer changes.Close()

	// Counted only now, after the subscription exists.
	//
	// It used to be counted first, which left the same window the comment above
	// describes — except on the other side of it: [Server.Clients] said a phone
	// was there while nothing was subscribed, so anything published in that
	// instant passed the "is there a screen" check and then reached nobody.
	// [Server.Draw] is the caller that made this visible, because it is the one
	// that answers the *app* on the strength of that count: an app was told its
	// card had been drawn and it had not been.
	s.addClient(1)
	defer s.addClient(-1)

	go conn.writeLoop(ctx, cancel)
	go conn.pushLoop(ctx, pings, changes)

	conn.send(ctx, s.sessionListFrame(ctx))
	conn.readLoop(ctx)
	cancel()
}

type wsConn struct {
	srv *Server
	c   *websocket.Conn
	out chan Envelope
}

func (w *wsConn) send(ctx context.Context, e Envelope) {
	select {
	case w.out <- e:
	case <-ctx.Done():
	default:
		// The phone is not reading. Dropping a frame silently would make a
		// store-and-forward queue believe it delivered; closing the socket makes
		// the phone reconnect and re-sync, which is the honest recovery.
		w.srv.log.Warn("api: phone socket is not draining, closing it")
		_ = w.c.Close(websocket.StatusPolicyViolation, "client is not reading")
	}
}

func (w *wsConn) writeLoop(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-w.out:
			b, err := json.Marshal(e)
			if err != nil {
				continue
			}
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err = w.c.Write(wctx, websocket.MessageText, b)
			wcancel()
			if err != nil {
				return
			}
		}
	}
}

// pushLoop is everything the server sends unprompted: pings, and a refreshed
// session list when the registry moves.
//
// The subscriptions are taken by the caller rather than here, so that they
// exist before the socket is announced as open. See handleWS.
func (w *wsConn) pushLoop(ctx context.Context, pings *bus.Sub[Ping], changes *bus.Sub[registry.Change]) {
	var pendingList <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return

		case p, ok := <-pings.C():
			if !ok {
				return
			}
			w.deliverPing(ctx, p)

		case _, ok := <-changes.C():
			if !ok {
				return
			}
			if pendingList == nil {
				pendingList = time.After(listDebounce)
			}

		case <-pendingList:
			pendingList = nil
			w.send(ctx, w.srv.sessionListFrame(ctx))
		}
	}
}

func (w *wsConn) deliverPing(ctx context.Context, p Ping) {
	now := w.srv.now()
	if p.Resolved != nil {
		w.send(ctx, w.srv.frame(TypeConfirmResolved, now, p.Resolved))
		return
	}
	// A mini-app's view is not a ping and is not batched with one: it rides this
	// topic because the topic is the fan-out to every transport. It goes out
	// alone, because everything below is built from p.Ping, which is zero here.
	if p.Render != nil {
		w.send(ctx, w.srv.frame(TypeUIRender, now, p.Render))
		return
	}
	// The confirmation goes first: it is the thing that needs an answer, and a
	// phone that renders the notification before the action has something to
	// attach it to.
	if p.Confirm != nil {
		w.send(ctx, w.srv.frame(TypeConfirmRequest, now, p.Confirm))
	}
	if p.Speak != nil {
		w.send(ctx, w.srv.frame(TypeSpeak, now, p.Speak))
	}
	if p.Notify != nil {
		w.send(ctx, w.srv.frame(TypeNotify, now, p.Notify))
	}
}

func (w *wsConn) readLoop(ctx context.Context) {
	for {
		typ, data, err := w.c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			w.send(ctx, w.srv.errFrame("", CodeBadEnvelope, "frames are JSON text", ""))
			continue
		}
		env, err := Decode(data)
		if err != nil {
			code := CodeBadEnvelope
			if errors.Is(err, ErrBadVersion) {
				code = CodeBadVersion
			}
			w.send(ctx, w.srv.errFrame(env.ID, code, err.Error(), ""))
			continue
		}
		w.handle(ctx, env)
	}
}

// handle dispatches one phone→server frame.
func (w *wsConn) handle(ctx context.Context, e Envelope) {
	s := w.srv
	switch e.Type {

	case TypeUtterance:
		u, err := Bind[Utterance](e)
		if err != nil {
			w.send(ctx, s.errFrame(e.ID, CodeBadPayload, err.Error(), ""))
			return
		}
		// Turn-taking is orchestrator-owned (ORCHESTRATOR.md §3) and the ping
		// policy reads it: a completion waits for a gap, a blocked session waits
		// only for the sentence to end. So the socket drives the gate directly
		// from ASR endpointing rather than guessing.
		if s.gate != nil {
			if u.Final {
				s.gate.EndUtterance()
			} else {
				s.gate.StartUtterance()
			}
		}
		if s.utterances == nil {
			w.send(ctx, s.errFrame(e.ID, CodeNotImplemented,
				"no router is wired, so relayd cannot decide which session this belongs to",
				"M1 step 3 — manual routing (internal/routing)"))
			return
		}
		if err := s.utterances.Utterance(ctx, u); err != nil {
			w.send(ctx, s.errFrame(e.ID, CodeFailed, err.Error(), ""))
			return
		}
		w.ack(ctx, e)

	case TypeTouch:
		t, err := Bind[Touch](e)
		if err != nil {
			w.send(ctx, s.errFrame(e.ID, CodeBadPayload, err.Error(), ""))
			return
		}
		if s.devices != nil {
			if err := s.devices.Touch(ctx, t); err != nil {
				w.send(ctx, s.errFrame(e.ID, CodeFailed, err.Error(), ""))
				return
			}
		}
		w.ack(ctx, e)

	case TypeWear:
		v, err := Bind[Wear](e)
		if err != nil {
			w.send(ctx, s.errFrame(e.ID, CodeBadPayload, err.Error(), ""))
			return
		}
		if s.devices != nil {
			if err := s.devices.Wear(ctx, v); err != nil {
				w.send(ctx, s.errFrame(e.ID, CodeFailed, err.Error(), ""))
				return
			}
		}
		w.ack(ctx, e)

	case TypeSessionCommand:
		cmd, err := Bind[SessionCommand](e)
		if err != nil {
			w.send(ctx, s.errFrame(e.ID, CodeBadPayload, err.Error(), ""))
			return
		}
		w.sessionCommand(ctx, e, cmd)

	case TypeConsentDecision:
		d, err := Bind[ConsentDecision](e)
		if err != nil {
			w.send(ctx, s.errFrame(e.ID, CodeBadPayload, err.Error(), ""))
			return
		}
		reply := event.Reply{
			OptionID:  d.Option,
			Decision:  event.DecisionDeny,
			Interrupt: d.Interrupt,
			Message:   d.Message,
		}
		if d.Approved {
			reply.Decision = event.DecisionAllow
		}
		if err := s.answer(ctx, d.ActionID, reply); err != nil {
			w.send(ctx, s.errFrame(e.ID, CodeFailed, err.Error(), ""))
			return
		}
		w.ack(ctx, e)

	// TODO(M4): capture. SYSTEM.md §10 steps 5–6 — capture → transcript →
	// episodes. The frames are defined and the envelope carries them; what does
	// not exist yet is the pipeline behind them. Answering with a named
	// milestone beats accepting bytes into a void, because a phone that believes
	// its upload landed will delete it.
	case TypeAudioChunk, TypePhoto, TypeSyncOffer:
		w.send(ctx, s.errFrame(e.ID, CodeNotImplemented,
			"relayd is not capturing yet, so this was not stored — keep it on the device",
			"M4 — capture and memory (SYSTEM.md §10 steps 5–6)"))

	default:
		w.send(ctx, s.errFrame(e.ID, CodeUnknownType, "no such message type: "+e.Type, ""))
	}
}

func (w *wsConn) sessionCommand(ctx context.Context, e Envelope, cmd SessionCommand) {
	s := w.srv
	fail := func(err error) {
		code := CodeFailed
		switch {
		case errors.Is(err, registry.ErrNoSuchSession), errors.Is(err, store.ErrNotFound):
			code = CodeNoSuchSession
		case errors.Is(err, adapter.ErrUnsupported):
			code = CodeUnsupported
		}
		w.send(ctx, s.errFrame(e.ID, code, err.Error(), ""))
	}

	switch cmd.Command {
	case "list", "":
		w.send(ctx, s.sessionListFrame(ctx))

	case "send":
		if _, err := s.reg.Send(ctx, cmd.Session, adapter.Turn{Text: cmd.Text}); err != nil {
			fail(err)
			return
		}
		w.ack(ctx, e)

	case "steer":
		// Verified absent on all three ACP runtimes. The error says so, and the
		// caller cancels and re-prompts rather than being told "failed".
		if err := s.reg.Steer(ctx, cmd.Session, cmd.Turn, adapter.Turn{Text: cmd.Text}); err != nil {
			fail(err)
			return
		}
		w.ack(ctx, e)

	case "cancel":
		if err := s.reg.Cancel(ctx, cmd.Session, cmd.Turn); err != nil {
			fail(err)
			return
		}
		w.ack(ctx, e)

	case "close":
		if err := s.reg.Close(ctx, cmd.Session); err != nil {
			fail(err)
			return
		}
		w.ack(ctx, e)

	case "answer":
		d, err := decision(cmd.Decision)
		if err != nil {
			w.send(ctx, s.errFrame(e.ID, CodeBadPayload, err.Error(), ""))
			return
		}
		err = s.reg.AnswerQuestion(ctx, cmd.Session, cmd.Question, event.Reply{
			OptionID: cmd.Option, Decision: d, Interrupt: cmd.Interrupt, Message: cmd.Text,
		})
		if err != nil {
			fail(err)
			return
		}
		w.ack(ctx, e)

	default:
		w.send(ctx, s.errFrame(e.ID, CodeUnknownType,
			"no such session command: "+cmd.Command, ""))
	}
}

func (w *wsConn) ack(ctx context.Context, e Envelope) {
	w.send(ctx, w.srv.frame(TypeAck, w.srv.now(), Ack{Re: e.ID, OK: true}))
}

// ------------------------------------------------------------- framing --

func (s *Server) frame(typ string, at time.Time, payload any) Envelope {
	e, err := Frame(s.newID(), typ, at, payload)
	if err != nil {
		s.log.Error("api: encode frame", "type", typ, "error", err)
		return Envelope{V: Version, ID: s.newID(), Type: TypeError, At: at.UnixMilli()}
	}
	return e
}

func (s *Server) errFrame(re, code, msg, milestone string) Envelope {
	return s.frame(TypeError, s.now(), ErrorPayload{
		Re: re, Code: code, Message: msg, Milestone: milestone,
	})
}

func (s *Server) sessionListFrame(ctx context.Context) Envelope {
	list, err := s.sessionList(ctx, store.SessionFilter{})
	if err != nil {
		return s.errFrame("", CodeFailed, err.Error(), "")
	}
	return s.frame(TypeSessionList, s.now(), list)
}

// Speak pushes a line to every attached phone outside the ping path — the
// routing announcement, which SYSTEM.md §7b calls the acknowledgement: "adding
// that to the payments refactor", spoken the moment routing decides.
func (s *Server) Speak(text string, sessionID string) {
	// The Speak frame is built directly rather than derived, because this path
	// never went through the ping policy: routing decided, and the
	// acknowledgement is the decision. The embedded bus.Ping carries only what
	// was observed — an id, a time, the line — and no delivery flag, since
	// there is no policy here for one to be the answer to.
	s.pings.Publish(Ping{
		Ping:  bus.Ping{ID: s.newID(), At: s.now(), Line: text},
		Speak: &Speak{Text: text, Session: sessionID},
	})
}
