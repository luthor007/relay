package compaction

import (
	"errors"
	"fmt"
	"strings"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/event"
)

// Numerator says which token count an [Observation] carries. It is a required
// field rather than a convenience, and the reason is ADAPTERS.md §2's cost
// trap.
//
// Claude Code's result.usage sums every request a turn took: turn 1 of the
// vendored fixture reports 51,997 cache-read tokens for a context that was
// actually ~33,600. Divided by a context window that number overstates pressure
// by the number of tool round trips, and a turn with eight tool calls would
// fire compaction — ten to sixty seconds of silence — on a session that is
// barely full. The same figure is perfectly correct for metering.
//
// So the two uses are separated at the type level. A caller has to say which
// number it is holding, and [Observe] refuses the wrong one.
type Numerator string

const (
	// NumeratorUnset is the zero value and is always an error. Defaulting it to
	// "live" would make the trap silent again for anyone who forgot the field.
	NumeratorUnset Numerator = ""

	// NumeratorLive is a genuine context size: the latest request's usage on
	// Claude Code (message_start / message_delta, which reads 33,497 → 33,609 →
	// 33,637 across the fixture — monotonic and true), or the thread running
	// total on Codex (tokenUsage.total).
	NumeratorLive Numerator = "live"

	// NumeratorTurn is a per-turn total, summed over the requests the turn took.
	// It is what metering wants and it is never a context size.
	NumeratorTurn Numerator = "turn"
)

var (
	// ErrTurnUsage is the refusal. It names the right source rather than only
	// the wrong one, because a caller holding the wrong number is one field away
	// from the right one.
	ErrTurnUsage = errors.New(
		"compaction: a per-turn usage sums the requests in a turn and is not a context size — " +
			"use the latest request's usage (Claude Code message_start) or the thread running total (Codex tokenUsage.total)")

	// ErrNumeratorUnset is returned for an observation that did not say where
	// its token count came from.
	ErrNumeratorUnset = errors.New("compaction: an observation must say whether its token count is live or per-turn")
)

// WindowSource records where a denominator came from, so a decision can say
// whether it divided by something the runtime reported or by something an
// installer guessed.
type WindowSource string

const (
	// WindowNone: there is no denominator. ACP carries no token field at all,
	// and Codex's modelContextWindow is int64|null even when the counts are
	// present.
	WindowNone WindowSource = ""
	// WindowRuntime: the runtime reported it. Claude Code gets this free from
	// result.modelUsage[<model>].contextWindow; Codex from modelContextWindow
	// when it is not null.
	WindowRuntime WindowSource = "runtime"
	// WindowFallback: a per-model window an installer configured, because the
	// runtime did not report one.
	WindowFallback WindowSource = "fallback"
)

// Windows is the fallback denominator table.
//
// It is empty by default and deliberately carries no built-in numbers. A window
// this package invented would be indistinguishable from one a runtime reported
// at the point of use, and the whole failure mode being guarded against here is
// a plausible number that is wrong. Whoever configures a model — the installer,
// or a model/list probe — supplies this.
type Windows struct {
	// ByModel is keyed on the canonical model name. See [CanonicalModel]: the
	// decorated id claude-opus-5[1m] must never be a table key.
	ByModel map[string]int64
	// Default applies to any model not in ByModel. Zero means "no fallback",
	// which is an honest answer and produces a degraded reading rather than a
	// confident wrong one.
	Default int64
}

// Lookup returns the fallback window for a model.
func (w Windows) Lookup(model string) (int64, bool) {
	if len(w.ByModel) > 0 {
		if v, ok := w.ByModel[CanonicalModel(model)]; ok && v > 0 {
			return v, true
		}
	}
	if w.Default > 0 {
		return w.Default, true
	}
	return 0, false
}

// CanonicalModel strips the decoration Claude Code puts on a model id.
//
// system/init reports claude-opus-5[1m] and only modelUsage's canonicalModel
// field carries the real name (ADAPTERS.md §2). Keying a table on the decorated
// form gives a miss for every long-context session, which is exactly the
// population this package cares about.
func CanonicalModel(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '['); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// Observation is one measurement handed to this package by whatever is watching
// a runtime. It is a plain struct with no methods so an adapter, a store reader
// or a test can build one.
type Observation struct {
	Runtime adapter.Runtime
	// Model is the model the session is running, decorated or not.
	Model string

	// Kind must be set. See [Numerator].
	Kind Numerator

	// Used is the token count. Have distinguishes "zero tokens" from "the
	// runtime did not say", which is the same rule event.Usage follows with
	// pointers: nil means unknown, never zero.
	Used int64
	Have bool

	// Window is the denominator the runtime reported. Zero means it reported
	// none — Codex's modelContextWindow is nullable, ACP has no such field.
	Window int64

	// Turns is how many turns this session has taken since it started or since
	// the last compaction, whichever is later. It is the degraded trigger for a
	// session with no measurable pressure at all.
	Turns int
}

// Reading is a context-pressure measurement, or an honest account of why there
// isn't one.
type Reading struct {
	Runtime adapter.Runtime
	Model   string

	Used   int64
	Window int64
	Source WindowSource
	Turns  int

	// why is the degradation reason, empty when the reading is real.
	why string
}

// Known reports whether this reading can produce a fraction at all.
func (r Reading) Known() bool { return r.Window > 0 && r.Used > 0 }

// Pressure is used/window in [0,1+], and ok is false when it could not be
// computed. It is never clamped: a session over its own window is a fact worth
// seeing rather than a 1.0 that looks like a threshold.
func (r Reading) Pressure() (float64, bool) {
	if !r.Known() {
		return 0, false
	}
	return float64(r.Used) / float64(r.Window), true
}

// Degraded is the sentence a console shows instead of a percentage. Empty when
// the reading is real.
//
// MEMORY.md §9: a runtime that reports tokens without reporting the window must
// degrade to "compact on idle after N turns" rather than silently never
// compacting. Half of "rather than silently" is this string.
func (r Reading) Degraded() string { return r.why }

// Estimated reports whether the denominator came from the fallback table rather
// than from the runtime. The pressure is usable; it is just not observed, and a
// decision made on it says so.
func (r Reading) Estimated() bool { return r.Source == WindowFallback }

// Observe turns an observation into a reading, applying the fallback table when
// the runtime reported no window.
func Observe(o Observation, w Windows) (Reading, error) {
	switch o.Kind {
	case NumeratorTurn:
		return Reading{}, ErrTurnUsage
	case NumeratorLive:
	default:
		return Reading{}, ErrNumeratorUnset
	}

	r := Reading{
		Runtime: o.Runtime,
		Model:   o.Model,
		Turns:   o.Turns,
	}
	if o.Have && o.Used > 0 {
		r.Used = o.Used
	}

	switch {
	case o.Window > 0:
		r.Window, r.Source = o.Window, WindowRuntime
	default:
		if v, ok := w.Lookup(o.Model); ok {
			r.Window, r.Source = v, WindowFallback
		}
	}

	switch {
	case r.Used == 0 && r.Window == 0:
		r.why = fmt.Sprintf("%s reports neither a context size nor a window; falling back to turns", label(o.Runtime))
	case r.Used == 0:
		r.why = fmt.Sprintf("%s reported a window but no context size; falling back to turns", label(o.Runtime))
	case r.Window == 0:
		r.why = fmt.Sprintf("no context window for model %q; falling back to turns", CanonicalModel(o.Model))
	}
	return r, nil
}

// FromLatestRequest is the Claude Code path: the numerator is the most recent
// request's usage and the denominator is modelUsage[<model>].contextWindow,
// which is exactly what the adapter's Session.Context() returns.
func FromLatestRequest(rt adapter.Runtime, model string, used, window int64, turns int, w Windows) (Reading, error) {
	return Observe(Observation{
		Runtime: rt,
		Model:   model,
		Kind:    NumeratorLive,
		Used:    used,
		Have:    used > 0,
		Window:  window,
		Turns:   turns,
	}, w)
}

// FromThreadTotal is the Codex path: pressure is a property of the whole thread,
// so the numerator is tokenUsage.total and not tokenUsage.last. The Codex
// adapter's Session.LastUsage() already returns the total for this reason.
//
// The window is event.Usage.ContextWindow, which is nil whenever Codex's
// modelContextWindow was null — and then the fallback table decides.
func FromThreadTotal(rt adapter.Runtime, model string, u *event.Usage, turns int, w Windows) (Reading, error) {
	o := Observation{Runtime: rt, Model: model, Kind: NumeratorLive, Turns: turns}
	if u != nil {
		for _, p := range []*int64{u.InputTokens, u.CachedInputTokens} {
			if p != nil {
				o.Used += *p
				o.Have = true
			}
		}
		if u.ContextWindow != nil && *u.ContextWindow > 0 {
			o.Window = *u.ContextWindow
		}
	}
	return Observe(o, w)
}

// FromTurnUsage always fails, and exists so the mistake has a name.
//
// TurnCompleted.Usage is not a pressure source on any of the three protocols.
// On Claude Code it sums the turn's requests and its ContextWindow is
// deliberately nil; on Codex it holds tokenUsage.last, which is one request out
// of a thread; on ACP it is nil entirely because the protocol has no usage
// object. A caller reaching for it gets [ErrTurnUsage] and the name of the
// right field rather than a number that looks fine.
func FromTurnUsage(event.TurnCompleted) (Reading, error) { return Reading{}, ErrTurnUsage }

// Unmeasured is the ACP path: three of the five runtimes report no tokens at
// all, so the only pressure signal is how many turns have gone by.
func Unmeasured(rt adapter.Runtime, model string, turns int) Reading {
	return Reading{
		Runtime: rt,
		Model:   model,
		Turns:   turns,
		why:     fmt.Sprintf("%s carries no token or usage field in its protocol; falling back to turns", label(rt)),
	}
}

func label(rt adapter.Runtime) string {
	if rt == "" {
		return "this runtime"
	}
	return string(rt)
}
