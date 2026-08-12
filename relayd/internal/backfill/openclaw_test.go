package backfill

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/index"
)

func openClawReader(t *testing.T) *OpenClaw {
	t.Helper()
	o := NewOpenClaw(fixtureEnv(t))
	o.Dir = testdata(t, "openclaw")
	return o
}

func TestOpenClawScanReadsEveryAgentsStore(t *testing.T) {
	res, err := openClawReader(t).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreOK {
		t.Fatalf("status %q: %v", res.Status, res.Notes)
	}
	if len(res.Refs) != 3 {
		t.Fatalf("%d sessions across two agents, want 3", len(res.Refs))
	}

	// The object-keyed store, with a transcript on disk beside it.
	withFile := refByID(t, res.Refs, "oc-9001")
	if !strings.HasSuffix(withFile.Path, "oc-9001.jsonl") {
		t.Errorf("oc-9001 should point at its transcript, got %q", withFile.Path)
	}
	if withFile.MTimeFrom != "transcript file mtime" {
		t.Errorf("oc-9001 resume key %q", withFile.MTimeFrom)
	}

	// The entry with no transcript keeps a real pointer: its byte range inside
	// sessions.json.
	noFile := refByID(t, res.Refs, "oc-9002")
	if !strings.HasSuffix(noFile.Path, "sessions.json") {
		t.Errorf("oc-9002 path %q", noFile.Path)
	}
	if noFile.ByteOffset <= 0 || noFile.ByteLength <= 0 {
		t.Errorf("oc-9002 has no byte range into sessions.json: %+v", noFile)
	}
	assertByteRangeIsTheEntry(t, noFile.Path, noFile.ByteOffset, noFile.ByteLength, "one-off question")

	// The array-shaped store from the second agent.
	arr := refByID(t, res.Refs, "oc-7100")
	if arr.Title != "survey ACP steering extensions" {
		t.Errorf("array-shaped entry lost its title: %q", arr.Title)
	}
}

func TestOpenClawReadUsesTranscriptWhenThereIsOne(t *testing.T) {
	o := openClawReader(t)
	scan, _ := o.Scan(context.Background())
	s, err := o.Read(context.Background(), refByID(t, scan.Refs, "oc-9001"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "trace the BLE MTU negotiation" || s.TitleSource != index.TitleStored {
		t.Errorf("title %q from %q", s.Title, s.TitleSource)
	}
	if s.Workspace != "/home/user/src/relay" || s.Model != "claude-sonnet-4" {
		t.Errorf("%+v", s)
	}
	if s.Messages != 6 {
		t.Errorf("messages %d, want the store's own count", s.Messages)
	}
	if s.ToolCalls != 1 {
		t.Errorf("tool calls %d", s.ToolCalls)
	}
	if !strings.Contains(s.Text, "renegotiation") || !strings.Contains(s.Text, "23-byte MTU") {
		t.Errorf("transcript not read: %q", s.Text)
	}
	if s.CostUSD != nil || s.TokensTotal != nil {
		t.Error("OpenClaw records neither cost nor tokens; both must stay nil")
	}
}

func TestOpenClawReadDegradesToMetadataOnly(t *testing.T) {
	o := openClawReader(t)
	scan, _ := o.Scan(context.Background())
	s, err := o.Read(context.Background(), refByID(t, scan.Refs, "oc-9002"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Text != "" {
		t.Errorf("no transcript exists, so there is no text to claim: %q", s.Text)
	}
	if !strings.Contains(strings.Join(s.Notes, "\n"), "only its metadata is indexed") {
		t.Errorf("the degradation was not disclosed: %v", s.Notes)
	}
	if s.ByteOffset == 0 {
		t.Error("the pointer must still locate the entry inside sessions.json")
	}
}

// MEMORY.md §1: ~/.openclaw did not exist on the measured machine. That is
// success with zero sessions, not an error.
func TestOpenClawAbsentDirectoryIsSuccess(t *testing.T) {
	env := fixtureEnv(t)
	env.Exec = &detect.FakeExec{}
	res, err := NewOpenClaw(env).Scan(context.Background())
	if err != nil {
		t.Fatalf("an absent directory must not be an error: %v", err)
	}
	if res.Status != StoreAbsent {
		t.Fatalf("status %q", res.Status)
	}
	if len(res.Refs) != 0 {
		t.Fatalf("%d refs", len(res.Refs))
	}
	if len(res.Roots) == 0 {
		t.Error("an absent store must still report where we looked")
	}
}

// MEMORY.md §4's trap: the state dir is relocatable, and a reader that assumes
// ~/.openclaw finds nothing and calls it success.
func TestOpenClawFollowsOPENCLAWSTATEDIR(t *testing.T) {
	home := t.TempDir()
	relocated := filepath.Join(home, "somewhere-else")
	copyTree(t, testdata(t, "openclaw"), relocated)

	env := fixtureEnv(t)
	env.Home = home
	env.Exec = &detect.FakeExec{}
	env.Getenv = func(k string) string {
		if k == "OPENCLAW_STATE_DIR" {
			return relocated
		}
		return ""
	}

	res, err := NewOpenClaw(env).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreOK || len(res.Refs) != 3 {
		t.Fatalf("the relocated store was missed: %q, %d refs, %v", res.Status, len(res.Refs), res.Notes)
	}
}

// `openclaw config file` is the authoritative answer, and it beats the default.
func TestOpenClawAsksTheBinaryFirst(t *testing.T) {
	home := t.TempDir()
	relocated := filepath.Join(home, ".openclaw-work")
	copyTree(t, testdata(t, "openclaw"), relocated)

	env := fixtureEnv(t)
	env.Home = home
	env.Exec = &detect.FakeExec{
		Paths: map[string]string{"openclaw": "/usr/local/bin/openclaw"},
		Responses: map[string]detect.Result{
			detect.Key("openclaw", "config", "file"): {
				Stdout: []byte(filepath.Join(relocated, "openclaw.json") + "\n"),
			},
		},
	}

	res, err := NewOpenClaw(env).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreOK || len(res.Refs) != 3 {
		t.Fatalf("the profile directory was missed: %q, %d refs, %v", res.Status, len(res.Refs), res.Notes)
	}
}

func TestOpenClawUnreadableStoreIsNotAnEmptyHistory(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "agents", "main", "sessions")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "sessions.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	o := NewOpenClaw(fixtureEnv(t))
	o.Dir = dir
	res, err := o.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StoreUnreadable {
		t.Fatalf("status %q, want unreadable", res.Status)
	}
	if res.Err == nil {
		t.Error("an unreadable store must carry its reason")
	}
}

func TestOpenClawAcceptsTheWrappedShape(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "agents", "main", "sessions")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":2,"sessions":[{"id":"w-1","title":"wrapped shape","messageCount":3}]}`
	if err := os.WriteFile(filepath.Join(store, "sessions.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	o := NewOpenClaw(fixtureEnv(t))
	o.Dir = dir
	res, err := o.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Refs) != 1 || res.Refs[0].SessionID != "w-1" {
		t.Fatalf("refs %+v", res.Refs)
	}
	assertByteRangeIsTheEntry(t, res.Refs[0].Path, res.Refs[0].ByteOffset, res.Refs[0].ByteLength, "wrapped shape")
}

// The pointer has to be real: reading the recorded byte range out of the file
// must yield that session's entry and nothing else.
func assertByteRangeIsTheEntry(t *testing.T, path string, offset, length int64, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if offset < 0 || offset+length > int64(len(b)) {
		t.Fatalf("byte range %d+%d is outside %s (%d bytes)", offset, length, path, len(b))
	}
	slice := string(b[offset : offset+length])
	if !strings.Contains(slice, want) {
		t.Fatalf("byte range does not hold %q:\n%s", want, slice)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
