package fork

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The PAT scope the create needs, and the event it writes.
const (
	scopeWrite = "workspaces:write"
	// EventFork is the type of the event a fork's creation writes.
	EventFork = "parley.workspace.fork"
)

// A generated name: nameLength characters of nameAlphabet, drawn at most
// nameDraws times for one create, a bound the 36^8 names never reach in
// practice.
const (
	nameAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	nameLength   = 8
	nameDraws    = 8
)

// Routes returns the fork routes of the REST API.
func (c *Component) Routes() []server.Route {
	return []server.Route{c.createRoute()}
}

// createRoute is POST /workspaces/:slug/forks. Reading the parent is enough
// to fork it, and the token the create may mint is shown once: a replay
// with the same Idempotency-Key answers without its secret.
func (c *Component) createRoute() server.Route {
	return server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/forks", Scope: scopeWrite, Create: true,
		ShowOnce: []string{"token.token"}, Marks: map[string]string{access.MarkNeed: string(access.NeedRead)},
		Handler: c.create}
}

// createRequest is a create body once its members are read. A nil pointer
// is a member that is absent or null.
type createRequest struct {
	Name *string
	Ref  *string
	// ExpiresAt is the new fork's expiry, from ttl or [forks].default_ttl.
	ExpiresAt  time.Time
	AutoChange bool
	Purpose    *string
	Token      *tokenRequest
}

// tokenRequest is the token member of a create body: the token to mint,
// bound to the fork, whose values the minting component judges.
type tokenRequest struct {
	Name        *string  `json:"name"`
	Permissions []string `json:"permissions"`
	Paths       []string `json:"paths"`
}

// createMember reads one member of a create body into the request; raw is
// nil when the member is absent or null. Its error is the reason, without
// the member's name, or a *server.Error that is the answer itself.
type createMember struct {
	name string
	read func(c *Component, req *createRequest, raw json.RawMessage, now time.Time) error
}

// createMembers are the members of a create body in the order they are
// judged: the first one refused answers the request. Members not listed
// are ignored.
var createMembers = []createMember{
	{"name", func(_ *Component, req *createRequest, raw json.RawMessage, _ time.Time) error {
		return readString(raw, &req.Name, workspace.ValidateForkName)
	}},
	{"ref", func(_ *Component, req *createRequest, raw json.RawMessage, _ time.Time) error {
		return readString(raw, &req.Ref, nil)
	}},
	{"ttl", func(c *Component, req *createRequest, raw json.RawMessage, now time.Time) error {
		var ttl *string
		if err := readString(raw, &ttl, nil); err != nil || ttl == nil {
			return err
		}
		at, err := ResolveTTL(*ttl, now, c.deps.Limits)
		req.ExpiresAt = at
		return err
	}},
	{"auto_change", func(_ *Component, req *createRequest, raw json.RawMessage, _ time.Time) error {
		if raw != nil && json.Unmarshal(raw, &req.AutoChange) != nil {
			return errors.New("must be true or false")
		}
		return nil
	}},
	{"token", func(_ *Component, req *createRequest, raw json.RawMessage, _ time.Time) error {
		if raw == nil {
			return nil
		}
		var t tokenRequest
		if raw[0] != '{' || json.Unmarshal(raw, &t) != nil {
			return errors.New("must be an object whose name is a string and whose permissions and paths are lists of strings")
		}
		req.Token = &t
		return nil
	}},
	{"purpose", func(_ *Component, req *createRequest, raw json.RawMessage, _ time.Time) error {
		return readString(raw, &req.Purpose, nil)
	}},
}

// readString reads the member raw, a string, into dst, and judges it by
// rule unless rule is nil; an absent member leaves dst nil.
func readString(raw json.RawMessage, dst **string, rule func(string) error) error {
	if raw == nil {
		return nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return errors.New("must be a string")
	}
	if rule != nil {
		if err := rule(v); err != nil {
			return err
		}
	}
	*dst = &v
	return nil
}

// parseCreate reads the body of r by createMembers, at the hub's time now.
// No body is the empty object, and a null member is an absent one, so that
// a client may send back what it read. It answers the first member refused
// with 400 invalid_request "<member>: <reason>", or with the answer the
// member's reader gave.
func (c *Component) parseCreate(r *http.Request, now time.Time) (createRequest, error) {
	body := map[string]json.RawMessage{}
	if r.ContentLength != 0 {
		if err := server.DecodeJSON(r, &body); err != nil {
			return createRequest{}, err
		}
	}
	req := createRequest{AutoChange: true, ExpiresAt: DefaultExpiry(now, c.deps.Limits)}
	for _, m := range createMembers {
		raw := body[m.name]
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			raw = nil
		}
		if err := m.read(c, &req, raw, now); err != nil {
			if answer, ok := errors.AsType[*server.Error](err); ok {
				return createRequest{}, answer
			}
			return createRequest{}, server.InvalidRequest(m.name + ": " + err.Error())
		}
	}
	return req, nil
}

// errAdminCreate answers the admin token: a fork is owned by a user.
var errAdminCreate = server.Forbidden("a fork needs a user as owner; the admin token cannot create one")

// create serves POST /workspaces/:slug/forks: it makes the fork's
// repository, which shares the parent's history and copies none of it, then
// in one transaction stores the fork, ready at once, mints the token the
// body asks for and writes the fork's event, and answers 201 with the fork's
// workspace object and its git remote. The checks run in the order of the
// refusals: the caller, the body, the parent's state, the base branch, the
// name in use. What the create made is removed when it fails, and a
// directory found at the fork's place without a record is a leftover of a
// create that stopped, which it replaces.
func (c *Component) create(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p, ok := identity.PrincipalOf(ctx)
	switch {
	case !ok:
		return server.Internal("the fork create found no authenticated principal")
	case p.Type == identity.TypeAdmin || p.User == nil:
		return errAdminCreate
	}
	now := c.deps.Now().UTC()
	req, err := c.parseCreate(r, now)
	if err != nil {
		return err
	}
	parent, found, err := workspace.BySlug(ctx, c.deps.Store.Main(), server.Param(r, "slug"))
	switch {
	case err != nil:
		return err
	case !found:
		// Deleted since the access check read it.
		return server.NotFound("workspace not found")
	}
	if err := workspace.RequireReady(parent); err != nil {
		return err
	}
	f, err := c.newFork(ctx, p, parent, req, now)
	if err != nil {
		return err
	}

	// One create of a parent at a time from the name check to the commit,
	// so that a directory removed as a leftover is never the repository
	// another create of the same name is making.
	unlock := c.lockParent(parent.Slug)
	defer unlock()
	if f.Slug, err = c.freeSlug(ctx, parent.Slug, req.Name); err != nil {
		return err
	}
	f.DisplayName = f.Slug
	dir := c.deps.Workspaces.RepoPath(f.Slug)
	if err := gitrepo.Remove(dir); err != nil {
		return err
	}
	if err := c.deps.CreateRepo(ctx, c.deps.Workspaces.RepoPath(parent.Slug), dir); err != nil {
		return errors.Join(fmt.Errorf("create fork %q: %w", f.Slug, err), gitrepo.Remove(dir))
	}
	ans, err := c.commit(ctx, p, f, req)
	if err != nil {
		err = errors.Join(err, gitrepo.Remove(dir))
		if errors.Is(err, access.ErrDuplicateSlug) {
			return nameTaken(parent.Slug, f.Slug)
		}
		return err
	}
	server.WriteJSON(w, http.StatusCreated, ans)
	return nil
}

// newFork is the record of the fork of parent that req asks the caller p
// for, at the hub's time now, without its slug: ready, standard, owned by
// p's user in the parent's org, taken from the base branch at the parent's
// head of it, tracking the parent's branches and tags as they are now, and
// created by p's PAT when p authenticated with one. A base branch the
// parent does not have is 400 fork_ref_not_found.
func (c *Component) newFork(ctx context.Context, p identity.Principal, parent workspace.Workspace, req createRequest, now time.Time) (Fork, error) {
	repo, err := c.deps.Git.Open(ctx, c.deps.Workspaces.RepoPath(parent.Slug))
	if err != nil {
		return Fork{}, fmt.Errorf("the repository of %q: %w", parent.Slug, err)
	}
	branches, err := repo.Branches(ctx)
	if err != nil {
		return Fork{}, fmt.Errorf("the branches of %q: %w", parent.Slug, err)
	}
	tags, err := repo.Tags(ctx)
	if err != nil {
		return Fork{}, fmt.Errorf("the tags of %q: %w", parent.Slug, err)
	}
	base := ""
	switch {
	case req.Ref != nil:
		base = *req.Ref
	case parent.DefaultBranch != nil:
		base = *parent.DefaultBranch
	}
	sha, ok := branches[base]
	if !ok {
		return Fork{}, server.NewError("fork_ref_not_found", http.StatusBadRequest,
			fmt.Sprintf("ref: %q is not a branch of %q", base, parent.Slug))
	}
	created := now.Truncate(time.Second)
	f := Fork{
		Workspace: workspace.Workspace{
			Kind: access.KindFork, Mode: workspace.ModeStandard, DefaultBranch: &base,
			OwnerID: p.User.ID, OrgID: parent.OrgID, Status: workspace.StatusReady,
			ParentSlug: &parent.Slug, ForkPoint: &workspace.ForkPoint{Ref: base, SHA: sha},
			ExpiresAt: &req.ExpiresAt, AutoChange: &req.AutoChange, Purpose: req.Purpose,
			CreatedAt: created, UpdatedAt: created, InstanceID: workspace.NewInstanceID(),
		},
		TrackingBranches: slices.Sorted(maps.Keys(branches)),
		TrackingTags:     slices.Sorted(maps.Keys(tags)),
	}
	if p.Type == identity.TypePAT {
		f.CreatingTokenID = p.CredentialID
	}
	return f, nil
}

// freeSlug is the slug of the new fork of parent: parent--name for the
// given name, 409 conflict when a workspace has it, or parent-- and a
// generated name not in use, drawn at most nameDraws times, after which the
// create fails.
func (c *Component) freeSlug(ctx context.Context, parent string, name *string) (string, error) {
	if name != nil {
		slug := parent + "--" + *name
		_, taken, err := access.RecordBySlug(ctx, c.deps.Store.Main(), slug)
		switch {
		case err != nil:
			return "", err
		case taken:
			return "", nameTaken(parent, slug)
		}
		return slug, nil
	}
	for range nameDraws {
		slug := parent + "--" + c.deps.NewName()
		_, taken, err := access.RecordBySlug(ctx, c.deps.Store.Main(), slug)
		if err != nil || !taken {
			return slug, err
		}
	}
	return "", fmt.Errorf("no free fork name for %q in %d draws", parent, nameDraws)
}

// nameTaken answers a create whose given name a fork of parent has.
func nameTaken(parent, slug string) error {
	return server.Conflict(fmt.Sprintf("name: %q has a fork %q", parent, slug))
}

// createAnswer is the body of a create's 201: the fork's workspace object,
// its git remote on the hub, null without an external URL, and the token
// minted with it when the body asked for one.
type createAnswer struct {
	Workspace workspace.Object `json:"workspace"`
	Remote    *string          `json:"remote"`
	Token     *mintedToken     `json:"token,omitempty"`
}

// commit stores the fork f that the caller p creates by req in one
// transaction, with the token req asks for and the fork's event, and
// returns the create's answer. The parent's state is judged again on its
// record in the transaction, so that a maintenance operation that began
// after the first check refuses the create rather than racing it.
func (c *Component) commit(ctx context.Context, p identity.Principal, f Fork, req createRequest) (createAnswer, error) {
	// Built before the commit, so that a fork once committed is always
	// answered 201.
	obj, err := c.deps.Workspaces.RenderWorkspace(ctx, f.Workspace, access.LevelOwner)
	if err != nil {
		return createAnswer{}, err
	}
	ans := createAnswer{Workspace: obj, Remote: obj.HubURL}
	err = c.deps.Transact(ctx, func(tx *sql.Tx) error {
		parent, found, err := workspace.BySlug(ctx, tx, *f.ParentSlug)
		switch {
		case err != nil:
			return err
		case !found:
			return server.NotFound("workspace not found")
		}
		if err := workspace.RequireReady(parent); err != nil {
			return err
		}
		if err := Insert(ctx, tx, f); err != nil {
			return err
		}
		var tokenID *string
		if req.Token != nil {
			name := "fork:" + f.Slug
			if req.Token.Name != nil {
				name = *req.Token.Name
			}
			never := 0
			pat, value, err := c.deps.Access.MintBound(ctx, tx, access.MintInput{Creator: p, Workspace: f.Slug,
				Name: name, Permissions: req.Token.Permissions, Paths: req.Token.Paths, Expires: &never})
			if err != nil {
				return err
			}
			ans.Token, tokenID = &mintedToken{pat, value}, &pat.TokenID
		}
		_, err = c.deps.Events.Emit(ctx, tx, forkEventInput(p, f, tokenID))
		return err
	})
	if err != nil {
		return createAnswer{}, err
	}
	return ans, nil
}

// mintedToken is the token member of a create's answer: the Token object
// and, as its member token, the token's value, which no other answer
// carries.
type mintedToken struct {
	pat   identity.PAT
	value string
}

func (t mintedToken) MarshalJSON() ([]byte, error) {
	obj, err := json.Marshal(t.pat)
	if err != nil {
		return nil, err
	}
	value, err := json.Marshal(t.value)
	if err != nil {
		return nil, err
	}
	// The Token object is a JSON object; token is appended as its last
	// member, as POST /workspaces/:slug/tokens answers it.
	out := append(obj[:len(obj)-1:len(obj)-1], `,"token":`...)
	return append(append(out, value...), '}'), nil
}

// forkEventInput is the parley.workspace.fork event of the fork f that the
// caller p created, with the id of the token minted with it, nil for none.
// It never holds the token's value.
func forkEventInput(p identity.Principal, f Fork, tokenID *string) event.Input {
	slug, actor := f.Slug, p.User.ID
	meta := map[string]any{
		"parent_slug": *f.ParentSlug,
		"fork_point":  workspace.ForkPointValue{Ref: f.ForkPoint.Ref, SHA: f.ForkPoint.SHA},
		"expires_at":  timefmt.Format(*f.ExpiresAt),
		"auto_change": *f.AutoChange,
		"purpose":     f.Purpose,
	}
	if tokenID != nil {
		meta["token_id"] = *tokenID
	}
	return event.Input{
		Type: EventFork, Workspace: &slug, ActorType: p.Type, ActorID: &actor,
		ResourceType: "workspace", ResourceID: f.Slug, SessionID: f.SessionID, Metadata: meta, Parent: *f.ParentSlug,
	}
}

// lockParent takes the create lock of the workspace parent and returns its
// release.
func (c *Component) lockParent(parent string) (unlock func()) {
	l, _ := c.createLocks.LoadOrStore(parent, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// randomName draws a fork name of nameLength characters of nameAlphabet
// from a cryptographic random source, each character equally likely.
func randomName() string {
	name := make([]byte, 0, nameLength)
	buf := make([]byte, 2*nameLength)
	for len(name) < nameLength {
		// crypto/rand.Read never fails; it ends the process when the
		// system's source does.
		_, _ = rand.Read(buf)
		for _, b := range buf {
			// 252 is the largest multiple of 36 not above 256: a byte from
			// it up is drawn again, so that no character is favoured.
			if b < 252 && len(name) < nameLength {
				name = append(name, nameAlphabet[int(b)%len(nameAlphabet)])
			}
		}
	}
	return string(name)
}
