package transcript

import (
	"context"
	"strings"
	"testing"
	"time"
)

func start() time.Time {
	t, _ := time.Parse(time.RFC3339, "2026-08-10T09:00:00Z")
	return t
}

func drain(t *testing.T, s Stream) []Result {
	t.Helper()
	done := make(chan []Result, 1)
	go func() {
		var out []Result
		for r := range s.Results() {
			out = append(out, r)
		}
		done <- out
	}()
	if err := s.CloseSend(); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		return out
	case <-time.After(2 * time.Second):
		t.Fatal("the recogniser never closed its results channel")
		return nil
	}
}

// "Streaming, not batch, so the prompt is ready the instant they stop talking
// rather than starting a 400 ms job at that point" — SYSTEM.md §7b. The test of
// that is partials arriving before the audio ends.
func TestFakeRecognizerStreamsPartialsBeforeTheAudioEnds(t *testing.T) {
	f := &Fake{}
	s, err := f.Open(context.Background(), StreamConfig{StartedAt: start()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(Audio{Seq: 0, Data: []byte("send Marc"), At: start()}); err != nil {
		t.Fatal(err)
	}

	// Results are already available; nothing has been closed.
	var early []Result
	for len(early) < 2 {
		select {
		case r := <-s.Results():
			early = append(early, r)
		case <-time.After(time.Second):
			t.Fatalf("only %d results before the audio ended; this recogniser is batch, not streaming", len(early))
		}
	}
	for _, r := range early {
		if r.Final {
			t.Fatalf("an unterminated phrase produced a final: %+v", r)
		}
	}
	if early[0].Text != "send" || early[1].Text != "send Marc" {
		t.Fatalf("partials = %q / %q, want them to grow word by word", early[0].Text, early[1].Text)
	}

	rest := drain(t, s)
	var finals []Result
	for _, r := range rest {
		if r.Final {
			finals = append(finals, r)
		}
	}
	if len(finals) != 1 || finals[0].Text != "send Marc" {
		t.Fatalf("finals = %+v, want one settling the phrase", finals)
	}
}

func TestFakeSettlesAFinalPerSentence(t *testing.T) {
	f := &Fake{Speaker: "me", Confidence: 0.9}
	s, err := f.Open(context.Background(), StreamConfig{StartedAt: start()})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = s.Write(Audio{Seq: 0, Data: []byte("I'll send the BOM on Friday."), At: start()})
		_ = s.Write(Audio{Seq: 1, Data: []byte("We decided to use the WCH part."), At: start().Add(time.Second)})
		_ = s.CloseSend()
	}()

	var finals []Result
	for r := range s.Results() {
		if r.Final {
			finals = append(finals, r)
		}
	}
	if len(finals) != 2 {
		t.Fatalf("finals = %+v, want one per sentence", finals)
	}
	if finals[0].Text != "I'll send the BOM on Friday." {
		t.Fatalf("first final = %q", finals[0].Text)
	}
	if finals[1].Start != time.Second {
		t.Fatalf("second final starts at %s, want the frame's own offset", finals[1].Start)
	}
	if finals[0].Speaker != "me" || finals[0].Confidence != 0.9 {
		t.Fatalf("the fake dropped its configured labels: %+v", finals[0])
	}
}

// A hole is not a pause. Running the words on either side together produces a
// sentence nobody said.
func TestAGapEndsTheUtteranceRatherThanSplicingIt(t *testing.T) {
	f := &Fake{}
	s, err := f.Open(context.Background(), StreamConfig{StartedAt: start()})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = s.Write(Audio{Seq: 0, Data: []byte("send the invoice to"), At: start()})
		_ = s.Write(Audio{Seq: 1, Gap: true, GapFor: 3 * time.Second, GapReason: "four chunks never arrived", At: start().Add(time.Second)})
		_ = s.Write(Audio{Seq: 2, Data: []byte("by Friday."), At: start().Add(4 * time.Second)})
		_ = s.CloseSend()
	}()

	var finals []Result
	for r := range s.Results() {
		if r.Final {
			finals = append(finals, r)
		}
	}
	if len(finals) != 3 {
		t.Fatalf("finals = %+v, want the phrase, the gap and the phrase after it", finals)
	}
	if finals[0].Text != "send the invoice to" {
		t.Fatalf("the text before the gap was not settled: %q", finals[0].Text)
	}
	if !finals[1].Gap || finals[1].GapReason == "" {
		t.Fatalf("the hole is not marked: %+v", finals[1])
	}
	if strings.Contains(finals[2].Text, "invoice") {
		t.Fatal("the recogniser spliced across the hole")
	}
}

func TestBuilderRefusesWithoutARedactor(t *testing.T) {
	if _, err := NewBuilder(BuilderOptions{}); err != ErrNoRedactor {
		t.Fatalf("NewBuilder without a detector = %v, want ErrNoRedactor", err)
	}
}

// Someone reading a key out loud is not a hypothetical, and an embedded key
// cannot be unembedded. The detector is `internal/index`'s measured one, not a
// second ruleset.
func TestCredentialsAreRedactedBeforeTheyReachATranscript(t *testing.T) {
	b, err := NewBuilder(BuilderOptions{StreamID: "turn", Redact: Detector(), Source: SourceLocal})
	if err != nil {
		t.Fatal(err)
	}
	// AWS's own documentation example key id. Synthetic by construction, and
	// deliberately not a shape scripts/build-public-repo.sh greps for.
	const spoken = "the access key is AKIAIOSFODNN7EXAMPLE and it goes in the env file"
	b.Add(Result{Text: spoken, Final: true})

	tr := b.Build()
	if strings.Contains(tr.Text(), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("the credential survived into the transcript: %q", tr.Text())
	}
	if !strings.Contains(tr.Text(), "[relay:redacted") {
		t.Fatalf("no marker replaced it: %q", tr.Text())
	}
	if tr.Redactions != 1 {
		t.Fatalf("Redactions = %d, want 1", tr.Redactions)
	}
	if len(b.Findings()) != 1 || b.Findings()[0].Service != "aws" {
		t.Fatalf("Findings = %+v, want the vault proposal flow to have something to offer", b.Findings())
	}
	found := false
	for _, n := range tr.Notes {
		if strings.Contains(n, "appeared in this session") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the transcript should say a credential was found: %v", tr.Notes)
	}
}

func TestBuilderKeepsOnlyFinals(t *testing.T) {
	b, err := NewBuilder(BuilderOptions{Redact: Detector()})
	if err != nil {
		t.Fatal(err)
	}
	b.Add(Result{Text: "send", Final: false})
	b.Add(Result{Text: "send the", Final: false})
	b.Add(Result{Text: "send the BOM", Final: true})

	tr := b.Build()
	if len(tr.Segments) != 1 {
		t.Fatalf("Segments = %+v, want only the settled one — partials would store four copies of every sentence", tr.Segments)
	}
	if tr.Text() != "send the BOM" {
		t.Fatalf("Text = %q", tr.Text())
	}
}

func TestTranscriptRendersItsHoles(t *testing.T) {
	b, err := NewBuilder(BuilderOptions{Redact: Detector()})
	if err != nil {
		t.Fatal(err)
	}
	b.Add(Result{Text: "first part", Final: true, Start: 0, End: time.Second})
	b.Add(Result{Final: true, Gap: true, GapReason: "chunks 4..7 never arrived", Start: time.Second, End: 4 * time.Second})
	b.Add(Result{Text: "second part", Final: true, Start: 4 * time.Second, End: 5 * time.Second})

	tr := b.Build()
	if tr.Complete() {
		t.Fatal("a transcript with a hole must not report itself complete")
	}
	if tr.Gaps() != 1 {
		t.Fatalf("Gaps = %d", tr.Gaps())
	}
	if !strings.Contains(tr.Text(), "[relay:gap 3s]") {
		t.Fatalf("the hole is invisible in the text: %q", tr.Text())
	}
}
