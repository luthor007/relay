package transcript

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// cloudFake is the fake wearing a cloud provider's costs: the audio leaves the
// machine and there is no offline mode.
type cloudFake struct{ Fake }

func (c *cloudFake) Name() string   { return "cloud-fake" }
func (c *cloudFake) Source() Source { return SourceCloud }
func (c *cloudFake) Capabilities() Capabilities {
	caps := c.Fake.Capabilities()
	caps.Offline = false
	caps.LeavesMachine = true
	caps.Diarization = true
	return caps
}
func (c *cloudFake) Open(ctx context.Context, cfg StreamConfig) (Stream, error) {
	return c.Fake.Open(ctx, cfg)
}

// opusOnly refuses anything that is not Opus, so codec capability is a routing
// decision rather than a failure halfway through a stream.
type opusOnly struct{ Fake }

func (o *opusOnly) Name() string { return "opus-only" }
func (o *opusOnly) Capabilities() Capabilities {
	caps := o.Fake.Capabilities()
	caps.Codecs = []string{"opus"}
	return caps
}
func (o *opusOnly) Open(ctx context.Context, cfg StreamConfig) (Stream, error) {
	return o.Fake.Open(ctx, cfg)
}

func TestPhoneNativeIsPreferredWhenItWorked(t *testing.T) {
	r := NewRouter(&Fake{}, &cloudFake{})
	c, err := r.Choose(Conditions{PhoneText: "add that to the payments refactor", PhoneConfidence: 0.94, CloudAllowed: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourcePhone {
		t.Fatalf("Source = %s, want the phone — it is free, offline and never leaves the handset", c.Source)
	}
	if c.Recognizer != nil {
		t.Fatal("choosing the phone must not also open a recogniser")
	}
	if c.Why == "" {
		t.Fatal("the console renders this sentence")
	}
}

func TestANoisyRoomIsWhatCloudSTTIsFor(t *testing.T) {
	local := &Fake{}
	cloud := &cloudFake{}
	r := NewRouter(local, cloud)

	c, err := r.Choose(Conditions{PhoneText: "something", PhoneConfidence: 0.9, NoisyRoom: true, CloudAllowed: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourceCloud {
		t.Fatalf("Source = %s, want cloud in a noisy room (SYSTEM.md §8)", c.Source)
	}
	if c.Degraded {
		t.Fatal("this is the intended path, not a degraded one")
	}
}

// Sending someone's conversation to a vendor is a consent decision, not a
// configuration one.
func TestCloudIsNotChosenWithoutAGrantOrANetwork(t *testing.T) {
	r := NewRouter(&Fake{}, &cloudFake{})

	c, err := r.Choose(Conditions{PhoneText: "x", NoisyRoom: true, CloudAllowed: false})
	if err != nil {
		t.Fatal(err)
	}
	if c.Source == SourceCloud {
		t.Fatal("audio left the machine without a grant")
	}
	if !c.Degraded || !strings.Contains(c.Why, "no grant") {
		t.Fatalf("the fallback must say what it lost: %+v", c)
	}

	c, err = r.Choose(Conditions{PhoneText: "x", NoisyRoom: true, CloudAllowed: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Source == SourceCloud {
		t.Fatal("a cloud recogniser was chosen with no network")
	}
	if !c.Degraded {
		t.Fatal("falling back off the network is degraded and should say so")
	}
}

func TestBulkNeverUsesThePhonesText(t *testing.T) {
	r := NewRouter(&Fake{})
	c, err := r.Choose(Conditions{Bulk: true, PhoneText: "a turn's worth of text", PhoneConfidence: 0.99})
	if err != nil {
		t.Fatal(err)
	}
	if c.Source == SourcePhone {
		t.Fatal("the handset recognised a voice turn, not sixteen hours of a day")
	}
	if !strings.Contains(c.Why, "file") {
		t.Fatalf("Why = %q, want it to name why bulk is different", c.Why)
	}
}

func TestNoRecognizerIsAnErrorWithAReason(t *testing.T) {
	r := NewRouter()
	_, err := r.Choose(Conditions{Bulk: true, Codec: "opus"})
	if !errors.Is(err, ErrNoRecognizer) {
		t.Fatalf("err = %v, want ErrNoRecognizer", err)
	}
	if !strings.Contains(err.Error(), "no local recogniser") {
		t.Fatalf("the error has to name what is missing: %v", err)
	}

	// But a live turn where the phone did send text degrades to that text
	// rather than to silence — the box says it is degraded.
	c, err := r.Choose(Conditions{PhoneText: "what did I say about the CRC", PhoneConfidence: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourcePhone || !c.Degraded {
		t.Fatalf("Choice = %+v, want a degraded phone result rather than nothing", c)
	}
}

func TestACodecTheRecognizerCannotTakeIsARoutingDecision(t *testing.T) {
	r := NewRouter(&opusOnly{})
	if _, err := r.Choose(Conditions{Bulk: true, Codec: "pcm16"}); !errors.Is(err, ErrNoRecognizer) {
		t.Fatalf("err = %v, want the router to refuse rather than fail mid-stream", err)
	}
	c, err := r.Choose(Conditions{Bulk: true, Codec: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Recognizer.Name() != "opus-only" {
		t.Fatalf("Recognizer = %s", c.Recognizer.Name())
	}
}

func TestLowPhoneConfidenceSpendsSomething(t *testing.T) {
	r := NewRouter(&Fake{})
	c, err := r.Choose(Conditions{PhoneText: "mumble", PhoneConfidence: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourceLocal {
		t.Fatalf("Source = %s, want the local recogniser when the phone was unsure", c.Source)
	}
	if !strings.Contains(c.Why, "confidence") {
		t.Fatalf("Why = %q, want it to name the confidence floor", c.Why)
	}
}

func TestDiarizationIsPreferredButNeverInvented(t *testing.T) {
	plain := &Fake{}                 // no speaker, so no diarisation
	labelled := &Fake{Speaker: "me"} // claims diarisation
	r := NewRouter(plain, labelled)

	c, err := r.Choose(Conditions{Bulk: true, Diarize: true})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Recognizer.Capabilities().Diarization {
		t.Fatal("a recogniser that can separate speakers should win when asked to")
	}

	// With only the plain one, diarisation is not silently claimed.
	r = NewRouter(plain)
	c, err = r.Choose(Conditions{Bulk: true, Diarize: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Recognizer.Capabilities().Diarization {
		t.Fatal("a recogniser that cannot diarise must not report that it can")
	}
}

// A batch provider is usable and labelled, never mistaken for a streaming one.
// SYSTEM.md §7b: a batch recogniser is "a 400 ms job at that point", and the
// point is when the speaker stops.
func TestBatchAdmitsThatItIsBatch(t *testing.T) {
	calls := 0
	b := &Batch{
		ProviderName: "whisper-file",
		From:         SourceLocal,
		Caps:         Capabilities{Streaming: true, Offline: true}, // a lie, forced false below
		Recognize: func(_ context.Context, _ StreamConfig, audio [][]byte) ([]Result, error) {
			calls++
			var joined string
			for _, a := range audio {
				joined += string(a)
			}
			return []Result{{Text: joined, Final: true}}, nil
		},
	}
	if b.Capabilities().Streaming {
		t.Fatal("a batch provider must not report itself as streaming")
	}

	s, err := b.Open(context.Background(), StreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(Audio{Seq: 0, Data: []byte("the board ")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(Audio{Seq: 1, Gap: true, GapReason: "one chunk was lost"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(Audio{Seq: 2, Data: []byte("is done")}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("a batch provider should not have run before the audio ended")
	}

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
	out := <-done

	if calls != 1 {
		t.Fatalf("Recognize ran %d times, want once", calls)
	}
	var gaps int
	for _, r := range out {
		if r.Gap {
			gaps++
		}
	}
	if gaps != 1 {
		t.Fatalf("results = %+v, want the hole re-emitted rather than concatenated away", out)
	}
	if out[0].Text != "the board is done" {
		t.Fatalf("text = %q", out[0].Text)
	}
}
