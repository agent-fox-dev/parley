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

// The PAT scope POST /workspaces needs, and the event a create writes.
const (
	scopeWrite  = "workspaces:write"
	eventCreate = "parley.workspace.create"
)

// createRoute is POST /workspaces. It creates a resource, so it takes an
// Idempotency-Key, and it names no workspace, so a workspace-bound token is
// refused by the binding guard.
func (s *Service) createRoute() server.Route {
	return server.Route{Method: http.MethodPost, Pattern: "/workspaces", Scope: scopeWrite, Create: true, Handler: s.create}
}

// createRequest is a create body once its members are read. A nil pointer is
// a member that is absent or null.
type createRequest struct {
	Slug          string
	GitURL        string
	DefaultBranch *string
	DisplayName   *string
	Description   *string
}

// createMember reads one member of a create body into the request; raw is
// nil when the member is absent or null. Its error is the reason, without
// the member's name.
type createMember struct {
	name string
	read func(req *createRequest, raw json.RawMessage) error
}

// createMembers are the members of a create body in the order they are
// judged: the first one refused answers the request, so this order decides
// which member a refusal names. Members not listed are ignored.
var createMembers = []createMember{
	{"slug", func(req *createRequest, raw json.RawMessage) error {
		return readRequired(raw, &req.Slug, ValidateSlug)
	}},
	{"git_url", func(req *createRequest, raw json.RawMessage) error {
		return readRequired(raw, &req.GitURL, ValidateGitURL)
	}},
	{"default_branch", func(req *createRequest, raw json.RawMessage) error {
		return readOptional(raw, &req.DefaultBranch)
	}},
	{"display_name", func(req *createRequest, raw json.RawMessage) error {
		return readOptional(raw, &req.DisplayName)
	}},
	{"description", func(req *createRequest, raw json.RawMessage) error {
		return readOptional(raw, &req.Description)
	}},
}

// readRequired reads the string member raw into dst and judges it by rule.
func readRequired(raw json.RawMessage, dst *string, rule func(string) error) error {
	if raw == nil {
		return errors.New("is required")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return errors.New("must be a string")
	}
	return rule(*dst)
}

// readOptional reads the member raw, a string or null, into dst.
func readOptional(raw json.RawMessage, dst **string) error {
	if raw == nil {
		return nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return errors.New("must be a string or null")
	}
	*dst = &v
	return nil
}

// parseCreate reads body by createMembers, answering the first member
// refused with 400 invalid_request "<member>: <reason>".
func parseCreate(body map[string]json.RawMessage) (createRequest, error) {
	var req createRequest
	for _, m := range createMembers {
		raw := body[m.name]
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			raw = nil
		}
		if err := m.read(&req, raw); err != nil {
			return createRequest{}, server.InvalidRequest(m.name + ": " + err.Error())
		}
	}
	return req, nil
}

// errAdminCreate answers the admin token: a workspace is owned by a user.
var errAdminCreate = server.Forbidden("a workspace needs a user as owner; the admin token cannot create one")

// create serves POST /workspaces: it records a repository workspace in
// status pending, which is its clone work, with its event in one
// transaction, tells the clone workers once that transaction has committed,
// and answers 201 with the workspace object. The checks run cheapest first:
// the caller, the body, the organization, then the slug in use; a slug
// taken by a request that raced this one is the same 409 as one taken
// before.
func (s *Service) create(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p, ok := identity.PrincipalOf(ctx)
	switch {
	case !ok:
		return server.Internal("the create found no authenticated principal")
	case p.Type == identity.TypeAdmin || p.User == nil:
		return errAdminCreate
	}
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	req, err := parseCreate(body)
	if err != nil {
		return err
	}
	org, err := s.organization(ctx, p.User)
	if err != nil {
		return err
	}
	_, taken, err := access.RecordBySlug(ctx, s.deps.Store.Main(), req.Slug)
	switch {
	case err != nil:
		return err
	case taken:
		return slugTaken(req.Slug)
	}

	now := s.deps.Now().UTC().Truncate(time.Second)
	ws := Workspace{
		Slug: req.Slug, Kind: access.KindRepository, Mode: ModeStandard, GitURL: &req.GitURL,
		DefaultBranch: req.DefaultBranch, OwnerID: p.User.ID, OrgID: org.ID, Status: StatusPending,
		DisplayName: req.Slug, Settings: map[string]json.RawMessage{},
		CreatedAt: now, UpdatedAt: now, InstanceID: NewInstanceID(),
	}
	if req.DisplayName != nil && *req.DisplayName != "" {
		ws.DisplayName = *req.DisplayName
	}
	if req.Description != nil {
		ws.Description = *req.Description
	}
	err = s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := Insert(ctx, tx, ws); err != nil {
			return err
		}
		_, err := s.deps.Events.Emit(ctx, tx, createEventInput(p, ws))
		return err
	})
	switch {
	case errors.Is(err, access.ErrDuplicateSlug):
		return slugTaken(req.Slug)
	case err != nil:
		return fmt.Errorf("create workspace %q: %w", req.Slug, err)
	}
	if s.deps.Notify != nil {
		s.deps.Notify(ws.Slug)
	}
	obj, err := s.RenderWorkspace(ctx, ws, access.LevelOwner)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusCreated, obj)
	return nil
}

// organization is the org a create of the user u records the workspace in:
// the user's personal org, of which u must be an active member.
func (s *Service) organization(ctx context.Context, u *identity.User) (identity.Org, error) {
	db := s.deps.Store.Main()
	org, err := identity.PersonalOrg(ctx, db, u.ID, u.Username)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return identity.Org{}, server.InvalidRequest("org_id: the caller has no personal organization")
	case err != nil:
		return identity.Org{}, err
	}
	active, err := identity.ActiveOrgIDs(ctx, db, u.ID)
	if err != nil {
		return identity.Org{}, err
	}
	if !slices.Contains(active, org.ID) {
		return identity.Org{}, server.Forbidden("the caller is not an active member of the organization")
	}
	return org, nil
}

// slugTaken answers a create whose slug a workspace has, in any status.
func slugTaken(slug string) error {
	return server.Conflict(fmt.Sprintf("slug: a workspace with the slug %q exists", slug))
}

// createEventInput is the parley.workspace.create event of the workspace ws
// that the caller p created. Its metadata never holds a credential: the
// git_url of a workspace carries none.
func createEventInput(p identity.Principal, ws Workspace) event.Input {
	slug, actor := ws.Slug, p.User.ID
	return event.Input{
		Type: eventCreate, Workspace: &slug, ActorType: p.Type, ActorID: &actor,
		ResourceType: "workspace", ResourceID: ws.Slug,
		Metadata: map[string]any{
			"kind": ws.Kind, "git_url": ws.GitURL, "workspace_mode": ws.Mode, "default_branch": ws.DefaultBranch,
		},
	}
}
