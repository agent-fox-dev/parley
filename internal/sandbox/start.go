package sandbox

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// EventStart is written when an ended sandbox is started again.
const EventStart = "parley.sandbox.start"

// errSandboxDeleted refuses to start a deleted sandbox, which nothing
// brings back.
var errSandboxDeleted = server.NewError("sandbox_deleted", http.StatusConflict, "the sandbox is deleted and cannot be started")

// StartSandbox starts the sandbox rec, stopped, completed or failed, again
// for actor, and returns the sandbox as it is stored then. A sandbox that
// is provisioning or ready is returned as it is, and nothing is written; a
// deleted one is refused with 409 sandbox_deleted.
//
// The start is judged as a create is, from the sandbox's stored snapshot
// and never from the configured profile, which may have changed or gone:
// the workspace's state (workspace.RequireReady and fork.GuardUpdate: 409
// workspace_archived, parent_archived), the credentials and exposed
// secrets the sandbox has, resolved again with each required or optional
// as the profile had it (503 secrets_unavailable, 422 credential_missing),
// then the quotas (429 sandbox_capacity), which count the sandbox only once
// it is provisioning. A refused start writes nothing.
//
// An accepted start, in one transaction, moves the sandbox to provisioning
// with EventStart by actor, clearing state_reason, ended_at and exit_code,
// records the hub's clock as its last activity, drops an optional name
// that no longer resolves from its credentials or exposed secrets, mints a new hub credential with the stored scopes and
// paths, expiring at the sandbox's expires_at, in place of the old one,
// which it revokes, and marks the sandbox for the provisioner, which starts
// it at the runtime (see provisionOne). When another action moved the
// sandbox meanwhile, nothing is written, and the sandbox is returned as
// that action left it.
func (s *Service) StartSandbox(ctx context.Context, rec Record, actor Actor) (Record, error) {
	switch rec.State {
	case StateProvisioning, StateReady:
		return rec, nil
	case StateDeleted:
		return rec, errSandboxDeleted
	}
	var (
		applied bool
		report  func()
		cur     Record
		values  Values
		minted  string
	)
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		res, err := s.judgeStart(ctx, tx, rec)
		if err != nil {
			return err
		}
		values = res.Values()
		applied, report, err = s.TransitionTx(ctx, tx, rec.ID, rec.State, StateProvisioning, "", &event.Input{
			Type: EventStart, ActorType: actor.Type, ActorID: actor.ID,
		})
		if err != nil {
			return err
		}
		if !applied {
			cur, err = Get(ctx, tx, rec.ID)
			return err
		}
		// A start is an activity: without it an idle stop's old
		// last_activity_at would have the next idle pass stop the sandbox
		// again as soon as it is ready.
		fields := []Field{SetExitCode(nil), SetMarker(MarkerStart), SetLastActivityAt(s.deps.Now())}
		if names := res.CredentialNames(); !slices.Equal(names, rec.Credentials) {
			fields = append(fields, SetCredentials(names))
		}
		if names := res.SecretNames(); !slices.Equal(names, rec.ExposedSecrets) {
			fields = append(fields, SetExposedSecrets(names))
		}
		if rec.HubAccess != nil {
			// The token the record holds now: rec may be older than an
			// action that replaced it.
			if cur, err = Get(ctx, tx, rec.ID); err != nil {
				return err
			}
			if err := s.RevokeHubCredential(ctx, tx, cur); err != nil {
				return err
			}
			pat, value, err := s.MintHubCredential(ctx, tx, cur, cur.ExpiresAt)
			if err != nil {
				return err
			}
			minted = value
			fields = append(fields, SetHubTokenID(pat.TokenID))
			// Kept before the commit, as a create keeps its value: the
			// provisioner may take the sandbox up as soon as it has
			// committed, and would replace a credential it does not find.
			s.SetHubCredential(rec.ID, value)
		}
		if err := Update(ctx, tx, rec.ID, fields...); err != nil {
			return err
		}
		cur, err = Get(ctx, tx, rec.ID)
		return err
	})
	values.HubCredential = minted
	if err != nil {
		if minted != "" {
			s.ForgetValues(rec.ID)
		}
		return rec, scrubError(values, err)
	}
	if !applied {
		if cur.State == StateDeleted {
			return cur, errSandboxDeleted
		}
		return cur, nil
	}
	report()
	s.SetValues(rec.ID, values)
	s.WakeProvisioner()
	return cur, nil
}

// judgeStart checks in tx whether the sandbox rec may start, in the order
// of a create's refusals, and returns its credentials and exposed secrets
// resolved again.
func (s *Service) judgeStart(ctx context.Context, tx *sql.Tx, rec Record) (Resolution, error) {
	w, found, err := workspace.BySlug(ctx, tx, rec.Workspace)
	switch {
	case err != nil:
		return Resolution{}, fmt.Errorf("read workspace %s: %w", rec.Workspace, err)
	case !found:
		return Resolution{}, server.NotFound(workspace.ErrNotFound.Error())
	}
	if err := workspace.RequireReady(w); err != nil {
		return Resolution{}, err
	}
	if err := fork.GuardUpdate(ctx, tx, w); err != nil {
		return Resolution{}, err
	}
	res, err := s.resolveIn(ctx, tx, w.Slug, attachedSnapshot(rec))
	if err != nil {
		return Resolution{}, err
	}
	if err := res.Missing(); err != nil {
		return Resolution{}, err
	}
	if err := s.CheckQuota(ctx, tx, w, rec.UserID); err != nil {
		return Resolution{}, err
	}
	return res, nil
}

// serveStart answers POST /workspaces/:slug/sandboxes/:id/start: it starts
// the sandbox again for the caller and answers with it as it is then, or
// with the refusal.
func (s *Service) serveStart(w http.ResponseWriter, r *http.Request) error {
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the start route found no access decision")
	}
	ctx := r.Context()
	rec, err := s.readSandbox(ctx, d.Record.Slug, server.Param(r, "id"))
	if err != nil {
		return err
	}
	caller, _ := identity.PrincipalOf(ctx)
	if rec, err = s.StartSandbox(ctx, rec, actorFor(caller)); err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, s.object(rec))
	return nil
}
