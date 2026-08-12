package compaction

import (
	"strings"
	"testing"
	"time"
)

func TestFlushTurnIsMarkedSoNothingSpeaksIt(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	turn := FlushTurn("s1", at)

	if !IsFlush(turn.ID) {
		t.Fatalf("turn id %q is not recognisable as a flush", turn.ID)
	}
	if IsFlush("turn-42") || IsFlush("") {
		t.Fatal("an ordinary turn must not look like a flush")
	}
	if turn.Text == "" {
		t.Fatal("the flush turn needs its prompt")
	}
	if FlushTurnID("s1", at) != turn.ID {
		t.Fatal("the id must be derivable without building the turn")
	}
}

func TestFlushPromptSaysWhatItIsAndIsNot(t *testing.T) {
	p := FlushPrompt()
	for _, want := range []string{NoReply, "Do not run any tools", "WORK:", "DECISION:", "FILE:", "NEXT:", "not a request from the user"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt is missing %q:\n%s", want, p)
		}
	}
}

func TestReadFlushParsesLabelsAndDropsProse(t *testing.T) {
	reply := strings.Join([]string{
		"Sure, here's what I've got:",
		"WORK: porting the codec to the new frame layout",
		"- DECISION: keep the old header for one release",
		"DECISION: drop the 16-bit path",
		"FILE: codec/frame.go",
		"FILE: codec/header.go",
		"NEXT: write the round-trip test",
		"Hope that helps!",
	}, "\n")

	n, ok := ReadFlush(reply, Detector())
	if !ok {
		t.Fatal("a reply with labelled lines is usable")
	}
	if n.Work != "porting the codec to the new frame layout" {
		t.Fatalf("work = %q", n.Work)
	}
	if len(n.Decisions) != 2 {
		t.Fatalf("decisions = %v", n.Decisions)
	}
	if len(n.Files) != 2 {
		t.Fatalf("files = %v", n.Files)
	}
	if n.Next != "write the round-trip test" {
		t.Fatalf("next = %q", n.Next)
	}
	if n.Ignored != 2 {
		t.Fatalf("ignored = %d, want the two prose lines counted rather than guessed at", n.Ignored)
	}
}

func TestReadFlushHonoursNoReply(t *testing.T) {
	for _, reply := range []string{NoReply, "  no_reply  ", "", "   "} {
		if _, ok := ReadFlush(reply, Detector()); ok {
			t.Fatalf("%q must produce nothing", reply)
		}
	}
	// Prose only, no labels: nothing usable, and we do not go looking for
	// sentences that might be steps.
	if _, ok := ReadFlush("I think we should probably refactor the parser.", Detector()); ok {
		t.Fatal("unlabelled prose must not become brief material")
	}
	if _, ok := ReadFlush("WORK: x", nil); ok {
		t.Fatal("no detector, no parse")
	}
}

func TestReadFlushRedacts(t *testing.T) {
	n, ok := ReadFlush("DECISION: the deploy token is "+fakeToken, Detector())
	if !ok {
		t.Fatal("the line is still a decision")
	}
	if n.Redactions == 0 {
		t.Fatal("a model that ignored the instruction must still be redacted")
	}
	for _, d := range n.Decisions {
		if strings.Contains(d, fakeToken) {
			t.Fatal("a credential survived the flush parser")
		}
	}
}

func TestNotesMergeIntoBriefInput(t *testing.T) {
	n := Notes{
		Work:      "porting the codec",
		Decisions: []string{"keep the old header"},
		Files:     []string{"codec/frame.go"},
		Next:      "write the round-trip test",
	}
	in := n.Merge(BriefInput{Session: "s", Decisions: []string{"stored decision"}, Files: []string{"other.go"}})

	if in.Summary != "porting the codec" {
		t.Fatalf("summary = %q", in.Summary)
	}
	if in.Next != "write the round-trip test" {
		t.Fatalf("next = %q", in.Next)
	}
	if len(in.Decisions) != 2 || in.Decisions[0] != "keep the old header" {
		t.Fatalf("decisions = %v: the flush answered moments ago and goes first", in.Decisions)
	}
	if len(in.Files) != 2 {
		t.Fatalf("files = %v", in.Files)
	}

	// An existing summary is not overwritten; the flush becomes the newest
	// recent turn instead.
	in = n.Merge(BriefInput{Summary: "the stored one"})
	if in.Summary != "the stored one" {
		t.Fatalf("summary = %q", in.Summary)
	}
	if len(in.Recent) != 1 || in.Recent[0] != "porting the codec" {
		t.Fatalf("recent = %v", in.Recent)
	}
}

func TestNotesEmpty(t *testing.T) {
	if !(Notes{}).Empty() {
		t.Fatal("zero notes are empty")
	}
	if (Notes{Files: []string{"a"}}).Empty() {
		t.Fatal("a file is material")
	}
}
