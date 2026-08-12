package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/detect"
	"github.com/luthor007/relay/relayd/internal/index"
)

// OpenCode reads sessions through `opencode export <id> --sanitize`.
//
// This is the runtime MEMORY.md §1 measured at **11 MB and zero sessions** —
// installed, never run. That is the expected outcome here, and it is success,
// not an error. What must never happen is reporting an empty history because we
// failed to ask properly, so the two are separated: an enumeration command that
// answered with an empty list is [StoreEmpty]; no enumeration command answering
// at all is [StoreUnreadable], carrying every command we tried.
//
// **--sanitize is used, always.** MEMORY.md §4 singles it out: it redacts
// secrets and file data, and §6 wants exactly that behaviour everywhere. It is
// a redaction primitive that already exists in a runtime we drive, so using it
// costs nothing and means the transcript arrives already stripped. Relay's own
// detector still runs over the result — defence in depth, and the two
// redactions are independent.
//
// One honest gap: only `opencode export <id>` is documented in §4. How to *list*
// ids has never been probed, so this reader tries a short list of `--json`
// shaped commands and records which one answered, and falls back to reading the
// on-disk storage layout. Every one of those is labelled a guess in the notes.
type OpenCode struct {
	env Env

	// Dir overrides the resolved data directory used for the on-disk fallback.
	Dir string

	// Binary is the command to run. Overridable so a test can point at a stub.
	Binary string
}

// NewOpenCode builds the reader.
func NewOpenCode(env Env) *OpenCode { return &OpenCode{env: env} }

// Runtime is opencode.
func (o *OpenCode) Runtime() adapter.Runtime { return adapter.OpenCode }

func (o *OpenCode) binary() string {
	if o.Binary != "" {
		return o.Binary
	}
	return "opencode"
}

// enumerationCommands are tried in order. None has been probed against a real
// OpenCode install; the first that exits 0 with parseable JSON wins, and the
// notes say which one it was.
var enumerationCommands = [][]string{
	{"sessions", "list", "--json"},
	{"session", "list", "--json"},
	{"sessions", "--json"},
}

// dataDir resolves where OpenCode keeps its storage. Every candidate is a
// guess: no relocation variable has been probed.
func (o *OpenCode) dataDir() []string {
	if o.Dir != "" {
		return []string{o.Dir}
	}
	var out []string
	if v := o.env.getenv("OPENCODE_DATA"); v != "" {
		out = append(out, o.env.expand(v))
	}
	if v := o.env.getenv("XDG_DATA_HOME"); v != "" {
		out = append(out, filepath.Join(o.env.expand(v), "opencode"))
	}
	return append(out,
		filepath.Join(o.env.Home, ".local", "share", "opencode"),
		filepath.Join(o.env.Home, ".opencode"),
	)
}

// Scan enumerates session ids.
func (o *OpenCode) Scan(ctx context.Context) (ScanResult, error) {
	res := ScanResult{Runtime: adapter.OpenCode}

	// Where we looked, recorded up front: an empty result is only honest when
	// it comes with the places that were checked.
	res.Roots = append([]string{o.binary() + " (on PATH)"}, o.dataDir()...)

	if o.env.Exec == nil {
		res.Status = StoreAbsent
		res.note("no way to run %s in this environment", o.binary())
		return res, nil
	}
	if _, err := o.env.Exec.LookPath(o.binary()); err != nil {
		res.Status = StoreAbsent
		res.note("%s is not on PATH — nothing to import, and nothing wrong", o.binary())
		return res, nil
	}

	var tried []string
	for _, args := range enumerationCommands {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		tried = append(tried, o.binary()+" "+strings.Join(args, " "))
		out, err := o.env.Exec.Run(ctx, detect.Cmd{Name: o.binary(), Args: args})
		if err != nil || out.Code != 0 {
			continue
		}
		ids, ok := parseOpenCodeIDs(out.Stdout)
		if !ok {
			res.note("%s exited 0 but its output was not JSON we could read", tried[len(tried)-1])
			continue
		}
		res.Roots = append(res.Roots, tried...)
		for _, id := range ids {
			res.Refs = append(res.Refs, Ref{
				Runtime:   adapter.OpenCode,
				SessionID: id,
				Path:      openCodePointer(id),
				MTimeFrom: "export payload timestamp, because the transcript is produced by a command rather than read from a file",
			})
		}
		res.note("session ids came from `%s`; that command is a guess — MEMORY.md §4 documents only `opencode export <id>`", tried[len(tried)-1])
		res.Status = StoreOK
		if len(res.Refs) == 0 {
			res.Status = StoreEmpty
			res.note("OpenCode answered with no sessions. MEMORY.md §1 measured exactly this: 11 MB, installed, never run")
		}
		o.attachStoragePaths(res.Refs, &res)
		return res, nil
	}

	// Nothing answered. Fall back to the on-disk layout before giving up.
	if ids, root, ok := o.scanStorage(); ok {
		res.Roots = append(append(res.Roots, tried...), root)
		for _, id := range ids {
			ref := Ref{Runtime: adapter.OpenCode, SessionID: id, MTimeFrom: "storage file mtime"}
			res.Refs = append(res.Refs, ref)
		}
		o.attachStoragePaths(res.Refs, &res)
		res.note("no enumeration command answered, so session ids were taken from the storage layout under %s — a guess, because that layout has not been probed", root)
		res.Status = StoreOK
		if len(res.Refs) == 0 {
			res.Status = StoreEmpty
		}
		return res, nil
	}

	res.Status = StoreUnreadable
	res.Roots = append(res.Roots, tried...)
	res.Err = fmt.Errorf("no OpenCode enumeration command answered")
	res.note("%s is installed but nothing would list its sessions. Tried: %s. Reporting this as unreadable rather than as an empty history, because the two lead to opposite decisions and only one of them is recoverable",
		o.binary(), strings.Join(tried, ", "))
	return res, nil
}

// attachStoragePaths points each ref at its storage file when one can be found,
// so the index holds a pointer to something on disk rather than only to a
// command.
func (o *OpenCode) attachStoragePaths(refs []Ref, res *ScanResult) {
	files, _, ok := o.storageFiles()
	if !ok {
		return
	}
	for i := range refs {
		if p, hit := files[refs[i].SessionID]; hit {
			refs[i].Path = p
			if info, err := os.Stat(p); err == nil {
				refs[i].MTime, refs[i].Size = info.ModTime(), info.Size()
				refs[i].MTimeFrom = "storage file mtime"
			}
		}
	}
	res.note("session transcripts were located under OpenCode's storage directory; the index points there rather than at the export command")
}

func (o *OpenCode) scanStorage() ([]string, string, bool) {
	files, root, ok := o.storageFiles()
	if !ok {
		return nil, "", false
	}
	ids := make([]string, 0, len(files))
	for id := range files {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, root, true
}

// storageFiles maps session id → file, from <data>/storage/session/**.json.
func (o *OpenCode) storageFiles() (map[string]string, string, bool) {
	for _, dir := range o.dataDir() {
		root := filepath.Join(dir, "storage", "session")
		if !dirExists(root) {
			continue
		}
		out := map[string]string{}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
				return nil
			}
			out[strings.TrimSuffix(d.Name(), ".json")] = path
			return nil
		})
		return out, root, true
	}
	return nil, "", false
}

// openCodePointer is the pointer used when the transcript is not a file we can
// name. It says how to re-read the session rather than pretending to a path.
func openCodePointer(id string) string { return "opencode://export/" + id }

// parseOpenCodeIDs accepts the shapes a list command could plausibly return.
func parseOpenCodeIDs(b []byte) ([]string, bool) {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return nil, false
	}

	var strs []string
	if err := json.Unmarshal([]byte(trimmed), &strs); err == nil {
		return strs, true
	}

	type entry struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
		Session   string `json:"session_id"`
	}
	pick := func(e entry) string {
		for _, v := range []string{e.ID, e.SessionID, e.Session} {
			if v != "" {
				return v
			}
		}
		return ""
	}

	var arr []entry
	if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			if id := pick(e); id != "" {
				out = append(out, id)
			}
		}
		return out, true
	}

	var wrapped struct {
		Sessions []entry `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(trimmed), &wrapped); err == nil && wrapped.Sessions != nil {
		out := make([]string, 0, len(wrapped.Sessions))
		for _, e := range wrapped.Sessions {
			if id := pick(e); id != "" {
				out = append(out, id)
			}
		}
		return out, true
	}
	return nil, false
}

// ------------------------------------------------------------ export format --

type openCodeExport struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	Cwd       string `json:"cwd"`
	Path      string `json:"path"`
	Git       struct {
		Branch string `json:"branch"`
	} `json:"git"`
	Time struct {
		Created any `json:"created"`
		Updated any `json:"updated"`
	} `json:"time"`
	ModelID    string   `json:"modelID"`
	Model      string   `json:"model"`
	ProviderID string   `json:"providerID"`
	Cost       *float64 `json:"cost"`
	Tokens     *struct {
		Input  int64 `json:"input"`
		Output int64 `json:"output"`
		Total  int64 `json:"total"`
	} `json:"tokens"`
	Messages []openCodeMessage `json:"messages"`
}

type openCodeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Parts   json.RawMessage `json:"parts"`
	Time    struct {
		Created any `json:"created"`
	} `json:"time"`
	Tool string `json:"tool"`
}

// Read runs `opencode export <id> --sanitize`.
func (o *OpenCode) Read(ctx context.Context, ref Ref) (Session, error) {
	s := Session{
		Runtime:     adapter.OpenCode,
		SessionID:   ref.SessionID,
		Path:        ref.Path,
		SourceMTime: ref.MTime,
		SourceSize:  ref.Size,
		MTimeFrom:   ref.MTimeFrom,
	}
	if s.Path == "" {
		s.Path = openCodePointer(ref.SessionID)
		s.Note("this session has no file on disk we could name; the pointer records the command that re-reads it")
	}
	if o.env.Exec == nil {
		return s, fmt.Errorf("backfill: opencode: no exec")
	}

	out, err := o.env.Exec.Run(ctx, detect.Cmd{
		Name: o.binary(),
		Args: []string{"export", ref.SessionID, "--sanitize"},
	})
	if err != nil {
		return s, fmt.Errorf("backfill: opencode export %s: %w", ref.SessionID, err)
	}
	if out.Code != 0 {
		return s, fmt.Errorf("backfill: opencode export %s: exit %d: %s", ref.SessionID, out.Code, out.Err())
	}

	var ex openCodeExport
	if err := json.Unmarshal(out.Stdout, &ex); err != nil {
		return s, fmt.Errorf("backfill: opencode export %s: %w", ref.SessionID, err)
	}
	s.Note("exported with --sanitize, which redacts secrets and file data before we ever see them (MEMORY.md §4); Relay's own detector runs over the result as well")

	if ex.Title != "" {
		s.Title, s.TitleSource = ex.Title, index.TitleStored
	}
	for _, cand := range []string{ex.Directory, ex.Cwd, ex.Path} {
		if cand != "" {
			s.Workspace = cand
			break
		}
	}
	s.GitBranch = ex.Git.Branch
	for _, cand := range []string{ex.ModelID, ex.Model} {
		if cand != "" {
			s.Model = cand
			break
		}
	}
	if ex.ProviderID != "" {
		s.Note("provider %s", ex.ProviderID)
	}
	s.StartedAt = anyTime(ex.Time.Created)
	s.EndedAt = anyTime(ex.Time.Updated)

	text := newTextBuilder(o.env.maxText())
	var firstUser string
	for _, m := range ex.Messages {
		if err := ctx.Err(); err != nil {
			return s, err
		}
		s.Messages++
		body := codexText(m.Content)
		if body == "" {
			body = codexText(m.Parts)
		}
		if m.Tool != "" {
			s.ToolCalls++
		}
		if m.Role == "user" && firstUser == "" {
			firstUser = body
		}
		text.add(m.Role, body)
	}
	s.Text = text.String()
	s.TextTruncated = text.truncated

	if s.Title == "" && firstUser != "" {
		s.Title, s.TitleSource = titleFrom(firstUser), index.TitleFirstMessage
	}
	if ex.Cost != nil {
		s.CostUSD = ex.Cost
	} else {
		s.Note("the export carried no cost figure; left nil rather than zero")
	}
	if ex.Tokens != nil {
		total := ex.Tokens.Total
		if total == 0 {
			total = ex.Tokens.Input + ex.Tokens.Output
		}
		s.TokensTotal = int64p(total)
	}
	if s.SourceMTime.IsZero() {
		// The resume key for a command-sourced session is the session's own
		// updated timestamp: there is no file whose mtime we could use.
		s.SourceMTime = s.EndedAt
		if s.SourceMTime.IsZero() {
			s.SourceMTime = s.StartedAt
		}
		s.SourceSize = s.Messages
		s.MTimeFrom = "export timestamp and message count, because the transcript is not a file"
	}
	if ex.ID != "" && ex.ID != ref.SessionID {
		s.Note("the export reports id %s for the session we asked for as %s", ex.ID, ref.SessionID)
	}
	return s, nil
}
