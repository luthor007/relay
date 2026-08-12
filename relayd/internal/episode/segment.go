package episode

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/transcript"
)

// Kind is SYSTEM.md §5's episode kind.
type Kind string

const (
	// KindMeeting is the wearer and two or more identified others.
	KindMeeting Kind = "meeting"
	// KindFocus is the wearer alone with speech — thinking aloud, dictating,
	// working through something.
	KindFocus Kind = "focus"
	// KindConversation is the wearer and one other person.
	KindConversation Kind = "conversation"
	// KindAmbient is everything that could not be attributed: speech with no
	// speaker labels, or a stretch that is mostly holes. It is the
	// least-claiming kind and is where an unattributable episode lands.
	KindAmbient Kind = "ambient"
)

// Utterance is one settled line of speech: who, when, and what.
type Utterance struct {
	At       time.Time
	End      time.Time
	Speaker  string
	Text     string
	Location string
	// Gap marks a hole where audio was lost. Text is empty, and it is carried
	// rather than dropped so an episode can say part of it is missing.
	Gap       bool
	GapReason string
}

// Words is a rough word count, used for the ambient threshold.
func (u Utterance) Words() int { return len(strings.Fields(u.Text)) }

// Episode is a stretch of somebody's day.
type Episode struct {
	ID           string
	StartedAt    time.Time
	EndedAt      time.Time
	Kind         Kind
	Transcript   string
	Participants []string
	Location     string

	// Utterances are what it was built from. Not persisted — the transcript is
	// the record — but the extractor needs the times and the speakers.
	Utterances []Utterance
	// Gaps is how many holes it contains.
	Gaps int
	// Notes are everything the segmenter could not observe, in words.
	Notes []string
	// Boundary is why this episode started, so a segmentation that looks wrong
	// can be argued with rather than guessed at.
	Boundary string
}

// Complete reports whether the episode covers all of its audio.
func (e Episode) Complete() bool { return e.Gaps == 0 }

// Duration is how long it ran.
func (e Episode) Duration() time.Duration { return e.EndedAt.Sub(e.StartedAt) }

// Options configures segmentation and extraction.
type Options struct {
	// Wearer is the speaker label for the person wearing the glasses.
	// Default [DefaultWearer].
	Wearer string
	// Silence is how much quiet ends an episode. Default [DefaultSilence].
	Silence time.Duration
	// SpeakerGap is how much quiet has to pass before a *new voice* counts as a
	// new conversation rather than somebody joining this one. Default
	// [DefaultSpeakerGap].
	SpeakerGap time.Duration
	// MaxDuration caps one episode, so a day at a desk does not become one
	// eight-hour row that retrieval cannot use. Default [DefaultMaxDuration].
	MaxDuration time.Duration
	// MinWords is the floor below which a stretch is ambient rather than
	// speech. Default [DefaultMinWords].
	MinWords int
	// AttributeUnlabelledToWearer treats speech with no speaker label as the
	// wearer's.
	//
	// **Off by default, and that is the whole point.** One microphone on
	// somebody's face does not separate voices; without diarisation nobody
	// knows who was talking, and filing a conversation as "you said" would be a
	// claim about a person. A deployment whose capture is genuinely
	// single-speaker can turn it on, and the episode records that it did.
	AttributeUnlabelledToWearer bool
	// MaxNotes caps the notes an episode contributes to a digest.
	MaxNotes int
}

// Defaults for [Options].
const (
	// DefaultWearer is the label a diariser gives the enrolled voice.
	DefaultWearer = "me"
	// DefaultSilence is eight minutes. Long enough to survive a pause in a
	// meeting, short enough that lunch is not part of the morning's episode.
	DefaultSilence = 8 * time.Minute
	// DefaultSpeakerGap is two minutes. Under it, a new voice is somebody
	// joining; over it, it is a different room.
	DefaultSpeakerGap = 2 * time.Minute
	// DefaultMaxDuration is ninety minutes. Beyond that an episode stops being
	// a retrievable unit whatever else is true about it.
	DefaultMaxDuration = 90 * time.Minute
	// DefaultMinWords is twelve. Under it a stretch is ambient rather than a
	// conversation somebody had.
	DefaultMinWords = 12
	// DefaultMaxNotes is five per episode.
	DefaultMaxNotes = 5
)

func (o Options) withDefaults() Options {
	if o.Wearer == "" {
		o.Wearer = DefaultWearer
	}
	if o.Silence <= 0 {
		o.Silence = DefaultSilence
	}
	if o.SpeakerGap <= 0 {
		o.SpeakerGap = DefaultSpeakerGap
	}
	if o.MaxDuration <= 0 {
		o.MaxDuration = DefaultMaxDuration
	}
	if o.MinWords <= 0 {
		o.MinWords = DefaultMinWords
	}
	if o.MaxNotes <= 0 {
		o.MaxNotes = DefaultMaxNotes
	}
	return o
}

// FromTranscript converts a transcript into utterances.
//
// The transcript carries offsets from its own start; episodes carry wall-clock
// times, because a day is segmented against a clock and a commitment is due on
// a date.
func FromTranscript(t transcript.Transcript, location string) []Utterance {
	out := make([]Utterance, 0, len(t.Segments))
	for _, s := range t.Segments {
		u := Utterance{
			At:        t.StartedAt.Add(s.Start),
			End:       t.StartedAt.Add(s.End),
			Speaker:   s.Speaker,
			Text:      s.Text,
			Location:  location,
			Gap:       s.Gap,
			GapReason: s.GapReason,
		}
		out = append(out, u)
	}
	return out
}

// Segment splits a day into episodes.
//
// Four boundaries, and each one is a thing that actually changes what the
// stretch is *about*:
//
//   - **Silence.** [Options.Silence] of nothing said. The cheapest and most
//     often correct signal.
//   - **Location.** Walking somewhere else ends the thing you were doing. It is
//     also the boundary consent cares about (ARCHITECTURE.md §6), so the two
//     agree by construction.
//   - **A new voice after a lull.** Under [Options.SpeakerGap] a new speaker is
//     somebody joining the meeting; over it, it is a different conversation.
//   - **Length.** [Options.MaxDuration], because a day at a desk would
//     otherwise be one row and retrieval quality is the reason episodes exist.
func Segment(us []Utterance, o Options) []Episode {
	o = o.withDefaults()
	if len(us) == 0 {
		return nil
	}
	sorted := append([]Utterance(nil), us...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })

	var (
		out     []Episode
		current []Utterance
		reason  = "the first speech of the day"
	)
	flush := func(next string) {
		if len(current) > 0 {
			out = append(out, build(current, reason, o))
		}
		current = nil
		reason = next
	}

	for _, u := range sorted {
		if len(current) == 0 {
			current = append(current, u)
			continue
		}
		last := current[len(current)-1]
		quiet := u.At.Sub(last.End)
		if quiet < 0 {
			quiet = 0
		}

		switch {
		case quiet >= o.Silence:
			flush(fmt.Sprintf("%s of silence", quiet.Round(time.Minute)))
		case u.Location != last.Location:
			flush(fmt.Sprintf("the location changed from %q to %q", last.Location, u.Location))
		case u.At.Sub(current[0].At) >= o.MaxDuration:
			flush(fmt.Sprintf("it had run for %s", o.MaxDuration))
		case u.Speaker != "" && quiet >= o.SpeakerGap && !speaks(current, u.Speaker):
			flush(fmt.Sprintf("a voice not heard in this episode (%s) spoke after %s of quiet",
				u.Speaker, quiet.Round(time.Second)))
		}
		current = append(current, u)
	}
	flush("")
	return out
}

func speaks(us []Utterance, speaker string) bool {
	for _, u := range us {
		if u.Speaker == speaker {
			return true
		}
	}
	return false
}

func build(us []Utterance, boundary string, o Options) Episode {
	e := Episode{
		StartedAt:  us[0].At,
		EndedAt:    us[len(us)-1].End,
		Location:   us[0].Location,
		Utterances: append([]Utterance(nil), us...),
		Boundary:   boundary,
	}
	if e.EndedAt.Before(e.StartedAt) {
		e.EndedAt = e.StartedAt
	}

	var (
		lines     []string
		speakers  = map[string]bool{}
		words     int
		unlabeled int
	)
	for _, u := range us {
		if u.Gap {
			e.Gaps++
			lines = append(lines, fmt.Sprintf("[relay:gap %s]", (u.End.Sub(u.At)).Round(time.Second)))
			continue
		}
		if strings.TrimSpace(u.Text) == "" {
			continue
		}
		words += u.Words()
		switch {
		case u.Speaker != "":
			speakers[u.Speaker] = true
			lines = append(lines, u.Speaker+": "+u.Text)
		case o.AttributeUnlabelledToWearer:
			speakers[o.Wearer] = true
			lines = append(lines, o.Wearer+": "+u.Text)
		default:
			unlabeled++
			lines = append(lines, u.Text)
		}
	}
	e.Transcript = strings.Join(lines, "\n")

	for s := range speakers {
		e.Participants = append(e.Participants, s)
	}
	sort.Strings(e.Participants)

	e.Kind = classify(e.Participants, words, unlabeled, o)

	if unlabeled > 0 && !o.AttributeUnlabelledToWearer {
		e.Notes = append(e.Notes,
			fmt.Sprintf("%d line(s) carry no speaker label, so nobody is named for them — this recogniser does not separate voices", unlabeled))
	}
	if o.AttributeUnlabelledToWearer {
		e.Notes = append(e.Notes,
			"unlabelled speech was attributed to the wearer because this box is configured single-speaker")
	}
	if e.Gaps > 0 {
		e.Notes = append(e.Notes,
			fmt.Sprintf("%d stretch(es) of audio are missing from this episode", e.Gaps))
	}
	e.ID = episodeID(e)
	return e
}

// classify names the kind, and refuses to name one it cannot see.
func classify(participants []string, words, unlabeled int, o Options) Kind {
	others := 0
	wearerSpoke := false
	for _, p := range participants {
		if p == o.Wearer {
			wearerSpoke = true
			continue
		}
		others++
	}
	switch {
	case words < o.MinWords:
		// Too little said to be a conversation about anything. Background.
		return KindAmbient
	case others >= 2:
		return KindMeeting
	case others == 1:
		return KindConversation
	case wearerSpoke:
		return KindFocus
	case unlabeled > 0:
		// There was speech, and nobody knows whose. Ambient is the kind that
		// claims the least, and the episode's notes say why it landed here.
		return KindAmbient
	}
	return KindAmbient
}

// episodeID is deterministic, so re-running the segmenter over the same day
// updates rows rather than duplicating them — store.PutEpisode is an upsert on
// the id and that only helps if the id is stable.
//
// It is derived from the start instant and the location, and deliberately not
// from the participants or the transcript: a better recogniser on a re-run
// changes both, and it should improve the episode rather than create a second
// one beside it.
func episodeID(e Episode) string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		"episode",
		strconv.FormatInt(e.StartedAt.UTC().UnixMilli(), 10),
		e.Location,
	}, "\x00")))
	return hex.EncodeToString(h[:16])
}
