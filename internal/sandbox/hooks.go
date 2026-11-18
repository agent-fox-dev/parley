package sandbox

import (
	"context"
	"database/sql"
	"slices"
	"sync"
)

// Hook is clean-up a sandbox package registers for the records of another:
// it runs in the transaction that marks a sandbox deleted (a delete hook) or
// removes its record (a purge hook), with the sandbox's id. Its error fails
// that transaction.
type Hook func(ctx context.Context, tx *sql.Tx, sandboxID string) error

// hooks is one list of hooks, safe for concurrent registration.
type hooks struct {
	mu   sync.Mutex
	list []Hook
}

func (h *hooks) add(fn Hook) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.list = append(h.list, fn)
}

// run calls the hooks registered so far, in the order they were registered,
// and stops at the first error, which it returns.
func (h *hooks) run(ctx context.Context, tx *sql.Tx, sandboxID string) error {
	h.mu.Lock()
	list := slices.Clone(h.list)
	h.mu.Unlock()
	for _, fn := range list {
		if err := fn(ctx, tx, sandboxID); err != nil {
			return err
		}
	}
	return nil
}

// RegisterDeleteHook adds fn to the delete hooks, which the transaction that
// marks a sandbox deleted runs. A package registers while the hub composes or
// in a start hook.
func (s *Service) RegisterDeleteHook(fn Hook) { s.deleteHooks.add(fn) }

// RegisterPurgeHook adds fn to the purge hooks, which the transaction that
// removes a sandbox's record runs.
func (s *Service) RegisterPurgeHook(fn Hook) { s.purgeHooks.add(fn) }

// RunDeleteHooks runs the delete hooks for the sandbox sandboxID in tx, in
// the order they were registered, and returns the first error, after which
// none runs.
func (s *Service) RunDeleteHooks(ctx context.Context, tx *sql.Tx, sandboxID string) error {
	return s.deleteHooks.run(ctx, tx, sandboxID)
}

// RunPurgeHooks runs the purge hooks for the sandbox sandboxID in tx, in the
// order they were registered, and returns the first error, after which none
// runs.
func (s *Service) RunPurgeHooks(ctx context.Context, tx *sql.Tx, sandboxID string) error {
	return s.purgeHooks.run(ctx, tx, sandboxID)
}
