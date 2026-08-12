package facts

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/luthor007/relay/relayd/internal/logx"
	"github.com/luthor007/relay/relayd/internal/summarize"
)

// Updater is MEMORY.md §4's live rule: every TurnCompleted re-runs fact
// extraction against that session only, and reconciles what comes back.
//
// It is extraction *and* reconciliation on purpose. internal/summarize
// deliberately stops at proposing, because §11 puts extraction and the review
// screen in the same step and writing facts before there is a screen to correct
// them on is how the unexamined inference store gets built. DASHBOARD.md §3.3's
// screen exists now, so this is the half that was waiting for it.
type Updater struct {
	extract Extractor
	store   *Store
	log     *slog.Logger
	now     func() time.Time

	// MinInterval throttles per session. A turn boundary can arrive every few
	// seconds and one model call per boundary is the difference between a
	// background task and a machine that is busy all day. Zero disables it,
	// which is what tests want.
	minInterval time.Duration

	mu   sync.Mutex
	last map[string]time.Time
}

// UpdaterOptions configures an [Updater].
type UpdaterOptions struct {
	Extractor Extractor
	Store     *Store
	// MinInterval defaults to DefaultMinInterval. Set it negative to disable.
	MinInterval time.Duration
	Log         *slog.Logger
	Now         func() time.Time
}

// DefaultMinInterval is how often one session may re-extract. Two minutes: long
// enough that a burst of short turns costs one call, short enough that a fact
// learned at the start of a session is on the screen before it ends.
const DefaultMinInterval = 2 * time.Minute

// NewUpdater builds the live fact path.
func NewUpdater(o UpdaterOptions) (*Updater, error) {
	if o.Store == nil {
		return nil, errors.New("facts: an updater needs a store")
	}
	u := &Updater{
		extract: o.Extractor, store: o.Store, log: o.Log, now: o.Now,
		minInterval: o.MinInterval, last: map[string]time.Time{},
	}
	if u.extract == nil {
		u.extract = None{}
	}
	if u.log == nil {
		u.log = logx.Discard()
	}
	if u.now == nil {
		u.now = time.Now
	}
	switch {
	case o.MinInterval < 0:
		u.minInterval = 0
	case o.MinInterval == 0:
		u.minInterval = DefaultMinInterval
	}
	return u, nil
}

// Update is one run of the live path.
type Update struct {
	Batch  Batch
	Result Result
	// Throttled is true when this session extracted too recently and the run
	// was skipped. It is reported rather than silent, because "no facts" and
	// "we did not look" lead to different conclusions.
	Throttled bool
}

// Session re-extracts and reconciles one session.
func (u *Updater) Session(ctx context.Context, sc Scope) (Update, error) {
	var out Update
	if err := sc.valid(); err != nil {
		return out, err
	}
	key := sc.Runtime + "/" + sc.SessionID
	now := u.now()

	if u.minInterval > 0 {
		u.mu.Lock()
		last, seen := u.last[key]
		if seen && now.Sub(last) < u.minInterval {
			u.mu.Unlock()
			out.Throttled = true
			out.Batch.Skipped = "extracted less than " + u.minInterval.String() + " ago"
			return out, nil
		}
		u.last[key] = now
		u.mu.Unlock()
	}

	batch, err := u.extract.Extract(ctx, sc)
	if err != nil {
		return out, err
	}
	out.Batch = batch
	if len(batch.Observations) == 0 {
		return out, nil
	}

	res, err := u.store.Reconcile(ctx, batch.Observations)
	out.Result = res
	if err != nil {
		return out, err
	}
	for _, r := range res.Rejected {
		u.log.Info("facts: refused an observation", "reason", r.Reason)
	}
	return out, nil
}

// Forget drops a session's throttle, for when a session ends.
func (u *Updater) Forget(runtime, sessionID string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.last, runtime+"/"+sessionID)
}

// ----------------------------------------------------- the summarize seam --

// Bridge adapts an [Updater] to summarize.FactExtractor, which is the interface
// summarize.Live already calls on every TurnCompleted.
//
// It exists so wiring step 5b into the live path is one assignment rather than
// a change to internal/summarize: that package extracts and proposes, this one
// decides and writes, and the seam between them is an interface neither owns.
type Bridge struct{ Updater *Updater }

var _ summarize.FactExtractor = Bridge{}

// Extract runs the updater and reports what actually landed in the tier.
//
// The returned facts are the *stored* ones, read back after reconciliation,
// not the model's proposals — an observation that was rejected for having no
// evidence, or suppressed because the user deleted that fact, must not be
// reported upward as though it had been learned.
func (b Bridge) Extract(ctx context.Context, scope summarize.FactScope) (summarize.FactResult, error) {
	var out summarize.FactResult
	if b.Updater == nil {
		out.Skipped = "no fact updater wired"
		return out, nil
	}
	up, err := b.Updater.Session(ctx, Scope{Runtime: scope.Runtime, SessionID: scope.SessionID})
	out.ModelCalls = up.Batch.ModelCalls
	out.Skipped = up.Batch.Skipped
	if err != nil {
		return out, err
	}
	if up.Throttled {
		return out, nil
	}

	for _, id := range append(append([]string{}, up.Result.Created...), up.Result.Updated...) {
		f, err := b.Updater.store.Get(ctx, id)
		if err != nil {
			continue
		}
		out.Facts = append(out.Facts, summarize.Fact{
			Predicate:  string(f.Predicate),
			Object:     f.Object,
			Text:       f.Text,
			Confidence: f.Confidence,
			Evidence:   toSummarizeEvidence(f.Evidence),
		})
	}
	return out, nil
}

func toSummarizeEvidence(in []Evidence) []summarize.Evidence {
	out := make([]summarize.Evidence, 0, len(in))
	for _, e := range in {
		out = append(out, summarize.Evidence{
			Runtime: e.Runtime, SessionID: e.SessionID, Path: e.Path,
			ByteOffset: e.ByteOffset, Quote: e.Quote, At: e.At,
		})
	}
	return out
}

// FromSummarize converts internal/summarize's proposals into observations, for
// a caller that already has a summarize.FactExtractor wired and wants the
// distilled tier behind it.
//
// It is lossy in one direction on purpose: summarize.Fact carries no citation
// structure, so every proposal arrives with the whole session's evidence
// attached. That is weaker provenance than [LLM] produces and the review screen
// shows it as such — which is the honest outcome, not a bug to paper over.
func FromSummarize(in []summarize.Fact) []Observation {
	out := make([]Observation, 0, len(in))
	for _, f := range in {
		p, ok := ParsePredicate(f.Predicate)
		if !ok {
			continue
		}
		ev := make([]Evidence, 0, len(f.Evidence))
		for _, e := range f.Evidence {
			ev = append(ev, Evidence{
				Runtime: e.Runtime, SessionID: e.SessionID, Path: e.Path,
				ByteOffset: e.ByteOffset, Quote: e.Quote, At: e.At,
			})
		}
		out = append(out, Observation{
			Subject: DefaultSubject, Predicate: p, Object: f.Object,
			Text: f.Text, Confidence: f.Confidence, Evidence: ev,
		})
	}
	return out
}
