package apps

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// `ctx.ui` — how an app puts something in front of the person wearing the
// glasses.
//
// ORCHESTRATOR.md §5's two sentences only look like a contradiction:
//
//   - **App code runs on the server**, sandboxed, never on the phone. That is
//     what keeps the author from ever seeing your transcript.
//   - **App UI renders in the phone app**, through a small declarative
//     vocabulary the host draws natively.
//
// This file is the join between them: an app yields data, this validates it
// against the vocabulary, redacts it, and hands it to whatever is in front of
// the user. Nothing third-party executes on the handset, and the app never
// learns anything about the phone — not its model, not its size, not whether
// anybody looked.

// Screen is where a view goes.
//
// Implemented outside this package, by the thing that has a phone on the other
// end. It is an interface for the same reason [Device] is: `internal/apps` must
// not know about WebSockets, and the test for "an app drew a card" must not need
// one.
type Screen interface {
	// Render hands a view to the transport. It returns when the frame has been
	// handed over, not when a human has looked at it — there is no signal for
	// the latter and inventing one would be an event we cannot observe.
	//
	// It returns [ErrNoPhone] when there is nowhere to draw.
	Render(ctx context.Context, r Rendered) error

	// Ask draws a view containing exactly one confirmation and waits for the
	// answer. False is both "no" and "nobody answered", and the caller cannot
	// tell them apart — see [UICap.Ask] for why that is deliberate.
	Ask(ctx context.Context, r Rendered) (bool, error)
}

// Rendered is one view on its way to the phone, with the app that drew it.
//
// The app is part of the frame rather than something the phone infers, because
// a card with no attribution is a card the user cannot act on: "which of my
// apps is asking me this" is the first question a confirmation raises.
type Rendered struct {
	AppID   string `json:"app"`
	AppName string `json:"appName,omitempty"`
	View    View   `json:"view"`
}

// ErrNoPhone is a box with nothing paired to draw on.
//
// Distinct from a missing capability: the app was granted this and there is
// simply no screen right now, which is a state that changes without the app
// being reinstalled. It answers [CodeUnavailable].
var ErrNoPhone = errors.New("no phone is paired with this box, so there is nowhere to draw")

// DefaultAskTimeout is how long one question may stand unanswered.
//
// It exists because the alternative is an app holding an invocation open until
// the wall-clock cap kills it, and reporting that as a crash rather than as
// nobody having answered.
const DefaultAskTimeout = 2 * time.Minute

// UIOptions configures the capability.
type UIOptions struct {
	Screen  Screen
	AppID   string
	AppName string
	// Granted is what the user consented to, which may be narrower than the
	// manifest asked for. It is checked per block, not per call: only `speak`
	// costs a scope.
	Granted []Scope
	// Redact is required for the same reason [GlassesOptions.Redact] is. A view
	// is text an app assembled, and an app that read a credential out of a
	// transcript and put it on a card must not have it drawn.
	Redact Redactor
	// AskTimeout caps one question. Zero means [DefaultAskTimeout].
	AskTimeout time.Duration
}

// UICap is the capability object.
type UICap struct {
	screen  Screen
	appID   string
	appName string
	granted []Scope
	redact  Redactor
	timeout time.Duration
}

// NewUI builds the capability. It refuses without a redactor — [ErrNoRedactor],
// the same one `internal/apps`'s memory capability refuses with — the same way
// [NewGlasses] refuses without an indicator: the check that must not be
// forgettable is the one made structurally impossible to forget.
func NewUI(o UIOptions) (*UICap, error) {
	if o.Redact == nil {
		return nil, ErrNoRedactor
	}
	timeout := o.AskTimeout
	if timeout <= 0 {
		timeout = DefaultAskTimeout
	}
	return &UICap{
		screen:  o.Screen,
		appID:   o.AppID,
		appName: o.AppName,
		granted: append([]Scope(nil), o.Granted...),
		redact:  o.Redact,
		timeout: timeout,
	}, nil
}

// Render validates a view and draws it.
//
// A view containing a confirmation is refused here and pointed at [UICap.Ask].
// The SDK's types allow it, and drawing it would put two buttons on someone's
// phone with nothing waiting for which one they press — a question whose answer
// goes nowhere is worse than no question, because the user believes they
// answered it. This is the host, so this is where that is decided.
func (u *UICap) Render(ctx context.Context, v View) error {
	clean, err := u.prepare(v)
	if err != nil {
		return err
	}
	if _, asks := clean.Confirm(); asks {
		return &ViewError{Message: "a view drawn with render() cannot contain a confirmation — " +
			"nothing would be waiting for the answer. Use ctx.ui.ask(), which draws the question " +
			"and resolves with what the user pressed"}
	}
	if u.screen == nil {
		return ErrNoPhone
	}
	return u.screen.Render(ctx, Rendered{AppID: u.appID, AppName: u.appName, View: clean})
}

// Ask draws a question and waits.
//
// False is returned for "no" and for "nobody answered", and the app cannot tell
// them apart. That is the whole point: an app must treat silence as a no, and
// giving it a third outcome is how "confirm before you send" becomes "send".
// The timeout is the host's, not the app's — an app cannot ask for a question
// that stands for an hour.
func (u *UICap) Ask(ctx context.Context, v View) (bool, error) {
	clean, err := u.prepare(v)
	if err != nil {
		return false, err
	}
	if _, asks := clean.Confirm(); !asks {
		return false, &ViewError{Message: "ask() needs a confirm block; a view with nothing to " +
			"answer would wait for a button that was never drawn"}
	}
	if u.screen == nil {
		return false, ErrNoPhone
	}

	ctx, cancel := context.WithTimeout(ctx, u.timeout)
	defer cancel()
	ok, err := u.screen.Ask(ctx, Rendered{AppID: u.appID, AppName: u.appName, View: clean})
	if err != nil {
		// Silence is a no, and it is not an error the app has to handle. An app
		// that got an exception here would have to decide what to do about it,
		// and the only safe decision is the one this makes for it.
		if errors.Is(err, context.DeadlineExceeded) {
			return false, nil
		}
		return false, err
	}
	return ok, nil
}

// prepare validates, redacts, and validates again.
//
// The order matters in both directions. Validating first means the app's error
// names the app's own string rather than a redacted one it never wrote.
// Validating again after redaction means what was checked is what is drawn:
// [Redactor] replaces a secret with a marker, and while a marker is shorter than
// the key it stood in for in every case we have, "in every case we have" is not
// a guarantee, and a view that grew past a cap between the check and the wire
// would be one the phone refuses with no explanation the app can read.
func (u *UICap) prepare(v View) (View, error) {
	checked, err := ParseView(v)
	if err != nil {
		return View{}, err
	}
	if err := CheckScopes(checked, u.granted); err != nil {
		return View{}, err
	}
	redacted := u.redactView(checked)
	final, err := ParseView(redacted)
	if err != nil {
		return View{}, fmt.Errorf("apps: redaction pushed this view past a limit: %w", err)
	}
	return final, nil
}

// redactView runs every string an app wrote through the detector.
//
// Every string, not only the ones that look like they might hold something: a
// field label is app-chosen text like everything else here, and a detector that
// is applied selectively is one whose coverage has to be re-argued each time
// somebody adds a field.
func (u *UICap) redactView(v View) View {
	out := View{Vocabulary: v.Vocabulary, Blocks: make([]Block, 0, len(v.Blocks))}
	for _, b := range v.Blocks {
		nb := Block{Kind: b.Kind}
		nb.Title = u.clean(b.Title)
		nb.Body = u.clean(b.Body)
		nb.Question = u.clean(b.Question)
		nb.ConfirmLabel = u.clean(b.ConfirmLabel)
		nb.CancelLabel = u.clean(b.CancelLabel)
		nb.Detail = u.clean(b.Detail)
		nb.Text = u.clean(b.Text)
		for _, f := range b.Fields {
			nb.Fields = append(nb.Fields, Field{Label: u.clean(f.Label), Value: u.clean(f.Value)})
		}
		for _, it := range b.Items {
			nb.Items = append(nb.Items, ListItem{
				Title:    u.clean(it.Title),
				Subtitle: u.clean(it.Subtitle),
				Detail:   u.clean(it.Detail),
			})
		}
		out.Blocks = append(out.Blocks, nb)
	}
	return out
}

func (u *UICap) clean(s string) string {
	if s == "" {
		return ""
	}
	out, _ := u.redact.Redact(s)
	return out
}
