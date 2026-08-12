package capture

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Ingester is the one door audio comes through.
//
// It owns three things and nothing else: the [Spool] the bytes land in, the
// consent [Registry] that decides whether they may, and the two paths that
// APPS-SCOPE.md §3 says are different products. Everything above it — the
// WebSocket in `internal/api`, the HTTP upload endpoint — talks to this
// interface rather than to files.
//
// # For whoever wires the transport
//
// The frames already exist (`internal/api/wire.go` defines `audio.chunk`,
// `sync.offer` and the `error` frame that carries a refusal) and today they
// answer `not_implemented, M4`. The mapping is:
//
//	audio.chunk  → Ingester.Live(device).Chunk(...)   // opened on the first chunk
//	              a refusal is CodeUnauthorized-shaped: consent, in words
//	sync.offer   → Ingester.Bulk().Offer(...)         // accept / defer / refuse
//	              declare + chunk + complete ride the resumable HTTP path in
//	              connector/src/protocol.ts, not the socket
//
// This package deliberately does not import `internal/api`: the dependency runs
// api → capture, and doing it the other way would make a cycle out of a wiring
// decision.
type Ingester struct {
	spool   *Spool
	consent *Registry
	live    LiveOptions
	now     func() time.Time
	log     *slog.Logger

	mu      sync.Mutex
	streams map[string]*LiveStream
	// byDevice is the currently-open live stream per device. `audio.chunk` on
	// the wire carries no stream id — the iOS client's sequence is "monotonic
	// per voice session" and nothing names the session — so the open turn *is*
	// the stream, exactly as APPS-SCOPE.md §3 describes Path A.
	byDevice map[string]string
}

// Options configures an [Ingester].
type Options struct {
	Spool   *Spool
	Consent *Registry
	Live    LiveOptions
	Now     func() time.Time
	Log     *slog.Logger
}

// New builds an ingester.
//
// It refuses without a consent registry. That is the same structural trick
// `internal/summarize` uses for the secret detector, and for the same reason:
// consent gating capture is a legal requirement in a two-party jurisdiction
// (ARCHITECTURE.md §6), and a constructor argument is how "check consent first"
// stops being a convention somebody can forget and becomes a thing the type
// system will not let you skip.
func New(o Options) (*Ingester, error) {
	if o.Spool == nil {
		return nil, errors.New("capture: no spool")
	}
	if o.Consent == nil {
		return nil, errors.New("capture: no consent registry, and ingesting without one is not allowed")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Live.ReorderWindow <= 0 {
		o.Live.ReorderWindow = DefaultReorderWindow
	}
	if o.Live.GapTimeout <= 0 {
		o.Live.GapTimeout = DefaultGapTimeout
	}
	if o.Live.RecentIDs <= 0 {
		o.Live.RecentIDs = DefaultRecentIDs
	}
	return &Ingester{
		spool: o.Spool, consent: o.Consent, live: o.Live,
		now: o.Now, log: o.Log,
		streams:  map[string]*LiveStream{},
		byDevice: map[string]string{},
	}, nil
}

// Spool is the audio store, for the sweeper's timer and the transcriber.
func (i *Ingester) Spool() *Spool { return i.spool }

// Consent is the registry, for the frames that update it.
func (i *Ingester) Consent() *Registry { return i.consent }

// Bulk is the nightly path.
func (i *Ingester) Bulk() *Bulk { return &Bulk{ing: i} }

// LiveSpec opens a voice turn.
type LiveSpec struct {
	// ID is the segment id. Empty means one is derived from the device and the
	// start time, which is stable enough to reopen after a reconnect within the
	// same turn.
	ID         string
	Device     string
	Codec      string
	SampleRate int
	Channels   int
	StartedAt  time.Time
	// UserInitiated marks a turn the wearer opened with a tap or the wake word.
	// It is what ScopeSession consents to, so it is recorded on the gate rather
	// than merely noted.
	UserInitiated bool
}

// OpenLive opens (or reopens) a live stream.
//
// Reopening is the normal case, not an error: the socket drops dozens of times
// a day and the phone replays from the head of its outbox. A second call for a
// device with a turn already open returns that turn.
func (i *Ingester) OpenLive(spec LiveSpec) (*LiveStream, error) {
	if strings.TrimSpace(spec.Device) == "" {
		return nil, errors.New("capture: a live stream needs a device")
	}
	at := spec.StartedAt
	if at.IsZero() {
		at = i.now()
	}
	if spec.UserInitiated {
		i.consent.Gate(spec.Device).StartSession()
	}
	if err := i.consent.Check(spec.Device, at); err != nil {
		return nil, err
	}

	// Held across the create, not just across the lookup. Two `audio.chunk`
	// frames can be dispatched concurrently on a reconnect, and a check-then-act
	// here would open two segments for one turn — splitting a sentence across
	// two files that each look complete.
	i.mu.Lock()
	defer i.mu.Unlock()

	if id, ok := i.byDevice[spec.Device]; ok {
		if s, ok := i.streams[id]; ok {
			return s, nil
		}
	}

	id := spec.ID
	if id == "" {
		id = liveID(spec.Device, at)
	}
	seg, err := i.spool.Create(SegmentSpec{
		ID: id, Device: spec.Device, Kind: KindLive,
		Codec: spec.Codec, SampleRate: spec.SampleRate, Channels: spec.Channels,
		StartedAt: at, Framed: true,
	})
	if err != nil {
		return nil, err
	}
	if seg.State != StateReceiving {
		return nil, fmt.Errorf("capture: segment %s is %s and cannot take more audio", id, seg.State)
	}

	s := &LiveStream{
		ing: i, id: id, device: spec.Device,
		codec:    seg.Codec,
		nextSeq:  seg.NextSeq,
		pending:  map[int64]pendingChunk{},
		recentAt: map[string]bool{},
	}
	// A restart mid-turn resumes from the manifest rather than re-accepting
	// chunks it already durably holds.
	for _, rid := range seg.RecentIDs {
		s.recent = append(s.recent, rid)
		s.recentAt[rid] = true
	}
	s.frames = seg.Frames

	i.streams[id] = s
	i.byDevice[spec.Device] = id
	return s, nil
}

// Live returns the open stream for a device, if there is one.
func (i *Ingester) Live(device string) (*LiveStream, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	id, ok := i.byDevice[device]
	if !ok {
		return nil, false
	}
	s, ok := i.streams[id]
	return s, ok
}

// CloseLive closes a device's open turn, if any.
func (i *Ingester) CloseLive(device string, at time.Time) (Segment, error) {
	s, ok := i.Live(device)
	if !ok {
		return Segment{}, fmt.Errorf("%w: no open stream for %s", ErrNoSegment, device)
	}
	seg, err := s.Close(at)
	if err != nil {
		return seg, err
	}
	// ScopeSession consent covers one conversation and lapses with it.
	i.consent.Gate(device).EndSession()
	return seg, nil
}

// OpenStreams is how many live turns are open, for the health surface.
func (i *Ingester) OpenStreams() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.streams)
}

func (i *Ingester) forget(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	s, ok := i.streams[id]
	if !ok {
		return
	}
	delete(i.streams, id)
	if i.byDevice[s.device] == id {
		delete(i.byDevice, s.device)
	}
}

// liveID derives a segment id from the device and the turn's start.
//
// The characters are restricted because the id becomes a filename: a device
// name from the wire that contains a slash would otherwise choose where audio
// is written.
func liveID(device string, at time.Time) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '-'
	}, device)
	// No dot in the format: [Spool.Create] refuses an id with one, and the
	// milliseconds are appended by hand rather than through ".000" for exactly
	// that reason.
	u := at.UTC()
	return fmt.Sprintf("live-%s-%s-%03d", safe, u.Format("20060102T150405"), u.Nanosecond()/int(time.Millisecond))
}
