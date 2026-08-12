package logx_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/logx"
)

func TestLevels(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		" DEBUG ": slog.LevelDebug,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	} {
		got, err := logx.ParseLevel(in)
		if err != nil || got != want {
			t.Fatalf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := logx.ParseLevel("shout"); err == nil {
		t.Fatal("an unknown level should report an error")
	}
}

// A daemon that will not start because of a log setting is worse than one that
// logs slightly wrong.
func TestUnknownLevelFallsBackRatherThanFailing(t *testing.T) {
	var buf bytes.Buffer
	log := logx.New(logx.Options{Level: "shout", Out: &buf})
	log.Info("hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("nothing was logged: %q", buf.String())
	}
}

func TestFormats(t *testing.T) {
	var text, jsonBuf bytes.Buffer
	logx.New(logx.Options{Format: "text", Out: &text}).Info("hi", "k", "v")
	logx.New(logx.Options{Format: "JSON", Out: &jsonBuf}).Info("hi", "k", "v")

	if !strings.Contains(text.String(), "msg=hi") {
		t.Fatalf("text handler: %q", text.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(jsonBuf.String()), "{") {
		t.Fatalf("json handler: %q", jsonBuf.String())
	}
}

func TestLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	log := logx.New(logx.Options{Level: "warn", Out: &buf})
	log.Info("quiet")
	log.Warn("loud")
	out := buf.String()
	if strings.Contains(out, "quiet") {
		t.Fatalf("info leaked past a warn level: %q", out)
	}
	if !strings.Contains(out, "loud") {
		t.Fatalf("warn was dropped: %q", out)
	}
}

// A log line is a file, and a file gets pasted into a support ticket.
func TestSecretRedaction(t *testing.T) {
	var buf bytes.Buffer
	log := logx.New(logx.Options{Out: &buf})
	log.Info("probing", logx.Attr("key", "tok_51QabcdefghijklmnopZ9"))

	out := buf.String()
	if strings.Contains(out, "tok_51Q") {
		t.Fatalf("the secret reached the log: %q", out)
	}
	if !strings.Contains(out, "****opZ9") {
		t.Fatalf("want the last four characters only: %q", out)
	}

	// Short values are redacted entirely: four characters of a six-character
	// token is the token.
	if got := logx.Secret("abc123").String(); got != "****" {
		t.Fatalf("short secret rendered as %q", got)
	}
	if got := logx.Secret("").String(); got != "" {
		t.Fatalf("empty secret rendered as %q", got)
	}
}

func TestDiscardWritesNothing(t *testing.T) {
	log := logx.Discard()
	log.Error("this should go nowhere")
	// No assertion beyond not panicking; the point is that tests can silence
	// a component without threading a writer through it.
	if log == nil {
		t.Fatal("Discard returned nil")
	}
}
