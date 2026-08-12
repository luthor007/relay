package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/luthor007/relay/relayd/internal/apps"
)

// The transport — SYSTEM.md §6.1's `ui.render`, and nothing new beside it.
//
// §6.1 already lists `ui.render` server→phone and `consent.decision`
// phone→server. A view travels in the first and the answer to a `confirm` comes
// back on the second, keyed by the render frame's id. That is the whole
// protocol addition, which is to say there is none.
//
// Reusing `consent.decision` rather than inventing a `ui.action` is the same
// argument internal/mcp's confirmation path makes about the NeedsInput path: a
// second channel for "the user answered a question" would have to re-earn
// everything the first one already enforces — the retraction when the question
// is gone, the re-ping at two minutes, the rule that quiet hours must not
// swallow it — and it would get one of them wrong.

// EnvelopeVersion is SYSTEM.md §6.1's `v`. Not the same number as
// [VocabularyVersion]: the envelope is the phone↔relayd contract and the
// vocabulary is what one frame happens to carry.
const EnvelopeVersion = 1

// RenderFrameType is the frame's `type`.
const RenderFrameType = "ui.render"

// RenderPayload is what a `ui.render` frame carries.
type RenderPayload struct {
	// App is which app drew it. Stamped by relayd and never by the app: a card
	// that could claim to be from another app is a phishing surface on a device
	// whose whole pitch is that you can trust what it shows you.
	App string `json:"app"`
	// Invocation ties a rendered card to a line in `relay logs`.
	Invocation string `json:"invocation,omitempty"`
	View       View   `json:"view"`
	// Expects is "decision" when the view contains a confirm, and empty
	// otherwise. The host answers on `consent.decision` carrying this frame's
	// id.
	Expects string `json:"expects,omitempty"`
}

// PayloadExpectsDecision is the only value [RenderPayload.Expects] takes.
const PayloadExpectsDecision = "decision"

// RenderFrame is SYSTEM.md §6.1's envelope carrying a view.
type RenderFrame struct {
	V       int           `json:"v"`
	ID      string        `json:"id"`
	Type    string        `json:"type"`
	At      int64         `json:"at"`
	Payload RenderPayload `json:"payload"`
}

// FrameMeta is what relayd knows about a render that the app does not get to
// decide.
type FrameMeta struct {
	ID         string
	At         time.Time
	App        string
	Invocation string
}

// NewRenderFrame validates a view and wraps it in the frame that carries it.
func NewRenderFrame(v View, meta FrameMeta) (RenderFrame, error) {
	checked, err := Validate(v)
	if err != nil {
		return RenderFrame{}, err
	}
	if meta.App == "" {
		return RenderFrame{}, viewErrf("a render frame must name the app that drew it")
	}
	if meta.ID == "" {
		return RenderFrame{}, viewErrf("a render frame needs an id, so an answer can name it")
	}
	f := RenderFrame{
		V:    EnvelopeVersion,
		ID:   meta.ID,
		Type: RenderFrameType,
		At:   meta.At.UnixMilli(),
		Payload: RenderPayload{
			App:        meta.App,
			Invocation: meta.Invocation,
			View:       checked,
		},
	}
	if ExpectsDecision(checked) {
		f.Payload.Expects = PayloadExpectsDecision
	}
	return f, nil
}

// ParseRenderFrame validates a frame off the wire — the host app's side, in Go,
// so a test can hold both ends of the format.
//
// A phone that received a frame it cannot fully validate draws nothing and says
// why. It does not draw the half it understood.
func ParseRenderFrame(data []byte) (RenderFrame, error) {
	var raw struct {
		V       *int             `json:"v"`
		ID      *string          `json:"id"`
		Type    *string          `json:"type"`
		At      *int64           `json:"at"`
		Payload *json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return RenderFrame{}, viewErrf("a frame must be a JSON object: %v", err)
	}
	if raw.V == nil || *raw.V != EnvelopeVersion {
		return RenderFrame{}, viewErrf("envelope version is not %d", EnvelopeVersion)
	}
	if raw.Type == nil || *raw.Type != RenderFrameType {
		return RenderFrame{}, viewErrf("this is not a %s frame", RenderFrameType)
	}
	if raw.ID == nil || *raw.ID == "" {
		return RenderFrame{}, viewErrf("a frame needs an id, so an answer can name it")
	}
	if raw.At == nil {
		return RenderFrame{}, viewErrf("at must be a unix millisecond timestamp")
	}
	if raw.Payload == nil {
		return RenderFrame{}, viewErrf("payload must be an object")
	}
	var p struct {
		App        *string          `json:"app"`
		Invocation string           `json:"invocation"`
		View       *json.RawMessage `json:"view"`
		Expects    string           `json:"expects"`
	}
	if err := json.Unmarshal(*raw.Payload, &p); err != nil {
		return RenderFrame{}, viewErrf("payload must be an object: %v", err)
	}
	if p.App == nil || *p.App == "" {
		return RenderFrame{}, viewErrf(
			"payload.app must name the app, so the host can label the card with something the app cannot forge")
	}
	if p.View == nil {
		return RenderFrame{}, viewErrf("payload.view is missing")
	}
	v, err := ParseView(*p.View)
	if err != nil {
		return RenderFrame{}, err
	}
	expects := ""
	if ExpectsDecision(v) {
		expects = PayloadExpectsDecision
	} else if p.Expects != "" {
		return RenderFrame{}, viewErrf(
			"payload.expects is set and no block asks a question; the host would wait for an answer nobody will give")
	}
	return RenderFrame{
		V: EnvelopeVersion, ID: *raw.ID, Type: RenderFrameType, At: *raw.At,
		Payload: RenderPayload{App: *p.App, Invocation: p.Invocation, View: v, Expects: expects},
	}, nil
}

// ViewSink is where a rendered view goes — internal/api's socket to the phone,
// in production.
//
// It is an interface here because internal/api belongs to another milestone and
// because a package that owns a wire format should not also own a WebSocket.
// Anything that can deliver a frame and report whether it landed satisfies it.
type ViewSink interface {
	// Render delivers one frame. An error means it did not land, and the caller
	// tells the app so rather than resolving.
	Render(ctx context.Context, f RenderFrame) error
}

// SinkFunc adapts a function to [ViewSink].
type SinkFunc func(ctx context.Context, f RenderFrame) error

// Render implements [ViewSink].
func (f SinkFunc) Render(ctx context.Context, fr RenderFrame) error { return f(ctx, fr) }

// ErrNoSurface is a render with nowhere to draw.
//
// A refusal and not a silent success: a box with no phone paired has no render
// surface, and an app told its card was drawn when nothing drew it will go on
// to say "as you can see above" into the void. The runtime's own answer to this
// is to not mint `ctx.ui` at all — absent, not refusing — and this is the second
// place, for the window where a phone disconnects mid-invocation.
var ErrNoSurface = errors.New("mcpbridge: nothing is paired to draw on")

// Renderer is the server side of `ctx.ui`.
//
// One invocation's worth: it knows which app is drawing, what that app was
// granted, and where the frames go. The four things it does are the four things
// that must not be left to the app — validate, refuse a block the grant did not
// pay for, stamp the app id, and say when there was nowhere to draw.
type Renderer struct {
	// App is the installed app doing the drawing.
	App apps.Installed
	// Invocation ties a card to a log line.
	Invocation string
	// Sink is where frames go. Nil means [ErrNoSurface].
	Sink ViewSink

	Now   func() time.Time
	NewID func() string
}

// Render validates, checks the grant, stamps and delivers one view.
func (r *Renderer) Render(ctx context.Context, v View) (RenderFrame, error) {
	checked, err := Validate(v)
	if err != nil {
		return RenderFrame{}, err
	}
	if err := CheckScopes(checked, r.App.Granted); err != nil {
		return RenderFrame{}, err
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	newID := uuid.NewString
	if r.NewID != nil {
		newID = r.NewID
	}
	f, err := NewRenderFrame(checked, FrameMeta{
		ID:         newID(),
		At:         now(),
		App:        r.App.Manifest.ID,
		Invocation: r.Invocation,
	})
	if err != nil {
		return RenderFrame{}, err
	}
	if r.Sink == nil {
		return RenderFrame{}, ErrNoSurface
	}
	if err := r.Sink.Render(ctx, f); err != nil {
		return RenderFrame{}, err
	}
	return f, nil
}
