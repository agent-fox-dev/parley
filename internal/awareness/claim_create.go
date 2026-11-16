package awareness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// EventClaimCreate is the event of a claim's creation.
const EventClaimCreate = "parley.claim.create"

// The bounds of a claim's members.
const (
	maxIntentLength = 2000
	maxBranchLength = 255
	minClaimTTL     = time.Second
)

// claimRequest is a claim creation's body, checked.
type claimRequest struct {
	paths  []string
	intent string
	// ttl is the claim's lifetime, 0 for the configured default.
	ttl time.Duration
	// target is the target branch, "" for the default branch of the
	// workspace the route acts on.
	target string
	// session is the session id as given, nil for none.
	session *string
}

// ClaimCreated is the answer to a claim's creation: the claim and the
// activity of its globs on its target branch, the claim itself left out.
type ClaimCreated struct {
	Claim    ClaimObject `json:"claim"`
	Activity Activity    `json:"activity"`
}

// createClaimRoute creates a claim on the workspace the route acts on (a
// task fork's parent) and answers 201 with it and the activity of its
// globs. A body that breaks a rule is 400 invalid_request, a session the
// session-linking rule refuses 400 unknown_session, and a claim over the
// workspace's or the session's limit of active claims 429
// claim_limit_reached; each of them stores nothing and writes no event.
func (c *Component) createClaimRoute(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	cfg := c.deps.Config
	req, err := claimRequestOf(r, cfg.MaxClaimTTL)
	if err != nil {
		return err
	}
	by, err := actorOfRequest(r)
	if err != nil {
		return err
	}
	workspace := actedOn(r)
	session, err := c.checkSession(ctx, by, req.session, server.Param(r, "slug"))
	if err != nil {
		return err
	}
	target := req.target
	if target == "" {
		if target, err = c.deps.DefaultBranch(ctx, workspace); err != nil {
			return err
		}
		if target == "" {
			return server.InvalidRequest("target_branch is required: workspace " + workspace + " has no default branch")
		}
	}
	ttl := req.ttl
	if ttl == 0 {
		ttl = cfg.DefaultClaimTTL
	}
	now := c.deps.Now()
	cl := Claim{
		ID: uuid.NewV4().String(), Workspace: workspace, TargetBranch: target, Paths: req.paths, Intent: req.intent, By: by,
		SessionID: session, State: ClaimActive, CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.storeClaim(ctx, tx, cl, now) }); err != nil {
		return err
	}
	// The activity reads the repository, so it is built once the claim's
	// transaction has released the store's write lock.
	a, err := c.Activity(ctx, workspace, ActivityQuery{Paths: cl.Paths, TargetBranch: cl.TargetBranch, LeaveOutClaim: cl.ID})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusCreated, ClaimCreated{Claim: objectOf(cl, now), Activity: a})
	return nil
}

// storeClaim stores the new claim cl in tx under the limits of active
// claims at now, links it to its session, and writes its
// parley.claim.create event. The limits are counted in the transaction
// that stores the claim, so two concurrent creations never both take the
// last place.
func (c *Component) storeClaim(ctx context.Context, tx *sql.Tx, cl Claim, now time.Time) error {
	n, err := countLiveClaimsOn(ctx, tx, cl.Workspace, now)
	if err != nil {
		return err
	}
	if limit := c.deps.Config.MaxClaimsPerWorkspace; n >= limit {
		return server.NewError("claim_limit_reached", http.StatusTooManyRequests, fmt.Sprintf(
			"workspace %s holds %d active claims, the most [awareness].max_claims_per_workspace allows", cl.Workspace, limit))
	}
	if cl.SessionID != nil {
		n, err := countLiveClaimsOf(ctx, tx, *cl.SessionID, now)
		if err != nil {
			return err
		}
		if n >= MaxClaimsPerSession {
			return server.NewError("claim_limit_reached", http.StatusTooManyRequests, fmt.Sprintf(
				"session %s holds %d active claims, the most a session may hold", *cl.SessionID, MaxClaimsPerSession))
		}
	}
	if err := insertClaim(ctx, tx, cl); err != nil {
		return err
	}
	if cl.SessionID != nil {
		// Checked before the transaction, so the interface is registered
		// unless it was removed since; then the claim names no session the
		// hub knows.
		sessions := c.registeredSessions()
		if sessions == nil {
			return unknownSession(*cl.SessionID)
		}
		if err := sessions.Record(ctx, tx, *cl.SessionID, cl.Workspace, cl.ID); err != nil {
			return err
		}
	}
	meta := map[string]any{"intent": cl.Intent, "target_branch": cl.TargetBranch, "expires_at": timefmt.Format(cl.ExpiresAt)}
	event.PathList(meta, "paths", cl.Paths)
	_, err = c.deps.Emitter.Emit(ctx, tx, event.Input{
		Type: EventClaimCreate, Workspace: &cl.Workspace, ActorType: cl.By.ActorType, ActorID: cl.By.ActorID,
		ResourceType: "claim", ResourceID: cl.ID, SessionID: cl.SessionID, Metadata: meta,
	})
	return err
}

// checkSession is the session a claim made by caller through the
// workspace slug stores for the session id it names (nil for none), as the
// session-linking rule accepts it; a refused id, and every id while no
// session interface is registered, is 400 unknown_session.
func (c *Component) checkSession(ctx context.Context, caller Actor, id *string, slug string) (*string, error) {
	if id == nil {
		return nil, nil
	}
	sessions := c.registeredSessions()
	if sessions == nil {
		return nil, unknownSession(*id)
	}
	got, err := sessions.Check(ctx, caller, *id, slug)
	switch {
	case errors.Is(err, ErrUnknownSession):
		return nil, unknownSession(*id)
	case err != nil:
		return nil, err
	}
	return &got, nil
}

// unknownSession is the 400 unknown_session of the session id id.
func unknownSession(id string) error {
	return server.NewError("unknown_session", http.StatusBadRequest, fmt.Sprintf("session_id: no session %q", id))
}

// actorOfRequest is the authenticated caller of r as a claim's by and its
// event's actor name it: the credential type (admin_token, api_key or pat)
// and the user's id and username, none for the admin token.
func actorOfRequest(r *http.Request) (Actor, error) {
	p, ok := identity.PrincipalOf(r.Context())
	if !ok {
		return Actor{}, server.Unauthenticated("a claim is made by an authenticated caller")
	}
	a := Actor{ActorType: p.Type}
	if p.Type == identity.TypeAdmin {
		a.ActorType = event.ActorAdminToken
	}
	if p.User != nil {
		a.ActorID, a.Username = &p.User.ID, &p.User.Username
	}
	return a, nil
}

// claimRequestOf reads the body of a claim creation and checks its
// members in order: paths, intent, ttl (at least 1 second and at most
// maxTTL), target_branch and the type of session_id. The first member that
// breaks its rule is a 400 invalid_request naming it. A member that is
// null is absent.
func claimRequestOf(r *http.Request, maxTTL time.Duration) (claimRequest, error) {
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return claimRequest{}, err
	}
	var req claimRequest
	var paths any
	if raw, ok := body["paths"]; ok {
		if err := json.Unmarshal(raw, &paths); err != nil {
			return claimRequest{}, server.InvalidRequest("paths must be an array of globs")
		}
	}
	switch v := paths.(type) {
	case nil:
		return claimRequest{}, server.InvalidRequest("paths is required: an array of 1 to 50 globs")
	case []any:
		if len(v) == 0 {
			return claimRequest{}, server.InvalidRequest("paths must hold at least one glob")
		}
	}
	globs, err := identity.ValidatePaths(paths)
	if err != nil {
		return claimRequest{}, server.InvalidRequest(err.Error())
	}
	req.paths = globs

	intent, ok, err := stringMember(body, "intent")
	switch {
	case err != nil:
		return claimRequest{}, err
	case !ok:
		return claimRequest{}, server.InvalidRequest("intent is required")
	case intent == "":
		return claimRequest{}, server.InvalidRequest("intent must not be empty")
	case utf8.RuneCountInString(intent) > maxIntentLength:
		return claimRequest{}, server.InvalidRequest(fmt.Sprintf("intent is longer than %d characters", maxIntentLength))
	}
	req.intent = intent

	ttl, ok, err := stringMember(body, "ttl")
	if err != nil {
		return claimRequest{}, err
	}
	if ok {
		d, err := timefmt.ParseDuration(ttl)
		switch {
		case err != nil:
			return claimRequest{}, server.InvalidRequest("ttl must be a duration with a unit of s, m, h or d, such as 1h: " + err.Error())
		case d < minClaimTTL:
			return claimRequest{}, server.InvalidRequest("ttl must be at least 1s")
		case d > maxTTL:
			return claimRequest{}, server.InvalidRequest("ttl " + ttl + " is longer than [awareness].max_claim_ttl allows")
		}
		req.ttl = d
	}

	target, ok, err := stringMember(body, "target_branch")
	switch {
	case err != nil:
		return claimRequest{}, err
	case ok && target == "":
		return claimRequest{}, server.InvalidRequest("target_branch must not be empty")
	case utf8.RuneCountInString(target) > maxBranchLength:
		return claimRequest{}, server.InvalidRequest(fmt.Sprintf("target_branch is longer than %d characters", maxBranchLength))
	}
	req.target = target

	session, ok, err := stringMember(body, "session_id")
	if err != nil {
		return claimRequest{}, err
	}
	if ok {
		req.session = &session
	}
	return req, nil
}

// stringMember is the member name of body as a string, and whether it is
// present and not null; a member of another type is a 400
// invalid_request.
func stringMember(body map[string]json.RawMessage, name string) (string, bool, error) {
	raw, ok := body[name]
	if !ok || string(raw) == "null" {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, server.InvalidRequest(name + " must be a string")
	}
	return s, true, nil
}
