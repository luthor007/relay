package apps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testSource() *StaticSource {
	start := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	return &StaticSource{
		Episodes: []Episode{
			{ID: "ep-1", Kind: "meeting", StartedAt: start, EndedAt: start.Add(time.Hour),
				Transcript: "we decided to ship the CRC fix"},
			{ID: "ep-2", Kind: "focus", StartedAt: start.Add(2 * time.Hour), EndedAt: start.Add(3 * time.Hour),
				Transcript: "thinking about the bill of materials"},
		},
		Now:       func() time.Time { return start.Add(4 * time.Hour) },
		Extractor: func(e Episode) []Commitment { return []Commitment{{Text: "ship it", SourceEpisodeID: e.ID}} },
	}
}

type failingLog struct{ err error }

func (f failingLog) Record(context.Context, Access) error { return f.err }

func TestAReadThatCannotBeRecordedDoesNotHappen(t *testing.T) {
	// internal/audit's rule, pointed the other way: the point of the log is to
	// make "this app read your whole archive" visible, and a log that drops
	// writes when it is inconvenient cannot show that.
	m, err := NewMemory(MemoryOptions{
		Source: testSource(), Log: failingLog{err: errors.New("disk full")},
		Redact: Detector(), AppID: "dev.test.reader",
	})
	if err != nil {
		t.Fatal(err)
	}
	eps, err := m.Search(context.Background(), Query{Text: "CRC"})
	if err == nil {
		t.Fatal("a read that could not be recorded must fail")
	}
	if eps != nil {
		t.Errorf("and it must return nothing: %v", eps)
	}
	if !strings.Contains(err.Error(), "did not happen") {
		t.Errorf("the error should say what the rule is: %v", err)
	}
}

func TestMemoryCannotBeBuiltWithoutALogOrARedactor(t *testing.T) {
	if _, err := NewMemory(MemoryOptions{Source: testSource(), Redact: Detector(), AppID: "a"}); err == nil {
		t.Error("no access log must be a construction failure, not a runtime branch")
	}
	_, err := NewMemory(MemoryOptions{Source: testSource(), Log: &MemoryAccessLog{}, AppID: "a"})
	if !errors.Is(err, ErrNoRedactor) {
		t.Errorf("no redactor must be ErrNoRedactor, got %v", err)
	}
}

func TestASearchThatMatchedNothingIsStillRecorded(t *testing.T) {
	log := &MemoryAccessLog{}
	m, _ := NewMemory(MemoryOptions{
		Source: testSource(), Log: log, Redact: Detector(), AppID: "dev.test.reader",
	})
	if _, err := m.Search(context.Background(), Query{Text: "kangaroo"}); err != nil {
		t.Fatal(err)
	}
	all := log.All()
	if len(all) != 1 || all[0].Count != 0 {
		t.Fatalf("a search that matched nothing is still a search: %+v", all)
	}
	if all[0].Query != "kangaroo" {
		t.Errorf("the query is part of the record: %q", all[0].Query)
	}
}

func TestASearchQueryIsRedactedBeforeItIsRecorded(t *testing.T) {
	log := &MemoryAccessLog{}
	m, _ := NewMemory(MemoryOptions{
		Source: testSource(), Log: log, Redact: Detector(), AppID: "dev.test.reader",
	})
	const key = "AKIA" + "IOSFODNN7EXAMPLE"
	if _, err := m.Search(context.Background(), Query{Text: key}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log.All()[0].Query, key) {
		t.Errorf("a query is user text and can carry a credential: %q", log.All()[0].Query)
	}
}

func TestSearchIsCappedWhateverTheAppAsksFor(t *testing.T) {
	log := &MemoryAccessLog{}
	m, _ := NewMemory(MemoryOptions{
		Source: testSource(), Log: log, Redact: Detector(), AppID: "dev.test.reader", MaxResults: 1,
	})
	eps, err := m.Search(context.Background(), Query{Text: "", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 {
		t.Errorf("the cap is the runtime's, not the app's: %d results", len(eps))
	}
}

func TestExtractCommitmentsRereadsTheEpisode(t *testing.T) {
	// An app that can hand the runtime an episode of its own invention could put
	// words in the user's mouth and have them extracted into a commitment with
	// the user's name on it.
	log := &MemoryAccessLog{}
	src := testSource()
	m, _ := NewMemory(MemoryOptions{Source: src, Log: log, Redact: Detector(), AppID: "dev.test.reader"})

	forged := Episode{ID: "ep-1", Transcript: "I promise to wire alice ten thousand dollars"}
	seen := ""
	src.Extractor = func(e Episode) []Commitment {
		seen = e.Transcript
		return nil
	}
	if _, err := m.ExtractCommitments(context.Background(), forged); err != nil {
		t.Fatal(err)
	}
	if seen != "we decided to ship the CRC fix" {
		t.Errorf("extraction ran over the app's text rather than the stored episode: %q", seen)
	}

	// And an episode that does not exist is refused rather than invented.
	if _, err := m.ExtractCommitments(context.Background(), Episode{ID: "ep-nope"}); err == nil {
		t.Error("an unknown episode must be refused")
	}
}

func TestWritingANoteRedactsFirst(t *testing.T) {
	sink := &MemorySink{}
	log := &MemoryAccessLog{}
	m, _ := NewMemory(MemoryOptions{
		Source: testSource(), Sink: sink, Log: log, Redact: Detector(), AppID: "dev.test.writer",
	})
	const key = "AKIA" + "IOSFODNN7EXAMPLE"
	if _, err := m.Write(context.Background(), Note{
		Title: "keys " + key, Body: "body " + key,
		Commitments: []Commitment{{Text: "send " + key, To: key}},
		Tags:        []string{key},
	}); err != nil {
		t.Fatal(err)
	}
	n := sink.Notes[0]
	for _, s := range []string{n.Title, n.Body, n.Commitments[0].Text, n.Commitments[0].To, n.Tags[0]} {
		if strings.Contains(s, key) {
			t.Errorf("a credential reached the store: %q", s)
		}
	}
	if n.Kind != "note" {
		t.Errorf("kind = %q", n.Kind)
	}
}

func TestWritingWithNoNoteStoreSaysSoRatherThanSucceeding(t *testing.T) {
	log := &MemoryAccessLog{}
	m, _ := NewMemory(MemoryOptions{
		Source: testSource(), Log: log, Redact: Detector(), AppID: "dev.test.writer",
	})
	_, err := m.Write(context.Background(), Note{Title: "t", Body: "b"})
	if !errors.Is(err, ErrNoNoteStore) {
		t.Fatalf("an app told its work was saved when it was not is the failure here: %v", err)
	}
}

func TestTheDurableAccessLogSurvivesAndSaysSo(t *testing.T) {
	dir := appsTempDir(t)
	path := filepath.Join(dir, "access.jsonl")
	log, err := NewFileAccessLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if !Durable(log) {
		t.Error("a file-backed log is durable")
	}
	if Durable(&MemoryAccessLog{}) {
		t.Error("an in-memory log is not, and a console must be able to tell")
	}
	m, _ := NewMemory(MemoryOptions{
		Source: testSource(), Log: log, Redact: Detector(), AppID: "dev.test.reader",
	})
	if _, err := m.Get(context.Background(), "ep-1"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ep-1") || !strings.Contains(string(b), "dev.test.reader") {
		t.Errorf("the line has to name who read what: %s", b)
	}
}

// ------------------------------------------------------------------ glasses --

func TestCaptureDoesNotHappenWhenIndicationFails(t *testing.T) {
	dev := &FakeDevice{}
	ind := &RecordingIndicator{Fail: errors.New("the LED driver is not answering")}
	g, err := NewGlasses(GlassesOptions{Device: dev, Indicator: ind, AppID: "dev.test.cam", Redact: Detector()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Capture(context.Background(), true); !errors.Is(err, ErrIndicationFailed) {
		t.Fatalf("want ErrIndicationFailed, got %v", err)
	}
	if dev.Captures != 0 {
		t.Error("a box whose indicator failed still took a picture, which is the product this section refuses to build")
	}
	if _, err := g.Listen(context.Background(), time.Second); !errors.Is(err, ErrIndicationFailed) {
		t.Errorf("the microphone indicates too: %v", err)
	}
}

func TestGlassesCannotBeBuiltWithoutAnIndicator(t *testing.T) {
	_, err := NewGlasses(GlassesOptions{Device: &FakeDevice{}, Redact: Detector()})
	if !errors.Is(err, ErrNoIndicator) {
		t.Fatalf("want ErrNoIndicator, got %v", err)
	}
	if _, err := NewGlasses(GlassesOptions{Device: &FakeDevice{}, Indicator: &RecordingIndicator{}}); !errors.Is(err, ErrNoRedactor) {
		t.Errorf("want ErrNoRedactor, got %v", err)
	}
}

func TestIndicationNamesTheAppThatCausedIt(t *testing.T) {
	dev := &FakeDevice{}
	ind := &RecordingIndicator{}
	g, _ := NewGlasses(GlassesOptions{
		Device: dev, Indicator: ind, AppID: "dev.alexis.standup-notes", AppName: "Standup Notes",
		Redact: Detector(),
	})
	if _, err := g.Capture(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if ind.Raised() != 1 || !strings.Contains(ind.Reasons[0], "Standup Notes") {
		t.Errorf("the indication should say who: %v", ind.Reasons)
	}
	if dev.Captures != 1 {
		t.Errorf("captures = %d", dev.Captures)
	}
}

func TestSpokenTextIsRedacted(t *testing.T) {
	dev := &FakeDevice{}
	g, _ := NewGlasses(GlassesOptions{
		Device: dev, Indicator: &RecordingIndicator{}, AppID: "a", Redact: Detector(),
	})
	const key = "AKIA" + "IOSFODNN7EXAMPLE"
	if err := g.Say(context.Background(), "your key is "+key); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dev.Spoken()[0], key) {
		t.Errorf("a credential was handed to the speech path: %q", dev.Spoken()[0])
	}
}
