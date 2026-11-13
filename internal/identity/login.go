package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// defaultKeyDays is the lifetime of the API key a login issues when the
// request does not give one.
const defaultKeyDays = 90

// keyLifetimes are the lifetimes in days a login may ask for; 0 is a key
// that does not expire.
var keyLifetimes = []int{0, 30, 60, 90}

// usernameSuffixes is the last numbered username, <name>-2 to <name>-10, a
// new user tries when the provider's username is another user's.
const usernameSuffixes = 10

// callbackRequest is the body of POST /auth/callback.
type callbackRequest struct {
	Provider    string `json:"provider"`
	Code        string `json:"code"`
	RedirectURI string `json:"redirect_uri"`
	// Expires is the key's lifetime in days; nil when the body has none or
	// null.
	Expires *int `json:"expires"`
}

// issuedKey is an API key as a login answers it, the only time its secret
// is shown.
type issuedKey struct {
	Key       string        `json:"key"`
	KeyID     string        `json:"key_id"`
	ExpiresAt *timefmt.Time `json:"expires_at"`
}

// callbackAnswer is the answer of POST /auth/callback.
type callbackAnswer struct {
	User   User      `json:"user"`
	APIKey issuedKey `json:"api_key"`
}

// callback answers POST /auth/callback: it checks the body, reads the
// identity behind the OAuth code from the provider, signs the user in and
// answers the user with a new API key. A code the provider rejects is 401
// and a provider that fails is 502, both with the fixed text of the error
// kind, so that nothing of the provider's answer reaches the client.
func (s *Service) callback(w http.ResponseWriter, r *http.Request) error {
	var req callbackRequest
	if err := server.DecodeJSON(r, &req); err != nil {
		return err
	}
	days, err := s.validCallback(req)
	if err != nil {
		return err
	}
	if s.deps.Client == nil {
		return server.UpstreamError(ErrUpstream.Error())
	}
	id, err := s.deps.Client.Identity(r.Context(), req.Provider, req.Code, req.RedirectURI)
	switch {
	case errors.Is(err, ErrRejected):
		return server.Unauthenticated(ErrRejected.Error())
	case errors.Is(err, ErrUpstream):
		return server.UpstreamError(ErrUpstream.Error())
	case err != nil:
		return err
	}
	u, key, rec, err := s.signIn(r.Context(), id, days)
	if ce := (conflictError{}); errors.As(err, &ce) {
		return server.Conflict(ce.msg)
	}
	if err != nil {
		return err
	}
	answer := callbackAnswer{User: u, APIKey: issuedKey{Key: key, KeyID: rec.KeyID}}
	if rec.ExpiresAt != nil {
		at := timefmt.Time(*rec.ExpiresAt)
		answer.APIKey.ExpiresAt = &at
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// validCallback checks the body of a login before the provider is called,
// since that call spends the client secret, and returns the key's lifetime
// in days. The body's decoding has refused members of the wrong JSON type
// already, such as a provider that is a number or an expires that is a
// string or a fraction. The messages are fixed texts: none repeats the
// code.
func (s *Service) validCallback(req callbackRequest) (days int, err error) {
	switch {
	case req.Provider == "":
		return 0, server.InvalidRequest("provider is required")
	case req.Code == "":
		return 0, server.InvalidRequest("code is required")
	case req.RedirectURI == "":
		return 0, server.InvalidRequest("redirect_uri is required")
	case !slices.ContainsFunc(s.deps.Providers, func(p config.Provider) bool { return p.Name == req.Provider }):
		return 0, server.InvalidRequest("provider is not a configured identity provider")
	case !allowedRedirect(req.RedirectURI, s.deps.ExternalURL):
		return 0, server.InvalidRequest("redirect_uri is not allowed: it must be http on localhost or 127.0.0.1, or on the hub's external_url")
	}
	if req.Expires == nil {
		return defaultKeyDays, nil
	}
	if !slices.Contains(keyLifetimes, *req.Expires) {
		return 0, server.InvalidRequest("expires must be one of 0, 30, 60 and 90")
	}
	return *req.Expires, nil
}

// signIn finds the user of the identity id and refreshes it, or creates the
// user with the personal org at the first login, revokes the user's live
// API keys and issues one that lasts days days, all in one transaction: a
// login that fails leaves nothing behind, and one that succeeds leaves the
// user exactly one live key.
func (s *Service) signIn(ctx context.Context, id Identity, days int) (u User, key string, rec APIKey, err error) {
	now := s.deps.Now().UTC()
	err = s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		u, err = UserByProvider(ctx, tx, id.Provider, id.ProviderID)
		switch {
		case errors.Is(err, ErrNotFound):
			u, err = createFirstLogin(ctx, tx, id, now)
		case err == nil:
			u, err = refreshLogin(ctx, tx, u, id, now)
		}
		if err != nil {
			return err
		}
		if err := revokeLiveAPIKeys(ctx, tx, u.ID, now); err != nil {
			return err
		}
		key, rec, err = IssueAPIKey(ctx, tx, u.ID, days, now)
		return err
	})
	return u, key, rec, err
}

// refreshLogin refreshes the user u of a later login with the e-mail and the
// username the provider now gives, id, and sets updated_at to now. It keeps
// the old username when the new one is another user's, and refuses a
// blocked user and an e-mail that is another user's. The id, role,
// full_name, created_at and the personal org stay as they are: the user
// changes full_name through the API, and the org keeps its name.
func refreshLogin(ctx context.Context, tx *sql.Tx, u User, id Identity, now time.Time) (User, error) {
	if u.Status == StatusBlocked {
		return User{}, server.Forbidden("user is blocked")
	}
	if id.Email != u.Email {
		taken, err := exists(ctx, tx, `SELECT count(*) FROM users WHERE email = ? AND id <> ?`, id.Email, u.ID)
		if err != nil {
			return User{}, err
		}
		if taken {
			return User{}, conflictError{"the e-mail belongs to another user"}
		}
		u.Email = id.Email
	}
	if id.Username != u.Username {
		taken, err := exists(ctx, tx, `SELECT count(*) FROM users WHERE username = ? AND id <> ?`, id.Username, u.ID)
		if err != nil {
			return User{}, err
		}
		if !taken {
			u.Username = id.Username
		}
	}
	u.UpdatedAt = now
	if err := updateUserLogin(ctx, tx, u); err != nil {
		return User{}, err
	}
	return u, nil
}

// createFirstLogin creates the user of a first login with the personal
// org. The user gets the provider's username, or the first free of
// <name>-2 to <name>-10, and the role newUserRole picks.
func createFirstLogin(ctx context.Context, tx *sql.Tx, id Identity, now time.Time) (User, error) {
	username, err := freeUsername(ctx, tx, id.Username)
	if err != nil {
		return User{}, err
	}
	role, err := newUserRole(ctx, tx, id.Email)
	if err != nil {
		return User{}, err
	}
	u, _, err := CreateUserAndPersonalOrg(ctx, tx, NewUser{
		Username: username, Email: id.Email, FullName: id.FullName, Provider: id.Provider, ProviderID: id.ProviderID,
		CreatedAt: now,
	}, role)
	return u, err
}

// freeUsername returns name, or the first of <name>-2 to <name>-10 that no
// user has.
func freeUsername(ctx context.Context, tx *sql.Tx, name string) (string, error) {
	for i := 1; i <= usernameSuffixes; i++ {
		candidate := name
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", name, i)
		}
		taken, err := exists(ctx, tx, `SELECT count(*) FROM users WHERE username = ?`, candidate)
		if err != nil || !taken {
			return candidate, err
		}
	}
	return "", conflictError{"no free username is left"}
}

// newUserRole is the role of a new user with the e-mail email: admin when it
// is the bootstrap admin_email, in any letter case, and no user with the
// role admin exists in any status; user otherwise. It reads admin_email in
// tx, so that the rule and the user it creates see the same store.
func newUserRole(ctx context.Context, tx *sql.Tx, email string) (string, error) {
	var adminEmail string
	err := tx.QueryRowContext(ctx, `SELECT email FROM admin_settings WHERE id = 1`).Scan(&adminEmail)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return RoleUser, nil
	case err != nil:
		return "", fmt.Errorf("read the admin e-mail: %w", err)
	case adminEmail == "" || !strings.EqualFold(adminEmail, email):
		return RoleUser, nil
	}
	adminExists, err := exists(ctx, tx, `SELECT count(*) FROM users WHERE role = ?`, RoleAdmin)
	if err != nil || adminExists {
		return RoleUser, err
	}
	return RoleAdmin, nil
}
