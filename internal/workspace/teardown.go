package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sync"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// TokenRevoker revokes, in the transaction tx, every live token bound to one
// of slugs and returns the ids of the tokens it revoked. Tokens have no
// remover of their own: a revoked token stays as a row, so that its id still
// names it. *access.Service satisfies it.
type TokenRevoker interface {
	RevokeBoundTo(ctx context.Context, tx *sql.Tx, slugs []string) ([]string, error)
}

// Remover removes, in the transaction tx that removes the workspace slug,
// the data its package keeps for that workspace. An error rolls the whole
// removal back. secret.RemoveWorkspace is one.
type Remover func(ctx context.Context, tx *sql.Tx, slug string) error

// SessionCloser ends, in the transaction tx, every active agent session of
// the workspaces slugs, with reason as the reason it gives.
type SessionCloser interface {
	CloseSessions(ctx context.Context, tx *sql.Tx, slugs []string, reason string) error
}

// SandboxRemover deletes the sandboxes of the workspaces slugs. A delete
// calls it before it changes anything in the store, since sandboxes live
// outside the store's transaction; its error stops the delete.
type SandboxRemover interface {
	RemoveSandboxes(ctx context.Context, slugs []string) error
}

// ArchiveObserver is told, after the archive of the workspace slug has
// committed, of the archive and of the slugs of the workspace's forks, such
// as to stop their sandboxes. An error is logged and changes nothing: the
// workspace is archived already.
type ArchiveObserver interface {
	WorkspaceArchived(ctx context.Context, slug string, forks []string) error
}

// ForkRemover is the task-fork feature as this package uses it, the one
// place forks are removed. DeleteFork serves DELETE /workspaces/:slug for
// the fork slug, asked by caller, and returns nil or the API error to
// answer. RemoveForksOf removes every fork of the workspace parent in the
// transaction tx of the parent's delete, by actor, writes each fork's
// delete event, and returns the removed slugs and a function that discards
// the forks' repository data once tx has committed.
type ForkRemover interface {
	DeleteFork(ctx context.Context, slug string, caller identity.Principal) error
	RemoveForksOf(ctx context.Context, tx *sql.Tx, parent string, actor identity.Principal) (slugs []string, discard func(), err error)
}

// Teardown is the set of collaborators through which an archive and a
// delete reach the data other packages keep for a workspace (ADR 07). The
// hub's composition fills it while the hub is composed, before any request:
// each package that keeps such data registers its own collaborator from its
// part in internal/hub. Its zero value, with nothing registered, is valid:
// an archive and a delete then touch the workspace records alone.
// Registration is safe from several goroutines.
type Teardown struct {
	mu        sync.RWMutex
	revoker   TokenRevoker
	removers  []Remover
	closer    SessionCloser
	sandboxes SandboxRemover
	observers []ArchiveObserver
	forks     ForkRemover
}

// RegisterTokenRevoker makes r the token revoker, replacing the one
// registered before; nil removes it.
func (t *Teardown) RegisterTokenRevoker(r TokenRevoker) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.revoker = r
}

// RegisterRemover adds fn to the removers of owned data, after those
// registered before it; nil adds none.
func (t *Teardown) RegisterRemover(fn Remover) {
	if fn == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removers = append(t.removers, fn)
}

// RegisterSessionCloser makes c the session closer, replacing the one
// registered before; nil removes it.
func (t *Teardown) RegisterSessionCloser(c SessionCloser) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closer = c
}

// RegisterSandboxRemover makes r the sandbox remover, replacing the one
// registered before; nil removes it.
func (t *Teardown) RegisterSandboxRemover(r SandboxRemover) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sandboxes = r
}

// RegisterArchiveObserver adds o to the observers an archive tells, after
// those registered before it; nil adds none.
func (t *Teardown) RegisterArchiveObserver(o ArchiveObserver) {
	if o == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.observers = append(t.observers, o)
}

// RegisterForkRemover makes f the fork remover, replacing the one
// registered before; nil removes it.
func (t *Teardown) RegisterForkRemover(f ForkRemover) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.forks = f
}

// teardownSet is what a Teardown holds at one moment, for one operation.
type teardownSet struct {
	revoker   TokenRevoker
	removers  []Remover
	closer    SessionCloser
	sandboxes SandboxRemover
	observers []ArchiveObserver
	forks     ForkRemover
}

// snapshot returns what t holds now, so that an operation calls the same
// collaborators from its start to its end.
func (t *Teardown) snapshot() teardownSet {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return teardownSet{revoker: t.revoker, removers: slices.Clone(t.removers), closer: t.closer,
		sandboxes: t.sandboxes, observers: slices.Clone(t.observers), forks: t.forks}
}

// Teardown is the service's collaborator set, which the hub's composition
// fills.
func (s *Service) Teardown() *Teardown { return &s.teardown }

// RemoveOwned removes, in the transaction tx, what the workspace slug owns
// outside its record: it revokes the tokens bound to it with the token
// revoker, calls every remover of owned data in registration order and ends
// its sessions with the session closer, giving reason. It returns the ids of
// the revoked tokens in ascending order, empty and not nil when there are
// none. The first error stops it and is returned; the caller rolls tx back.
// It is the one way a workspace's or a fork's owned data is removed.
func (s *Service) RemoveOwned(ctx context.Context, tx *sql.Tx, slug, reason string) ([]string, error) {
	set := s.teardown.snapshot()
	ids := []string{}
	if set.revoker != nil {
		revoked, err := set.revoker.RevokeBoundTo(ctx, tx, []string{slug})
		if err != nil {
			return nil, fmt.Errorf("revoke the tokens bound to workspace %q: %w", slug, err)
		}
		ids = append(ids, revoked...)
		slices.Sort(ids)
	}
	for _, fn := range set.removers {
		if err := fn(ctx, tx, slug); err != nil {
			return nil, fmt.Errorf("remove the data of workspace %q: %w", slug, err)
		}
	}
	if set.closer != nil {
		if err := set.closer.CloseSessions(ctx, tx, []string{slug}, reason); err != nil {
			return nil, fmt.Errorf("close the sessions of workspace %q: %w", slug, err)
		}
	}
	return ids, nil
}
