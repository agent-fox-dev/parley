package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// EventDelete is written when a sandbox is deleted, with the reason of the
// deletion as its metadata.
const EventDelete = "parley.sandbox.delete"

// The reasons of a deletion, its state_reason and its event's reason.
const (
	// DeleteAPI is a deletion a caller asked for.
	DeleteAPI = "api"
	// DeleteExpired is the deletion of a sandbox at its expires_at.
	DeleteExpired = "expired"
	// DeleteWorkspaceDeleted is the deletion of a sandbox whose workspace
	// goes: nothing is left to keep a checkpoint in.
	DeleteWorkspaceDeleted = "workspace_deleted"
)

// Delete deletes the sandbox rec for reason, one of the Delete reasons, by
// actor. A deleted sandbox is left as it is: nothing is called or written.
//
// A ready sandbox gets a checkpoint first when checkpoint is set and reason
// is not DeleteWorkspaceDeleted; a failed checkpoint does not keep it from
// being deleted, since Checkpoint has recorded the failure. The runtime is
// then asked to delete the sandbox and every provider of it; what it cannot
// delete, because it cannot be reached or fails, is logged and left to the
// reconciliation, which deletes what the runtime holds of a deleted
// sandbox. Then, in one transaction, the sandbox moves from the state it
// is in to deleted with reason as its state_reason, its hub credential is
// revoked, the delete hooks run and EventDelete is written. When another
// action deleted the sandbox meanwhile, nothing is written. The error is
// that of the transaction alone.
func (s *Service) Delete(ctx context.Context, rec Record, reason string, actor Actor, checkpoint bool) error {
	if rec.State == StateDeleted {
		return nil
	}
	defer s.lifecycle.lock(rec.ID)()
	cur, err := Get(ctx, s.deps.Store.Main(), rec.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case cur.State == StateDeleted:
		return nil
	}
	if s.client != nil {
		if checkpoint && reason != DeleteWorkspaceDeleted && cur.State == StateReady {
			_, _ = s.Checkpoint(ctx, cur, actor)
		}
		if err := s.clearRuntime(ctx, cur); err != nil {
			s.logger().Warn("the runtime still holds part of a deleted sandbox; the reconciliation deletes it", "sandbox", cur.ID,
				"error", s.ScrubSandbox(context.WithoutCancel(ctx), cur.ID, err.Error()))
		}
	}
	var (
		applied bool
		report  func()
	)
	// The runtime has let the sandbox go by now, so the deletion is
	// recorded even when the caller has left.
	ctx = context.WithoutCancel(ctx)
	err = s.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		// The state the record is in now: the provisioning may have moved
		// it since it was read.
		if cur, err = Get(ctx, tx, rec.ID); err != nil || cur.State == StateDeleted {
			return err
		}
		applied, report, err = s.TransitionTx(ctx, tx, cur.ID, cur.State, StateDeleted, reason, &event.Input{
			Type: EventDelete, ActorType: actor.Type, ActorID: actor.ID, Metadata: map[string]any{"reason": reason},
		})
		if err != nil || !applied {
			return err
		}
		if err := s.RevokeHubCredential(ctx, tx, cur); err != nil {
			return err
		}
		return s.RunDeleteHooks(ctx, tx, cur.ID)
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	if applied {
		report()
		s.ForgetValues(rec.ID)
	}
	return nil
}

// serveDelete answers DELETE /workspaces/:slug/sandboxes/:id: it deletes
// the sandbox for the caller and answers 204 with no body, also for a
// sandbox already deleted. The query checkpoint, true or false, says
// whether a ready sandbox gets a checkpoint first; without it the profile's
// checkpoint flag, as the sandbox's snapshot holds it, says so.
func (s *Service) serveDelete(w http.ResponseWriter, r *http.Request) error {
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the delete route found no access decision")
	}
	query := r.URL.Query()
	var asked *bool
	if query.Has("checkpoint") {
		switch query.Get("checkpoint") {
		case "true":
			asked = new(true)
		case "false":
			asked = new(false)
		default:
			return server.InvalidRequest("checkpoint must be true or false")
		}
	}
	ctx := r.Context()
	rec, err := s.readSandbox(ctx, d.Record.Slug, server.Param(r, "id"))
	if err != nil {
		return err
	}
	checkpoint := rec.Snapshot.Checkpoint
	if asked != nil {
		checkpoint = *asked
	}
	caller, _ := identity.PrincipalOf(ctx)
	if err := s.Delete(ctx, rec, DeleteAPI, actorFor(caller), checkpoint); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// sandboxLocks has the actions that call the runtime about one sandbox,
// stop and delete, take their turns: an action that waited finds the
// sandbox as the one before it left it, so that racing requests make one
// runtime call where one is due. The conditional transition still decides
// what is recorded, for whatever does not take a turn here.
type sandboxLocks struct {
	mu   sync.Mutex
	held map[string]*sandboxLock
}

// sandboxLock is the turn of one sandbox, with the number of actions that
// hold it or wait for it.
type sandboxLock struct {
	mu    sync.Mutex
	users int
}

// lock waits for the turn of the sandbox id and returns what ends it.
func (l *sandboxLocks) lock(id string) (unlock func()) {
	l.mu.Lock()
	if l.held == nil {
		l.held = map[string]*sandboxLock{}
	}
	e := l.held[id]
	if e == nil {
		e = &sandboxLock{}
		l.held[id] = e
	}
	e.users++
	l.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		defer l.mu.Unlock()
		if e.users--; e.users == 0 {
			delete(l.held, id)
		}
	}
}
