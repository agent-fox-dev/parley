package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// updateRoute is PATCH /workspaces/:slug. The scope check and then the
// access check run before the handler, so that a PAT without
// workspaces:write is refused before any lookup, a member of the
// workspace's org who does not own it is refused, and a caller who cannot
// read the workspace gets the same 404 as for a slug no workspace has.
func (s *Service) updateRoute() server.Route {
	return server.Route{Method: http.MethodPatch, Pattern: "/workspaces/:slug", Scope: scopeWrite,
		Marks: map[string]string{access.MarkNeed: string(access.NeedWrite)}, Handler: s.update}
}

// updateField is one mutable field of an update body. apply judges the
// field's value raw, which may be null but is never absent, and applies it
// to next, the workspace as the update leaves it. Its error is the reason
// the value is refused, without the field's name, or a *server.Error that
// is the answer itself.
type updateField struct {
	name  string
	apply func(s *Service, next *Workspace, raw json.RawMessage) error
}

// updateFields are the mutable fields of an update body in the order their
// values are judged: the first one refused answers the request, so this
// order decides which field a refusal names. Members not listed are
// ignored.
var updateFields = []updateField{
	{"settings", func(s *Service, next *Workspace, raw json.RawMessage) error {
		settings, err := s.settings.ApplySettings(next.Settings, raw)
		if err != nil {
			return err
		}
		next.Settings = settings
		return nil
	}},
}

// update serves PATCH /workspaces/:slug. In one store transaction it reads
// the workspace, applies the fields the body carries to it and writes the
// result with updated_at, so that the update is judged against the record
// it changes and a refusal writes nothing, updated_at included. It answers
// 200 with the workspace read back after the commit, the answer a GET
// would give.
func (s *Service) update(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the workspace update found no access decision")
	}
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	slug := server.Param(r, "slug")
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		cur, found, err := BySlug(ctx, tx, slug)
		switch {
		case err != nil:
			return err
		case !found:
			return server.NotFound(ErrNotFound.Error())
		}
		next, err := s.applyUpdate(cur, body)
		if err != nil {
			return err
		}
		next.UpdatedAt = s.deps.Now().UTC().Truncate(time.Second)
		return writeUpdate(ctx, tx, next)
	})
	if err != nil {
		return err
	}
	rec, found, err := BySlug(ctx, s.deps.Store.Main(), slug)
	switch {
	case err != nil:
		return err
	case !found:
		// Deleted since the commit: the answer a GET would give now.
		return server.NotFound(ErrNotFound.Error())
	}
	obj, err := s.RenderWorkspace(ctx, rec, d.Level)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, obj)
	return nil
}

// applyUpdate is cur with the fields body carries applied, by
// updateFields. A body that carries none of them is refused, and so is the
// first field whose value is refused, with 400 invalid_request
// "<field>: <reason>" or the answer the field gave.
func (s *Service) applyUpdate(cur Workspace, body map[string]json.RawMessage) (Workspace, error) {
	if !slices.ContainsFunc(updateFields, func(f updateField) bool { _, ok := body[f.name]; return ok }) {
		return Workspace{}, server.InvalidRequest("the body carries no field to change")
	}
	next := cur
	for _, f := range updateFields {
		raw, ok := body[f.name]
		if !ok {
			continue
		}
		if err := f.apply(s, &next, raw); err != nil {
			if answer, ok := errors.AsType[*server.Error](err); ok {
				return Workspace{}, answer
			}
			return Workspace{}, server.InvalidRequest(f.name + ": " + err.Error())
		}
	}
	return next, nil
}

// writeUpdate writes the fields an update changes of w, and its updated_at,
// through tx.
func writeUpdate(ctx context.Context, tx *sql.Tx, w Workspace) error {
	settings, err := encodeSettings(w.Settings)
	if err != nil {
		return fmt.Errorf("update workspace %q: %w", w.Slug, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET settings = ?, updated_at = ? WHERE slug = ?`,
		settings, w.UpdatedAt.UnixNano(), w.Slug); err != nil {
		return fmt.Errorf("update workspace %q: %w", w.Slug, err)
	}
	return nil
}
