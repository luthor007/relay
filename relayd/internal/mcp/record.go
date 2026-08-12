package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/luthor007/relay/relayd/internal/index"
	"github.com/luthor007/relay/relayd/internal/store"
)

// One audit trail — ORCHESTRATOR.md §4b: "every tool call from every agent
// through one place, which is what makes the 'last used, and for what' screen
// possible at all."
//
// Two records, because they answer different questions and live in different
// places:
//
//   - **Grant and revoke** are credential-shaped mutations and go through
//     internal/audit, which is append-only and hash-chained. That is
//     internal/connector's job; this file does not write there.
//   - **Every individual call** goes into the `tool_call` table, which is what
//     internal/api's lastToolFor already reads to fill DASHBOARD.md §3.4's
//     "last used, and for what" column, plus `grant.last_used_at` so an unused
//     connector can name itself as unused.
//
// The `tool_call` row is deliberately not the arguments. The schema comment
// says "a digest, never the arguments" and this file is what makes that true:
// [Digest] is a SHA-256 over the canonical JSON, and the only free text stored
// is Target, which goes through the same secret detector internal/index uses
// before anything is written. Detect before writing, never after — an argument
// that carried an API key must not become a permanent row that did.

// ErrUnattributed is a call the recorder could not tie to a session row.
//
// `tool_call.session_id` is a foreign key onto `session`, so a call from a
// runtime Relay is not driving has nowhere to land. That is a real gap and it
// is reported rather than papered over: the call still ran, so claiming a
// durable record of it would be worse than admitting there is none.
var ErrUnattributed = errors.New("mcp: this call is not attributable to a known session, so it was not recorded durably")

// Recorded is one call, as it goes into the trail.
type Recorded struct {
	ID        string
	Session   string
	Turn      string
	Connector string
	Tool      string
	// Target is the thing acted on. It is redacted before it is stored.
	Target string
	// ArgsDigest is a digest of the arguments. Never the arguments.
	ArgsDigest string
	At         time.Time
	// Status is "completed" | "failed" | "denied", matching the vocabulary the
	// tool_call column already uses plus the one the gateway adds for a
	// confirmation the user refused.
	Status string
}

// Recorder writes the per-call trail.
type Recorder interface {
	Record(ctx context.Context, r Recorded) error
}

// Digest is the argument digest: canonical JSON, SHA-256, hex. Two calls with
// the same arguments produce the same digest, which is what makes "it did this
// again" visible without storing what "this" was.
func Digest(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	// encoding/json sorts map keys, so this is already canonical for the shapes
	// MCP arguments take.
	b, err := json.Marshal(args)
	if err != nil {
		return "undigestable"
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// SQLRecorder writes into the main database.
type SQLRecorder struct {
	DB *store.DB
	// Detector redacts Target before it is written. Required — a recorder with
	// no detector would be a path that writes text to disk without going
	// through the detection step, which is the ordering MEMORY.md §12.2 says
	// cannot be undone once it is wrong.
	Detector *index.Detector
}

// NewSQLRecorder builds a recorder with the standard ruleset.
func NewSQLRecorder(db *store.DB) (*SQLRecorder, error) {
	d, err := index.NewDetector()
	if err != nil {
		return nil, err
	}
	return &SQLRecorder{DB: db, Detector: d}, nil
}

// Record writes one call. It returns ErrUnattributed when the session is not in
// the registry, having still updated the connector's last-used time — that part
// has no foreign key and is what the console's "unused access" warning reads.
func (r *SQLRecorder) Record(ctx context.Context, rec Recorded) error {
	if r == nil || r.DB == nil {
		return ErrNoRecorder
	}
	if r.Detector == nil {
		return errors.New("mcp: recorder has no secret detector, refusing to write text")
	}
	target, _ := r.Detector.Redact(rec.Target)

	if rec.Connector != "" {
		if _, err := r.DB.SQL().ExecContext(ctx,
			`UPDATE "grant" SET last_used_at = ?
			 WHERE connector = ? AND revoked_at IS NULL`,
			rec.At.UnixMilli(), rec.Connector); err != nil {
			return err
		}
	}

	res, err := r.DB.SQL().ExecContext(ctx,
		`INSERT INTO tool_call (id, session_id, turn_id, tool, target, args_digest, at, result_status)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?
		 WHERE EXISTS (SELECT 1 FROM session WHERE id = ?)`,
		rec.ID, rec.Session, rec.Turn, rec.Tool, target, rec.ArgsDigest,
		rec.At.UnixMilli(), rec.Status, rec.Session)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrUnattributed
	}
	return nil
}

// ErrNoRecorder is a recorder with no database behind it.
var ErrNoRecorder = errors.New("mcp: no call recorder configured")

// MemoryRecorder keeps the trail in memory. It exists for the same reason
// audit.Memory does: a box with no writable data directory still has to keep
// the call path honest, and the health screen can say the trail is not durable
// rather than implying a file that is not there.
type MemoryRecorder struct {
	// Limit bounds the ring. Zero means MemoryRecorderLimit.
	Limit int

	mu   sync.Mutex
	rows []Recorded
}

// MemoryRecorderLimit is how many calls a memory recorder keeps.
const MemoryRecorderLimit = 1000

// Record appends one call.
func (m *MemoryRecorder) Record(_ context.Context, r Recorded) error {
	limit := m.Limit
	if limit <= 0 {
		limit = MemoryRecorderLimit
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, r)
	if len(m.rows) > limit {
		m.rows = append([]Recorded(nil), m.rows[len(m.rows)-limit:]...)
	}
	return nil
}

// Calls returns the recorded calls, oldest first.
func (m *MemoryRecorder) Calls() []Recorded {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Recorded(nil), m.rows...)
}
