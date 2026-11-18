package sandbox

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// hubScopes are the scopes a hub credential may carry, sorted: what a
// sandbox needs to read and push its workspace, propose changes and report
// its session and audit records, and nothing that reaches beyond it.
var hubScopes = []string{"audit:write", "changes:read", "changes:write", "git:read", "git:write", "sessions:write"}

// hubAccessOf is the hub access the caller p gives a sandbox of the profile
// p's hub_access is profile, expiring at expiresAt: the hub scopes p holds,
// all of them for an API key, which holds every scope, and p's paths limit.
// It is nil, for a sandbox without a hub credential, when the profile gives
// none, when p holds none of the hub scopes, since a token needs a scope,
// and when externalURL is empty, since the sandbox would have no address
// for the hub.
//
// A scope counts only when p holds it by name: the minting component holds
// a PAT creator to exactly the scopes it holds, so a git:write that
// satisfies git:read does not give git:read.
func hubAccessOf(p identity.Principal, profile, externalURL string, expiresAt time.Time) *HubAccess {
	if profile != HubAccessFork || externalURL == "" {
		return nil
	}
	ha := &HubAccess{ExpiresAt: expiresAt}
	for _, scope := range hubScopes {
		if p.Type != identity.TypePAT || slices.Contains(p.Permissions, scope) {
			ha.Scopes = append(ha.Scopes, scope)
		}
	}
	if len(ha.Scopes) == 0 {
		return nil
	}
	if p.Type == identity.TypePAT {
		ha.Paths = slices.Clone(p.Paths)
	}
	return ha
}

// canClone reports whether a hub credential of ha can clone the workspace,
// by the scope check: the checkout needs git:read.
func canClone(ha *HubAccess) bool {
	return ha != nil && identity.Satisfies(ha.Scopes, "git:read")
}

// hubTokenName is the name of the hub credential of the sandbox id.
func hubTokenName(id string) string { return "sandbox:" + id }

// mint mints in tx, through the minting component, the hub credential of
// the sandbox id with the access ha, bound to its workspace and minted by
// creator, whose user owns it. The minting component is asked for what the
// hub computed, so any refusal of it is the hub's fault: it is an error
// that is not a *server.Error, answered 500 internal, and the caller's
// transaction rolls back the whole create.
func (s *Service) mint(ctx context.Context, tx *sql.Tx, creator identity.Principal, id, workspace string, ha *HubAccess) (identity.PAT, string, error) {
	pat, value, err := s.deps.Mint(ctx, tx, access.MintInput{
		Creator: creator, Workspace: workspace, Name: hubTokenName(id),
		Permissions: slices.Clone(ha.Scopes), Paths: slices.Clone(ha.Paths), ExpiresAt: &ha.ExpiresAt,
	})
	if err != nil {
		return identity.PAT{}, "", fmt.Errorf("mint the hub credential of sandbox %s: %s", id, err)
	}
	return pat, value, nil
}

// MintHubCredential mints in tx a replacement for the hub credential of the
// sandbox rec, as a start or a provisioning resumed after a restart needs
// one: with the scopes and paths of rec's stored hub access, bound to rec's
// workspace, owned by the user who created the sandbox and expiring at
// expiresAt, through the minting component, with its parley.token.bind
// event. It changes neither the record nor the old token: the caller
// revokes that one (RevokeHubCredential), stores the new token's id and
// expiry in the same tx and, once the tx committed, hands the value to the
// provisioning with SetHubCredential. rec must have hub access.
//
// The hub mints on behalf of the owner, who presents no credential here, so
// the minter is the owner with the type of an API key: the subset rule of a
// PAT creator does not apply, and the scopes and paths are those the
// creator's own credential was held to at creation.
func (s *Service) MintHubCredential(ctx context.Context, tx *sql.Tx, rec Record, expiresAt time.Time) (identity.PAT, string, error) {
	if rec.HubAccess == nil {
		return identity.PAT{}, "", fmt.Errorf("sandbox %s has no hub access to mint a credential for", rec.ID)
	}
	owner := identity.Principal{Type: identity.TypeAPIKey, User: &identity.User{ID: rec.UserID, Username: rec.CreatedBy.Username}}
	ha := *rec.HubAccess
	ha.ExpiresAt = expiresAt
	return s.mint(ctx, tx, owner, rec.ID, rec.Workspace, &ha)
}

// RevokeHubCredential revokes in tx, at the hub's time, the stored hub
// credential of the sandbox rec by its token id, as a stop, a delete, a
// failed provisioning or a replacement does. A sandbox without one has
// nothing to revoke; a token already revoked keeps its time. It writes no
// event and does not commit.
func (s *Service) RevokeHubCredential(ctx context.Context, tx *sql.Tx, rec Record) error {
	if rec.HubTokenID == "" {
		return nil
	}
	if err := s.deps.Revoke(ctx, tx, rec.HubTokenID, s.deps.Now()); err != nil {
		return fmt.Errorf("revoke the hub credential of sandbox %s: %w", rec.ID, err)
	}
	return nil
}

// HubCredential returns the value of the hub credential of the sandbox id
// as this process minted it, for the provisioning to give the runtime. ok
// is false when the process holds none: the sandbox has no hub credential,
// or it was minted before the hub restarted, and the value, never stored,
// is gone with it.
func (s *Service) HubCredential(id string) (value string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok = s.hubValues[id]
	return value, ok
}

// SetHubCredential keeps value, in memory only, as the hub credential of
// the sandbox id, and adds it to the values the sandbox's text is scrubbed
// of. Call it once the transaction that minted it has committed.
func (s *Service) SetHubCredential(id, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hubValues == nil {
		s.hubValues = map[string]string{}
	}
	s.hubValues[id] = value
	if v, ok := s.values[id]; ok {
		v.HubCredential = value
		s.values[id] = v
	}
}
