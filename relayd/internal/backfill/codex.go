package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/index"
)

// Codex reads ~/.codex/sessions/YYYY/MM/DD/rollout-<iso>-<uuid>.jsonl.
//
// 295 MB on the measured machine. The rollout format was probed against 133
// real rollouts (MEMORY.md §4): four line types, each {type, timestamp,
// payload}.
//
//	session_meta   cwd, git, id, model_provider, cli_version, instructions,
//	               originator, source — free metadata, once per file
//	response_item  message, function_call, function_call_output, reasoning,
//	               ghost_snapshot — the turns
//	event_msg      user_message, agent_reasoning, token_count — so Codex does
//	               carry cost data for backfill, in tokens
//	turn_context   per-turn settings
//
// Two consequences worth stating. Codex records **no title anywhere**, so every
// Codex row is index.TitleFirstMessage and is marked as derived. And
// token_count is tokens, not currency: CostUSD stays nil, because Codex never
// says what a session cost in money and a zero would claim it did.
//
// session_index.jsonl is treated as a hint, not as the authority. The detector
// in internal/detect already found that a machine can have rollouts and no
// index; a reader that trusted the index would silently skip them.
type Codex struct {
	env Env

	// Dir overrides the resolved CODEX_HOME.
	Dir string
}

// NewCodex builds the reader.
func NewCodex(env Env) *Codex { return &Codex{env: env} }

// Runtime is codex.
func (c *Codex) Runtime() adapter.Runtime { return adapter.Codex }

func (c *Codex) stateDir() (dir, source string) {
	if c.Dir != "" {
		return c.Dir, "explicit"
	}
	if v := c.env.getenv("CODEX_HOME"); v != "" {
		return c.env.expand(v), "CODEX_HOME"
	}
	return filepath.Join(c.env.Home, ".codex"), "~/.codex, the documented default"
}

// Scan walks the rollout tree.
func (c *Codex) Scan(ctx context.Context) (ScanResult, error) {
	res := ScanResult{Runtime: adapter.Codex}

	dir, source := c.stateDir()
	sessions := filepath.Join(dir, "sessions")
	indexFile := filepath.Join(dir, "session_index.jsonl")
	res.Roots = []string{sessions, indexFile}

	if !dirExists(dir) {
		res.Status = StoreAbsent
		res.note("no Codex state directory at %s (%s) — nothing to import, and nothing wrong", dir, source)
		return res, nil
	}
	if !dirExists(sessions) {
		res.Status = StoreEmpty
		res.note("%s exists but has no sessions/ — Codex is installed and has never completed a turn", dir)
		return res, nil
	}

	hints := c.readIndexHints(indexFile, &res)

	err := filepath.WalkDir(sessions, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			res.note("could not walk %s: %v", path, err)
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if !strings.HasPrefix(d.Name(), "rollout-") {
			res.note("%s is not a rollout file name; read anyway", d.Name())
		}
		info, err := d.Info()
		if err != nil {
			res.note("could not stat %s: %v", path, err)
			return nil
		}
		id := rolloutID(d.Name())
		ref := Ref{
			Runtime:   adapter.Codex,
			SessionID: id,
			Path:      path,
			MTime:     info.ModTime(),
			Size:      info.Size(),
			MTimeFrom: "file mtime",
		}
		if h, ok := hints[id]; ok {
			ref.Title, ref.StartedAt = h.Title, h.StartedAt
		}
		res.Refs = append(res.Refs, ref)
		return nil
	})
	if err != nil {
		return res, err
	}

	sort.Slice(res.Refs, func(i, j int) bool { return res.Refs[i].Path < res.Refs[j].Path })
	res.Status = StoreOK
	if len(res.Refs) == 0 {
		res.Status = StoreEmpty
	}
	return res, nil
}

type codexHint struct {
	Title     string
	StartedAt time.Time
}

// readIndexHints reads session_index.jsonl if it is there. Its exact shape has
// never been probed — only that it exists and has one line per rollout — so
// every field is optional and nothing here is authoritative.
func (c *Codex) readIndexHints(path string, res *ScanResult) map[string]codexHint {
	out := map[string]codexHint{}
	if !fileExists(path) {
		res.note("no session_index.jsonl; the rollout tree was walked instead, which is the authority anyway")
		return out
	}
	stats, err := scanJSONL(path, func(line []byte, _ int64) error {
		var rec struct {
			ID        string `json:"id"`
			SessionID string `json:"session_id"`
			Path      string `json:"path"`
			Title     string `json:"title"`
			Preview   string `json:"preview"`
			Timestamp string `json:"timestamp"`
			CreatedAt string `json:"created_at"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		id := rec.ID
		if id == "" {
			id = rec.SessionID
		}
		if id == "" && rec.Path != "" {
			id = rolloutID(filepath.Base(rec.Path))
		}
		if id == "" {
			return nil
		}
		title := rec.Title
		if title == "" {
			title = rec.Preview
		}
		ts := rec.Timestamp
		if ts == "" {
			ts = rec.CreatedAt
		}
		out[id] = codexHint{Title: titleFrom(title), StartedAt: parseTimestamp(ts)}
		return nil
	})
	if err != nil {
		res.note("session_index.jsonl could not be read (%v); walking the rollout tree instead", err)
		return out
	}
	if stats.Malformed > 0 {
		res.note("%d of %d session_index.jsonl lines did not parse; they are hints only, so this changes nothing", stats.Malformed, stats.Lines)
	}
	return out
}

// rolloutID extracts the uuid from rollout-<iso>-<uuid>.jsonl.
func rolloutID(name string) string {
	base := strings.TrimSuffix(name, ".jsonl")
	base = strings.TrimPrefix(base, "rollout-")
	// A uuid is 36 characters with four dashes; the ISO stamp in front also
	// contains dashes, so take the tail rather than splitting.
	if len(base) >= 36 {
		tail := base[len(base)-36:]
		if isUUID(tail) {
			return tail
		}
	}
	return base
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// ------------------------------------------------------------- line format --

type codexLine struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type codexSessionMeta struct {
	ID            string    `json:"id"`
	Timestamp     string    `json:"timestamp"`
	Cwd           string    `json:"cwd"`
	Git           *codexGit `json:"git"`
	ModelProvider string    `json:"model_provider"`
	Model         string    `json:"model"`
	CLIVersion    string    `json:"cli_version"`
	Instructions  string    `json:"instructions"`
	Originator    string    `json:"originator"`
	Source        string    `json:"source"`

	// Some versions nest the meta under a "meta" key alongside git.
	Meta *codexSessionMeta `json:"meta"`
}

type codexGit struct {
	Branch        string `json:"branch"`
	CommitHash    string `json:"commit_hash"`
	RepositoryURL string `json:"repository_url"`
}

type codexResponseItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Name    string          `json:"name"`
	Content json.RawMessage `json:"content"`
	Output  json.RawMessage `json:"output"`
	Summary json.RawMessage `json:"summary"`
	Text    string          `json:"text"`
}

type codexEventMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Text    string `json:"text"`
	Info    *struct {
		TotalTokenUsage *codexTokenUsage `json:"total_token_usage"`
		LastTokenUsage  *codexTokenUsage `json:"last_token_usage"`
		ModelWindow     *int64           `json:"model_context_window"`
	} `json:"info"`
}

type codexTokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

func (u codexTokenUsage) total() int64 {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.InputTokens + u.OutputTokens
}

type codexTurnContext struct {
	Cwd   string `json:"cwd"`
	Model string `json:"model"`
}

// Read parses one rollout.
func (c *Codex) Read(ctx context.Context, ref Ref) (Session, error) {
	s := Session{
		Runtime:     adapter.Codex,
		SessionID:   ref.SessionID,
		Path:        ref.Path,
		SourceMTime: ref.MTime,
		SourceSize:  ref.Size,
		MTimeFrom:   ref.MTimeFrom,
	}

	text := newTextBuilder(c.env.maxText())
	var (
		firstUser   string
		metaID      string
		provider    string
		cliVersion  string
		userEvents  int64
		itemMsgs    int64
		tokens      int64
		haveTokens  bool
		unknownKind = map[string]int{}
	)

	stats, err := scanJSONL(ref.Path, func(line []byte, _ int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var rec codexLine
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		if ts := parseTimestamp(rec.Timestamp); !ts.IsZero() {
			if s.StartedAt.IsZero() || ts.Before(s.StartedAt) {
				s.StartedAt = ts
			}
			if ts.After(s.EndedAt) {
				s.EndedAt = ts
			}
		}

		switch rec.Type {
		case "session_meta":
			var m codexSessionMeta
			if err := json.Unmarshal(rec.Payload, &m); err != nil {
				return err
			}
			if m.ID == "" && m.Meta != nil {
				inner := *m.Meta
				if m.Git == nil {
					m.Git = inner.Git
				}
				inner.Git = m.Git
				m = inner
			}
			metaID = m.ID
			if m.Cwd != "" {
				s.Workspace = m.Cwd
			}
			if m.Git != nil {
				s.GitBranch = m.Git.Branch
			}
			if m.Model != "" {
				s.Model = m.Model
			}
			provider, cliVersion = m.ModelProvider, m.CLIVersion

		case "turn_context":
			var tc codexTurnContext
			if err := json.Unmarshal(rec.Payload, &tc); err != nil {
				return err
			}
			if tc.Model != "" {
				s.Model = tc.Model
			}
			if tc.Cwd != "" && s.Workspace == "" {
				s.Workspace = tc.Cwd
			}

		case "response_item":
			var item codexResponseItem
			if err := json.Unmarshal(rec.Payload, &item); err != nil {
				return err
			}
			switch item.Type {
			case "message":
				itemMsgs++
				body := codexText(item.Content)
				if item.Role == "user" && firstUser == "" {
					firstUser = body
				}
				text.add(item.Role, body)
			case "function_call":
				s.ToolCalls++
				if item.Name != "" {
					text.add("", "["+item.Name+"]")
				}
			case "function_call_output":
				// Command output is where credentials surface, so it is
				// extracted rather than skipped.
				text.add("", codexText(item.Output))
			case "reasoning":
				text.add("reasoning", codexText(item.Summary))
			case "ghost_snapshot":
				// A snapshot of the working tree, not conversation. Counted,
				// never extracted: it would swamp the summary.
			default:
				unknownKind["response_item/"+item.Type]++
			}

		case "event_msg":
			var ev codexEventMsg
			if err := json.Unmarshal(rec.Payload, &ev); err != nil {
				return err
			}
			switch ev.Type {
			case "user_message":
				userEvents++
				if firstUser == "" {
					firstUser = ev.Message
				}
				text.add("user", ev.Message)
			case "agent_reasoning":
				text.add("reasoning", ev.Text)
			case "token_count":
				if ev.Info != nil && ev.Info.TotalTokenUsage != nil {
					// Cumulative for the session, so the last one wins rather
					// than being summed.
					tokens = ev.Info.TotalTokenUsage.total()
					haveTokens = true
				}
			default:
				unknownKind["event_msg/"+ev.Type]++
			}

		default:
			unknownKind[rec.Type]++
		}
		return nil
	})
	if err != nil {
		return s, fmt.Errorf("backfill: codex %s: %w", ref.Path, err)
	}

	s.Text = text.String()
	s.TextTruncated = text.truncated
	if text.truncated {
		s.Note("stopped extracting text at %d bytes; %d bytes were neither scanned for secrets nor summarised", c.env.maxText(), text.skipped)
	}
	if stats.Malformed > 0 {
		s.Note("%d of %d lines did not parse as JSON and were skipped", stats.Malformed, stats.Lines)
	}

	s.Messages = itemMsgs
	if s.Messages == 0 {
		s.Messages = userEvents
	}

	// Codex writes no title. Deriving one is honest as long as it says so.
	switch {
	case firstUser != "":
		s.Title, s.TitleSource = titleFrom(firstUser), index.TitleFirstMessage
	case ref.Title != "":
		s.Title, s.TitleSource = ref.Title, index.TitleStored
		s.Note("title came from session_index.jsonl, whose shape has not been probed")
	}

	if haveTokens {
		s.TokensTotal = int64p(tokens)
	}
	s.Note("Codex records tokens (event_msg/token_count) and never a currency amount, so cost is nil rather than zero")

	if metaID != "" && metaID != ref.SessionID {
		s.Note("session_meta.id is %s and the file is named for %s; the file name is used so the resume key stays stable", metaID, ref.SessionID)
	}
	if provider != "" {
		s.Note("model provider %s", provider)
	}
	if cliVersion != "" {
		s.Note("written by Codex CLI %s", cliVersion)
	}
	for k, n := range unknownKind {
		s.Note("%d records of unrecognised kind %q were skipped — the rollout format may have moved", n, k)
	}
	return s, nil
}

// codexText flattens the several shapes a Codex content field takes: a string,
// an array of {type, text} parts, or an array of strings.
func codexText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			appendLine(&b, p.Text)
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	var strs []string
	if err := json.Unmarshal(raw, &strs); err == nil {
		return strings.Join(strs, "\n")
	}
	var obj struct {
		Text    string `json:"text"`
		Content string `json:"content"`
		Output  string `json:"output"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, v := range []string{obj.Text, obj.Content, obj.Output} {
			if v != "" {
				return v
			}
		}
	}
	return ""
}
