package identity

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sync"
)

// Remover removes, in the transaction tx of a delete, the data its package
// keeps for the user or the org with the id id. An error rolls the whole
// delete back.
type Remover func(ctx context.Context, tx *sql.Tx, id string) error

// removers are the removers registered on a Service, each list in
// registration order. Removers are registered on the Service, not in the
// package, so that two services, such as two hubs in one test binary, do
// not share them (ADR 03).
type removers struct {
	mu         sync.Mutex
	user, orgs []Remover
}

// RegisterUserRemover adds fn to the removers a user delete calls, after
// those registered before it. A package that keeps data for a user
// registers its remover while the hub is composed, before any request.
func (s *Service) RegisterUserRemover(fn Remover) {
	s.removers.mu.Lock()
	defer s.removers.mu.Unlock()
	s.removers.user = append(s.removers.user, fn)
}

// RegisterOrgRemover adds fn to the removers an org delete calls, after
// those registered before it. A package that keeps data for an org
// registers its remover while the hub is composed, before any request.
func (s *Service) RegisterOrgRemover(fn Remover) {
	s.removers.mu.Lock()
	defer s.removers.mu.Unlock()
	s.removers.orgs = append(s.removers.orgs, fn)
}

// removeOrgData calls every org remover, in registration order, with tx
// and the org's id, and stops at the first error.
func (s *Service) removeOrgData(ctx context.Context, tx *sql.Tx, orgID string) error {
	s.removers.mu.Lock()
	fns := slices.Clone(s.removers.orgs)
	s.removers.mu.Unlock()
	return callRemovers(ctx, tx, fns, orgID)
}

// callRemovers calls fns in order with tx and id. A remover's error is
// returned as text only, not wrapped: whatever a remover returns, a
// not-found or an API error included, the delete answers 500 internal.
func callRemovers(ctx context.Context, tx *sql.Tx, fns []Remover, id string) error {
	for _, fn := range fns {
		if err := fn(ctx, tx, id); err != nil {
			return fmt.Errorf("remove the data of %s: %s", id, err)
		}
	}
	return nil
}
