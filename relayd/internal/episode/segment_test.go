package episode

import (
	"strings"
	"testing"
	"time"

	"github.com/luthor007/relay/relayd/internal/transcript"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func said(when, speaker, text string) Utterance {
	t := at(when)
	return Utterance{At: t, End: t.Add(4 * time.Second), Speaker: speaker, Text: text}
}

func (u Utterance) in(place string) Utterance { u.Location = place; return u }

func TestSilenceEndsAnEpisode(t *testing.T) {
	us := []Utterance{
		said("2026-08-10T09:00:00Z", "me", "Let's look at the CRC first."),
		said("2026-08-10T09:01:00Z", "marc", "The appendix has the wrong polynomial."),
		// Nine minutes of nothing.
		said("2026-08-10T09:10:30Z", "me", "Right, back to the BOM."),
	}
	eps := Segment(us, Options{})
	if len(eps) != 2 {
		t.Fatalf("got %d episodes, want 2: %+v", len(eps), eps)
	}
	if !strings.Contains(eps[1].Boundary, "silence") {
		t.Fatalf("Boundary = %q, want it to name the silence", eps[1].Boundary)
	}
	if eps[0].Kind != KindConversation {
		t.Fatalf("first episode kind = %s, want conversation", eps[0].Kind)
	}
}

func TestLocationEndsAnEpisode(t *testing.T) {
	us := []Utterance{
		said("2026-08-10T09:00:00Z", "me", "I'll grab the multimeter from the bench.").in("lab"),
		said("2026-08-10T09:00:30Z", "me", "Now I'm in the meeting room with the board.").in("meeting-room"),
	}
	eps := Segment(us, Options{})
	if len(eps) != 2 {
		t.Fatalf("got %d episodes, want 2", len(eps))
	}
	if !strings.Contains(eps[1].Boundary, "location") {
		t.Fatalf("Boundary = %q", eps[1].Boundary)
	}
	if eps[1].Location != "meeting-room" {
		t.Fatalf("Location = %q", eps[1].Location)
	}
}

// A new voice mid-meeting is somebody joining; a new voice after a lull is a
// different conversation.
func TestANewVoiceAfterALullStartsANewEpisode(t *testing.T) {
	joining := []Utterance{
		said("2026-08-10T09:00:00Z", "me", "So the plan is to ship the board first."),
		said("2026-08-10T09:00:20Z", "marc", "Agreed, that is the right order."),
		said("2026-08-10T09:00:40Z", "ana", "I just walked in, what did I miss here?"),
	}
	if eps := Segment(joining, Options{}); len(eps) != 1 {
		t.Fatalf("a voice joining mid-conversation split the episode: %d", len(eps))
	}

	arriving := []Utterance{
		said("2026-08-10T09:00:00Z", "me", "So the plan is to ship the board first."),
		said("2026-08-10T09:05:00Z", "ana", "Do you have a minute about the invoice?"),
	}
	eps := Segment(arriving, Options{})
	if len(eps) != 2 {
		t.Fatalf("a new voice after five minutes of quiet should be a new conversation: %d", len(eps))
	}
	if !strings.Contains(eps[1].Boundary, "ana") {
		t.Fatalf("Boundary = %q, want it to name the voice", eps[1].Boundary)
	}
}

func TestKindsFollowWhoWasTalking(t *testing.T) {
	long := "This is a long enough sentence about the CRC polynomial to clear the ambient floor."
	tests := []struct {
		name string
		us   []Utterance
		want Kind
	}{
		{"alone is focus", []Utterance{said("2026-08-10T09:00:00Z", "me", long)}, KindFocus},
		{"one other is a conversation", []Utterance{
			said("2026-08-10T09:00:00Z", "me", long),
			said("2026-08-10T09:00:20Z", "marc", long),
		}, KindConversation},
		{"two others is a meeting", []Utterance{
			said("2026-08-10T09:00:00Z", "me", long),
			said("2026-08-10T09:00:20Z", "marc", long),
			said("2026-08-10T09:00:40Z", "ana", long),
		}, KindMeeting},
		{"barely any speech is ambient", []Utterance{
			said("2026-08-10T09:00:00Z", "me", "hm"),
		}, KindAmbient},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eps := Segment(tc.us, Options{})
			if len(eps) != 1 {
				t.Fatalf("got %d episodes", len(eps))
			}
			if eps[0].Kind != tc.want {
				t.Fatalf("Kind = %s, want %s (participants %v)", eps[0].Kind, tc.want, eps[0].Participants)
			}
		})
	}
}

// The rule that runs through this whole milestone: never claim what was not
// observed. Speech with no speaker label is not the wearer's just because that
// would be convenient.
func TestUnlabelledSpeechIsNotAttributedToTheWearer(t *testing.T) {
	us := []Utterance{
		said("2026-08-10T09:00:00Z", "", "We should move the regulator to the other side of the board."),
	}
	eps := Segment(us, Options{})
	if len(eps) != 1 {
		t.Fatalf("got %d episodes", len(eps))
	}
	e := eps[0]
	if len(e.Participants) != 0 {
		t.Fatalf("Participants = %v, want nobody named — this recogniser does not diarise", e.Participants)
	}
	if e.Kind != KindAmbient {
		t.Fatalf("Kind = %s, want ambient — the least-claiming kind", e.Kind)
	}
	if strings.Contains(e.Transcript, "me:") {
		t.Fatalf("the transcript attributed unlabelled speech: %q", e.Transcript)
	}
	found := false
	for _, n := range e.Notes {
		if strings.Contains(n, "no speaker label") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the episode should say why nobody is named: %v", e.Notes)
	}

	// A box that is genuinely single-speaker can say so, explicitly, and the
	// episode records that it did.
	eps = Segment(us, Options{AttributeUnlabelledToWearer: true})
	if eps[0].Kind != KindFocus {
		t.Fatalf("Kind = %s, want focus once attribution was turned on", eps[0].Kind)
	}
	if !strings.Contains(strings.Join(eps[0].Notes, " "), "single-speaker") {
		t.Fatalf("the choice has to be recorded: %v", eps[0].Notes)
	}
}

func TestAGapSurvivesIntoTheEpisode(t *testing.T) {
	base := at("2026-08-10T09:00:00Z")
	us := []Utterance{
		said("2026-08-10T09:00:00Z", "me", "Send the invoice to Ana."),
		{At: base.Add(10 * time.Second), End: base.Add(40 * time.Second), Gap: true, GapReason: "chunks 4..12 never arrived"},
		said("2026-08-10T09:01:00Z", "me", "Anyway, the board is done."),
	}
	eps := Segment(us, Options{})
	if len(eps) != 1 {
		t.Fatalf("got %d episodes", len(eps))
	}
	e := eps[0]
	if e.Complete() {
		t.Fatal("an episode over lost audio must not report itself complete")
	}
	if !strings.Contains(e.Transcript, "[relay:gap 30s]") {
		t.Fatalf("the hole is invisible in the episode transcript: %q", e.Transcript)
	}
}

func TestMaxDurationCapsAnEpisode(t *testing.T) {
	var us []Utterance
	base := at("2026-08-10T09:00:00Z")
	for i := 0; i < 30; i++ {
		t := base.Add(time.Duration(i) * 5 * time.Minute)
		us = append(us, Utterance{
			At: t, End: t.Add(4 * time.Second), Speaker: "me",
			Text: "Still working through the power supply layout here.",
		})
	}
	eps := Segment(us, Options{})
	if len(eps) < 2 {
		t.Fatalf("a two-and-a-half hour desk session became %d episode(s)", len(eps))
	}
	for _, e := range eps {
		if e.Duration() > DefaultMaxDuration+5*time.Minute {
			t.Fatalf("episode ran %s, past the cap", e.Duration())
		}
	}
}

func TestEpisodeIDIsStableAcrossRuns(t *testing.T) {
	us := []Utterance{said("2026-08-10T09:00:00Z", "me", "The CRC polynomial in the appendix is wrong.")}
	first := Segment(us, Options{})
	second := Segment(us, Options{})
	if first[0].ID != second[0].ID {
		t.Fatalf("ids differ across runs: %s vs %s", first[0].ID, second[0].ID)
	}

	// A better recogniser on a re-run changes the participants and the text. It
	// should improve the episode, not create a second one beside it.
	better := []Utterance{said("2026-08-10T09:00:00Z", "marc", "The CRC polynomial in the appendix is wrong, actually.")}
	third := Segment(better, Options{})
	if third[0].ID != first[0].ID {
		t.Fatalf("re-transcribing the same moment produced a second episode: %s vs %s", third[0].ID, first[0].ID)
	}
}

func TestFromTranscriptCarriesOffsetsOntoTheClock(t *testing.T) {
	tr := transcript.Transcript{
		StreamID:  "turn",
		StartedAt: at("2026-08-10T09:00:00Z"),
		Segments: []transcript.Segment{
			{Start: 0, End: 2 * time.Second, Text: "first", Speaker: "me"},
			{Start: 90 * time.Second, End: 95 * time.Second, Text: "second", Speaker: "marc"},
			{Start: 100 * time.Second, End: 130 * time.Second, Gap: true, GapReason: "lost"},
		},
	}
	us := FromTranscript(tr, "lab")
	if len(us) != 3 {
		t.Fatalf("got %d utterances", len(us))
	}
	if !us[1].At.Equal(at("2026-08-10T09:01:30Z")) {
		t.Fatalf("second utterance at %s, want the transcript start plus its offset", us[1].At)
	}
	if us[0].Location != "lab" {
		t.Fatalf("Location = %q", us[0].Location)
	}
	if !us[2].Gap {
		t.Fatal("the gap did not survive the conversion")
	}
}
