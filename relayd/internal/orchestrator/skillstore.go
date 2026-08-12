package orchestrator

import (
	"context"
	"encoding/json"
	"time"

	"github.com/luthor007/relay/relayd/internal/store"
)

// sqlSkills is [SkillStore] over the main database.
type sqlSkills struct{ db *store.DB }

// SkillsIn returns the skill store for a database.
func SkillsIn(db *store.DB) SkillStore {
	if db == nil {
		return nil
	}
	return sqlSkills{db: db}
}

func (s sqlSkills) Load(ctx context.Context) ([]Skill, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT name, title, trigger_when, steps, needs, origin, created_at
		  FROM skill
		 WHERE deleted_at IS NULL
		 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Skill
	for rows.Next() {
		var sk Skill
		var needs string
		var created int64
		if err := rows.Scan(&sk.Name, &sk.Title, &sk.When, &sk.Steps, &needs, &sk.Origin, &created); err != nil {
			return nil, err
		}
		// A malformed needs list is not a reason to drop the skill: the field
		// is advisory, repeated in the instruction text, and the steps are the
		// part that matters.
		_ = json.Unmarshal([]byte(needs), &sk.Needs)
		sk.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, sk)
	}
	return out, rows.Err()
}

func (s sqlSkills) Save(ctx context.Context, sk Skill) error {
	needs, err := json.Marshal(sk.Needs)
	if err != nil {
		return err
	}
	if sk.Needs == nil {
		needs = []byte("[]")
	}
	now := time.Now().UTC().UnixMilli()
	created := now
	if !sk.CreatedAt.IsZero() {
		created = sk.CreatedAt.UnixMilli()
	}

	// A rewrite keeps the original created_at and clears any prior delete: a
	// skill improves in place, so the second version of a playbook is the same
	// playbook, and re-authoring one the user removed is how it comes back.
	_, err = s.db.SQL().ExecContext(ctx, `
		INSERT INTO skill (name, title, trigger_when, steps, needs, origin, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			title        = excluded.title,
			trigger_when = excluded.trigger_when,
			steps        = excluded.steps,
			needs        = excluded.needs,
			origin       = excluded.origin,
			updated_at   = excluded.updated_at,
			deleted_at   = NULL`,
		sk.Name, sk.Title, sk.When, sk.Steps, string(needs), sk.Origin, created, now)
	return err
}

// Forget marks a skill deleted. It is a soft delete for the reason every other
// soft delete here is: the console shows what was removed and when, and a
// playbook that vanishes without trace is indistinguishable from one that was
// never written.
func (s sqlSkills) Forget(ctx context.Context, name string) error {
	_, err := s.db.SQL().ExecContext(ctx,
		`UPDATE skill SET deleted_at = ? WHERE name = ? AND deleted_at IS NULL`,
		time.Now().UTC().UnixMilli(), name)
	return err
}
