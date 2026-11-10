package identity

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/store"
)

// Routes are this package's public routes: the provider list and the login
// callback.
type Routes struct {
	Providers []Provider
	Store     *store.Store
	// Reader reads the identity behind an OAuth code (the OAuthClient in
	// production).
	Reader IdentityReader
	// Now is the clock of the sign-in.
	Now func() time.Time
}

// Register mounts the routes on the server.
func (rs *Routes) Register(srv *server.Server) {
	srv.Register(server.Route{Method: "GET", Pattern: "/auth/providers", Public: true,
		CacheControl: "public, max-age=300", Handler: rs.listProviders})
	// Public and not a create route: Idempotency-Key is ignored.
	srv.Register(server.Route{Method: "POST", Pattern: "/auth/callback", Public: true, Handler: rs.callback})
}

// DefaultKeyDays is the lifetime of a key when the callback names none.
const DefaultKeyDays = 90

// callbackBody is the body of POST /auth/callback.
type callbackBody struct {
	Provider    string `json:"provider"`
	Code        string `json:"code"`
	RedirectURI string `json:"redirect_uri"`
	Expires     *int   `json:"expires"`
}

// issuedKey is the key a login answers, shown once.
type issuedKey struct {
	Key       string        `json:"key"`
	KeyID     string        `json:"key_id"`
	ExpiresAt *timefmt.Time `json:"expires_at"`
}

// callbackAnswer is the answer of a login.
type callbackAnswer struct {
	User   User      `json:"user"`
	APIKey issuedKey `json:"api_key"`
}

func (rs *Routes) callback(w http.ResponseWriter, r *http.Request) error {
	var body callbackBody
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	days := DefaultKeyDays
	if body.Expires != nil {
		days = *body.Expires
	}
	id, err := rs.Reader.Identity(r.Context(), body.Provider, body.Code, body.RedirectURI)
	if err != nil {
		return err
	}
	var out signedIn
	err = rs.Store.Tx(r.Context(), func(tx *sql.Tx) error {
		var err error
		out, err = signIn(r.Context(), tx, id, days, rs.Now())
		return err
	})
	if errors.Is(err, errBlocked) {
		return server.Forbidden("user is blocked")
	}
	if errors.Is(err, ErrConflict) {
		return server.Conflict("the login conflicts with an existing user or organization")
	}
	if err != nil {
		return err
	}
	answer := callbackAnswer{User: out.user, APIKey: issuedKey{Key: out.key, KeyID: out.keyID}}
	if out.expiresAt != nil {
		at := timefmt.Time(*out.expiresAt)
		answer.APIKey.ExpiresAt = &at
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// providerItem is a provider as GET /auth/providers lists it; it never
// carries the client secret.
type providerItem struct {
	Name         string `json:"name"`
	AuthorizeURL string `json:"authorize_url"`
}

func (rs *Routes) listProviders(w http.ResponseWriter, _ *http.Request) error {
	items := make([]providerItem, 0, len(rs.Providers))
	for _, p := range rs.Providers {
		items = append(items, providerItem{Name: p.Name, AuthorizeURL: p.AuthorizeURL()})
	}
	server.WritePage(w, server.Page{Limit: len(items)}, items, func(it providerItem) string { return it.Name })
	return nil
}
