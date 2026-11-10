package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Target is the store a schema step applies to.
type Target int

// The two stores.
const (
	// Main is the transactional store.
	Main Target = iota
	// Analytical is the analytical store.
	Analytical
)

// Step is a schema step: a unique name, the store it targets and the
// function that changes the schema in one transaction. Steps are applied in
// registration order and each store records the names it has applied.
type Step struct {
	Name   string
	Target Target
	Apply  func(ctx context.Context, tx *sql.Tx) error
}

// checkSteps refuses a list with an empty or duplicated name before any step
// is applied.
func checkSteps(steps []Step) error {
	seen := map[string]bool{}
	for _, st := range steps {
		if st.Name == "" {
			return fmt.Errorf("a schema step has no name")
		}
		if seen[st.Name] {
			return fmt.Errorf("schema step %s is registered twice", st.Name)
		}
		seen[st.Name] = true
	}
	return nil
}

const stepsTable = `CREATE TABLE IF NOT EXISTS schema_steps (
	name       TEXT PRIMARY KEY,
	applied_at TEXT NOT NULL
)`

// migrate applies, in order, every step its store has not recorded, each in
// one transaction together with its record. A failing step is rolled back,
// no later step is applied, and the error names the step.
func (s *Store) migrate(ctx context.Context, steps []Step) error {
	applied := map[Target]map[string]bool{}
	for target, db := range map[Target]*sql.DB{Main: s.main, Analytical: s.analytics} {
		if _, err := db.ExecContext(ctx, stepsTable); err != nil {
			return fmt.Errorf("cannot prepare schema steps in %s: %w", s.pathOf(target), err)
		}
		names, err := appliedSteps(ctx, db)
		if err != nil {
			return fmt.Errorf("cannot read schema steps in %s: %w", s.pathOf(target), err)
		}
		applied[target] = names
	}
	for _, st := range steps {
		if applied[st.Target][st.Name] {
			continue
		}
		db := s.main
		if st.Target == Analytical {
			db = s.analytics
		}
		err := s.tx(ctx, db, func(tx *sql.Tx) error {
			if err := st.Apply(ctx, tx); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_steps (name, applied_at) VALUES (?, ?)`,
				st.Name, time.Now().UTC().Format(time.RFC3339))
			return err
		})
		if err != nil {
			return fmt.Errorf("schema step %s failed: %w", st.Name, err)
		}
	}
	return nil
}

func appliedSteps(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_steps`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names[n] = true
	}
	return names, rows.Err()
}

func (s *Store) pathOf(t Target) string {
	if t == Analytical {
		return s.AnalyticsPath()
	}
	return s.opts.DatabasePath
}
