package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// SweepBindings revokes, in one store transaction, every live PAT bound to
// a workspace that has no workspace record, and returns how many it
// revoked. It is the maintenance task token_binding_sweep: should a removal
// ever leave a bound token behind, the token stops working within one
// maintenance interval. A token that is unbound, bound to a workspace with a
// record in any status, revoked or expired is left as it is; the tokens it
// revokes get revoked_at as RevokeBoundTo writes it. It logs one info line
// with the count when it revoked any, and emits no event. On a store
// failure the transaction is rolled back and it returns 0 and the error,
// which the maintenance loop logs.
func (s *Service) SweepBindings(ctx context.Context) (int, error) {
	if s.deps.Store == nil {
		return 0, errors.New("sweep the token bindings: the access service has no store")
	}
	// A stop that cancels the context ends the task before its transaction
	// begins, never inside it: database/sql rolls a cancelled transaction
	// back in a goroutine of its own, which can close the store's last
	// connection after the hub has closed the store and returned. The
	// transaction is one read and one update, so the stop waits little.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	ctx = context.WithoutCancel(ctx)
	var ids []string
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		slugs, err := orphanedBindings(ctx, tx)
		if err != nil {
			return err
		}
		ids, err = s.RevokeBoundTo(ctx, tx, slugs)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("sweep the token bindings: %w", err)
	}
	if len(ids) > 0 {
		s.logger().Info("revoked the tokens bound to workspaces that no longer exist", "count", len(ids))
	}
	return len(ids), nil
}

// orphanedBindings returns the slugs, without workspace record, to which a
// PAT that is not revoked is bound. Whether such a token has expired is
// left to RevokeBoundTo, which applies the rule authentication uses.
func orphanedBindings(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT p.workspace FROM pats p
WHERE p.workspace IS NOT NULL AND p.revoked_at IS NULL
AND NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.slug = p.workspace)`)
	if err != nil {
		return nil, fmt.Errorf("read the workspaces of bound tokens: %w", err)
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("read the workspaces of bound tokens: %w", err)
		}
		slugs = append(slugs, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the workspaces of bound tokens: %w", err)
	}
	return slugs, nil
}

// logger is the service's log: Deps.Logger, or one that discards.
func (s *Service) logger() *slog.Logger {
	if s.deps.Logger != nil {
		return s.deps.Logger
	}
	return slog.New(slog.DiscardHandler)
}
