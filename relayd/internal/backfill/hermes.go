package backfill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/adapter"
	"github.com/luthor007/relay/relayd/internal/index"

	// Sets sqlite3.Binary. Blank-importing this — and never
	// go-sqlite3/embed — is what keeps the driver working in this package the
	// same way it does in internal/store (MEMORY.md §3).
	_ "github.com/asg017/sqlite-vec-go-bindings/ncruces"
	_ "github.com/ncruces/go-sqlite3/driver"
)

// Hermes reads ~/.hermes/state.db.
//
// The largest store by a wide margin: 2.5 GB, 27 sessions, 4,379 messages on
// the measured machine — 70% of the whole corpus (MEMORY.md §1). It is also the
// only store that is a live database rather than a pile of files, and that
// brings two rules that are not negotiable:
//
//   - **Open it read-only.** Backfill reads another program's working
//     database. `mode=ro` plus `query_only` means a bug here cannot corrupt
//     2.5 GB of somebody's history.
//   - **Never take the compression lease.** Hermes coordinates its own
//     compaction through a `compression_locks` lease (session_id, holder,
//     expires_at) which has dedicated upstream concurrency tests, so the
//     contention is real. Backfill has no business in it: it is a reader, and
//     taking a lease it does not need would block Hermes's own compression.
//     [Hermes.query] refuses any statement that mentions that table, and
//     TestHermesNeverTouchesTheCompressionLease is what holds it.
//
// The schema is read by introspection rather than by a fixed SELECT. MEMORY.md
// §4 names the columns that matter — title, cwd, model, started_at,
// message_count, tool_call_count, estimated_cost_usd, actual_cost_usd — but
// nobody has probed the full schema, and a rigid query breaks on the first
// version drift. What we cannot find, we leave nil and say so.
type Hermes struct {
	env Env

	// Dir overrides the resolved state directory.
	Dir string
	// DBPath overrides the database file outright.
	DBPath string
}

// NewHermes builds the reader.
func NewHermes(env Env) *Hermes { return &Hermes{env: env} }

// Runtime is hermes.
func (h *Hermes) Runtime() adapter.Runtime { return adapter.Hermes }

func (h *Hermes) dbPath() (path, source string) {
	if h.DBPath != "" {
		return h.DBPath, "explicit"
	}
	dir := h.Dir
	source = "~/.hermes, where MEMORY.md §4 found state.db"
	switch {
	case dir != "":
		source = "explicit"
	case h.env.getenv("HERMES_STATE_DIR") != "":
		dir = h.env.expand(h.env.getenv("HERMES_STATE_DIR"))
		source = "HERMES_STATE_DIR"
	default:
		dir = filepath.Join(h.env.Home, ".hermes")
	}
	return filepath.Join(dir, "state.db"), source
}

// ErrCompressionLease is returned if anything in this package ever tries to
// read or write Hermes's compaction lease.
var ErrCompressionLease = errors.New("backfill: refusing to touch Hermes's compression_locks lease")

// open opens state.db read-only.
func (h *Hermes) open() (*sql.DB, error) {
	path, _ := h.dbPath()
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "query_only(true)")
	q.Set("mode", "ro")
	db, err := sql.Open("sqlite3", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// query is the only way this reader talks to the database, and it refuses the
// one table Hermes uses to coordinate with itself.
func (h *Hermes) query(ctx context.Context, db *sql.DB, q string, args ...any) (*sql.Rows, error) {
	if strings.Contains(strings.ToLower(q), "compression_lock") {
		return nil, fmt.Errorf("%w: %s", ErrCompressionLease, q)
	}
	return db.QueryContext(ctx, q, args...)
}

// hermesSchema is what introspection concluded about this database.
type hermesSchema struct {
	sessionTable string
	messageTable string

	// session columns, logical name → actual name. Empty means absent.
	id, title, cwd, model               string
	startedAt, endedAt                  string
	messageCount, toolCallCount         string
	actualCost, estimatedCost           string
	inputTokens, outputTokens, totalTok string
	cacheReadTokens                     string

	// message columns
	msgSession, msgRole, msgContent, msgAt string

	missing []string
}

func (h *Hermes) introspect(ctx context.Context, db *sql.DB) (hermesSchema, error) {
	var s hermesSchema

	tables, err := h.tableNames(ctx, db)
	if err != nil {
		return s, err
	}
	s.sessionTable = pickTable(tables, "sessions", "session", "conversations", "threads")
	if s.sessionTable == "" {
		return s, fmt.Errorf("no session table in %v", tables)
	}
	s.messageTable = pickTable(tables, "messages", "message", "conversation_messages")

	cols, err := h.columnNames(ctx, db, s.sessionTable)
	if err != nil {
		return s, err
	}
	s.id = pickCol(cols, "id", "session_id", "uuid")
	s.title = pickCol(cols, "title", "name", "summary")
	s.cwd = pickCol(cols, "cwd", "working_directory", "working_dir", "workdir", "directory")
	s.model = pickCol(cols, "model", "model_name", "model_id")
	s.startedAt = pickCol(cols, "started_at", "created_at", "start_time")
	s.endedAt = pickCol(cols, "ended_at", "updated_at", "last_active_at", "last_message_at", "end_time")
	s.messageCount = pickCol(cols, "message_count", "messages", "num_messages")
	s.toolCallCount = pickCol(cols, "tool_call_count", "tool_calls", "num_tool_calls")
	s.actualCost = pickCol(cols, "actual_cost_usd", "actual_cost")
	s.estimatedCost = pickCol(cols, "estimated_cost_usd", "estimated_cost")
	s.inputTokens = pickCol(cols, "input_tokens", "prompt_tokens")
	s.outputTokens = pickCol(cols, "output_tokens", "completion_tokens")
	s.totalTok = pickCol(cols, "total_tokens", "tokens_total")
	s.cacheReadTokens = pickCol(cols, "cache_read_tokens", "cached_input_tokens")

	if s.id == "" {
		return s, fmt.Errorf("%s has no id column: %v", s.sessionTable, cols)
	}
	for name, col := range map[string]string{
		"title": s.title, "cwd": s.cwd, "model": s.model,
		"started_at": s.startedAt, "message_count": s.messageCount,
		"tool_call_count": s.toolCallCount,
	} {
		if col == "" {
			s.missing = append(s.missing, name)
		}
	}
	sort.Strings(s.missing)

	if s.messageTable != "" {
		mcols, err := h.columnNames(ctx, db, s.messageTable)
		if err == nil {
			s.msgSession = pickCol(mcols, "session_id", "conversation_id", "thread_id")
			s.msgRole = pickCol(mcols, "role", "sender", "author")
			s.msgContent = pickCol(mcols, "content", "text", "body", "message")
			s.msgAt = pickCol(mcols, "created_at", "at", "timestamp", "ts")
			if s.msgSession == "" || s.msgContent == "" {
				s.messageTable = ""
			}
		} else {
			s.messageTable = ""
		}
	}
	return s, nil
}

func (h *Hermes) tableNames(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := h.query(ctx, db, `SELECT name FROM sqlite_master WHERE type IN ('table','view')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (h *Hermes) columnNames(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := h.query(ctx, db, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Scan lists sessions.
//
// The resume key is the session's own last activity and its message count, not
// state.db's mtime: one database holds every session, so the file's mtime moves
// whenever any session does and would re-index all 27 on every run.
func (h *Hermes) Scan(ctx context.Context) (ScanResult, error) {
	res := ScanResult{Runtime: adapter.Hermes}
	path, source := h.dbPath()
	res.Roots = []string{path}

	if !fileExists(path) {
		res.Status = StoreAbsent
		res.note("no Hermes database at %s (%s) — nothing to import, and nothing wrong", path, source)
		return res, nil
	}

	db, err := h.open()
	if err != nil {
		res.Status = StoreUnreadable
		res.Err = err
		res.note("%s exists but could not be opened read-only: %v. Reporting this rather than an empty history, because they lead to opposite decisions", path, err)
		return res, nil
	}
	defer db.Close()

	schema, err := h.introspect(ctx, db)
	if err != nil {
		res.Status = StoreUnreadable
		res.Err = err
		res.note("%s does not have the schema MEMORY.md §4 describes: %v", path, err)
		return res, nil
	}
	if len(schema.missing) > 0 {
		res.note("Hermes's %s table has no %s column; those fields are left empty rather than guessed",
			schema.sessionTable, strings.Join(schema.missing, ", "))
	}
	if schema.messageTable == "" {
		res.note("no readable message table, so session text is unavailable: metadata will be indexed, and nothing will be summarised or scanned for secrets")
	}

	sel := []string{schema.id}
	sel = append(sel, optCol(schema.title), optCol(schema.startedAt), optCol(schema.endedAt), optCol(schema.messageCount))
	q := fmt.Sprintf(`SELECT %s FROM "%s"`, strings.Join(sel, ", "), schema.sessionTable)

	rows, err := h.query(ctx, db, q)
	if err != nil {
		res.Status = StoreUnreadable
		res.Err = err
		res.note("could not list sessions: %v", err)
		return res, nil
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var title, started, ended, count any
		if err := rows.Scan(&id, &title, &started, &ended, &count); err != nil {
			res.note("skipped a session row: %v", err)
			continue
		}
		last := anyTime(ended)
		if last.IsZero() {
			last = anyTime(started)
		}
		res.Refs = append(res.Refs, Ref{
			Runtime:   adapter.Hermes,
			SessionID: id,
			Path:      path,
			MTime:     last,
			Size:      anyInt(count),
			MTimeFrom: "session last-activity column, because one database holds every session",
			Title:     anyString(title),
			StartedAt: anyTime(started),
		})
	}
	if err := rows.Err(); err != nil {
		res.Status = StoreUnreadable
		res.Err = err
		return res, nil
	}

	sort.Slice(res.Refs, func(i, j int) bool { return res.Refs[i].SessionID < res.Refs[j].SessionID })
	res.Status = StoreOK
	if len(res.Refs) == 0 {
		res.Status = StoreEmpty
		res.note("%s opened cleanly and holds no sessions", path)
	}
	return res, nil
}

// Read pulls one session's row and its messages.
func (h *Hermes) Read(ctx context.Context, ref Ref) (Session, error) {
	s := Session{
		Runtime:     adapter.Hermes,
		SessionID:   ref.SessionID,
		Path:        ref.Path,
		SourceMTime: ref.MTime,
		SourceSize:  ref.Size,
		MTimeFrom:   ref.MTimeFrom,
	}
	s.Note("the pointer for a Hermes session is (runtime, session_id) inside %s; byte offsets do not apply to a database row", ref.Path)

	db, err := h.open()
	if err != nil {
		return s, fmt.Errorf("backfill: hermes: %w", err)
	}
	defer db.Close()

	schema, err := h.introspect(ctx, db)
	if err != nil {
		return s, fmt.Errorf("backfill: hermes schema: %w", err)
	}

	sel := []string{
		optCol(schema.title), optCol(schema.cwd), optCol(schema.model),
		optCol(schema.startedAt), optCol(schema.endedAt),
		optCol(schema.messageCount), optCol(schema.toolCallCount),
		optCol(schema.actualCost), optCol(schema.estimatedCost),
		optCol(schema.inputTokens), optCol(schema.outputTokens),
		optCol(schema.totalTok), optCol(schema.cacheReadTokens),
	}
	q := fmt.Sprintf(`SELECT %s FROM "%s" WHERE "%s" = ?`,
		strings.Join(sel, ", "), schema.sessionTable, schema.id)

	rows, err := h.query(ctx, db, q, ref.SessionID)
	if err != nil {
		return s, fmt.Errorf("backfill: hermes session: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return s, fmt.Errorf("backfill: hermes: no session %s", ref.SessionID)
	}

	var title, cwd, model, started, ended, msgs, tools, actual, estimated, in, out, total, cacheRead any
	if err := rows.Scan(&title, &cwd, &model, &started, &ended, &msgs, &tools,
		&actual, &estimated, &in, &out, &total, &cacheRead); err != nil {
		return s, fmt.Errorf("backfill: hermes scan: %w", err)
	}
	rows.Close()

	if t := anyString(title); t != "" {
		// MEMORY.md §4: Hermes titles its own sessions, so this is the
		// summariser's first job already done.
		s.Title, s.TitleSource = t, index.TitleGenerated
	}
	s.Workspace = anyString(cwd)
	s.Model = anyString(model)
	s.StartedAt = anyTime(started)
	s.EndedAt = anyTime(ended)
	s.Messages = anyInt(msgs)
	s.ToolCalls = anyInt(tools)

	// Cost: prefer what was actually charged, and say which one this is. An
	// estimate presented as an actual is a small lie that compounds across 27
	// sessions.
	switch {
	case anyFloatOK(actual):
		s.CostUSD = float64p(anyFloat(actual))
	case anyFloatOK(estimated):
		s.CostUSD = float64p(anyFloat(estimated))
		s.Note("cost is Hermes's estimated_cost_usd; actual_cost_usd was null for this session")
	default:
		s.Note("Hermes recorded no cost for this session; left nil rather than zero")
	}

	switch {
	case anyIntOK(total):
		s.TokensTotal = int64p(anyInt(total))
	case anyIntOK(in) || anyIntOK(out):
		s.TokensTotal = int64p(anyInt(in) + anyInt(out) + anyInt(cacheRead))
		s.Note("tokens are input + output + cache_read summed from the session row; Hermes has no single total column here")
	}

	if schema.messageTable == "" {
		s.Note("no readable message table: this session is indexed from its metadata only, and its text was never scanned for secrets")
		return s, nil
	}

	text := newTextBuilder(h.env.maxText())
	mq := fmt.Sprintf(`SELECT %s, %s FROM "%s" WHERE "%s" = ?`,
		optCol(schema.msgRole), optCol(schema.msgContent), schema.messageTable, schema.msgSession)
	if schema.msgAt != "" {
		mq += fmt.Sprintf(` ORDER BY "%s"`, schema.msgAt)
	}

	mrows, err := h.query(ctx, db, mq, ref.SessionID)
	if err != nil {
		s.Note("could not read messages: %v", err)
		return s, nil
	}
	defer mrows.Close()

	var counted int64
	for mrows.Next() {
		if err := ctx.Err(); err != nil {
			return s, err
		}
		var role, content any
		if err := mrows.Scan(&role, &content); err != nil {
			continue
		}
		counted++
		text.add(anyString(role), anyString(content))
	}
	if err := mrows.Err(); err != nil {
		s.Note("stopped reading messages early: %v", err)
	}

	s.Text = text.String()
	s.TextTruncated = text.truncated
	if text.truncated {
		s.Note("stopped extracting text at %d bytes; %d bytes were neither scanned for secrets nor summarised", h.env.maxText(), text.skipped)
	}
	if s.Messages == 0 {
		s.Messages = counted
		s.Note("message_count was absent; counted %d rows in %s instead", counted, schema.messageTable)
	}
	return s, nil
}

// -------------------------------------------------------------- schema help --

func pickTable(have []string, candidates ...string) string {
	set := map[string]string{}
	for _, h := range have {
		set[strings.ToLower(h)] = h
	}
	for _, c := range candidates {
		if actual, ok := set[c]; ok {
			return actual
		}
	}
	return ""
}

func pickCol(have []string, candidates ...string) string {
	set := map[string]string{}
	for _, h := range have {
		set[strings.ToLower(h)] = h
	}
	for _, c := range candidates {
		if actual, ok := set[c]; ok {
			return actual
		}
	}
	return ""
}

// optCol renders a column that may not exist as a NULL literal, so one SELECT
// works against several schema versions.
func optCol(name string) string {
	if name == "" {
		return "NULL"
	}
	return `"` + name + `"`
}

// ------------------------------------------------- loosely-typed SQL values --

func anyString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func anyInt(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	case []byte:
		n, _ := strconv.ParseInt(strings.TrimSpace(string(t)), 10, 64)
		return n
	}
	return 0
}

func anyIntOK(v any) bool {
	switch t := v.(type) {
	case int64, float64:
		return true
	case string:
		_, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return err == nil
	}
	return false
}

func anyFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int64:
		return float64(t)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	case []byte:
		f, _ := strconv.ParseFloat(strings.TrimSpace(string(t)), 64)
		return f
	}
	return 0
}

func anyFloatOK(v any) bool {
	switch t := v.(type) {
	case float64, int64:
		return true
	case string:
		_, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return err == nil
	}
	return false
}

// anyTime accepts the three shapes a timestamp arrives in from an unprobed
// schema: unix seconds, unix milliseconds, or text.
func anyTime(v any) time.Time {
	switch t := v.(type) {
	case nil:
		return time.Time{}
	case int64:
		return unixGuess(t)
	case float64:
		return unixGuess(int64(t))
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return unixGuess(n)
		}
		return parseTimestamp(t)
	case []byte:
		return anyTime(string(t))
	case time.Time:
		return t.UTC()
	}
	return time.Time{}
}

// unixGuess reads an integer timestamp. The thresholds are the only honest
// heuristic available without a probed schema: seconds since 2001 are ten
// digits, milliseconds thirteen, microseconds sixteen.
func unixGuess(n int64) time.Time {
	switch {
	case n <= 0:
		return time.Time{}
	case n > 1e17:
		return time.Unix(0, n).UTC()
	case n > 1e14:
		return time.UnixMicro(n).UTC()
	case n > 1e11:
		return time.UnixMilli(n).UTC()
	default:
		return time.Unix(n, 0).UTC()
	}
}
