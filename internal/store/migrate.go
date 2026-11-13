package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Target names the store a schema step applies to.
type Target int

const (
	// Main is the transactional store.
	Main Target = iota
	// Analytics is the analytical store.
	Analytics
)

// Step is one schema step a package registers. Open applies a step once per
// store: in the transaction it hands Apply, together with the record of the
// step's Name in the target store, so a step that fails leaves neither its
// changes nor its record.
type Step struct {
	// Name identifies the step for good: it is what the store records, so a
	// step is never renamed.
	Name string
	// Target is the store the step applies to.
	Target Target
	// Apply makes the step's changes in tx.
	Apply func(ctx context.Context, tx *sql.Tx) error
}

// stepsTable records, in each store, the names of the steps applied to it.
const stepsTable = `CREATE TABLE IF NOT EXISTS schema_steps (
	name       TEXT PRIMARY KEY,
	applied_at TEXT NOT NULL
)`

// validate refuses a step list Open cannot apply as a whole, before anything
// is created or applied.
func validate(steps []Step) error {
	seen := make(map[string]bool, len(steps))
	for i, st := range steps {
		switch {
		case st.Name == "":
			return fmt.Errorf("schema step %d has no name", i+1)
		case seen[st.Name]:
			return fmt.Errorf("schema step %q is registered twice", st.Name)
		case st.Apply == nil:
			return fmt.Errorf("schema step %q has no function", st.Name)
		case st.Target != Main && st.Target != Analytics:
			return fmt.Errorf("schema step %q has an unknown target store %d", st.Name, st.Target)
		}
		seen[st.Name] = true
	}
	return nil
}

// migrate applies, in order, every step its target store has not recorded.
// It stops at the first step that fails; the steps before it stay applied.
func (s *Store) migrate(ctx context.Context, steps []Step) error {
	for _, db := range []*sql.DB{s.main, s.analytics} {
		if _, err := db.ExecContext(ctx, stepsTable); err != nil {
			return fmt.Errorf("create schema step record: %w", err)
		}
	}
	for _, step := range steps {
		db := s.main
		if step.Target == Analytics {
			db = s.analytics
		}
		err := inTx(ctx, db, func(tx *sql.Tx) error {
			var applied int
			err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM schema_steps WHERE name = ?`, step.Name).Scan(&applied)
			if err != nil || applied > 0 {
				return err
			}
			if err := step.Apply(ctx, tx); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx,
				`INSERT INTO schema_steps (name, applied_at) VALUES (?, ?)`,
				step.Name, time.Now().UTC().Format(time.RFC3339Nano))
			return err
		})
		if err != nil {
			return fmt.Errorf("schema step %q: %w", step.Name, err)
		}
	}
	return nil
}
