package apps

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Triggers — APP-PLATFORM.md §4. "Apps do not poll. They are woken by something
// that happened."
//
// That sentence is a resource decision as much as a design one. Five installed
// apps polling a box that also transcribes a sixteen-hour day is a box with no
// battery and no headroom; five installed apps that are processes only while
// something is happening is the same five apps costing nothing at rest. So there
// is no "list my apps and ask them each if they care" path in this package — a
// [Dispatcher] holds the index, and an app that declared no matching trigger is
// never started.
//
// The five kinds arrive from five different places, and the dispatcher is
// deliberately the only thing that knows all five:
//
//	phrase    the live transcript, from internal/transcript
//	touch     a gesture, from the phone bridge
//	memory    a pipeline event, from internal/episode (see pipeline.go)
//	schedule  a cron expression, from [Scheduler]
//	tool      the user's agent, through internal/apps/mcpbridge

// Dispatcher holds the installed apps and wakes the ones a trigger names.
type Dispatcher struct {
	rt *Runtime

	mu        sync.RWMutex
	installed map[string]Installed
	// lastFired is the schedule cursor per app, so a scheduler tick that covers
	// several minutes fires each app once rather than once per minute crossed.
	lastFired map[string]time.Time

	// MaxParallel bounds how many apps one event may start at once. Two, by
	// default: the box is the user's and it is also transcribing their day.
	maxParallel int
	loc         *time.Location
	now         func() time.Time
}

// DispatcherOptions configures a [Dispatcher].
type DispatcherOptions struct {
	Runtime *Runtime
	// MaxParallel bounds concurrent invocations. Defaults to
	// DefaultMaxParallelApps.
	MaxParallel int
	// Location is the user's timezone, for schedule triggers. APP-PLATFORM.md
	// §4 says "in the user's timezone" and means it: a cron expression
	// interpreted in UTC fires a "every morning at 8" app at midnight for half
	// the year.
	Location *time.Location
	Now      func() time.Time
}

// DefaultMaxParallelApps bounds how many apps one event may wake at once.
const DefaultMaxParallelApps = 2

// NewDispatcher builds a dispatcher.
func NewDispatcher(o DispatcherOptions) (*Dispatcher, error) {
	if o.Runtime == nil {
		return nil, errors.New("apps: a dispatcher needs a runtime")
	}
	if o.MaxParallel <= 0 {
		o.MaxParallel = DefaultMaxParallelApps
	}
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Dispatcher{
		rt: o.Runtime, installed: map[string]Installed{}, lastFired: map[string]time.Time{},
		maxParallel: o.MaxParallel, loc: o.Location, now: o.Now,
	}, nil
}

// Add installs an app into the dispatcher's index.
//
// It refuses an app this machine cannot contain, at the moment it is added
// rather than at the moment it is first triggered. A console that lists an app
// which fails on every wake word is worse than one that says "this box cannot
// run this app, and here is why".
func (d *Dispatcher) Add(inst Installed) error {
	if err := d.rt.CanRun(inst); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.installed[inst.Manifest.ID] = inst
	return nil
}

// Remove drops an app.
func (d *Dispatcher) Remove(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.installed, id)
	delete(d.lastFired, id)
}

// List returns the installed apps, by id.
func (d *Dispatcher) List() []Installed {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Installed, 0, len(d.installed))
	for _, i := range d.installed {
		out = append(out, i)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.ID < out[j].Manifest.ID })
	return out
}

// Get returns one installed app.
func (d *Dispatcher) Get(id string) (Installed, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	i, ok := d.installed[id]
	return i, ok
}

// Phrase wakes every app whose wake phrase appears in a stretch of transcript.
//
// A wake phrase competes with a speech recogniser's idea of where the commas and
// the hyphens go, so the match is on normalised text: lowercased, punctuation
// dropped, whitespace collapsed. Matching raw strings would make the app look
// broken and the author reach for a regexp.
//
// [PhraseMatches] also compares the two with all separators removed, which is
// what makes "stand-up", "stand up" and "standup" the same word. That can in
// principle match across a word boundary — a phrase ending in "the stand" would
// match "the standup" — and the trade is deliberate: a spurious wake on a
// multi-word phrase is rare and recoverable, and a wake phrase that never fires
// because the recogniser hyphenated is an app the user concludes is broken.
func (d *Dispatcher) Phrase(ctx context.Context, transcript string) []Invocation {
	if strings.TrimSpace(transcript) == "" {
		return nil
	}
	return d.fire(ctx, func(inst Installed) (TriggerFrame, bool) {
		for _, t := range inst.Manifest.Triggers {
			if t.Type != TriggerPhrase {
				continue
			}
			if PhraseMatches(t.Match, transcript) {
				return TriggerFrame{Type: TriggerPhrase, Transcript: transcript}, true
			}
		}
		return TriggerFrame{}, false
	})
}

// PhraseMatches reports whether a wake phrase appears in a transcript.
func PhraseMatches(phrase, transcript string) bool {
	want := normalisePhrase(phrase)
	if want == "" {
		return false
	}
	hay := normalisePhrase(transcript)
	if strings.Contains(hay, want) {
		return true
	}
	return strings.Contains(squash(hay), squash(want))
}

func squash(s string) string { return strings.ReplaceAll(s, " ", "") }

// Touch wakes every app that asked for this gesture.
func (d *Dispatcher) Touch(ctx context.Context, g Gesture) []Invocation {
	return d.fire(ctx, func(inst Installed) (TriggerFrame, bool) {
		for _, t := range inst.Manifest.Triggers {
			if t.Type == TriggerTouch && t.Gesture == g {
				return TriggerFrame{Type: TriggerTouch, Gesture: g}, true
			}
		}
		return TriggerFrame{}, false
	})
}

// Event wakes every app that asked for a pipeline event.
func (d *Dispatcher) Event(ctx context.Context, ev MemoryEvent, episodeID string) []Invocation {
	return d.fire(ctx, func(inst Installed) (TriggerFrame, bool) {
		for _, t := range inst.Manifest.Triggers {
			if t.Type == TriggerMemory && t.Event == ev {
				return TriggerFrame{Type: TriggerMemory, Event: ev, EpisodeID: episodeID}, true
			}
		}
		return TriggerFrame{}, false
	})
}

// Tool runs one app because the agent asked for it by name.
//
// Exposing the app to the agent as an MCP tool is `internal/apps/mcpbridge`'s
// job — APP-PLATFORM.md §8 step 4, and a separate package because the gateway,
// the grant and the confirmation are internal/mcp's rules rather than this
// package's. This is the other end of that: the trigger, checked against the
// manifest, because an app that did not declare itself callable has not been
// reviewed as something an agent may invoke on its own initiative.
func (d *Dispatcher) Tool(ctx context.Context, id string, args map[string]any) (Invocation, error) {
	inst, ok := d.Get(id)
	if !ok {
		return Invocation{}, fmt.Errorf("apps: %s is not installed", id)
	}
	if _, ok := inst.Manifest.ToolTrigger(); !ok {
		return Invocation{}, fmt.Errorf("apps: %s did not declare a tool trigger, so the agent cannot call it", id)
	}
	return d.rt.Invoke(ctx, inst, TriggerFrame{Type: TriggerTool, Arguments: args})
}

// Due lists the apps whose cron expression fired in (last, now]. It is exported
// so a scheduler outside this package — or a test with a fake clock — can drive
// the same decision.
func (d *Dispatcher) Due(now time.Time) []Installed {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Installed
	for id, inst := range d.installed {
		last, seen := d.lastFired[id]
		if !seen {
			// First sight of an app is not a firing. Otherwise every restart
			// would run every scheduled app, which for a "post yesterday's
			// summary" app means posting it again.
			d.lastFired[id] = now
			continue
		}
		for _, t := range inst.Manifest.Triggers {
			if t.Type != TriggerSchedule {
				continue
			}
			c, err := ParseCron(t.Cron)
			if err != nil {
				continue
			}
			next := c.Next(last, d.loc)
			if !next.IsZero() && !next.After(now) {
				out = append(out, inst)
				break
			}
		}
		d.lastFired[id] = now
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.ID < out[j].Manifest.ID })
	return out
}

// RunSchedule fires every app due at now.
func (d *Dispatcher) RunSchedule(ctx context.Context, now time.Time) []Invocation {
	due := d.Due(now)
	if len(due) == 0 {
		return nil
	}
	return d.run(ctx, due, func(Installed) TriggerFrame { return TriggerFrame{Type: TriggerSchedule} })
}

// fire selects and runs.
func (d *Dispatcher) fire(ctx context.Context, sel func(Installed) (TriggerFrame, bool)) []Invocation {
	type job struct {
		inst Installed
		tf   TriggerFrame
	}
	var jobs []job
	for _, inst := range d.List() {
		if tf, ok := sel(inst); ok {
			jobs = append(jobs, job{inst, tf})
		}
	}
	if len(jobs) == 0 {
		return nil
	}
	insts := make([]Installed, 0, len(jobs))
	frames := map[string]TriggerFrame{}
	for _, j := range jobs {
		insts = append(insts, j.inst)
		frames[j.inst.Manifest.ID] = j.tf
	}
	return d.run(ctx, insts, func(i Installed) TriggerFrame { return frames[i.Manifest.ID] })
}

func (d *Dispatcher) run(ctx context.Context, insts []Installed, frame func(Installed) TriggerFrame) []Invocation {
	out := make([]Invocation, len(insts))
	sem := make(chan struct{}, d.maxParallel)
	var wg sync.WaitGroup
	for i, inst := range insts {
		wg.Add(1)
		go func(i int, inst Installed) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			inv, err := d.rt.Invoke(ctx, inst, frame(inst))
			if err != nil && inv.Error == "" {
				inv.Error = err.Error()
			}
			out[i] = inv
		}(i, inst)
	}
	wg.Wait()
	return out
}

// normalisePhrase lowercases, drops punctuation and collapses whitespace.
func normalisePhrase(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			space = false
		default:
			if !space {
				b.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// Scheduler drives [Dispatcher.RunSchedule] on a ticker.
//
// Minute granularity, because a five-field cron expression has no finer
// resolution and a scheduler that ticks faster than its own vocabulary is
// burning wakeups to look responsive.
type Scheduler struct {
	d      *Dispatcher
	every  time.Duration
	stop   chan struct{}
	closed sync.Once
}

// NewScheduler builds a scheduler. every defaults to a minute.
func NewScheduler(d *Dispatcher, every time.Duration) *Scheduler {
	if every <= 0 {
		every = time.Minute
	}
	return &Scheduler{d: d, every: every, stop: make(chan struct{})}
}

// Run blocks until ctx is done or [Scheduler.Stop] is called.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			s.d.RunSchedule(ctx, s.d.now())
		}
	}
}

// Stop ends the scheduler.
func (s *Scheduler) Stop() { s.closed.Do(func() { close(s.stop) }) }
