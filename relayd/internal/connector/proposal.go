package connector

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/luthor007/relay/relayd/internal/mcp"
)

// Proposals — ORCHESTRATOR.md §4b.
//
//	At install, take only what the product cannot work without. […] After that,
//	connectors are proposed in context, from evidence. This is where the capture
//	pipeline earns its place — the extraction step already names the tools and
//	services someone talks about. So:
//
//	  > You have mentioned your Prusa four times this week. Want me to connect
//	  > it? I could queue prints and tell you when they finish.
//
//	That is a better prompt than a checkbox, because it arrives at the moment the
//	capability would have been useful, with a reason the user recognises as true.
//	It is also the honest version of "the system suggests connectors" from
//	AGENT-BRIEF §8 — grounded in something observed, not guessed.
//
// Three properties of this type follow from that paragraph, and each is
// enforced rather than documented:
//
//   - **A proposal needs evidence.** No mentions, no proposal. There is no code
//     path that produces one from a connector list alone, which is what stops
//     this becoming "a settings screen that talks" (§6 build order, step 7).
//   - **A proposal cannot grant.** [Proposer] holds an [mcp.Grants], whose only
//     method is a read. Accepting a proposal means calling [Grants.Grant] with
//     Decided set, from whichever surface the human used.
//   - **Only the read half is ever proposed.** §4b: most useful connectors need
//     only the read half, and the write half should cost a second decision. A
//     write half offered alongside the read one is not a second decision, it is
//     the same click.

// Defaults for the proposal window.
const (
	// DefaultWindow is how far back evidence counts. A week, because that is
	// the span the example sentence uses and because it is the longest span a
	// person will recognise as "recently" without checking.
	DefaultWindow = 7 * 24 * time.Hour

	// DefaultMinEpisodes is how many separate conversations must mention
	// something. Separate conversations, not separate sentences: saying
	// "Prusa" four times inside one rant is one occasion, and treating it as
	// four is how a suggestion engine becomes a nuisance.
	DefaultMinEpisodes = 3

	// DefaultCooldown is how long a dismissed proposal stays dismissed.
	DefaultCooldown = 30 * 24 * time.Hour
)

// Evidence is one observed mention, from the capture pipeline's extraction
// step. Text is the extracted mention or utterance — never a raw transcript,
// which stays on disk in place (MEMORY.md §3).
type Evidence struct {
	// Episode is the conversation it came from. Mentions are counted per
	// episode, so this is what makes "four times this week" mean four
	// occasions.
	Episode string
	At      time.Time
	Text    string
	// Entities are the extracted entity strings, when the pipeline produced
	// them. They are matched alongside Text.
	Entities []string
}

// Episode is the shape the capture pipeline will hand over. It is accepted as
// well as [Evidence] so this package is ready for episodes without depending on
// the tier that produces them.
type Episode struct {
	ID       string
	At       time.Time
	Text     string
	Entities []string
}

// Proposal is one suggestion, with the reason attached.
type Proposal struct {
	Connector string     `json:"connector"`
	Title     string     `json:"title,omitempty"`
	Access    mcp.Access `json:"access"`

	// Evidence is the sentence that says why now — "You have mentioned your
	// Prusa four times this week." Built from counts, never generated.
	Evidence string `json:"evidence"`
	// Opens is what granting it would let the agent do that it cannot now, in
	// the connector's own words.
	Opens string `json:"opens"`
	// Scopes is the single scope this proposal would grant.
	Scopes []string `json:"scopes"`

	Episodes int       `json:"episodes"`
	Mentions int       `json:"mentions"`
	FirstAt  time.Time `json:"first_at"`
	LastAt   time.Time `json:"last_at"`
}

// Line is the whole prompt, in the shape §4b gives it.
func (p Proposal) Line() string {
	name := p.Title
	if name == "" {
		name = p.Connector
	}
	s := p.Evidence + " Want me to connect it?"
	if strings.TrimSpace(p.Opens) != "" {
		s += " " + capitalizeFirst(strings.TrimSpace(p.Opens))
		if !strings.HasSuffix(s, ".") {
			s += "."
		}
	}
	return s
}

// Proposer turns evidence into proposals.
type Proposer struct {
	// Set is what could be connected. A connector not in the set is never
	// proposed, however often it is mentioned.
	Set *Set

	// Granted is consulted so an already-connected service is not proposed
	// again. Its only method is a read: this type has no way to grant anything,
	// and that is the point.
	Granted mcp.Grants

	// Window, MinEpisodes and Cooldown default to the constants above.
	Window      time.Duration
	MinEpisodes int
	Cooldown    time.Duration

	// Memory persists the evidence and the dismissals. Nil keeps everything in
	// this process, which is what every test in this package uses and what the
	// type did before there was a store.
	//
	// It is not an optimisation. Two things break without it, and both are the
	// paragraph above turned inside out. Evidence resets on every restart, so a
	// daemon restarted daily can never reach DefaultMinEpisodes over a seven-day
	// window — the feature is silently off on exactly the machines that reboot.
	// And a dismissal is forgotten, so the connector the user said no to is
	// proposed again the next morning, which is the failure [Proposer.Dismiss]
	// exists to prevent.
	Memory ProposalMemory

	Now func() time.Time
	Log *slog.Logger

	mu        sync.Mutex
	loaded    bool
	seen      map[string][]sighting
	dismissed map[string]time.Time
}

// StoredSighting is one persisted mention.
//
// It carries no text, and that is enforced by the type rather than by manners:
// there is no field for the sentence, so a store implementing this interface
// has nothing to write one from. See migration 0004.
type StoredSighting struct {
	Connector string
	Episode   string
	At        time.Time
}

// ProposalMemory is where evidence and dismissals survive a restart.
type ProposalMemory interface {
	// Sightings returns everything recorded, oldest or newest first — the
	// proposer sorts nothing and only counts.
	Sightings(ctx context.Context) ([]StoredSighting, error)
	// AddSighting records one mention. Never the text of it.
	AddSighting(ctx context.Context, connector, episode string, at time.Time) error
	// Dismissals returns each connector's most recent "not now".
	Dismissals(ctx context.Context) (map[string]time.Time, error)
	// PutDismissal records one, replacing any earlier answer for the same
	// connector: the cooldown runs from the latest decision.
	PutDismissal(ctx context.Context, connector string, at time.Time, reason string) error
	// Expire drops evidence older than the window, so the table does not grow
	// for the life of the machine holding mentions that can no longer count.
	Expire(ctx context.Context, before time.Time) error
}

func (p *Proposer) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// load pulls persisted evidence in once, on the first Observe or Proposals.
//
// The whole read happens under the lock. It runs once per process against a
// local SQLite table with an index on it, and the alternative — releasing the
// lock to read and re-taking it to merge — lets a concurrent Observe land a
// sighting that the merge then duplicates.
func (p *Proposer) load(ctx context.Context) {
	if p.Memory == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded {
		return
	}
	p.loaded = true

	stored, err := p.Memory.Sightings(ctx)
	if err != nil {
		// Degrade to this process's own memory rather than refusing to propose.
		// The consequence is stated so it is not mistaken for nothing: evidence
		// from before this restart is lost, so a suggestion may take longer to
		// arrive than the user's history warrants.
		p.log().Warn("connector: could not read stored proposal evidence; "+
			"only mentions from this run will count", "err", err)
	}
	if p.seen == nil {
		p.seen = map[string][]sighting{}
	}
	for _, s := range stored {
		name := strings.ToLower(strings.TrimSpace(s.Connector))
		p.seen[name] = append(p.seen[name], sighting{episode: s.Episode, at: s.At})
	}

	dis, err := p.Memory.Dismissals(ctx)
	if err != nil {
		// This one is worse than the sighting failure and is logged as such: a
		// forgotten dismissal means asking again, which §4b names as how
		// blind-accept is trained.
		p.log().Warn("connector: could not read stored dismissals; a connector the user "+
			"already declined may be proposed again", "err", err)
	}
	if p.dismissed == nil {
		p.dismissed = map[string]time.Time{}
	}
	for k, v := range dis {
		p.dismissed[strings.ToLower(strings.TrimSpace(k))] = v
	}
}

type sighting struct {
	episode string
	at      time.Time
}

// NewProposer builds a proposer over a set.
func NewProposer(s *Set, granted mcp.Grants) *Proposer {
	return &Proposer{Set: s, Granted: granted}
}

func (p *Proposer) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Proposer) window() time.Duration {
	if p.Window > 0 {
		return p.Window
	}
	return DefaultWindow
}

func (p *Proposer) minEpisodes() int {
	if p.MinEpisodes > 0 {
		return p.MinEpisodes
	}
	return DefaultMinEpisodes
}

func (p *Proposer) cooldown() time.Duration {
	if p.Cooldown > 0 {
		return p.Cooldown
	}
	return DefaultCooldown
}

// Observe records one mention and returns the connectors it matched.
//
// Note what does NOT survive this call: ev.Text. It is matched against and then
// dropped — the sentence the user eventually sees is built from counts by
// evidenceLine, so there is no path from an utterance to anything stored or
// displayed.
func (p *Proposer) Observe(ev Evidence) []string {
	if p.Set == nil || ev.At.IsZero() {
		return nil
	}
	hay := strings.ToLower(ev.Text + " " + strings.Join(ev.Entities, " "))
	if strings.TrimSpace(hay) == "" {
		return nil
	}
	// Before the append, so a stored sighting is never merged in twice.
	p.load(context.Background())

	var matched []string
	for _, d := range p.Set.Descriptors() {
		if !mentions(hay, d.Mentions) {
			continue
		}
		matched = append(matched, d.Name)
		p.mu.Lock()
		if p.seen == nil {
			p.seen = map[string][]sighting{}
		}
		p.seen[d.Name] = append(p.seen[d.Name], sighting{episode: ev.Episode, at: ev.At})
		p.mu.Unlock()

		if p.Memory != nil {
			if err := p.Memory.AddSighting(context.Background(), d.Name, ev.Episode, ev.At); err != nil {
				// The mention still counts for this run. Losing it costs a
				// suggestion arriving later than it should, which is not worth
				// failing an utterance over.
				p.log().Warn("connector: could not record a mention",
					"connector", d.Name, "err", err)
			}
		}
	}
	sort.Strings(matched)
	return matched
}

// ObserveEpisode is [Proposer.Observe] for a capture episode.
func (p *Proposer) ObserveEpisode(ep Episode) []string {
	return p.Observe(Evidence{Episode: ep.ID, At: ep.At, Text: ep.Text, Entities: ep.Entities})
}

// Dismiss silences a connector for the cooldown. A proposal the user said no to
// is not a proposal to make again next week: unused access is a risk and
// repeated asking is how blind-accept is trained.
func (p *Proposer) Dismiss(connector string) {
	// The error is dropped here and returned by DismissWithReason, which is what
	// the HTTP surface calls. A caller that cannot report the failure should not
	// be handed one to ignore silently in its own way.
	if err := p.DismissWithReason(context.Background(), connector, ""); err != nil {
		p.log().Warn("connector: a dismissal was not persisted and will be forgotten on restart",
			"connector", connector, "err", err)
	}
}

// DismissWithReason is [Proposer.Dismiss] with the user's reason recorded and
// the persistence failure returned rather than logged.
//
// The error matters and is not decoration. If the store refuses the write, the
// dismissal holds only until this process exits — so the surface that took the
// user's "no" has to be able to say the answer may not stick, rather than
// answering 200 and asking again tomorrow.
func (p *Proposer) DismissWithReason(ctx context.Context, connector, reason string) error {
	name := strings.ToLower(strings.TrimSpace(connector))
	at := p.now()

	p.mu.Lock()
	if p.dismissed == nil {
		p.dismissed = map[string]time.Time{}
	}
	p.dismissed[name] = at
	p.mu.Unlock()

	if p.Memory == nil {
		return nil
	}
	return p.Memory.PutDismissal(ctx, name, at, reason)
}

// Proposals returns everything worth suggesting right now, most-mentioned
// first. It never returns a proposal for a connector that is already granted,
// that has been dismissed inside the cooldown, or that has not been mentioned
// often enough.
func (p *Proposer) Proposals(ctx context.Context) []Proposal {
	if p.Set == nil {
		return nil
	}
	p.load(ctx)
	now := p.now()
	cutoff := now.Add(-p.window())

	if p.Memory != nil {
		// The same sweep the in-memory map gets below, on the table. Without it
		// the sighting table is append-only for the life of the machine and the
		// next restart reloads years of mentions that can no longer count.
		if err := p.Memory.Expire(ctx, cutoff); err != nil {
			p.log().Warn("connector: could not drop expired proposal evidence", "err", err)
		}
	}

	p.mu.Lock()
	// Drop anything past the window while we are here: evidence does not accrue
	// forever, and a mention from March is not a reason in August.
	for name, list := range p.seen {
		kept := list[:0]
		for _, s := range list {
			if !s.at.Before(cutoff) {
				kept = append(kept, s)
			}
		}
		if len(kept) == 0 {
			delete(p.seen, name)
			continue
		}
		p.seen[name] = kept
	}
	snapshot := make(map[string][]sighting, len(p.seen))
	for k, v := range p.seen {
		snapshot[k] = append([]sighting(nil), v...)
	}
	dismissed := make(map[string]time.Time, len(p.dismissed))
	for k, v := range p.dismissed {
		dismissed[k] = v
	}
	p.mu.Unlock()

	var out []Proposal
	for _, d := range p.Set.Descriptors() {
		list := snapshot[d.Name]
		if len(list) == 0 {
			continue
		}
		if at, ok := dismissed[d.Name]; ok && now.Sub(at) < p.cooldown() {
			continue
		}
		// Only the read half is ever proposed. A connector with no read half
		// has nothing to suggest.
		opens, hasRead := d.Opens[mcp.AccessRead]
		if !hasRead {
			continue
		}
		if p.Granted != nil {
			if ok, _ := p.Granted.Allowed(ctx, d.Name, mcp.AccessRead); ok {
				continue
			}
		}

		episodes := map[string]bool{}
		first, last := list[0].at, list[0].at
		for _, s := range list {
			key := s.episode
			if key == "" {
				// An unattributed mention still counts, but only as itself —
				// it cannot merge with another.
				key = s.at.Format(time.RFC3339Nano)
			}
			episodes[key] = true
			if s.at.Before(first) {
				first = s.at
			}
			if s.at.After(last) {
				last = s.at
			}
		}
		if len(episodes) < p.minEpisodes() {
			continue
		}

		out = append(out, Proposal{
			Connector: d.Name,
			Title:     d.Title,
			Access:    mcp.AccessRead,
			Evidence:  evidenceLine(d.Title, d.Name, len(episodes), p.window()),
			Opens:     opens,
			Scopes:    []string{mcp.AccessRead.Scope(d.Name)},
			Episodes:  len(episodes),
			Mentions:  len(list),
			FirstAt:   first.UTC(),
			LastAt:    last.UTC(),
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Episodes != out[j].Episodes {
			return out[i].Episodes > out[j].Episodes
		}
		return out[i].Connector < out[j].Connector
	})
	return out
}

// evidenceLine is §4b's sentence, built from counts alone.
func evidenceLine(title, name string, episodes int, window time.Duration) string {
	subject := title
	if strings.TrimSpace(subject) == "" {
		subject = name
	}
	return fmt.Sprintf("You have mentioned your %s %s %s.", subject, times(episodes), windowPhrase(window))
}

func times(n int) string {
	switch n {
	case 1:
		return "once"
	case 2:
		return "twice"
	default:
		return spell(n) + " times"
	}
}

var smallNumbers = []string{
	"zero", "one", "two", "three", "four", "five", "six", "seven", "eight",
	"nine", "ten", "eleven", "twelve",
}

func spell(n int) string {
	if n >= 0 && n < len(smallNumbers) {
		return smallNumbers[n]
	}
	return fmt.Sprint(n)
}

func windowPhrase(w time.Duration) string {
	days := int(w / (24 * time.Hour))
	switch {
	case days == 1:
		return "today"
	case days == 7:
		return "this week"
	case days == 14:
		return "in the last fortnight"
	case days >= 28 && days <= 31:
		return "this month"
	case days > 0:
		return fmt.Sprintf("in the last %d days", days)
	default:
		return "recently"
	}
}

// mentions reports whether any of a connector's words appears in the text as a
// word rather than as a substring. "codex" must not match "codexample", and a
// proposal built on a false match is worse than no proposal — it is a claim
// about what the user said.
func mentions(hay string, words []string) bool {
	for _, w := range words {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" {
			continue
		}
		if containsWord(hay, w) {
			return true
		}
	}
	return false
}

func containsWord(hay, needle string) bool {
	from := 0
	for {
		i := strings.Index(hay[from:], needle)
		if i < 0 {
			return false
		}
		i += from
		startOK := i == 0 || !isWordRune(rune(hay[i-1]))
		end := i + len(needle)
		endOK := end == len(hay) || !isWordRune(rune(hay[end]))
		if startOK && endOK {
			return true
		}
		from = i + 1
		if from >= len(hay) {
			return false
		}
	}
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
