package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// errExtendDeleted refuses to change a deleted sandbox, which has no
// expiry left to move.
var errExtendDeleted = server.NewError("sandbox_deleted", http.StatusConflict, "the sandbox is deleted and cannot be changed")

// Extension is what a change of a sandbox sets: each member is nil when it
// is left as it is.
type Extension struct {
	// TTL counts the new expires_at from the clock's time.
	TTL     *time.Duration
	Purpose *string
	// Labels replace the sandbox's labels; nil leaves them.
	Labels map[string]string
}

// Extend applies ext to the sandbox rec and returns the sandbox as it is
// stored then. A deleted sandbox is refused with 409 sandbox_deleted, also
// when it was deleted since rec was read.
//
// A TTL sets expires_at to the clock's time plus TTL, cut to the expiry of
// the fork the sandbox is on, and moves the expiry of hub_access and of the
// hub credential's token to the same instant, so that the credential lives
// exactly as long as the sandbox; a purpose and labels replace those the
// sandbox has. Everything is written in one transaction, with updated_at
// and no event, and a new expires_at moves the lifecycle loop's expiry
// timer.
func (s *Service) Extend(ctx context.Context, rec Record, ext Extension) (Record, error) {
	if rec.State == StateDeleted {
		return rec, errExtendDeleted
	}
	var cur Record
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
		if cur, err = Get(ctx, tx, rec.ID); err != nil {
			return err
		}
		if cur.State == StateDeleted {
			return errExtendDeleted
		}
		now := s.deps.Now()
		fields := []Field{SetUpdatedAt(now)}
		if ext.TTL != nil {
			w, found, err := workspace.BySlug(ctx, tx, cur.Workspace)
			switch {
			case err != nil:
				return fmt.Errorf("read workspace %s: %w", cur.Workspace, err)
			case !found:
				return server.NotFound(workspace.ErrNotFound.Error())
			}
			at := ExpiresAt(now, *ext.TTL, w).Truncate(time.Second)
			fields = append(fields, SetExpiresAt(at))
			if cur.HubAccess != nil {
				ha := *cur.HubAccess
				ha.ExpiresAt = at
				fields = append(fields, SetHubAccess(&ha))
			}
			if cur.HubTokenID != "" && s.deps.SetTokenExpiry != nil {
				// A token the identity store no longer holds has no expiry
				// to move.
				if err := s.deps.SetTokenExpiry(ctx, tx, cur.HubTokenID, at); err != nil && !errors.Is(err, identity.ErrNotFound) {
					return err
				}
			}
		}
		if ext.Purpose != nil {
			fields = append(fields, SetPurpose(*ext.Purpose))
		}
		if ext.Labels != nil {
			fields = append(fields, SetLabels(ext.Labels))
		}
		if err := Update(ctx, tx, cur.ID, fields...); err != nil {
			return err
		}
		cur, err = Get(ctx, tx, cur.ID)
		return err
	})
	if err != nil {
		return rec, err
	}
	if ext.TTL != nil {
		// The lifecycle loop moves its expiry timer on every change it is
		// told of; a read waiting for a state reads again and goes on
		// waiting.
		s.changes.notify()
	}
	return cur, nil
}

// readExtension reads the body of a change of the sandbox rec: the members
// ttl, at least MinTTL and at most the ttl of the profile as rec's snapshot
// holds it, purpose and labels, each optional, judged as a create judges
// them. It answers the first member it refuses with 400 invalid_request
// "<member>: <reason>", before anything is changed.
func readExtension(raw []byte, rec Record) (Extension, error) {
	body, err := readBody(raw)
	if err != nil {
		return Extension{}, err
	}
	var ext Extension
	steps := []struct {
		member string
		read   func(raw json.RawMessage) error
	}{
		{"ttl", func(raw json.RawMessage) error {
			if raw == nil {
				return nil
			}
			ttl, err := readTTL(raw, rec.Snapshot.TTL)
			ext.TTL = &ttl
			return err
		}},
		{"purpose", func(raw json.RawMessage) (err error) {
			ext.Purpose, err = readPurpose(raw)
			return err
		}},
		{"labels", func(raw json.RawMessage) (err error) {
			ext.Labels, err = readLabels(raw)
			return err
		}},
	}
	for _, step := range steps {
		if err := step.read(body[step.member]); err != nil {
			if errors.Is(err, errInvalid) {
				return Extension{}, server.InvalidRequest(step.member + ": " + strings.TrimPrefix(err.Error(), errInvalid.Error()+": "))
			}
			return Extension{}, err
		}
	}
	return ext, nil
}

// serveExtend answers PATCH /workspaces/:slug/sandboxes/:id: it applies the
// body to the sandbox and answers with it, or refuses the body, or a
// deleted sandbox, without changing anything.
func (s *Service) serveExtend(w http.ResponseWriter, r *http.Request) error {
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the extend route found no access decision")
	}
	ctx := r.Context()
	rec, err := s.readSandbox(ctx, d.Record.Slug, server.Param(r, "id"))
	if err != nil {
		return err
	}
	if rec.State == StateDeleted {
		return errExtendDeleted
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return server.InvalidRequest("request body could not be read")
	}
	ext, err := readExtension(raw, rec)
	if err != nil {
		return err
	}
	if rec, err = s.Extend(ctx, rec, ext); err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, s.object(rec))
	return nil
}
