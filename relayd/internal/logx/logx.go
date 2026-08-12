// Package logx is relayd's structured logger. It is a thin wrapper over
// log/slog and intends to stay that way.
package logx

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Options configures a logger.
type Options struct {
	Level  string // debug | info | warn | error
	Format string // text | json
	Out    io.Writer
	// Source adds the calling file and line. Off by default; it is noise in a
	// daemon's ordinary log.
	Source bool
}

// New builds a logger. An unrecognised level or format falls back to info and
// text rather than failing: a daemon that will not start because of a log
// setting is worse than one that logs slightly wrong.
func New(o Options) *slog.Logger {
	out := o.Out
	if out == nil {
		out = os.Stderr
	}
	level, err := ParseLevel(o.Level)
	if err != nil {
		level = slog.LevelInfo
	}
	h := &slog.HandlerOptions{Level: level, AddSource: o.Source}

	var handler slog.Handler
	if strings.EqualFold(o.Format, "json") {
		handler = slog.NewJSONHandler(out, h)
	} else {
		handler = slog.NewTextHandler(out, h)
	}
	return slog.New(handler)
}

// ParseLevel maps a name onto a slog level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("logx: unknown level %q", s)
}

// Discard is a logger that writes nothing, for tests.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// Secret redacts a value for logging: the last four characters and nothing
// else, matching what DASHBOARD.md §3.2 allows the console to display. Short
// values are redacted entirely, because four characters of a six-character
// token is the token.
//
// Use it on anything that could be a credential. A log line is a file, and a
// file gets pasted into a support ticket.
func Secret(v string) slog.Value {
	r := []rune(v)
	if len(r) == 0 {
		return slog.StringValue("")
	}
	if len(r) < 12 {
		return slog.StringValue("****")
	}
	return slog.StringValue("****" + string(r[len(r)-4:]))
}

// Attr is a convenience for logging a redacted value.
func Attr(key, secret string) slog.Attr { return slog.Any(key, Secret(secret)) }
