package backfill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/index"
)

// OpenClaw reads <state>/agents/<agent>/sessions/sessions.json plus whatever
// transcripts sit beside it.
//
// This is the runtime whose directory **did not exist at all** on the measured
// machine (MEMORY.md §1) — installed, never run. So the important behaviour
// here is the boring one: an absent directory is [StoreAbsent], zero sessions,
// success.
//
// And the state directory is **relocatable**, which is the trap MEMORY.md §4
// spells out: OPENCLAW_STATE_DIR, `--profile <name>` (→ ~/.openclaw-<name>),
// `--dev` (→ ~/.openclaw-dev) and a configurable session-store path in the
// gateway config all move it. A reader that assumes ~/.openclaw finds nothing
// and reports an empty history as success, which is worse than an error because
// it looks like a clean install and the user never learns their sessions were
// skipped. Resolution is therefore delegated to internal/detect, which asks
// `openclaw config file` first and labels every fallback — rather than
// duplicated here, so there is one implementation of the trap and not two.
type OpenClaw struct {
	env Env

	// Dir overrides the resolved state directory.
	Dir string
}

// NewOpenClaw builds the reader.
func NewOpenClaw(env Env) *OpenClaw { return &OpenClaw{env: env} }

// Runtime is openclaw.
func (o *OpenClaw) Runtime() adapter.Runtime { return adapter.OpenClaw }

// Scan resolves the state directory and reads every sessions.json under it.
func (o *OpenClaw) Scan(ctx context.Context) (ScanResult, error) {
	res := ScanResult{Runtime: adapter.OpenClaw}

	var stores []string
	var dir string
	if o.Dir != "" {
		dir = o.Dir
		stores = findSessionStores(dir)
	} else {
		st := detect.ResolveOpenClawState(ctx, o.env.detectEnv(), detect.Options{
			OpenClawProfile: o.env.OpenClawProfile,
			OpenClawDev:     o.env.OpenClawDev,
		})
		dir, stores = st.Dir, st.SessionStores
		res.Roots = st.Candidates
		res.Notes = append(res.Notes, st.Notes...)
		if !st.Source.Trusted() {
			res.note("%s was assumed rather than confirmed (%s); if OpenClaw runs under a profile its history will be skipped and the empty result would look like success",
				st.Dir, st.Detail)
		}
	}
	if len(res.Roots) == 0 {
		res.Roots = []string{dir}
	}

	if !dirExists(dir) {
		res.Status = StoreAbsent
		res.note("no OpenClaw state directory at %s — installed and never run, or not installed. Nothing to import, and nothing wrong", dir)
		return res, nil
	}
	if len(stores) == 0 {
		res.Status = StoreEmpty
		res.note("%s exists but holds no agents/<agent>/sessions/sessions.json", dir)
		return res, nil
	}

	for _, store := range stores {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Roots = append(res.Roots, store)
		entries, err := readOpenClawStore(store)
		if err != nil {
			res.Status = StoreUnreadable
			res.Err = err
			res.note("could not read %s: %v", store, err)
			continue
		}
		for _, e := range entries {
			ref := Ref{
				Runtime:    adapter.OpenClaw,
				SessionID:  e.ID,
				Path:       store,
				ByteOffset: e.Offset,
				ByteLength: e.Length,
				MTime:      e.Updated,
				Size:       e.Messages,
				MTimeFrom:  "the session's own updated timestamp, because one sessions.json holds every session",
				Title:      e.Title,
				StartedAt:  e.Created,
			}
			if t := findTranscript(store, e.ID); t != "" {
				ref.Path, ref.ByteOffset, ref.ByteLength = t, 0, 0
				if info, err := os.Stat(t); err == nil {
					ref.MTime, ref.Size = info.ModTime(), info.Size()
					ref.MTimeFrom = "transcript file mtime"
				}
			}
			res.Refs = append(res.Refs, ref)
		}
	}

	sort.Slice(res.Refs, func(i, j int) bool { return res.Refs[i].SessionID < res.Refs[j].SessionID })
	if res.Status == StoreUnreadable {
		return res, nil
	}
	res.Status = StoreOK
	if len(res.Refs) == 0 {
		res.Status = StoreEmpty
	}
	return res, nil
}

// openClawEntry is one session as sessions.json describes it. Every field is
// optional: the file's schema has not been probed, only its path.
type openClawEntry struct {
	ID       string
	Title    string
	Cwd      string
	Model    string
	Agent    string
	Created  time.Time
	Updated  time.Time
	Messages int64

	// Offset and Length locate this entry inside sessions.json, so a session
	// with no separate transcript still gets a real pointer.
	Offset, Length int64
}

func readOpenClawStore(path string) ([]openClawEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))

	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch d := tok.(type) {
	case json.Delim:
		switch d {
		case '[':
			return decodeArray(dec)
		case '{':
			return decodeObject(dec, b)
		}
	}
	return nil, fmt.Errorf("sessions.json is neither an array nor an object")
}

// decodeArray handles [{...}, {...}], recording each element's byte range.
func decodeArray(dec *json.Decoder) ([]openClawEntry, error) {
	var out []openClawEntry
	for dec.More() {
		start := dec.InputOffset()
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return out, err
		}
		end := dec.InputOffset()
		e := decodeEntry(raw, "")
		if e.ID == "" {
			continue
		}
		e.Offset, e.Length = start, end-start
		out = append(out, e)
	}
	return out, nil
}

// decodeObject handles {"sessions": [...]}, {"sessions": {...}} and the bare
// {"<id>": {...}} map. All three shapes exist across versions, which is why
// internal/detect counts all three too.
func decodeObject(dec *json.Decoder, whole []byte) ([]openClawEntry, error) {
	var out []openClawEntry
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return out, err
		}
		key, _ := keyTok.(string)

		start := dec.InputOffset()
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return out, err
		}
		end := dec.InputOffset()

		if key == "sessions" {
			inner, err := readOpenClawValue(raw, start)
			if err != nil {
				return out, err
			}
			out = append(out, inner...)
			continue
		}
		e := decodeEntry(raw, key)
		if e.ID == "" {
			continue
		}
		e.Offset, e.Length = start, end-start
		out = append(out, e)
	}
	_ = whole
	return out, nil
}

// readOpenClawValue decodes a nested sessions value, keeping byte offsets
// relative to the whole file.
func readOpenClawValue(raw json.RawMessage, base int64) ([]openClawEntry, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil, nil
	}
	var out []openClawEntry
	switch d {
	case '[':
		out, err = decodeArray(dec)
	case '{':
		out, err = decodeObject(dec, raw)
	}
	for i := range out {
		out[i].Offset += base
	}
	return out, err
}

func decodeEntry(raw json.RawMessage, key string) openClawEntry {
	var m struct {
		ID           string `json:"id"`
		SessionID    string `json:"sessionId"`
		Key          string `json:"key"`
		Title        string `json:"title"`
		Name         string `json:"name"`
		Summary      string `json:"summary"`
		Cwd          string `json:"cwd"`
		Directory    string `json:"directory"`
		Workspace    string `json:"workspace"`
		Model        string `json:"model"`
		Agent        string `json:"agent"`
		CreatedAt    any    `json:"createdAt"`
		Created      any    `json:"created"`
		StartedAt    any    `json:"startedAt"`
		UpdatedAt    any    `json:"updatedAt"`
		LastActiveAt any    `json:"lastActiveAt"`
		MessageCount any    `json:"messageCount"`
		Messages     any    `json:"messages"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return openClawEntry{}
	}
	first := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	firstTime := func(vals ...any) time.Time {
		for _, v := range vals {
			if t := anyTime(v); !t.IsZero() {
				return t
			}
		}
		return time.Time{}
	}
	e := openClawEntry{
		ID:       first(m.ID, m.SessionID, m.Key, key),
		Title:    first(m.Title, m.Name, m.Summary),
		Cwd:      first(m.Cwd, m.Directory, m.Workspace),
		Model:    m.Model,
		Agent:    m.Agent,
		Created:  firstTime(m.CreatedAt, m.Created, m.StartedAt),
		Updated:  firstTime(m.UpdatedAt, m.LastActiveAt),
		Messages: anyInt(m.MessageCount),
	}
	if e.Messages == 0 {
		e.Messages = anyInt(m.Messages)
	}
	return e
}

// findSessionStores finds agents/<agent>/sessions/sessions.json under a state
// directory. Used only when Dir was set explicitly; otherwise
// detect.ResolveOpenClawState has already done it.
func findSessionStores(dir string) []string {
	agents := filepath.Join(dir, "agents")
	entries, err := os.ReadDir(agents)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(agents, e.Name(), "sessions", "sessions.json")
		if fileExists(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// findTranscript looks for a per-session transcript beside sessions.json. The
// layout has not been probed, so several shapes are tried and a miss is normal.
func findTranscript(store, id string) string {
	dir := filepath.Dir(store)
	for _, cand := range []string{
		filepath.Join(dir, id+".jsonl"),
		filepath.Join(dir, id+".json"),
		filepath.Join(dir, id, "messages.jsonl"),
		filepath.Join(dir, id, "transcript.jsonl"),
	} {
		if fileExists(cand) {
			return cand
		}
	}
	return ""
}

// Read parses one session.
func (o *OpenClaw) Read(ctx context.Context, ref Ref) (Session, error) {
	s := Session{
		Runtime:     adapter.OpenClaw,
		SessionID:   ref.SessionID,
		Path:        ref.Path,
		ByteOffset:  ref.ByteOffset,
		ByteLength:  ref.ByteLength,
		SourceMTime: ref.MTime,
		SourceSize:  ref.Size,
		MTimeFrom:   ref.MTimeFrom,
		Messages:    ref.Size,
		StartedAt:   ref.StartedAt,
	}
	if ref.Title != "" {
		s.Title, s.TitleSource = ref.Title, index.TitleStored
	}

	// Metadata always comes from sessions.json, whether or not a transcript
	// exists beside it.
	store := ref.Path
	if !strings.HasSuffix(store, "sessions.json") {
		store = filepath.Join(filepath.Dir(ref.Path), "sessions.json")
	}
	if fileExists(store) {
		entries, err := readOpenClawStore(store)
		if err != nil {
			s.Note("could not re-read %s: %v", store, err)
		}
		for _, e := range entries {
			if e.ID != ref.SessionID {
				continue
			}
			if e.Title != "" {
				s.Title, s.TitleSource = e.Title, index.TitleStored
			}
			s.Workspace, s.Model = e.Cwd, e.Model
			if !e.Created.IsZero() {
				s.StartedAt = e.Created
			}
			if !e.Updated.IsZero() {
				s.EndedAt = e.Updated
			}
			if e.Messages > 0 {
				s.Messages = e.Messages
			}
			if e.Agent != "" {
				s.Note("agent %s", e.Agent)
			}
		}
	}

	s.Note("OpenClaw records no cost or token counts in its session store, so both stay nil")

	transcript := ref.Path
	if strings.HasSuffix(transcript, "sessions.json") {
		s.Note("no separate transcript was found for this session; the pointer is its entry inside %s at byte %d, and only its metadata is indexed", store, ref.ByteOffset)
		return s, nil
	}

	text := newTextBuilder(o.env.maxText())
	var counted int64
	stats, err := scanJSONL(transcript, func(line []byte, _ int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var m struct {
			Role    string          `json:"role"`
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
			Text    string          `json:"text"`
			Tool    string          `json:"tool"`
			Name    string          `json:"name"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			return err
		}
		counted++
		if m.Tool != "" || m.Type == "tool_call" || m.Type == "tool_use" {
			s.ToolCalls++
		}
		body := m.Text
		if body == "" {
			body = codexText(m.Content)
		}
		role := m.Role
		if role == "" {
			role = m.Type
		}
		text.add(role, body)
		return nil
	})
	if err != nil {
		s.Note("could not read the transcript %s: %v", transcript, err)
		return s, nil
	}
	s.Text = text.String()
	s.TextTruncated = text.truncated
	if stats.Malformed > 0 {
		s.Note("%d of %d transcript lines did not parse as JSON and were skipped", stats.Malformed, stats.Lines)
	}
	if s.Messages == 0 {
		s.Messages = counted
	}
	return s, nil
}
