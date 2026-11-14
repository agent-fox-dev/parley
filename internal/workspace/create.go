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
	"strings"
	"time"
	"unicode/utf8"

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
	Slug              string
	GitURL            string
	DefaultBranch     *string
	OrgID             *string
	DisplayName       *string
	Description       *string
	Mode              string // ModeStandard or ModeCarryPatch
	UpstreamURL       *string
	IntegrationBranch *string
	// Settings is the stored settings document the request's settings
	// make, holding only the keys it sets.
	Settings    map[string]json.RawMessage
	GitPAT      *string
	GitUsername *string
	GitPassword *string
}

// createMember reads one member of a create body into the request; raw is
// nil when the member is absent, or null for a nullable member. A member
// that is not nullable is refused when it is null. Its error is the reason,
// without the member's name, or a *server.Error that is the answer itself.
type createMember struct {
	name     string
	nullable bool
	read     func(s *Service, req *createRequest, raw json.RawMessage) error
}

// createMembers are the members of a create body in the order they are
// judged: the first one refused answers the request, so this order decides
// which member a refusal names, and a member may depend on the members
// before it. Members not listed are ignored.
var createMembers = []createMember{
	{"slug", false, func(_ *Service, req *createRequest, raw json.RawMessage) error {
		return readRequired(raw, &req.Slug, ValidateSlug)
	}},
	{"git_url", false, func(_ *Service, req *createRequest, raw json.RawMessage) error {
		return readRequired(raw, &req.GitURL, ValidateGitURL)
	}},
	{"default_branch", true, func(_ *Service, req *createRequest, raw json.RawMessage) error {
		return readOptional(raw, &req.DefaultBranch, ValidateBranchName)
	}},
	{"org_id", true, func(_ *Service, req *createRequest, raw json.RawMessage) error {
		return readOptional(raw, &req.OrgID, nil)
	}},
	{"display_name", true, func(_ *Service, req *createRequest, raw json.RawMessage) error {
		return readOptional(raw, &req.DisplayName, atMost(128))
	}},
	{"description", true, func(_ *Service, req *createRequest, raw json.RawMessage) error {
		return readOptional(raw, &req.Description, atMost(1024))
	}},
	{"workspace_mode", true, func(_ *Service, req *createRequest, raw json.RawMessage) error {
		var mode *string
		if err := readOptional(raw, &mode, nil); err != nil {
			return err
		}
		switch deref(mode) {
		case "", ModeStandard:
			req.Mode = ModeStandard
		case ModeCarryPatch:
			req.Mode = ModeCarryPatch
		default:
			return fmt.Errorf("must be %q or %q", ModeStandard, ModeCarryPatch)
		}
		return nil
	}},
	{"upstream_url", true, func(_ *Service, req *createRequest, raw json.RawMessage) error {
		if err := readCarryPatch(req, raw, &req.UpstreamURL, ValidateGitURL); err != nil {
			return err
		}
		if req.Mode == ModeCarryPatch && req.UpstreamURL == nil {
			return fmt.Errorf("is required with workspace_mode %s", ModeCarryPatch)
		}
		return nil
	}},
	{"integration_branch", true, func(_ *Service, req *createRequest, raw json.RawMessage) error {
		return readCarryPatch(req, raw, &req.IntegrationBranch, ValidateBranchName)
	}},
	{"settings", true, func(s *Service, req *createRequest, raw json.RawMessage) error {
		settings, err := s.settings.ApplySettings(nil, raw)
		req.Settings = settings
		return err
	}},
	{"git_pat", false, func(_ *Service, req *createRequest, raw json.RawMessage) error {
		return readCredential(req, raw, &req.GitPAT)
	}},
	{"git_username", false, func(_ *Service, req *createRequest, raw json.RawMessage) error {
		if raw != nil && req.GitPAT != nil {
			return errors.New("must not be given with git_pat")
		}
		return readCredential(req, raw, &req.GitUsername)
	}},
	{"git_password", false, func(_ *Service, req *createRequest, raw json.RawMessage) error {
		switch {
		case raw != nil && req.GitPAT != nil:
			return errors.New("must not be given with git_pat")
		case raw == nil && req.GitUsername != nil:
			return errors.New("is required with git_username")
		case raw != nil && req.GitUsername == nil:
			return errors.New("is accepted only with git_username")
		}
		return readCredential(req, raw, &req.GitPassword)
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

// readOptional reads the member raw, a string or null, into dst, and judges
// a string by rule unless rule is nil.
func readOptional(raw json.RawMessage, dst **string, rule func(string) error) error {
	if raw == nil {
		return nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return errors.New("must be a string or null")
	}
	if rule != nil {
		if err := rule(v); err != nil {
			return err
		}
	}
	*dst = &v
	return nil
}

// readCarryPatch reads a member only a carry-patch workspace has: with
// workspace_mode standard any value but null is refused.
func readCarryPatch(req *createRequest, raw json.RawMessage, dst **string, rule func(string) error) error {
	if raw != nil && req.Mode != ModeCarryPatch {
		return fmt.Errorf("is accepted only with workspace_mode %s", ModeCarryPatch)
	}
	return readOptional(raw, dst, rule)
}

// readCredential reads a credential member, a string that is not empty,
// into dst. A git@ origin takes no credential: its login is the form's own,
// so a credential given with one is refused rather than ignored.
func readCredential(req *createRequest, raw json.RawMessage, dst **string) error {
	if raw == nil {
		return nil
	}
	if strings.HasPrefix(req.GitURL, "git@") {
		return errors.New("must not be given with a git_url of the form git@<host>:<path>")
	}
	var v string
	switch {
	case json.Unmarshal(raw, &v) != nil:
		return errors.New("must be a string")
	case v == "":
		return errors.New("must not be empty")
	}
	*dst = &v
	return nil
}

// atMost is the rule of a text member of at most n characters, counted as
// Unicode code points so that a name in any script has the same room.
func atMost(n int) func(string) error {
	return func(s string) error {
		if utf8.RuneCountInString(s) > n {
			return fmt.Errorf("must be at most %d characters", n)
		}
		return nil
	}
}

// parseCreate reads body by createMembers, answering the first member
// refused with 400 invalid_request "<member>: <reason>", or with the answer
// the member's reader gave.
func (s *Service) parseCreate(body map[string]json.RawMessage) (createRequest, error) {
	req := createRequest{Mode: ModeStandard}
	for _, m := range createMembers {
		raw := body[m.name]
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if !m.nullable {
				return createRequest{}, server.InvalidRequest(m.name + ": must be a string")
			}
			raw = nil
		}
		if err := m.read(s, &req, raw); err != nil {
			if answer, ok := errors.AsType[*server.Error](err); ok {
				return createRequest{}, answer
			}
			return createRequest{}, server.InvalidRequest(m.name + ": " + err.Error())
		}
	}
	return req, nil
}

// errAdminCreate answers the admin token: a workspace is owned by a user.
var errAdminCreate = server.Forbidden("a workspace needs a user as owner; the admin token cannot create one")

// create serves POST /workspaces: it records a repository workspace in
// status pending, which is its clone work, with its origin credential as
// workspace secrets and its event in one transaction, tells the clone
// workers once that transaction has committed, and answers 201 with the
// workspace object. The checks run cheapest first, so that every local
// refusal comes before the call to origin: the caller, the body, the
// organization, the slug in use, then, for a request with a credential,
// whether secrets can be stored and whether origin takes the credential. A
// slug taken by a request that raced this one is the same 409 as one taken
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
	req, err := s.parseCreate(body)
	if err != nil {
		return err
	}
	org, err := s.organization(ctx, p.User, req.OrgID)
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
	if cred, given := req.credential(); given {
		if err := s.checkCredential(ctx, req.Slug, req.GitURL, cred); err != nil {
			return err
		}
	}

	now := s.deps.Now().UTC().Truncate(time.Second)
	ws := Workspace{
		Slug: req.Slug, Kind: access.KindRepository, Mode: req.Mode, GitURL: &req.GitURL,
		DefaultBranch: req.DefaultBranch, OwnerID: p.User.ID, OrgID: org.ID, Status: StatusPending,
		DisplayName: req.Slug, UpstreamURL: req.UpstreamURL, IntegrationBranch: req.IntegrationBranch,
		Settings:  req.Settings,
		CreatedAt: now, UpdatedAt: now, InstanceID: NewInstanceID(),
	}
	if req.DisplayName != nil && *req.DisplayName != "" {
		ws.DisplayName = *req.DisplayName
	}
	if req.Description != nil {
		ws.Description = *req.Description
	}
	// Built before the commit, so that a workspace once committed and queued
	// is always answered 201.
	obj, err := s.render(ctx, ws, access.LevelOwner, org)
	if err != nil {
		return err
	}
	err = s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := s.deps.InsertWorkspace(ctx, tx, ws); err != nil {
			return err
		}
		if err := s.storeCredential(ctx, tx, ws.Slug, req); err != nil {
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
	s.Enqueue(ws.Slug)
	server.WriteJSON(w, http.StatusCreated, obj)
	return nil
}

// organization is the org a create of the user u records the workspace in:
// the org selector names, by id or by slug, or the user's personal org when
// selector is nil. An org that does not exist is the request's fault (400);
// one u is not an active member of is refused (403).
func (s *Service) organization(ctx context.Context, u *identity.User, selector *string) (identity.Org, error) {
	db := s.deps.Store.Main()
	var org identity.Org
	var err error
	if selector == nil {
		org, err = identity.PersonalOrg(ctx, db, u.ID)
	} else {
		org, err = identity.OrgBySelector(ctx, db, *selector)
	}
	switch {
	case errors.Is(err, identity.ErrNotFound) && selector == nil:
		return identity.Org{}, server.InvalidRequest("org_id: the caller has no personal organization")
	case errors.Is(err, identity.ErrNotFound):
		return identity.Org{}, server.InvalidRequest(fmt.Sprintf("org_id: no organization has the id or slug %q", *selector))
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
