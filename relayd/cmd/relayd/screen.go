package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/luthor007/relay/relayd/internal/api"
	"github.com/luthor007/relay/relayd/internal/apps"
)

// phoneScreen is [apps.Screen] served by the phone's WebSocket.
//
// It lives here rather than in either package for the reason vaultqueue.go and
// connectors.go do: `internal/api` must not import `internal/apps`, or the
// transport grows an opinion about the UI vocabulary, and `internal/apps` must
// not import `internal/api`, or the app runtime grows a dependency on
// WebSockets. The composition root is where the two meet, and the adapter is
// four methods long because there is nothing else to it.
//
// The seam is worth naming precisely, because it is what ORCHESTRATOR.md §5's
// two sentences rest on: the view crosses this boundary as **data**. Nothing
// third-party is executed on the handset, and the phone renders from a
// vocabulary it knows rather than from anything the app sent it.
type phoneScreen struct {
	srv *api.Server
	// AskTimeout bounds how long a question stands. It is the same value the
	// capability enforces; having it here too means the transport stops waiting
	// even if the capability was built with a longer one.
	askTimeout time.Duration
}

var _ apps.Screen = (*phoneScreen)(nil)

func newPhoneScreen(srv *api.Server) *phoneScreen {
	if srv == nil {
		return nil
	}
	return &phoneScreen{srv: srv, askTimeout: apps.DefaultAskTimeout}
}

func (p *phoneScreen) Render(ctx context.Context, r apps.Rendered) error {
	frame, err := p.frame(r)
	if err != nil {
		return err
	}
	return p.translate(p.srv.Draw(ctx, frame))
}

func (p *phoneScreen) Ask(ctx context.Context, r apps.Rendered) (bool, error) {
	frame, err := p.frame(r)
	if err != nil {
		return false, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(p.askTimeout)
	}
	ok2, err := p.srv.DrawAndAsk(ctx, frame, deadline)
	if err != nil {
		return false, p.translate(err)
	}
	return ok2, nil
}

// frame serialises the view for the wire.
//
// The view is already validated — [apps.UICap] parses, scope-checks and redacts
// before anything reaches a Screen — so this is a marshal and not a second
// gate. Putting a check here would be a second definition of the vocabulary in
// a package that deliberately does not have one.
func (p *phoneScreen) frame(r apps.Rendered) (api.UIRender, error) {
	raw, err := json.Marshal(r.View)
	if err != nil {
		return api.UIRender{}, err
	}
	return api.UIRender{App: r.AppID, AppName: r.AppName, View: raw}, nil
}

// translate turns the transport's "nobody is connected" into the app-facing
// error, so an app gets `unavailable` and a sentence about pairing rather than
// `failed` and a sentence about WebSockets.
func (p *phoneScreen) translate(err error) error {
	if errors.Is(err, api.ErrNoScreen) {
		return apps.ErrNoPhone
	}
	return err
}
