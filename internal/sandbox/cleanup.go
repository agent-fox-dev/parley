package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DeletedRetention is how long the record of a deleted sandbox is kept after
// its deletion, so that a read still finds it deleted for a while.
const DeletedRetention = 24 * time.Hour

// CleanupRecords removes the record of every deleted sandbox whose ended_at
// is more than DeletedRetention before the hub's clock, counted in whole
// seconds: one deleted exactly that long ago stays until the next pass.
// Each record goes in a transaction of its own, which runs the purge hooks
// for it first and reports the states after; one that fails is left for
// the next pass, and the others still go. It is the hourly maintenance
// task sandbox_record_cleanup, and acts on the stored records whether or
// not sandboxes are on.
func (s *Service) CleanupRecords(ctx context.Context) error {
	if s.deps.Store == nil {
		return nil
	}
	cutoff := s.deps.Now().Truncate(time.Second).Add(-DeletedRetention).Unix()
	ids, err := deletedBefore(ctx, s.deps.Store.Main(), cutoff)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
			if err := s.removeRecord(ctx, tx, id); err != nil {
				return err
			}
			return s.ReportStates(ctx, tx)
		})
		if err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// deletedBefore reads through db the ids of the deleted sandboxes whose
// ended_at, in Unix seconds, is before cutoff.
func deletedBefore(ctx context.Context, db *sql.DB, cutoff int64) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM sandboxes WHERE state = ? AND ended_at < ? ORDER BY ended_at, id`,
		StateDeleted, cutoff)
	if err != nil {
		return nil, fmt.Errorf("list the deleted sandboxes to remove: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list the deleted sandboxes to remove: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the deleted sandboxes to remove: %w", err)
	}
	return ids, nil
}
