package workspace

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// eventUpdate is the event an update writes.
const eventUpdate = "parley.workspace.update"

// updateRoute is PATCH /workspaces/:slug. The scope check and then the
// access check run before the handler, so that a PAT without
// workspaces:write is refused before any lookup, a member of the
// workspace's org who does not own it is refused, and a caller who cannot
// read the workspace gets the same 404 as for a slug no workspace has.
func (s *Service) updateRoute() server.Route {
	return server.Route{Method: http.MethodPatch, Pattern: "/workspaces/:slug", Scope: scopeWrite,
		Marks: map[string]string{access.MarkNeed: string(access.NeedWrite)}, Handler: s.update}
}

// fieldClass is what an update may do with a field, by the workspace's
// kind.
type fieldClass int

const (
	// fieldImmutable defines the workspace: it is refused on every kind,
	// whatever its value, a value equal to the current one included.
	fieldImmutable fieldClass = iota
	// fieldShared is mutable on both kinds.
	fieldShared
	// fieldRepository is mutable on a repository workspace; a fork has no
	// org or settings of its own, so it is refused there with
	// not_supported_on_fork.
	fieldRepository
	// fieldFork is mutable on a fork through the handler registered for it
	// (RegisterForkField), and refused on a repository workspace and on a
	// fork while no handler is registered.
	fieldFork
)

// updateField is one field an update body may name. apply, nil for a field
// that is not judged here, judges the field's value raw, which may be null
// but is never absent, and applies it to next, the workspace as the update
// leaves it, reading what it needs through tx, the update's transaction.
// Its error is the reason the value is refused, without the field's name, a
// *server.Error that is the answer itself, or a failure of the hub.
type updateField struct {
	name  string
	class fieldClass
	apply func(ctx context.Context, tx *sql.Tx, s *Service, next *Workspace, raw json.RawMessage) error
}

// failure is an error of the hub met while a value is judged, answered as
// a 500 and never as a refusal of the value.
type failure struct{ error }

// updateFields are the fields of an update body, the one place that says
// which fields an update changes on which kind. The mutable ones come in
// the order their values are judged: the first one refused answers the
// request, so this order decides which field a refusal names. Members not
// listed are ignored.
var updateFields = []updateField{
	{"display_name", fieldShared, func(_ context.Context, _ *sql.Tx, _ *Service, next *Workspace, raw json.RawMessage) error {
		name, err := nullableText(raw, 128)
		if name == "" {
			// A workspace always has a name to show: its slug unless it
			// was given one.
			name = next.Slug
		}
		next.DisplayName = name
		return err
	}},
	{"description", fieldShared, func(_ context.Context, _ *sql.Tx, _ *Service, next *Workspace, raw json.RawMessage) error {
		text, err := nullableText(raw, 1024)
		next.Description = text
		return err
	}},
	{"org_id", fieldRepository, func(ctx context.Context, tx *sql.Tx, _ *Service, next *Workspace, raw json.RawMessage) error {
		org, err := moveTarget(ctx, tx, next.OwnerID, raw)
		next.OrgID = org.ID
		return err
	}},
	{"settings", fieldRepository, func(_ context.Context, _ *sql.Tx, s *Service, next *Workspace, raw json.RawMessage) error {
		settings, err := s.settings.ApplySettings(next.Settings, raw)
		if err != nil {
			return err
		}
		next.Settings = settings
		return nil
	}},
	{"ttl", fieldFork, nil},
	{"purpose", fieldFork, nil},
	{"auto_change", fieldFork, nil},
	{"slug", fieldImmutable, nil},
	{"kind", fieldImmutable, nil},
	{"git_url", fieldImmutable, nil},
	{"default_branch", fieldImmutable, nil},
	{"owner_id", fieldImmutable, nil},
	{"workspace_mode", fieldImmutable, nil},
	{"upstream_url", fieldImmutable, nil},
	{"integration_branch", fieldImmutable, nil},
	{"parent_slug", fieldImmutable, nil},
	{"fork_point", fieldImmutable, nil},
}

// nullableText reads raw, null or a string of at most n characters, as a
// string, null being the empty one.
func nullableText(raw json.RawMessage, n int) (string, error) {
	var text *string
	if err := readOptional(raw, &text, atMost(n)); err != nil {
		return "", err
	}
	return deref(text), nil
}

// moveTarget is the org raw, the org_id of an update of a workspace of the
// user owner, names: null is the owner's personal org, a string an org's id
// or slug, as in a create. Whether the owner may have the workspace there
// is not judged here.
func moveTarget(ctx context.Context, tx *sql.Tx, owner string, raw json.RawMessage) (identity.Org, error) {
	var selector *string
	if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := readOptional(raw, &selector, nil); err != nil {
			return identity.Org{}, err
		}
	}
	var org identity.Org
	var err error
	if selector == nil {
		org, err = identity.PersonalOrg(ctx, tx, owner)
	} else {
		org, err = identity.OrgBySelector(ctx, tx, *selector)
	}
	switch {
	case errors.Is(err, identity.ErrNotFound) && selector == nil:
		return identity.Org{}, errors.New("the workspace's owner has no personal organization")
	case errors.Is(err, identity.ErrNotFound):
		return identity.Org{}, fmt.Errorf("no organization has the id or slug %q", *selector)
	case err != nil:
		return identity.Org{}, failure{err}
	}
	return org, nil
}

// ForkFieldWrite stores the value of a fork-only field in fork, the record
// as the update leaves it, through tx, the update's transaction.
type ForkFieldWrite func(ctx context.Context, tx *sql.Tx, fork Workspace) error

// ForkFieldHandler judges raw, the JSON value of one fork-only field in an
// update of fork, which may be null but is never absent. It returns the
// write that stores the value, run after every check of the update has
// passed, or the reason the value is refused, without the field's name, or
// a *server.Error that is the answer itself.
type ForkFieldHandler func(fork Workspace, raw json.RawMessage) (ForkFieldWrite, error)

// ForkUpdateGuard is asked about every update of fork once the body is
// valid and the workspace not archived, before anything is written, through
// tx, the update's transaction. An error refuses the update: a
// *server.Error is the answer unchanged, anything else is a 500.
type ForkUpdateGuard func(ctx context.Context, tx *sql.Tx, fork Workspace) error

// RegisterForkField makes h the handler of the fork-only field name (ttl,
// purpose or auto_change) in an update of a fork; nil removes it. The fork
// features register theirs, so that they add a field without changing the
// rules of the update. Any other name is a programmer error and panics.
func (s *Service) RegisterForkField(name string, h ForkFieldHandler) {
	if !slices.ContainsFunc(updateFields, func(f updateField) bool { return f.name == name && f.class == fieldFork }) {
		panic(fmt.Sprintf("workspace: %q is not a fork-only field of an update", name))
	}
	s.forkMu.Lock()
	defer s.forkMu.Unlock()
	if h == nil {
		delete(s.forkFields, name)
		return
	}
	if s.forkFields == nil {
		s.forkFields = map[string]ForkFieldHandler{}
	}
	s.forkFields[name] = h
}

// RegisterForkUpdateGuard makes g the guard asked about every update of a
// fork; nil removes it.
func (s *Service) RegisterForkUpdateGuard(g ForkUpdateGuard) {
	s.forkMu.Lock()
	defer s.forkMu.Unlock()
	s.forkGuard = g
}

// forkUpdate is the fork-only field handlers and the guard registered now.
func (s *Service) forkUpdate() (map[string]ForkFieldHandler, ForkUpdateGuard) {
	s.forkMu.RLock()
	defer s.forkMu.RUnlock()
	return s.forkFields, s.forkGuard
}

// update serves PATCH /workspaces/:slug. In one store transaction it reads
// the workspace, judges the body against it (applyUpdate) and writes the
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
	// A map, so that a member given as null is told from one not given.
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
		next, writes, err := s.applyUpdate(ctx, tx, cur, body)
		if err != nil {
			return err
		}
		next.UpdatedAt = s.deps.Now().UTC().Truncate(time.Second)
		if err := writeUpdate(ctx, tx, next); err != nil {
			return err
		}
		if next.OrgID != cur.OrgID {
			// A fork lives in its parent's org, so a move takes the forks
			// along, in this transaction: all of them move or none.
			if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET org_id = ? WHERE parent_slug = ?`, next.OrgID, slug); err != nil {
				return fmt.Errorf("move the forks of workspace %q: %w", slug, err)
			}
		}
		for _, write := range writes {
			if err := write(ctx, tx, next); err != nil {
				return fmt.Errorf("update workspace %q: %w", slug, err)
			}
		}
		// In the transaction: an update the log cannot record is not made.
		in, err := updateEventInput(ctx, cur, body)
		if err != nil {
			return err
		}
		if _, err := s.deps.Events.Emit(ctx, tx, in); err != nil {
			return fmt.Errorf("write the update event of workspace %q: %w", slug, err)
		}
		return nil
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

// applyUpdate judges body against cur and returns cur with the fields body
// carries applied, and the writes of the fork-only fields it carries. The
// checks run in this order, and the first that fails answers: an immutable
// field (400 invalid_request); a field that does not apply to cur's kind
// (400 not_supported_on_fork for a repository's field on a fork, 400
// invalid_request for a fork-only field on a repository workspace or with
// no handler registered); no mutable field (400 invalid_request); the
// values, in the order of updateFields (400 invalid_request
// "<field>: <reason>", or the answer the field gave); an org_id naming an
// org of which the workspace's owner, whoever the caller is, is not an
// active member (403 forbidden); an archived workspace (409
// workspace_archived), because validation comes before state; and for a
// fork the registered guard.
func (s *Service) applyUpdate(ctx context.Context, tx *sql.Tx, cur Workspace, body map[string]json.RawMessage) (Workspace, []ForkFieldWrite, error) {
	forkFields, guard := s.forkUpdate()
	fork := cur.Kind == access.KindFork
	var given []updateField
	for _, f := range updateFields {
		if _, ok := body[f.name]; ok {
			given = append(given, f)
		}
	}
	for _, f := range given {
		if f.class == fieldImmutable {
			return Workspace{}, nil, server.InvalidRequest(f.name + ": cannot be changed")
		}
	}
	for _, f := range given {
		switch {
		case f.class == fieldRepository && fork:
			return Workspace{}, nil, server.NewError("not_supported_on_fork", http.StatusBadRequest,
				f.name+": a task fork takes its org and its settings from its parent")
		case f.class == fieldFork && !fork:
			return Workspace{}, nil, server.InvalidRequest(f.name + ": applies only to a task fork")
		case f.class == fieldFork && forkFields[f.name] == nil:
			return Workspace{}, nil, server.InvalidRequest(f.name + ": cannot be changed")
		}
	}
	// Every field left is mutable on cur's kind.
	if len(given) == 0 {
		return Workspace{}, nil, server.InvalidRequest("the body carries no field to change")
	}
	next := cur
	var writes []ForkFieldWrite
	for _, f := range given {
		var err error
		switch {
		case f.apply != nil:
			err = f.apply(ctx, tx, s, &next, body[f.name])
		case f.class == fieldFork:
			var write ForkFieldWrite
			if write, err = forkFields[f.name](cur, body[f.name]); write != nil {
				writes = append(writes, write)
			}
		}
		if err != nil {
			if answer, ok := errors.AsType[*server.Error](err); ok {
				return Workspace{}, nil, answer
			}
			if hubErr, ok := errors.AsType[failure](err); ok {
				return Workspace{}, nil, hubErr.error
			}
			return Workspace{}, nil, server.InvalidRequest(f.name + ": " + err.Error())
		}
	}
	if _, ok := body["org_id"]; ok {
		// The workspace stays its owner's: an org it moves to must be one
		// of the owner's, even when the admin token moves it.
		active, err := identity.ActiveOrgIDs(ctx, tx, cur.OwnerID)
		if err != nil {
			return Workspace{}, nil, err
		}
		if !slices.Contains(active, next.OrgID) {
			return Workspace{}, nil, server.Forbidden("the workspace's owner is not an active member of the organization")
		}
	}
	if cur.Status == StatusArchived {
		return Workspace{}, nil, server.WorkspaceArchived("workspace " + cur.Slug + " is archived")
	}
	if fork && guard != nil {
		if err := guard(ctx, tx, next); err != nil {
			return Workspace{}, nil, err
		}
	}
	return next, writes, nil
}

// updateEventInput is the parley.workspace.update event of an update of
// cur, whose body has passed every check, by the request's caller: the
// admin token, with no actor id, or the credential's type with its user's
// id. Its metadata names the mutable fields body carries, in the order of
// updateFields, and for a fork the parent, never a value, so that the log
// shows who changed what without holding a setting.
func updateEventInput(ctx context.Context, cur Workspace, body map[string]json.RawMessage) (event.Input, error) {
	slug := cur.Slug
	in := event.Input{Type: eventUpdate, Workspace: &slug, ResourceType: "workspace", ResourceID: slug}
	p, ok := identity.PrincipalOf(ctx)
	switch {
	case ok && p.Type == identity.TypeAdmin:
		in.ActorType = event.ActorAdminToken
	case ok && p.User != nil:
		id := p.User.ID
		in.ActorType, in.ActorID = p.Type, &id
	default:
		return event.Input{}, errors.New("the workspace update found no caller")
	}
	fields := []string{}
	for _, f := range updateFields {
		if _, given := body[f.name]; given && f.class != fieldImmutable {
			fields = append(fields, f.name)
		}
	}
	meta := map[string]any{"fields": fields}
	if cur.ParentSlug != nil {
		meta["parent_slug"], in.Parent = *cur.ParentSlug, *cur.ParentSlug
	}
	in.Metadata = meta
	return in, nil
}

// writeUpdate writes the fields an update changes of w, and its updated_at,
// through tx.
func writeUpdate(ctx context.Context, tx *sql.Tx, w Workspace) error {
	settings, err := encodeSettings(w.Settings)
	if err != nil {
		return fmt.Errorf("update workspace %q: %w", w.Slug, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET display_name = ?, description = ?, org_id = ?, settings = ?,
		updated_at = ? WHERE slug = ?`, w.DisplayName, w.Description, w.OrgID, settings, w.UpdatedAt.UnixNano(), w.Slug); err != nil {
		return fmt.Errorf("update workspace %q: %w", w.Slug, err)
	}
	return nil
}
