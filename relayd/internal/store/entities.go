package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned by every Get in this package.
var ErrNotFound = errors.New("store: not found")

// SYSTEM.md §5's seven entities. Two absences are deliberate and are enforced
// by the schema rather than by convention: no raw audio after transcription,
// and no user table — one box, one person.

// Device is a paired glasses or phone.
type Device struct {
	ID       string
	Kind     string // glasses | phone
	Name     string
	PairedAt time.Time
	LastSeen time.Time
}

// SessionState is a live session's status.
type SessionState string

const (
	SessionRunning SessionState = "running"
	// SessionAwaiting is blocked on a human. DASHBOARD.md §3.1 puts these at
	// the top of the list, unmissable, because a blocked session is the one
	// failure mode that silently stops all work.
	SessionAwaiting SessionState = "awaiting"
	SessionIdle     SessionState = "idle"
	SessionClosed   SessionState = "closed"
)

// Session is one conversation with one runtime, in the registry tier.
type Session struct {
	ID         string
	Runtime    string
	NativeID   string
	Agent      string
	Subject    string
	Workspace  string
	GitBranch  string
	Entities   []string
	CreatedAt  time.Time
	LastActive time.Time
	State      SessionState

	// Nil rather than zero where the runtime cannot report it.
	CostUSD       *float64
	TokensTotal   *int64
	TokensInput   *int64
	ContextWindow *int64
}

// Turn is one exchange within a session.
type Turn struct {
	ID         string
	SessionID  string
	Role       string // user | agent
	Text       string
	At         time.Time
	AudioRef   string
	StopReason string
	OK         bool
	Duration   time.Duration
	CostUSD    *float64
	Tokens     *int64
}

// ToolCall records that a tool ran. args_digest is a digest and never the
// arguments: tool arguments routinely carry paths, tokens and payloads.
type ToolCall struct {
	ID           string
	SessionID    string
	TurnID       string
	Tool         string
	Target       string
	ArgsDigest   string
	At           time.Time
	ResultStatus string
}

// Episode is a stretch of captured day.
type Episode struct {
	ID           string
	StartedAt    time.Time
	EndedAt      time.Time
	Kind         string // meeting | focus | conversation | ambient
	Transcript   string
	Participants []string
	Location     string
}

// Commitment is something the user said they would do.
type Commitment struct {
	ID        string
	EpisodeID string
	Text      string
	OwedTo    string
	DueAt     time.Time
	DoneAt    time.Time
	CreatedAt time.Time
}

// Grant is a connector's authorisation. Revoke once, everywhere.
type Grant struct {
	ID         string
	Connector  string
	Scopes     []string
	GrantedAt  time.Time
	LastUsedAt time.Time
	RevokedAt  time.Time
}

// ---------------------------------------------------------------- devices --

func (d *DB) PutDevice(ctx context.Context, v Device) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO device (id, kind, name, paired_at, last_seen)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			kind = excluded.kind, name = excluded.name, last_seen = excluded.last_seen`,
		v.ID, v.Kind, v.Name, ms(v.PairedAt), ms(v.LastSeen))
	return err
}

func (d *DB) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, kind, name, paired_at, last_seen FROM device ORDER BY paired_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Device
	for rows.Next() {
		var v Device
		var paired, seen int64
		if err := rows.Scan(&v.ID, &v.Kind, &v.Name, &paired, &seen); err != nil {
			return nil, err
		}
		v.PairedAt, v.LastSeen = at(paired), at(seen)
		out = append(out, v)
	}
	return out, rows.Err()
}

// --------------------------------------------------------------- sessions --

func (d *DB) PutSession(ctx context.Context, v Session) error {
	ents, err := json.Marshal(nonNil(v.Entities))
	if err != nil {
		return err
	}
	_, err = d.sql.ExecContext(ctx, `
		INSERT INTO session (id, runtime, native_id, agent, subject, workspace, git_branch,
		                     entities, created_at, last_active, state,
		                     cost_usd, tokens_total, tokens_input, context_window)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			runtime = excluded.runtime, native_id = excluded.native_id,
			agent = excluded.agent, subject = excluded.subject,
			workspace = excluded.workspace, git_branch = excluded.git_branch,
			entities = excluded.entities, last_active = excluded.last_active,
			state = excluded.state, cost_usd = excluded.cost_usd,
			tokens_total = excluded.tokens_total, tokens_input = excluded.tokens_input,
			context_window = excluded.context_window`,
		v.ID, v.Runtime, v.NativeID, v.Agent, v.Subject, v.Workspace, v.GitBranch,
		string(ents), ms(v.CreatedAt), ms(v.LastActive), string(v.State),
		v.CostUSD, v.TokensTotal, v.TokensInput, v.ContextWindow)
	return err
}

const sessionCols = `id, runtime, native_id, agent, subject, workspace, git_branch,
	entities, created_at, last_active, state, cost_usd, tokens_total, tokens_input, context_window`

func scanSession(sc interface{ Scan(...any) error }) (Session, error) {
	var v Session
	var ents string
	var created, active int64
	var state string
	err := sc.Scan(&v.ID, &v.Runtime, &v.NativeID, &v.Agent, &v.Subject, &v.Workspace,
		&v.GitBranch, &ents, &created, &active, &state,
		&v.CostUSD, &v.TokensTotal, &v.TokensInput, &v.ContextWindow)
	if err != nil {
		return Session{}, err
	}
	v.CreatedAt, v.LastActive = at(created), at(active)
	v.State = SessionState(state)
	if err := json.Unmarshal([]byte(ents), &v.Entities); err != nil {
		return Session{}, fmt.Errorf("store: session %s entities: %w", v.ID, err)
	}
	return v, nil
}

func (d *DB) GetSession(ctx context.Context, id string) (Session, error) {
	row := d.sql.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM session WHERE id = ?`, id)
	v, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	return v, err
}

// SessionFilter narrows ListSessions. The zero value lists everything.
type SessionFilter struct {
	Runtime   string
	State     SessionState
	Workspace string
	Limit     int
}

func (d *DB) ListSessions(ctx context.Context, f SessionFilter) ([]Session, error) {
	q := `SELECT ` + sessionCols + ` FROM session WHERE 1 = 1`
	var args []any
	if f.Runtime != "" {
		q += ` AND runtime = ?`
		args = append(args, f.Runtime)
	}
	if f.State != "" {
		q += ` AND state = ?`
		args = append(args, string(f.State))
	}
	if f.Workspace != "" {
		q += ` AND workspace = ?`
		args = append(args, f.Workspace)
	}
	q += ` ORDER BY last_active DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		v, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ turns --

func (d *DB) PutTurn(ctx context.Context, v Turn) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO turn (id, session_id, role, text, at, audio_ref,
		                  stop_reason, ok, duration_ms, cost_usd, tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			text = excluded.text, stop_reason = excluded.stop_reason,
			ok = excluded.ok, duration_ms = excluded.duration_ms,
			cost_usd = excluded.cost_usd, tokens = excluded.tokens`,
		v.ID, v.SessionID, v.Role, v.Text, ms(v.At), nullString(v.AudioRef),
		v.StopReason, boolInt(v.OK), v.Duration.Milliseconds(), v.CostUSD, v.Tokens)
	return err
}

func (d *DB) ListTurns(ctx context.Context, sessionID string, limit int) ([]Turn, error) {
	q := `SELECT id, session_id, role, text, at, audio_ref, stop_reason, ok, duration_ms, cost_usd, tokens
	      FROM turn WHERE session_id = ? ORDER BY at`
	args := []any{sessionID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		var v Turn
		var atMS, durMS int64
		var okInt int64
		var audio sql.NullString
		if err := rows.Scan(&v.ID, &v.SessionID, &v.Role, &v.Text, &atMS, &audio,
			&v.StopReason, &okInt, &durMS, &v.CostUSD, &v.Tokens); err != nil {
			return nil, err
		}
		v.At = at(atMS)
		v.AudioRef = audio.String
		v.OK = okInt != 0
		v.Duration = time.Duration(durMS) * time.Millisecond
		out = append(out, v)
	}
	return out, rows.Err()
}

// -------------------------------------------------------------- toolcalls --

func (d *DB) PutToolCall(ctx context.Context, v ToolCall) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO tool_call (id, session_id, turn_id, tool, target, args_digest, at, result_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET result_status = excluded.result_status, target = excluded.target`,
		v.ID, v.SessionID, v.TurnID, v.Tool, v.Target, v.ArgsDigest, ms(v.At), v.ResultStatus)
	return err
}

func (d *DB) ListToolCalls(ctx context.Context, sessionID string) ([]ToolCall, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, session_id, turn_id, tool, target, args_digest, at, result_status
		 FROM tool_call WHERE session_id = ? ORDER BY at`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ToolCall
	for rows.Next() {
		var v ToolCall
		var atMS int64
		if err := rows.Scan(&v.ID, &v.SessionID, &v.TurnID, &v.Tool, &v.Target,
			&v.ArgsDigest, &atMS, &v.ResultStatus); err != nil {
			return nil, err
		}
		v.At = at(atMS)
		out = append(out, v)
	}
	return out, rows.Err()
}

// --------------------------------------------------- episodes, commitments --

func (d *DB) PutEpisode(ctx context.Context, v Episode) error {
	parts, err := json.Marshal(nonNil(v.Participants))
	if err != nil {
		return err
	}
	_, err = d.sql.ExecContext(ctx, `
		INSERT INTO episode (id, started_at, ended_at, kind, transcript, participants, location)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			ended_at = excluded.ended_at, transcript = excluded.transcript,
			participants = excluded.participants, location = excluded.location`,
		v.ID, ms(v.StartedAt), nullMS(v.EndedAt), v.Kind, v.Transcript,
		string(parts), nullString(v.Location))
	return err
}

func (d *DB) ListEpisodes(ctx context.Context, limit int) ([]Episode, error) {
	q := `SELECT id, started_at, ended_at, kind, transcript, participants, location
	      FROM episode ORDER BY started_at DESC`
	var args []any
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Episode
	for rows.Next() {
		var v Episode
		var started int64
		var ended *int64
		var parts string
		var loc sql.NullString
		if err := rows.Scan(&v.ID, &started, &ended, &v.Kind, &v.Transcript, &parts, &loc); err != nil {
			return nil, err
		}
		v.StartedAt, v.EndedAt = at(started), atPtr(ended)
		v.Location = loc.String
		if err := json.Unmarshal([]byte(parts), &v.Participants); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (d *DB) PutCommitment(ctx context.Context, v Commitment) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO commitment (id, episode_id, text, owed_to, due_at, done_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			text = excluded.text, owed_to = excluded.owed_to,
			due_at = excluded.due_at, done_at = excluded.done_at`,
		v.ID, nullString(v.EpisodeID), v.Text, nullString(v.OwedTo),
		nullMS(v.DueAt), nullMS(v.DoneAt), ms(v.CreatedAt))
	return err
}

func (d *DB) ListCommitments(ctx context.Context, openOnly bool) ([]Commitment, error) {
	q := `SELECT id, episode_id, text, owed_to, due_at, done_at, created_at FROM commitment`
	if openOnly {
		q += ` WHERE done_at IS NULL`
	}
	q += ` ORDER BY COALESCE(due_at, created_at)`
	rows, err := d.sql.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Commitment
	for rows.Next() {
		var v Commitment
		var ep, owed sql.NullString
		var due, done *int64
		var created int64
		if err := rows.Scan(&v.ID, &ep, &v.Text, &owed, &due, &done, &created); err != nil {
			return nil, err
		}
		v.EpisodeID, v.OwedTo = ep.String, owed.String
		v.DueAt, v.DoneAt, v.CreatedAt = atPtr(due), atPtr(done), at(created)
		out = append(out, v)
	}
	return out, rows.Err()
}

// ----------------------------------------------------------------- grants --

func (d *DB) PutGrant(ctx context.Context, v Grant) error {
	scopes, err := json.Marshal(nonNil(v.Scopes))
	if err != nil {
		return err
	}
	_, err = d.sql.ExecContext(ctx, `
		INSERT INTO grant (id, connector, scopes, granted_at, last_used_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			scopes = excluded.scopes, last_used_at = excluded.last_used_at,
			revoked_at = excluded.revoked_at`,
		v.ID, v.Connector, string(scopes), ms(v.GrantedAt),
		nullMS(v.LastUsedAt), nullMS(v.RevokedAt))
	return err
}

func (d *DB) ListGrants(ctx context.Context) ([]Grant, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, connector, scopes, granted_at, last_used_at, revoked_at
		 FROM grant ORDER BY granted_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Grant
	for rows.Next() {
		var v Grant
		var scopes string
		var granted int64
		var used, revoked *int64
		if err := rows.Scan(&v.ID, &v.Connector, &scopes, &granted, &used, &revoked); err != nil {
			return nil, err
		}
		v.GrantedAt, v.LastUsedAt, v.RevokedAt = at(granted), atPtr(used), atPtr(revoked)
		if err := json.Unmarshal([]byte(scopes), &v.Scopes); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
