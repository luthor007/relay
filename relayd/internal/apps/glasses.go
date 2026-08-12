package apps

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/luthor007/relay/relayd/internal/index"
)

// Finding is `internal/index`'s, not a second copy. MEMORY.md §12.2's recall
// figures were measured against that one ruleset and belong to it.
type Finding = index.Finding

// Detector returns the measured secret detector.
func Detector() Redactor { return index.MustDetector() }

// The glasses capability, and the one rule in APP-PLATFORM.md §3 that is written
// as a promise about the future: *there is no "record without indication" scope,
// and there never will be. The LEDs are wired to capture and apps cannot address
// them.*
//
// A promise in a document is not a mechanism, so here it is three mechanisms:
//
//  1. [capabilities] has no method that addresses the indicators and [Scopes]
//     has no scope that could pay for one. A capability that is not in the table
//     cannot be minted, and the table is walked by a test.
//  2. [NewGlasses] refuses to build without an [Indicator]. A camera capability
//     that can exist without the thing that lights the LED is the bug; making it
//     unconstructable is the fix.
//  3. [GlassesCap.Capture] calls the indicator *first* and returns its error
//     **without capturing**. There is no argument that skips it, no options
//     struct with a `silent` field, and no ordering in which the still exists
//     before the indication does.
//
// The third is the one that matters. If indication were best-effort — light the
// LED, then capture regardless — then a box whose indicator had failed would
// still take pictures, silently, which is precisely the product this section
// exists to refuse to build.

// Indicator is the bystander-visible recording indication: ARCHITECTURE.md §6,
// and a legal requirement in the user's home jurisdiction rather than a
// preference. It is an interface because the LEDs are on the glasses and this
// package is on the box; the implementation lives with the device link.
type Indicator interface {
	// Indicate raises the capture indication and returns once it is up. An error
	// means the indication did not happen — and so neither does the capture.
	Indicate(ctx context.Context, reason string) error
}

// IndicatorFunc adapts a function to [Indicator].
type IndicatorFunc func(ctx context.Context, reason string) error

func (f IndicatorFunc) Indicate(ctx context.Context, reason string) error { return f(ctx, reason) }

// ErrNoIndicator is a glasses capability built without an indicator.
var ErrNoIndicator = errors.New(
	"apps: no capture indicator, and a camera capability that can exist without one is the thing " +
		"APP-PLATFORM.md §3 says will never exist")

// ErrIndicationFailed is a capture that did not happen because the indication
// did not.
var ErrIndicationFailed = errors.New("apps: capture indication failed, so nothing was captured")

// Device is the glasses, as this package needs them.
type Device interface {
	// Say speaks and returns when playback finishes, so a sequence of calls does
	// not talk over itself.
	Say(ctx context.Context, text string) error
	// Capture takes a still. It is never called without [Indicator.Indicate]
	// having returned nil first.
	Capture(ctx context.Context, immediate bool) (CaptureResult, error)
	// Listen returns one utterance's transcript.
	Listen(ctx context.Context, timeout time.Duration) (string, error)
}

// CaptureResult is one still.
type CaptureResult struct {
	ID string `json:"id"`
	// Data is present only for an immediate capture. The default syncs with the
	// day rather than paying a BLE transfer nobody is waiting on.
	Data []byte `json:"data,omitempty"`
}

// GlassesOptions configures a [GlassesCap].
type GlassesOptions struct {
	Device    Device
	Indicator Indicator
	AppID     string
	AppName   string
	// Redact is required: an app's spoken text is text, and text goes through
	// the detector before it is logged.
	Redact Redactor
	// ListenTimeout is the ceiling on one `listen`, whatever the app asked for.
	ListenTimeout time.Duration
}

// DefaultListenTimeout is how long one `listen` may hold the microphone.
const DefaultListenTimeout = 15 * time.Second

// GlassesCap serves the `glasses.*` methods.
type GlassesCap struct {
	dev     Device
	ind     Indicator
	appID   string
	appName string
	redact  Redactor
	listen  time.Duration
}

// NewGlasses builds the glasses capability. It refuses without an indicator —
// see [ErrNoIndicator].
func NewGlasses(o GlassesOptions) (*GlassesCap, error) {
	if o.Device == nil {
		return nil, errors.New("apps: glasses capability needs a device")
	}
	if o.Indicator == nil {
		return nil, ErrNoIndicator
	}
	if o.Redact == nil {
		return nil, ErrNoRedactor
	}
	if o.ListenTimeout <= 0 {
		o.ListenTimeout = DefaultListenTimeout
	}
	return &GlassesCap{dev: o.Device, ind: o.Indicator, appID: o.AppID, appName: o.AppName,
		redact: o.Redact, listen: o.ListenTimeout}, nil
}

// Say speaks through the glasses.
func (g *GlassesCap) Say(ctx context.Context, text string) error {
	clean, _ := g.redact.Redact(text)
	return g.dev.Say(ctx, clean)
}

// Capture takes a still, after the indication and never without it.
func (g *GlassesCap) Capture(ctx context.Context, immediate bool) (CaptureResult, error) {
	who := g.appName
	if who == "" {
		who = g.appID
	}
	if err := g.ind.Indicate(ctx, who+" is taking a photo"); err != nil {
		return CaptureResult{}, fmt.Errorf("%w: %v", ErrIndicationFailed, err)
	}
	return g.dev.Capture(ctx, immediate)
}

// Listen opens the microphone for one utterance.
//
// The microphone is capture, so it indicates too. `glasses.audio` reads "live
// microphone during an open voice session" and an open voice session is one the
// wearer started — but the app is asking for the microphone here, and the people
// in the room are entitled to the same indication a still gets.
func (g *GlassesCap) Listen(ctx context.Context, timeout time.Duration) (string, error) {
	if timeout <= 0 || timeout > g.listen {
		timeout = g.listen
	}
	who := g.appName
	if who == "" {
		who = g.appID
	}
	if err := g.ind.Indicate(ctx, who+" is listening"); err != nil {
		return "", fmt.Errorf("%w: %v", ErrIndicationFailed, err)
	}
	return g.dev.Listen(ctx, timeout)
}

// --------------------------------------------------------------- doubles --

// RecordingIndicator records that the indication was raised. It is the test
// double, and it is also what a box with no glasses attached should use, because
// an app that asks for the camera on a box with no camera should get an error
// about the camera rather than a silent success.
type RecordingIndicator struct {
	mu      sync.Mutex
	Reasons []string
	// Fail makes every indication fail, which is how
	// TestCaptureDoesNotHappenWhenIndicationFails proves the ordering.
	Fail error
}

func (r *RecordingIndicator) Indicate(_ context.Context, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Fail != nil {
		return r.Fail
	}
	r.Reasons = append(r.Reasons, reason)
	return nil
}

// Raised is how many times the indication went up.
func (r *RecordingIndicator) Raised() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Reasons)
}

// FakeDevice is a [Device] that records what it was asked to do.
type FakeDevice struct {
	mu       sync.Mutex
	Said     []string
	Captures int
	Heard    []string
	SayErr   error
}

func (d *FakeDevice) Say(_ context.Context, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.SayErr != nil {
		return d.SayErr
	}
	d.Said = append(d.Said, text)
	return nil
}

func (d *FakeDevice) Capture(_ context.Context, immediate bool) (CaptureResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Captures++
	res := CaptureResult{ID: fmt.Sprintf("still-%d", d.Captures)}
	if immediate {
		res.Data = []byte{0xff, 0xd8, 0xff}
	}
	return res, nil
}

func (d *FakeDevice) Listen(_ context.Context, _ time.Duration) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.Heard) == 0 {
		return "", nil
	}
	out := d.Heard[0]
	d.Heard = d.Heard[1:]
	return out, nil
}

// Spoken returns everything said, for assertions.
func (d *FakeDevice) Spoken() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.Said...)
}
