package fork

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The PAT scopes the create needs, the second only to mint a token, and
// the event it writes.
const (
	scopeWrite  = "workspaces:write"
	scopeTokens = "tokens:manage"
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

// purposeMax is the most code points a fork's purpose may hold.
const purposeMax = 1024

// Routes returns the fork routes of the REST API.
func (c *Component) Routes() []server.Route {
	return []server.Route{c.createRoute(), c.listRoute()}
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
	// SessionID is the session the fork is created in, a UUID.
	SessionID *string
	// Sandbox is the sandbox member as received, a JSON object, nil when
	// the body has none.
	Sandbox json.RawMessage
}

// tokenRequest is the token member of a create body: the token to mint,
// bound to the fork. The create judges only expires, which the minting
// component takes as an instant; the minting component judges the rest.
type tokenRequest struct {
	Name        *string  `json:"name"`
	Permissions []string `json:"permissions"`
	Paths       []string `json:"paths"`
	Expires     *string  `json:"expires"`
	// lifetime is expires as a duration, 0 for a token that never expires.
	lifetime time.Duration
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
		return readString(raw, &req.Ref, func(s string) error {
			if s == "" {
				return errors.New("must not be empty")
			}
			return nil
		})
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
			return errors.New("must be an object whose name and expires are strings and whose permissions and paths are lists of strings")
		}
		if t.Expires != nil {
			d, err := workspace.ParseDuration(*t.Expires)
			if err != nil {
				return fmt.Errorf("expires: %w", err)
			}
			t.lifetime = d
		}
		req.Token = &t
		return nil
	}},
	{"purpose", func(_ *Component, req *createRequest, raw json.RawMessage, _ time.Time) error {
		return readString(raw, &req.Purpose, func(s string) error {
			if utf8.RuneCountInString(s) > purposeMax {
				return fmt.Errorf("must be at most %d characters long", purposeMax)
			}
			return nil
		})
	}},
	{"session_id", func(_ *Component, req *createRequest, raw json.RawMessage, _ time.Time) error {
		return readString(raw, &req.SessionID, func(s string) error {
			if !server.IsUUID(s) {
				return errors.New("must be a UUID")
			}
			return nil
		})
	}},
	{"sandbox", func(_ *Component, req *createRequest, raw json.RawMessage, _ time.Time) error {
		if raw != nil && raw[0] != '{' {
			return errors.New("must be an object")
		}
		req.Sandbox = raw
		return nil
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
// No body, or an empty one however it was sent, is the empty object, and a
// null member is an absent one, so that a client may send back what it
// read. It answers the first member refused with 400 invalid_request
// "<member>: <reason>", or with the answer the member's reader gave.
func (c *Component) parseCreate(r *http.Request, now time.Time) (createRequest, error) {
	body := map[string]json.RawMessage{}
	// Judged on the bytes, not on the declared length, which a body sent in
	// chunks does not have.
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return createRequest{}, server.InvalidRequest("request body could not be read")
	}
	if len(data) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(data))
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
// refusals: the caller, the body, the session, a fork as parent, the
// parent's state, the base branch, the fork limit, the name in use, the
// token, the sandbox. What the create made is removed when it fails, and a
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
	slug := server.Param(r, "slug")
	if req.SessionID, err = c.checkSession(ctx, p, req.SessionID, slug); err != nil {
		return err
	}
	parent, found, err := workspace.BySlug(ctx, c.deps.Store.Main(), slug)
	switch {
	case err != nil:
		return err
	case !found:
		// Deleted since the access check read it.
		return server.NotFound("workspace not found")
	case parent.Kind == access.KindFork:
		return server.NewError("not_supported_on_fork", http.StatusBadRequest,
			fmt.Sprintf("workspace %q is a task fork, which cannot be forked; fork its parent instead", parent.Slug))
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
	// Counted before the repository is made, so that a parent at its limit
	// costs no directory, and again in the transaction.
	if err := c.checkLimit(ctx, c.deps.Store.Main(), parent.Slug); err != nil {
		return err
	}
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
			ExpiresAt: &req.ExpiresAt, AutoChange: &req.AutoChange, Purpose: req.Purpose, SessionID: req.SessionID,
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

// checkSession judges the session_id the caller p names for a fork of the
// workspace parent, nil for none, by the session-linking rule, and returns
// the id as the rule gives it back, in lower case. Without a rule no session
// exists, so every id is unknown.
func (c *Component) checkSession(ctx context.Context, p identity.Principal, id *string, parent string) (*string, error) {
	if id == nil {
		return nil, nil
	}
	unknown := server.NewError("unknown_session", http.StatusBadRequest, fmt.Sprintf("session_id: no session %q", *id))
	if c.linker == nil {
		return nil, unknown
	}
	got, err := c.linker.Check(ctx, p, *id, parent)
	switch {
	case errors.Is(err, ErrUnknownSession):
		return nil, unknown
	case err != nil:
		return nil, fmt.Errorf("check session %q: %w", *id, err)
	}
	return &got, nil
}

// checkLimit is 429 fork_limit_reached when the workspace parent has
// [forks].max_per_workspace forks or more, counting the fork records q
// reads.
func (c *Component) checkLimit(ctx context.Context, q access.Querier, parent string) error {
	n, err := CountOf(ctx, q, parent)
	switch {
	case err != nil:
		return err
	case n >= c.deps.Limits.MaxPerWorkspace:
		return server.NewError("fork_limit_reached", http.StatusTooManyRequests,
			fmt.Sprintf("workspace %q has %d forks, the hub's [forks].max_per_workspace", parent, n))
	}
	return nil
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
	// Sandbox is the sandbox component's object, as it gave it.
	Sandbox json.RawMessage `json:"sandbox,omitempty"`
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
		if err := c.checkLimit(ctx, tx, parent.Slug); err != nil {
			return err
		}
		if err := Insert(ctx, tx, f); err != nil {
			return err
		}
		if f.SessionID != nil && c.linker != nil {
			if err := c.linker.Record(ctx, tx, *f.SessionID, parent.Slug, f.Slug); err != nil {
				return fmt.Errorf("link fork %q to session %q: %w", f.Slug, *f.SessionID, err)
			}
		}
		var tokenID *string
		if req.Token != nil {
			pat, value, err := c.mint(ctx, tx, p, f, *req.Token)
			if err != nil {
				return err
			}
			ans.Token, tokenID = &mintedToken{pat, value}, &pat.TokenID
		}
		if _, err := c.deps.Events.Emit(ctx, tx, forkEventInput(p, f, tokenID)); err != nil {
			return err
		}
		// The sandbox is asked last, once the fork, its token and its
		// repository exist, and its refusal, answered as it is, takes the
		// fork with it.
		if req.Sandbox == nil {
			return nil
		}
		ans.Sandbox, err = c.createSandbox(ctx, tx, p, f, req.Sandbox)
		return err
	})
	if err != nil {
		return createAnswer{}, err
	}
	return ans, nil
}

// createSandbox asks the sandbox component in tx for the sandbox raw, the
// body's member as received, on the fork f that the caller p creates, with
// the fork's session and purpose as defaults. Without a component no
// sandbox can be created, which is answered as a hub without sandboxes
// answers.
func (c *Component) createSandbox(ctx context.Context, tx *sql.Tx, p identity.Principal, f Fork, raw json.RawMessage) (json.RawMessage, error) {
	if c.sandbox == nil {
		return nil, server.NewError("sandboxes_disabled", http.StatusNotFound, "sandbox: no sandbox component is available on this hub to create one")
	}
	obj, err := c.sandbox.CreateSandbox(ctx, tx, SandboxRequest{Caller: p, Fork: f, Sandbox: raw, SessionID: f.SessionID, Purpose: f.Purpose})
	switch {
	case err != nil:
		return nil, err
	case !json.Valid(obj) || bytes.TrimSpace(obj)[0] != '{':
		// Judged before the commit, so that a fork once committed is always
		// answered 201.
		return nil, fmt.Errorf("the sandbox of fork %q is not a JSON object: %q", f.Slug, obj)
	}
	return obj, nil
}

// mint mints in tx the token t that the caller p asks for with the fork f,
// bound to f and owned by p's user, through the minting component, whose
// refusals are the answer. A PAT caller must hold tokens:manage, as it must
// to mint a bound token by its own route; the minting component holds it to
// the subset rule. The token expires its lifetime after the fork's
// creation, or never.
func (c *Component) mint(ctx context.Context, tx *sql.Tx, p identity.Principal, f Fork, t tokenRequest) (identity.PAT, string, error) {
	if p.Type == identity.TypePAT && !identity.Satisfies(p.Permissions, scopeTokens) {
		return identity.PAT{}, "", server.InsufficientScope("token: minting a token needs the scope " + scopeTokens)
	}
	in := access.MintInput{Creator: p, Workspace: f.Slug, Name: "fork:" + f.Slug, Permissions: t.Permissions, Paths: t.Paths}
	if t.Name != nil {
		in.Name = *t.Name
	}
	if t.lifetime > 0 {
		in.ExpiresAt = new(f.CreatedAt.Add(t.lifetime))
	} else {
		// Without expires the token lives as long as the fork; nil would
		// be the minting component's default of 90 days.
		in.Expires = new(0)
	}
	return c.deps.Access.MintBound(ctx, tx, in)
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

// parentLock is the create lock of one parent.
type parentLock struct {
	sync.Mutex
	// users counts the creates holding or waiting for the lock, guarded by
	// the component's locksMu.
	users int
}

// lockParent takes the create lock of the workspace parent and returns its
// release. The last create to release a lock removes it, so that the map
// holds only the parents a create is working on.
func (c *Component) lockParent(parent string) (unlock func()) {
	c.locksMu.Lock()
	l := c.createLocks[parent]
	if l == nil {
		if c.createLocks == nil {
			c.createLocks = map[string]*parentLock{}
		}
		l = &parentLock{}
		c.createLocks[parent] = l
	}
	l.users++
	c.locksMu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		c.locksMu.Lock()
		defer c.locksMu.Unlock()
		if l.users--; l.users == 0 {
			delete(c.createLocks, parent)
		}
	}
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
