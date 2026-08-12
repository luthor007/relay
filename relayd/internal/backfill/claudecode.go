package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/index"
)

// ClaudeCode reads ~/.claude/projects/<slug>/<uuid>.jsonl.
//
// 786 MB and one JSONL per session on the measured machine (MEMORY.md §1),
// which makes it the second largest store and the easiest shape: one file is
// one session, so the resume key is the file's own mtime and size and Scan is a
// directory walk with a stat.
//
// MEMORY.md §4 lists the free metadata — cwd, gitBranch, timestamp, version and
// **aiTitle**, because Claude Code titles its own sessions. That last one is
// worth stealing rather than rebuilding: for a large share of the corpus the
// summariser's first job is already done, and a session that arrives titled is
// marked index.TitleGenerated so 2c can skip it.
type ClaudeCode struct {
	env Env

	// Dir overrides the resolved state directory. Tests point it at a fixture.
	Dir string
}

// NewClaudeCode builds the reader.
func NewClaudeCode(env Env) *ClaudeCode { return &ClaudeCode{env: env} }

// Runtime is claude-code.
func (c *ClaudeCode) Runtime() adapter.Runtime { return adapter.ClaudeCode }

// stateDir resolves ~/.claude, honouring CLAUDE_CONFIG_DIR, which relocates the
// whole config directory.
func (c *ClaudeCode) stateDir() (dir, source string) {
	if c.Dir != "" {
		return c.Dir, "explicit"
	}
	if v := c.env.getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return c.env.expand(v), "CLAUDE_CONFIG_DIR"
	}
	return filepath.Join(c.env.Home, ".claude"), "~/.claude, the documented default"
}

// Scan walks projects/<slug>/*.jsonl. One file, one session.
func (c *ClaudeCode) Scan(ctx context.Context) (ScanResult, error) {
	res := ScanResult{Runtime: adapter.ClaudeCode}

	dir, source := c.stateDir()
	projects := filepath.Join(dir, "projects")
	res.Roots = []string{projects}

	if !dirExists(dir) {
		res.Status = StoreAbsent
		res.note("no Claude Code state directory at %s (%s) — nothing to import, and nothing wrong", dir, source)
		return res, nil
	}
	if !dirExists(projects) {
		res.Status = StoreEmpty
		res.note("%s exists but has no projects/ — Claude Code is installed and has never run in a repo", dir)
		return res, nil
	}

	entries, err := os.ReadDir(projects)
	if err != nil {
		res.Status = StoreUnreadable
		res.Err = err
		res.note("could not read %s: %v", projects, err)
		return res, nil
	}

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !e.IsDir() {
			continue
		}
		slug := filepath.Join(projects, e.Name())
		files, err := os.ReadDir(slug)
		if err != nil {
			res.note("could not read %s: %v", slug, err)
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				res.note("could not stat %s: %v", filepath.Join(slug, f.Name()), err)
				continue
			}
			res.Refs = append(res.Refs, Ref{
				Runtime:   adapter.ClaudeCode,
				SessionID: strings.TrimSuffix(f.Name(), ".jsonl"),
				Path:      filepath.Join(slug, f.Name()),
				MTime:     info.ModTime(),
				Size:      info.Size(),
				MTimeFrom: "file mtime",
			})
		}
	}

	sort.Slice(res.Refs, func(i, j int) bool { return res.Refs[i].Path < res.Refs[j].Path })
	res.Status = StoreOK
	if len(res.Refs) == 0 {
		res.Status = StoreEmpty
	}
	return res, nil
}

// ---------------------------------------------------------- the line format --

type ccLine struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	SessionID   string `json:"sessionId"`
	Cwd         string `json:"cwd"`
	GitBranch   string `json:"gitBranch"`
	Version     string `json:"version"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`

	// aiTitle is the one MEMORY.md §4 singles out: Claude Code titles its own
	// sessions. summary is the other title-shaped record it writes.
	AITitle string `json:"aiTitle"`
	Summary string `json:"summary"`

	CostUSD *float64   `json:"costUSD"`
	Message *ccMessage `json:"message"`
}

type ccMessage struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *ccUsage        `json:"usage"`
}

type ccUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

func (u ccUsage) total() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

type ccBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Content json.RawMessage `json:"content"`
}

// Read parses one transcript.
func (c *ClaudeCode) Read(ctx context.Context, ref Ref) (Session, error) {
	s := Session{
		Runtime:     adapter.ClaudeCode,
		SessionID:   ref.SessionID,
		Path:        ref.Path,
		SourceMTime: ref.MTime,
		SourceSize:  ref.Size,
		MTimeFrom:   ref.MTimeFrom,
	}

	text := newTextBuilder(c.env.maxText())
	var (
		aiTitle, summaryTitle, firstUser string
		lastCwd, lastBranch, lastModel   string
		version                          string
		tokens                           int64
		haveTokens                       bool
		cost                             float64
		haveCost                         bool
		sidechain                        int
		recordID                         string
	)

	stats, err := scanJSONL(ref.Path, func(line []byte, _ int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var rec ccLine
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}

		if rec.AITitle != "" && aiTitle == "" {
			aiTitle = rec.AITitle
		}
		if rec.Type == "summary" && rec.Summary != "" && summaryTitle == "" {
			summaryTitle = rec.Summary
		}
		if rec.SessionID != "" && recordID == "" {
			recordID = rec.SessionID
		}
		if rec.Cwd != "" {
			lastCwd = rec.Cwd
		}
		if rec.GitBranch != "" {
			lastBranch = rec.GitBranch
		}
		if rec.Version != "" {
			version = rec.Version
		}
		if rec.CostUSD != nil {
			cost += *rec.CostUSD
			haveCost = true
		}
		if ts := parseTimestamp(rec.Timestamp); !ts.IsZero() {
			if s.StartedAt.IsZero() || ts.Before(s.StartedAt) {
				s.StartedAt = ts
			}
			if ts.After(s.EndedAt) {
				s.EndedAt = ts
			}
		}
		if rec.IsSidechain {
			sidechain++
		}

		if rec.Message == nil {
			return nil
		}
		if rec.Message.Model != "" {
			lastModel = rec.Message.Model
		}
		if u := rec.Message.Usage; u != nil {
			tokens += u.total()
			haveTokens = true
		}

		role := rec.Message.Role
		if role == "" {
			role = rec.Type
		}
		body, tools := ccContent(rec.Message.Content)
		s.ToolCalls += int64(tools)

		switch rec.Type {
		case "user", "assistant":
			s.Messages++
		}
		if role == "user" && firstUser == "" && !rec.IsSidechain {
			firstUser = body
		}
		text.add(role, body)
		return nil
	})
	if err != nil {
		return s, fmt.Errorf("backfill: claude-code %s: %w", ref.Path, err)
	}

	s.Text = text.String()
	s.TextTruncated = text.truncated
	if text.truncated {
		s.Note("stopped extracting text at %d bytes; %d bytes were neither scanned for secrets nor summarised", c.env.maxText(), text.skipped)
	}
	if stats.Malformed > 0 {
		s.Note("%d of %d lines did not parse as JSON and were skipped", stats.Malformed, stats.Lines)
	}
	if stats.Oversized > 0 {
		s.Note("%d lines were longer than %d bytes and were skipped", stats.Oversized, maxJSONLLine)
	}

	switch {
	case aiTitle != "":
		s.Title, s.TitleSource = aiTitle, index.TitleGenerated
	case summaryTitle != "":
		s.Title, s.TitleSource = summaryTitle, index.TitleGenerated
	case firstUser != "":
		s.Title, s.TitleSource = titleFrom(firstUser), index.TitleFirstMessage
	}

	s.GitBranch = lastBranch
	s.Model = lastModel

	// The workspace: prefer what the transcript says. The directory slug
	// encodes it too, but lossily — a real path segment containing a dash is
	// indistinguishable from a separator — so it is a labelled fallback, never
	// a silent one.
	if lastCwd != "" {
		s.Workspace = lastCwd
	} else if slug := filepath.Base(filepath.Dir(ref.Path)); slug != "" && slug != "." {
		s.Workspace = decodeProjectSlug(slug)
		s.Note("workspace was decoded from the directory slug %q because no record carried a cwd; dashes in real path segments make this lossy", slug)
	}

	if haveTokens {
		s.TokensTotal = int64p(tokens)
	}
	if haveCost {
		s.CostUSD = float64p(cost)
	} else {
		s.Note("Claude Code transcripts carry per-request token usage but no currency; cost is left nil rather than zero")
	}
	if sidechain > 0 {
		s.Note("%d records were sidechain (sub-agent) messages", sidechain)
	}
	if version != "" {
		s.Note("written by Claude Code %s", version)
	}
	if recordID != "" && recordID != ref.SessionID {
		s.Note("the transcript's sessionId is %s but the file is named %s; the file name is used as the id so the resume key stays stable", recordID, ref.SessionID)
	}
	return s, nil
}

// ccContent flattens a message body and counts tool_use blocks. Content is
// either a plain string or an array of blocks, and both shapes occur.
func ccContent(raw json.RawMessage) (string, int) {
	if len(raw) == 0 {
		return "", 0
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str, 0
	}
	var blocks []ccBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", 0
	}

	var b strings.Builder
	tools := 0
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			appendLine(&b, blk.Text)
		case "tool_use":
			tools++
			if blk.Name != "" {
				appendLine(&b, "["+blk.Name+"]")
			}
		case "tool_result":
			// Tool output is where credentials most often appear — an env dump,
			// a curl with a header, a printed config — so it is extracted for
			// detection rather than skipped as noise.
			inner, _ := ccContent(blk.Content)
			appendLine(&b, inner)
		default:
			appendLine(&b, blk.Text)
		}
	}
	return b.String(), tools
}

func appendLine(b *strings.Builder, s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	b.WriteString(s)
}

// decodeProjectSlug turns "-home-user-src-relay" back into a path. Lossy on
// purpose-built names with dashes in them, which is why every caller says so.
func decodeProjectSlug(slug string) string {
	if slug == "" {
		return ""
	}
	return "/" + strings.Trim(strings.ReplaceAll(slug, "-", "/"), "/")
}

// parseTimestamp accepts the shapes a transcript timestamp arrives in.
func parseTimestamp(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
