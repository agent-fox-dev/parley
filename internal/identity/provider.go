package identity

import (
	"net/url"

	"github.com/agent-fox-dev/parley/internal/config"
)

// oauthProvider is what the hub needs to know about an identity provider
// beyond its configuration. The endpoints are not here: internal/config
// resolves them, from the configuration file or the provider's public ones.
type oauthProvider struct {
	// scope is the scopes the hub asks for, space-separated.
	scope string
	// authorizeParams are query parameters the provider's authorize endpoint
	// needs besides client_id and scope.
	authorizeParams url.Values
}

// oauthProviders are the providers the hub can log in with, by name.
var oauthProviders = map[string]oauthProvider{
	"github": {scope: "user:email"},
	// Google requires response_type on its authorize endpoint.
	"google": {scope: "openid email profile", authorizeParams: url.Values{"response_type": {"code"}}},
}

// authorizeURL is where a client sends the browser to sign in with p: the
// provider's authorize endpoint with the hub's client id and scopes. The
// client secret never goes into it.
func authorizeURL(p config.Provider) (string, error) {
	u, err := url.Parse(p.AuthorizeURL)
	if err != nil {
		return "", err
	}
	known := oauthProviders[p.Name]
	q := u.Query()
	q.Set("client_id", p.ClientID)
	q.Set("scope", known.scope)
	for k, vs := range known.authorizeParams {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
