package awareness

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// EventClaimRelease is the event of a claim's release.
const EventClaimRelease = "parley.claim.release"

// The error types of the claim routes.
const (
	errClaimNotFound  = "claim_not_found"
	errClaimNotActive = "claim_not_active"
)

// claimOf is the claim :id of r stored on the workspace the route acts on,
// read through q (nil for the store). An :id that is not a UUID, that no
// claim has, or whose claim is stored on another workspace, is 404
// claim_not_found.
func (c *Component) claimOf(ctx context.Context, q Querier, r *http.Request) (Claim, error) {
	param := server.Param(r, "id")
	id, err := uuid.Parse(param)
	if err != nil {
		return Claim{}, claimNotFound(param)
	}
	return c.storedClaim(ctx, q, actedOn(r), id.String())
}

// storedClaim is the claim id stored on workspace, read through q (nil for
// the store), or 404 claim_not_found.
func (c *Component) storedClaim(ctx context.Context, q Querier, workspace, id string) (Claim, error) {
	cl, found, err := readClaim(ctx, c.querier(q), workspace, id)
	if err != nil {
		return Claim{}, err
	}
	if !found {
		return Claim{}, claimNotFound(id)
	}
	return cl, nil
}

// claimNotFound is the 404 claim_not_found of the claim id id.
func claimNotFound(id string) error {
	return server.NewError(errClaimNotFound, http.StatusNotFound, fmt.Sprintf("no claim %q", id))
}

// readClaimRoute answers the claim :id, with the state a read now reports.
func (c *Component) readClaimRoute(w http.ResponseWriter, r *http.Request) error {
	cl, err := c.claimOf(r.Context(), nil, r)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, objectOf(cl, c.deps.Now()))
	return nil
}

// listClaimsRoute answers, in pages of the list envelope, the claims stored
// on the workspace the route acts on that the query selects: the reported
// state given by state (active when absent), the session given by
// session_id and a glob able to match the same files as the glob given by
// path, newest first. An unknown state, or a path that is not a glob, is
// 400 invalid_request.
func (c *Component) listClaimsRoute(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var after claimPosition
	paged, err := page.After(&after)
	if err != nil {
		return err
	}
	f, err := claimFilterOf(r.URL.Query())
	if err != nil {
		return err
	}
	var from *claimPosition
	if paged {
		from = &after
	}
	now := c.deps.Now()
	claims, err := listClaims(r.Context(), c.querier(nil), actedOn(r), f, now, from, page.Limit+1)
	if err != nil {
		return err
	}
	items := make([]listedClaim, len(claims))
	for i, cl := range claims {
		items[i] = listedClaim{ClaimObject: objectOf(cl, now), position: positionOfClaim(cl)}
	}
	return server.WritePage(w, page, items, func(l listedClaim) any { return l.position })
}

// listedClaim is an item of the claim list: the claim object, and the
// claim's place in the list, which the object's whole-second times cannot
// give.
type listedClaim struct {
	ClaimObject
	position claimPosition
}

// claimFilterOf reads the filters of a claim list from the query
// parameters v.
func claimFilterOf(v url.Values) (claimFilter, error) {
	f := claimFilter{state: ClaimActive, session: v.Get("session_id")}
	if v.Has("state") {
		switch f.state = v.Get("state"); f.state {
		case ClaimActive, ClaimReleased, ClaimExpired, ClaimFulfilled:
		default:
			return claimFilter{}, server.InvalidRequest(fmt.Sprintf("state must be one of %s, %s, %s and %s, not %q",
				ClaimActive, ClaimReleased, ClaimExpired, ClaimFulfilled, f.state))
		}
	}
	if v.Has("path") {
		globs, err := identity.ValidatePaths([]string{v.Get("path")})
		if err != nil {
			return claimFilter{}, server.InvalidRequest("path: " + err.Error())
		}
		f.path = globs[0]
	}
	return f, nil
}

// renewal is the body of a claim's renewal, checked.
type renewal struct {
	// ttl is the claim's new lifetime from now, 0 to leave expires_at alone.
	ttl time.Duration
	// intent is the new intent, nil to leave it alone.
	intent *string
}

// renewalOf reads the body of a claim's renewal: ttl, intent or both,
// with the bounds of a claim's creation. Neither, or a member that breaks
// its rule, is a 400 invalid_request.
func renewalOf(r *http.Request, maxTTL time.Duration) (renewal, error) {
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return renewal{}, err
	}
	var ren renewal
	ttl, hasTTL, err := ttlMember(body, maxTTL)
	if err != nil {
		return renewal{}, err
	}
	intent, hasIntent, err := intentMember(body)
	if err != nil {
		return renewal{}, err
	}
	if !hasTTL && !hasIntent {
		return renewal{}, server.InvalidRequest("a renewal sets ttl, intent or both")
	}
	ren.ttl = ttl
	if hasIntent {
		ren.intent = &intent
	}
	return ren, nil
}

// renewClaimRoute renews the claim :id and answers it: with ttl its
// expires_at becomes now plus ttl, with intent its intent is replaced. It
// checks, in this order, that the caller may change the claim (403
// forbidden), the body (400 invalid_request) and that the claim is active
// (409 claim_not_active), and writes no event.
func (c *Component) renewClaimRoute(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	cl, err := c.claimOf(ctx, nil, r)
	if err != nil {
		return err
	}
	if err := c.mayModify(r, cl); err != nil {
		return err
	}
	ren, err := renewalOf(r, c.deps.Config.MaxClaimTTL)
	if err != nil {
		return err
	}
	now := c.deps.Now()
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		// Read again in the transaction, so that a release or an expiry
		// since the first read is not undone.
		if cl, err = c.storedClaim(ctx, tx, cl.Workspace, cl.ID); err != nil {
			return err
		}
		if state := cl.reportedState(now); state != ClaimActive {
			return server.NewError(errClaimNotActive, http.StatusConflict, fmt.Sprintf("claim %s is %s", cl.ID, state))
		}
		if ren.ttl > 0 {
			cl.ExpiresAt = now.Add(ren.ttl)
		}
		if ren.intent != nil {
			cl.Intent = *ren.intent
		}
		return renewClaim(ctx, tx, cl)
	})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, objectOf(cl, now))
	return nil
}

// releaseClaimRoute releases the claim :id and answers 204 with an empty
// body: an active claim becomes released, with the time it ended, and
// parley.claim.release is written in the same transaction; a claim in any
// other state is left as it is, without an event. The caller must be
// allowed to change the claim (403 forbidden).
func (c *Component) releaseClaimRoute(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	cl, err := c.claimOf(ctx, nil, r)
	if err != nil {
		return err
	}
	if err := c.mayModify(r, cl); err != nil {
		return err
	}
	by, err := actorOfRequest(r)
	if err != nil {
		return err
	}
	now := c.deps.Now()
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if cl, err = c.storedClaim(ctx, tx, cl.Workspace, cl.ID); err != nil {
			return err
		}
		if cl.reportedState(now) != ClaimActive {
			return nil
		}
		if err := releaseClaim(ctx, tx, cl.ID, now); err != nil {
			return err
		}
		meta := map[string]any{}
		event.PathList(meta, "paths", cl.Paths)
		_, err := c.deps.Emitter.Emit(ctx, tx, event.Input{
			Type: EventClaimRelease, Workspace: &cl.Workspace, ActorType: by.ActorType, ActorID: by.ActorID,
			ResourceType: "claim", ResourceID: cl.ID, SessionID: cl.SessionID, Metadata: meta,
		})
		return err
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// mayModify answers nil when the caller of r may renew or release the claim
// cl: the admin token, the owner of the workspace the claim is stored on,
// and the user who made the claim, with any credential of theirs. Anyone
// else is 403 forbidden.
func (c *Component) mayModify(r *http.Request, cl Claim) error {
	p, ok := identity.PrincipalOf(r.Context())
	if ok && p.Type == identity.TypeAdmin {
		return nil
	}
	if ok && p.User != nil {
		if cl.By.ActorID != nil && *cl.By.ActorID == p.User.ID {
			return nil
		}
		owns, err := c.deps.Owns(r.Context(), p.User.ID, cl.Workspace)
		if err != nil {
			return err
		}
		if owns {
			return nil
		}
	}
	return server.Forbidden(fmt.Sprintf("claim %s may be changed only by its creator, the owner of %s or the admin token",
		cl.ID, cl.Workspace))
}
