package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
	"github.com/google/uuid"
)

// The values of a profile's hub_access.
const (
	// HubAccessFork gives a sandbox a hub credential bound to its workspace.
	HubAccessFork = "fork"
	// HubAccessNone gives a sandbox no hub credential.
	HubAccessNone = "none"
)

// EventCreate is the type of the event a create writes with the sandbox.
const EventCreate = "parley.sandbox.create"

// ErrUnknownSession is the answer of a SessionLinker's check that refuses a
// session id: the create answers it 400 unknown_session.
var ErrUnknownSession = errors.New("unknown session")

// SessionLinker is the session-linking rule as the create asks it. The
// session feature registers it on the service; without one no session
// exists, and every session_id a body names is refused.
type SessionLinker interface {
	// Check judges the session id the caller names for a sandbox of the
	// workspace. It returns the id as the session has it when it accepts
	// it, an error matching ErrUnknownSession when it refuses it, and any
	// other error when it cannot tell.
	Check(ctx context.Context, caller identity.Principal, sessionID, workspace string) (string, error)
	// Record links the sandbox sandboxID of the workspace to the session
	// sessionID in tx, the sandbox's creating transaction, so that the link
	// is stored with the sandbox or not at all.
	Record(ctx context.Context, tx *sql.Tx, sessionID, workspace, sandboxID string) error
}

// RegisterSessionLinker makes l the session-linking rule of the create; nil
// removes it. It is safe to call while the hub serves; a create takes the
// rule once.
func (s *Service) RegisterSessionLinker(l SessionLinker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.linker = l
}

// sessionLinker is the registered session-linking rule; nil for none.
func (s *Service) sessionLinker() SessionLinker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.linker
}

// CreateInput is what a create works from.
type CreateInput struct {
	// Caller is the authenticated user the sandbox is created for.
	Caller identity.Principal
	// Workspace is the record of the workspace the sandbox is attached to.
	Workspace workspace.Workspace
	// Body is the request's JSON object as received; empty is {}.
	Body []byte
	// SessionID and Purpose are what the sandbox gets when the body names
	// none; nil for none. A purpose longer than a sandbox's is cut.
	SessionID *string
	Purpose   *string
}

// CreateRoute declares POST /workspaces/:slug/sandboxes: it creates a
// sandbox on the workspace and answers 201 with its object in state
// provisioning, before the runtime is called, so that it never answers with
// a runtime error. It needs the scope sandboxes:write of a PAT and write
// access to the workspace, takes an Idempotency-Key, and refuses the admin
// token, since the sandbox's hub credential is minted for a user.
func (s *Service) CreateRoute() server.Route {
	return s.WorkspaceRoute(KindWrite, server.Route{
		Method:  http.MethodPost,
		Pattern: "/workspaces/:slug/sandboxes",
		Create:  true,
		Handler: s.createSandbox,
	}, NeedsUser())
}

func (s *Service) createSandbox(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p, ok := identity.PrincipalOf(ctx)
	if !ok || p.User == nil {
		return server.Forbidden(msgNeedsUser)
	}
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the sandbox create found no access decision")
	}
	ws, found, err := workspace.BySlug(ctx, s.deps.Store.Main(), d.Record.Slug)
	switch {
	case err != nil:
		return err
	case !found:
		return server.NotFound(workspace.ErrNotFound.Error())
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return server.InvalidRequest("request body could not be read")
	}
	in := CreateInput{Caller: p, Workspace: ws, Body: body}
	if ws.Kind == access.KindFork {
		in.SessionID = ws.SessionID
	}
	rec, err := s.Create(ctx, nil, in)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusCreated, s.object(rec))
	return nil
}

// object is the sandbox object of rec, runtime_reachable as the runtime
// client reports it.
func (s *Service) object(rec Record) Object {
	c := s.Client()
	return ObjectOf(rec, c != nil && c.Reachable())
}

// Create creates the sandbox in asks for and returns its record, stored in
// state provisioning for the provisioner to bring up. It is the one create
// of the hub: the route calls it, and so does the fork create, inside its
// own transaction.
//
// tx is the caller's open transaction, in which everything is read and
// written and which the caller commits; nil runs Create in a transaction of
// its own, after whose commit the provisioner is woken. A sandbox created in
// the caller's transaction is found by the provisioner's next pass, once
// the caller has committed.
//
// The checks run in the order of the refusals: the body's values (400
// invalid_request), the session link (400 unknown_session), the workspace's
// state (409), the secret store's (503 secrets_unavailable) and the
// required credentials and exposed secrets (422 credential_missing, both
// Resolve), the quotas (429 sandbox_capacity, CheckQuota). A refused create
// writes nothing. The record keeps the names of what was resolved, never a
// value. A store or event failure is an error that is not a *server.Error,
// answered 500, and writes nothing either; its text is scrubbed of the
// resolved values (Scrub).
func (s *Service) Create(ctx context.Context, tx *sql.Tx, in CreateInput) (Record, error) {
	if in.Caller.User == nil {
		return Record{}, server.Forbidden(msgNeedsUser)
	}
	body, err := readBody(in.Body)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	run := func(tx *sql.Tx) (err error) {
		rec, err = s.create(ctx, tx, in, body)
		return err
	}
	if tx != nil {
		return rec, run(tx)
	}
	if err := s.deps.Store.Tx(ctx, run); err != nil {
		return Record{}, err
	}
	s.wakeProvisioner()
	return rec, nil
}

// create is Create in tx, the body read.
func (s *Service) create(ctx context.Context, tx *sql.Tx, in CreateInput, body map[string]json.RawMessage) (Record, error) {
	w := in.Workspace
	req, err := s.judge(ctx, tx, w, body)
	if err != nil {
		return Record{}, err
	}
	session, err := s.checkSession(ctx, in, req.sessionID)
	if err != nil {
		return Record{}, err
	}
	if err := workspace.RequireReady(w); err != nil {
		return Record{}, err
	}
	if err := fork.GuardUpdate(ctx, tx, w); err != nil {
		return Record{}, err
	}
	snapshot, err := s.snapshotOf(req.profile)
	if err != nil {
		return Record{}, err
	}
	res, err := s.Resolve(ctx, w.Slug, snapshot)
	if err != nil {
		return Record{}, err
	}
	if err := res.Missing(); err != nil {
		return Record{}, err
	}
	// From here on an error's text may hold what was resolved.
	rec, err := s.store(ctx, tx, in, req, session, snapshot, res)
	return rec, scrubError(res.Values(), err)
}

// store is the end of create, once the request passed every check but the
// quotas: it checks them, then stores the sandbox with the names res
// resolved, links it to its session and writes its create event, all in tx.
func (s *Service) store(ctx context.Context, tx *sql.Tx, in CreateInput, req createRequest, session *string, snapshot Snapshot, res Resolution) (Record, error) {
	w := in.Workspace
	// The quotas come last: their count is atomic with the insert below
	// only in this transaction.
	if err := s.CheckQuota(ctx, tx, w, in.Caller.User.ID); err != nil {
		return Record{}, err
	}
	rec, err := s.newRecord(ctx, tx, in, req, session, snapshot)
	if err != nil {
		return Record{}, err
	}
	rec.Credentials, rec.ExposedSecrets = res.CredentialNames(), res.SecretNames()
	if err := Insert(ctx, tx, rec); err != nil {
		return Record{}, err
	}
	if rec.SessionID != nil {
		if l := s.sessionLinker(); l != nil {
			if err := l.Record(ctx, tx, *rec.SessionID, w.Slug, rec.ID); err != nil {
				return Record{}, fmt.Errorf("link sandbox %s to session %s: %w", rec.ID, *rec.SessionID, err)
			}
		}
	}
	if err := s.emitCreate(ctx, tx, in, rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// checkSession judges the session id the body names, nil for none, by the
// session-linking rule, and returns the session of the sandbox: the one the
// rule gives back, or in's default when the body names none. A default is
// the workspace's own session, which was linked when the fork was created,
// so it is not asked again. Without a rule no session exists, so every id
// is unknown; an id that is not a UUID names no session.
func (s *Service) checkSession(ctx context.Context, in CreateInput, id *string) (*string, error) {
	if id == nil {
		return in.SessionID, nil
	}
	unknown := server.NewError("unknown_session", http.StatusBadRequest, fmt.Sprintf("session_id: no session %q", *id))
	l := s.sessionLinker()
	if l == nil || !isUUID(*id) {
		return nil, unknown
	}
	got, err := l.Check(ctx, in.Caller, *id, in.Workspace.Slug)
	switch {
	case errors.Is(err, ErrUnknownSession):
		return nil, unknown
	case err != nil:
		return nil, fmt.Errorf("check session %q: %w", *id, err)
	}
	return &got, nil
}

// isUUID reports whether s is a UUID in its 36-character form, the one form
// a session id is written in.
func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return len(s) == 36 && err == nil
}

// newRecord is the record of the sandbox req asks the caller for on in's
// workspace, at the hub's time, in state provisioning and marked for the
// provisioner.
func (s *Service) newRecord(ctx context.Context, tx *sql.Tx, in CreateInput, req createRequest, session *string, snapshot Snapshot) (Record, error) {
	w, p := in.Workspace, req.profile
	checkout, err := s.checkoutOf(ctx, tx, w, req.checkout)
	if err != nil {
		return Record{}, err
	}
	now := s.deps.Now().UTC().Truncate(time.Second)
	purpose := ""
	switch {
	case req.purpose != nil:
		purpose = *req.purpose
	case in.Purpose != nil:
		purpose = cutRunes(*in.Purpose, maxPurposeRune)
	}
	creator := Creator{ActorType: actorType(in.Caller), ActorID: in.Caller.User.ID, Username: in.Caller.User.Username}
	return Record{
		ID: NewID(), Workspace: w.Slug, SessionID: session, Profile: p.Name, Purpose: purpose,
		State: StateProvisioning, Command: req.command, Env: req.env, Checkout: checkout,
		Image: p.Image, CPU: p.CPU, Memory: p.Memory, GPU: p.GPU,
		ExpiresAt: ExpiresAt(now, req.ttl, w), IdleTimeout: p.IdleTimeout, LastActivityAt: now,
		RestoredFrom: req.restoreFrom, Labels: req.labels, CreatedBy: creator, CreatedAt: now, UpdatedAt: now,
		Snapshot: snapshot, UserID: in.Caller.User.ID, OrgID: w.OrgID, FamilyRoot: FamilyRoot(w), Marker: MarkerCreate,
	}, nil
}

// actorType is the event actor type of the caller p, a user's credential.
func actorType(p identity.Principal) string {
	if p.Type == identity.TypePAT {
		return event.ActorPAT
	}
	return event.ActorAPIKey
}

// cutRunes is s cut to its first n characters.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// snapshotOf is the part of the profile p the sandbox keeps: a later change
// or the removal of the profile does not change the sandbox. The base policy
// is read from the profile's file now.
func (s *Service) snapshotOf(p Profile) (Snapshot, error) {
	snap := Snapshot{
		TTL: p.TTL, IdleTimeout: p.IdleTimeout, Checkpoint: p.Checkpoint, AllowProposals: p.AllowProposals,
		HubAccess: p.HubAccess, Env: maps.Clone(p.Env), Credentials: slices.Clone(p.Credentials),
		OptionalCredentials: slices.Clone(p.OptionalCredentials), ExposeSecrets: slices.Clone(p.ExposeSecrets),
		OptionalExposeSecrets: slices.Clone(p.OptionalExposeSecrets), ProviderProfiles: map[string]string{},
	}
	for _, name := range slices.Concat(p.Credentials, p.OptionalCredentials) {
		if c, ok := s.settings.Credential(name); ok {
			snap.ProviderProfiles[name] = c.ProviderProfile
		}
	}
	if p.PolicyPath != "" {
		doc, err := os.ReadFile(p.PolicyPath)
		if err != nil {
			return Snapshot{}, fmt.Errorf("the base policy of profile %s: %w", p.Name, err)
		}
		snap.BasePolicy = doc
	}
	return snap, nil
}

// checkoutOf is what the sandbox checks out of the workspace w, nil for
// checkout false: the ref the body names, or w's default branch (a fork's
// base branch); on a fork the work branch, the body's or the fork's name,
// and nil on a repository, where none is created. sha is the commit the ref
// names, or the head of the work branch when the fork has that branch
// already; "" while the repository has neither, which the checkout sets.
func (s *Service) checkoutOf(ctx context.Context, q access.Querier, w workspace.Workspace, c checkoutRequest) (*Checkout, error) {
	if c.off {
		return nil, nil
	}
	out := &Checkout{Path: WorkDir, SHA: c.sha}
	switch {
	case c.ref != nil:
		out.Ref = *c.ref
	case w.DefaultBranch != nil:
		out.Ref = *w.DefaultBranch
		sha, _, err := s.resolveRef(ctx, q, w, out.Ref)
		if err != nil {
			return nil, err
		}
		out.SHA = sha
	}
	if w.Kind != access.KindFork || w.ParentSlug == nil {
		return out, nil
	}
	branch := strings.TrimPrefix(w.Slug, *w.ParentSlug+"--")
	if c.branch != nil {
		branch = *c.branch
	}
	out.Branch = &branch
	tracking, err := s.trackingOf(ctx, q, w)
	if err != nil {
		return nil, err
	}
	if slices.Contains(tracking.branches, branch) {
		return out, nil
	}
	repo, err := s.deps.Repos(ctx, w.Slug)
	if err != nil {
		return nil, err
	}
	if head, found, err := repo.ResolveBranch(ctx, branch); err != nil {
		return nil, err
	} else if found {
		out.SHA = head
	}
	return out, nil
}

// forkTracking is what a fork tracks of its parent: the parent's slug and
// the names of the branches and the tags it tracks.
type forkTracking struct {
	parent         string
	branches, tags []string
}

// trackingOf reads, through q, what the fork w tracks.
func (s *Service) trackingOf(ctx context.Context, q access.Querier, w workspace.Workspace) (forkTracking, error) {
	f, found, err := fork.BySlug(ctx, q, w.Slug)
	if err != nil || !found || f.ParentSlug == nil {
		return forkTracking{}, err
	}
	return forkTracking{parent: *f.ParentSlug, branches: f.TrackingBranches, tags: f.TrackingTags}, nil
}

// emitCreate writes the create event of rec in tx: who created it, and with
// what, by name only. The command is its first argument, since the others
// may carry secrets.
func (s *Service) emitCreate(ctx context.Context, tx *sql.Tx, in CreateInput, rec Record) error {
	o := ObjectOf(rec, false)
	var command *string
	if len(rec.Command) > 0 {
		command = &rec.Command[0]
	}
	in2 := event.Input{
		Type: EventCreate, Workspace: &rec.Workspace, ActorType: rec.CreatedBy.ActorType, ActorID: &in.Caller.User.ID,
		ResourceType: "sandbox", ResourceID: rec.ID, SessionID: rec.SessionID,
		Metadata: map[string]any{
			"profile": rec.Profile, "purpose": rec.Purpose, "command": command,
			"credentials": o.Credentials, "exposed_secrets": o.ExposedSecrets, "workspace_rules": o.WorkspaceRules,
			"hub_access": o.HubAccess, "expires_at": o.ExpiresAt, "restored_from": rec.RestoredFrom,
		},
	}
	if in.Workspace.Kind == access.KindFork && in.Workspace.ParentSlug != nil {
		in2.Parent = *in.Workspace.ParentSlug
	}
	if _, err := s.deps.Events.Emit(ctx, tx, in2); err != nil {
		return fmt.Errorf("event of sandbox %s: %w", rec.ID, err)
	}
	return nil
}

// wakeProvisioner tells the provisioner that a sandbox waits for it,
// without waiting for it to listen.
func (s *Service) wakeProvisioner() {
	select {
	case s.wakeup <- struct{}{}:
	default:
	}
}
