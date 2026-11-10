package identity

import (
	"database/sql"
	"encoding/json"
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
	// ExternalURL is [server].external_url; a redirect_uri on its scheme,
	// host and port is allowed.
	ExternalURL string
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

// callbackBody is the validated body of POST /auth/callback.
type callbackBody struct {
	Provider    string
	Code        string
	RedirectURI string
	Days        int
}

// validExpires are the lifetimes, in days, a login may ask for.
var validExpires = map[int]bool{0: true, 30: true, 60: true, 90: true}

// parseCallback validates the body before the provider is called:
// provider, code and redirect_uri are required strings, code is not empty,
// provider is configured, expires is absent, null or one of 0, 30, 60 and
// 90, and redirect_uri is allowed.
func (rs *Routes) parseCallback(r *http.Request) (callbackBody, error) {
	var raw map[string]json.RawMessage
	if err := server.DecodeJSON(r, &raw); err != nil {
		return callbackBody{}, err
	}
	str := func(name string) (string, error) {
		v, ok := raw[name]
		var s string
		if !ok || json.Unmarshal(v, &s) != nil {
			return "", server.InvalidRequest(name + " is required and must be a string")
		}
		return s, nil
	}
	var b callbackBody
	var err error
	if b.Provider, err = str("provider"); err != nil {
		return b, err
	}
	if b.Code, err = str("code"); err != nil {
		return b, err
	}
	if b.RedirectURI, err = str("redirect_uri"); err != nil {
		return b, err
	}
	if b.Code == "" {
		return b, server.InvalidRequest("code must not be empty")
	}
	if !rs.configured(b.Provider) {
		return b, server.InvalidRequest("provider is not configured")
	}
	b.Days = DefaultKeyDays
	if v, ok := raw["expires"]; ok && string(v) != "null" {
		var n int
		if json.Unmarshal(v, &n) != nil || !validExpires[n] {
			return b, server.InvalidRequest("expires must be 0, 30, 60 or 90")
		}
		b.Days = n
	}
	if !allowedRedirect(b.RedirectURI, rs.ExternalURL) {
		return b, server.InvalidRequest("redirect_uri is not allowed")
	}
	return b, nil
}

func (rs *Routes) configured(name string) bool {
	for _, p := range rs.Providers {
		if p.Name == name {
			return true
		}
	}
	return false
}

// providerFailure maps an identity read's error to the callback's answer
// with a fixed message.
func providerFailure(err error) error {
	switch KindOf(err) {
	case KindRejected:
		return server.Unauthenticated("the provider rejected the code")
	case KindUpstream:
		return server.UpstreamError("the provider could not be read")
	}
	return err
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
	body, err := rs.parseCallback(r)
	if err != nil {
		return err
	}
	id, err := rs.Reader.Identity(r.Context(), body.Provider, body.Code, body.RedirectURI)
	if err != nil {
		return providerFailure(err)
	}
	var out signedIn
	err = rs.Store.Tx(r.Context(), func(tx *sql.Tx) error {
		var err error
		out, err = signIn(r.Context(), tx, id, body.Days, rs.Now())
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
