package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// BindEventType is the type of the event that records the minting of a
// bound token.
const BindEventType = "parley.token.bind"

// MintInput is what a bound token is minted with.
type MintInput struct {
	// Creator is the credential that mints the token. The token is owned by
	// its user, and a PAT creator is held to the subset rule.
	Creator identity.Principal
	// Workspace is the slug the token is bound to.
	Workspace string
	// Name, Permissions and Paths are the token's, validated as for
	// POST /user/tokens; nil Paths is a token without path limits.
	Name        string
	Permissions []string
	Paths       []string
	// Expires is the lifetime in days, 0, 30, 60 or 90, where 0 never
	// expires and nil means 90. A hub-internal caller sets ExpiresAt, an
	// instant, in its place.
	Expires   *int
	ExpiresAt *time.Time
}

// errNoBindEmitter is a mint on a service bound without an event emitter: a
// fault of the wiring, refused rather than minting a token without its
// event.
var errNoBindEmitter = errors.New("the access service has no event emitter to record the binding")

// MintBound mints a PAT bound to in.Workspace in tx, which it does not
// commit, and records parley.token.bind in the same tx, so that the token
// and its event commit or roll back together and no token exists without
// its binding or its event. It mints through identity.MintPAT, the one way a
// PAT is created, and returns its errors unchanged: 400 invalid_request for
// an invalid input, 403 insufficient_scope for a PAT creator beyond what it
// holds, with nothing written. Package 15 calls it for the token of a new
// fork and packages 45 and 47 for the hub credential of a sandbox.
func (s *Service) MintBound(ctx context.Context, tx *sql.Tx, in MintInput) (identity.PAT, string, error) {
	return s.mintBound(ctx, tx, in, nil)
}

// mintBound is MintBound with gate, when not nil, run after the token is
// written and before its event: an error of gate ends the mint before the
// event is emitted, and the caller rolls back the token with tx. It lets a
// route refuse a valid request (an archived workspace) only after the
// request has been validated, without emitting an event that is rolled
// back.
func (s *Service) mintBound(ctx context.Context, tx *sql.Tx, in MintInput, gate func() error) (identity.PAT, string, error) {
	if s.deps.Events == nil {
		return identity.PAT{}, "", errNoBindEmitter
	}
	value, pat, err := identity.MintPAT(ctx, tx, in.Creator, identity.MintRequest{
		Name: in.Name, Permissions: in.Permissions, Paths: in.Paths,
		Expires: in.Expires, ExpiresAt: in.ExpiresAt, Workspace: in.Workspace,
	}, s.now())
	if err != nil {
		return identity.PAT{}, "", err
	}
	if gate != nil {
		if err := gate(); err != nil {
			return identity.PAT{}, "", err
		}
	}
	if err := s.deps.Events.Emit(ctx, tx, bindEvent(in.Creator, pat)); err != nil {
		return identity.PAT{}, "", fmt.Errorf("record the binding of token %s: %w", pat.TokenID, err)
	}
	return pat, value, nil
}

// bindEvent is the parley.token.bind event of pat, minted by creator. It
// carries what the token is bound to and may do, never its value.
func bindEvent(creator identity.Principal, pat identity.PAT) Event {
	var expires any
	if pat.ExpiresAt != nil {
		expires = timefmt.Format(*pat.ExpiresAt)
	}
	return Event{
		Type: BindEventType, Workspace: pat.Workspace, ActorType: creator.Type, ActorID: pat.UserID,
		ResourceType: "token", ResourceID: pat.TokenID,
		Metadata: map[string]any{
			"token_id": pat.TokenID, "workspace": pat.Workspace, "permissions": pat.Permissions,
			"paths": pat.Paths, "expires_at": expires,
		},
	}
}

// now is the service's clock: Deps.Now, or the system clock.
func (s *Service) now() time.Time {
	if s.deps.Now != nil {
		return s.deps.Now()
	}
	return time.Now()
}
