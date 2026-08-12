package connector

import (
	"context"
	"strings"
	"time"

	"github.com/luthor007/relay/relayd/internal/store"
)

// SQLMemory is migration 0004's two tables, and it sits next to [Proposer] the
// same way [SQLStore] sits next to the grant store: the package owns both the
// behaviour and the one place that behaviour is written down.
//
// What it deliberately cannot do is store what was said. [StoredSighting] has
// no text field and neither does the table, so the whole "show the user the
// sentence that triggered this" idea — which sounds helpful and would write
// unredacted speech into relay.db — has nowhere to land without a migration
// somebody would have to argue for.
type SQLMemory struct{ DB *store.DB }

// NewSQLMemory builds proposal memory on the main database.
func NewSQLMemory(db *store.DB) *SQLMemory { return &SQLMemory{DB: db} }

var _ ProposalMemory = (*SQLMemory)(nil)

func (m *SQLMemory) Sightings(ctx context.Context) ([]StoredSighting, error) {
	rows, err := m.DB.SQL().QueryContext(ctx,
		`SELECT connector, episode, at FROM connector_sighting ORDER BY at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StoredSighting
	for rows.Next() {
		var s StoredSighting
		var at int64
		if err := rows.Scan(&s.Connector, &s.Episode, &at); err != nil {
			return nil, err
		}
		s.At = time.UnixMilli(at).UTC()
		out = append(out, s)
	}
	return out, rows.Err()
}

func (m *SQLMemory) AddSighting(ctx context.Context, connector, episode string, at time.Time) error {
	_, err := m.DB.SQL().ExecContext(ctx,
		`INSERT INTO connector_sighting (connector, episode, at) VALUES (?, ?, ?)`,
		strings.ToLower(strings.TrimSpace(connector)), episode, at.UnixMilli())
	return err
}

func (m *SQLMemory) Dismissals(ctx context.Context) (map[string]time.Time, error) {
	rows, err := m.DB.SQL().QueryContext(ctx,
		`SELECT connector, at FROM connector_dismissal`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var at int64
		if err := rows.Scan(&name, &at); err != nil {
			return nil, err
		}
		out[name] = time.UnixMilli(at).UTC()
	}
	return out, rows.Err()
}

// PutDismissal replaces any earlier answer rather than appending one. The
// cooldown runs from the most recent decision, so a second "not now" has to
// move the clock forward.
func (m *SQLMemory) PutDismissal(ctx context.Context, connector string, at time.Time, reason string) error {
	_, err := m.DB.SQL().ExecContext(ctx,
		`INSERT INTO connector_dismissal (connector, at, reason) VALUES (?, ?, ?)
		 ON CONFLICT (connector) DO UPDATE SET at = excluded.at, reason = excluded.reason`,
		strings.ToLower(strings.TrimSpace(connector)), at.UnixMilli(), reason)
	return err
}

// Expire drops evidence that has fallen out of the window.
//
// Dismissals are NOT expired here even though they too have a cooldown. A
// dismissal that has aged out is a connector that may be proposed again, and
// the row is the only record that the user was ever asked — deleting it would
// make a second proposal look like a first one to anybody reading the database.
func (m *SQLMemory) Expire(ctx context.Context, before time.Time) error {
	_, err := m.DB.SQL().ExecContext(ctx,
		`DELETE FROM connector_sighting WHERE at < ?`, before.UnixMilli())
	return err
}
