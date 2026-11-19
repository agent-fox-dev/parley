package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// EventStop is written when a sandbox is stopped, with the reason of the
// stop as its metadata.
const EventStop = "parley.sandbox.stop"

// The reasons of a stop, its state_reason and its event's reason.
const (
	// StopAPI is a stop a caller asked for.
	StopAPI = "api"
	// StopIdle is the stop of a sandbox idle for longer than its idle
	// timeout.
	StopIdle = "idle"
	// StopArchived is the stop of a sandbox whose workspace was archived.
	StopArchived = "archived"
)

// Stop stops the sandbox rec for reason, one of the Stop reasons, by actor,
// and returns the sandbox as it is stored then. Only a ready sandbox has
// compute to release: one in another state is returned as it is, and
// nothing is called or written.
//
// A ready sandbox gets a checkpoint first when its profile asked for one;
// a failed checkpoint does not keep it from stopping, since Checkpoint has
// recorded the failure. The runtime is then asked to release the
// sandbox's compute and keep its working directory. When the runtime cannot
// be reached or refuses, unsupported included, nothing changes: the
// sandbox stays ready with its hub credential, and the runtime's error is
// returned with its text scrubbed of the sandbox's values. Otherwise the
// sandbox moves from ready to stopped with reason as its state_reason, its
// hub credential is revoked and EventStop is written, in one transaction.
// When another action moved the sandbox meanwhile, nothing is written, and
// the sandbox is returned as that action left it. When ctx ends before the
// runtime is asked to stop the sandbox, as when the hub stops, nothing is
// called or written and the context's error is returned. A stop waits for the
// stop or deletion of the same sandbox in progress, and then works from
// the stored record.
func (s *Service) Stop(ctx context.Context, rec Record, reason string, actor Actor) (Record, error) {
	if rec.State != StateReady {
		return rec, nil
	}
	defer s.lifecycle.lock(rec.ID)()
	// The sandbox as the action before this one left it.
	switch cur, err := Get(ctx, s.deps.Store.Main(), rec.ID); {
	case errors.Is(err, ErrNotFound):
		return rec, nil
	case err != nil:
		return rec, err
	case cur.State != StateReady:
		return cur, nil
	default:
		rec = cur
	}
	if rec.Snapshot.Checkpoint {
		_, _ = s.Checkpoint(ctx, rec, actor)
	}
	if err := ctx.Err(); err != nil {
		// The hub stops, or the caller left, before the runtime was asked:
		// the sandbox stays ready, and the next idle pass tries again.
		return rec, err
	}
	if err := s.client.StopSandbox(ctx, rec.ID); err != nil {
		return rec, &scrubbedError{text: s.ScrubSandbox(context.WithoutCancel(ctx), rec.ID, err.Error()), err: err}
	}
	var (
		applied bool
		report  func()
		cur     Record
	)
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		applied, report, err = s.TransitionTx(ctx, tx, rec.ID, StateReady, StateStopped, reason, &event.Input{
			Type: EventStop, ActorType: actor.Type, ActorID: actor.ID, Metadata: map[string]any{"reason": reason},
		})
		if err != nil {
			return err
		}
		if cur, err = Get(ctx, tx, rec.ID); err != nil || !applied {
			return err
		}
		// The token the record holds now: rec may be older than the
		// last start, which minted a new one.
		return s.RevokeHubCredential(ctx, tx, cur)
	})
	if err != nil {
		return rec, err
	}
	report()
	if applied {
		s.ForgetValues(rec.ID)
	}
	return cur, nil
}

// serveStop answers POST /workspaces/:slug/sandboxes/:id/stop: it stops
// the sandbox for the caller and answers with it as it is then, in every
// state, or with the runtime error when the runtime failed the stop.
func (s *Service) serveStop(w http.ResponseWriter, r *http.Request) error {
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the stop route found no access decision")
	}
	ctx := r.Context()
	rec, err := s.readSandbox(ctx, d.Record.Slug, server.Param(r, "id"))
	if err != nil {
		return err
	}
	caller, _ := identity.PrincipalOf(ctx)
	if rec, err = s.Stop(ctx, rec, StopAPI, actorFor(caller)); err != nil {
		return s.answerUse(ctx, rec.ID, err)
	}
	server.WriteJSON(w, http.StatusOK, s.object(rec))
	return nil
}
