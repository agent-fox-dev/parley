package identity

import (
	"net/url"
	"sort"

	"github.com/agent-fox-dev/parley/internal/config"
)

// Supported providers.
const (
	ProviderGitHub = "github"
	ProviderGoogle = "google"
)

// providerDefaults are the public endpoints and the scopes the hub needs of
// each supported provider.
var providerDefaults = map[string]struct {
	authorize, token, user, scope string
}{
	ProviderGitHub: {"https://github.com/login/oauth/authorize", "https://github.com/login/oauth/access_token", "https://api.github.com/user", "user:email"},
	ProviderGoogle: {"https://accounts.google.com/o/oauth2/v2/auth", "https://oauth2.googleapis.com/token", "https://openidconnect.googleapis.com/v1/userinfo", "openid email profile"},
}

// Provider is a configured identity provider with its endpoints resolved.
type Provider struct {
	Name              string
	ClientID          string
	ClientSecret      string
	AuthorizeEndpoint string
	TokenURL          string
	UserURL           string
}

// ProvidersFrom resolves the configured [[oauth.providers]] entries against
// the supported providers' public endpoints, taking an endpoint from the
// entry when it sets one, ordered by name.
func ProvidersFrom(cfg []config.Provider) []Provider {
	out := make([]Provider, 0, len(cfg))
	for _, c := range cfg {
		d := providerDefaults[c.Name]
		p := Provider{Name: c.Name, ClientID: c.ClientID, ClientSecret: c.ClientSecret,
			AuthorizeEndpoint: d.authorize, TokenURL: d.token, UserURL: d.user}
		if c.AuthorizeURL != "" {
			p.AuthorizeEndpoint = c.AuthorizeURL
		}
		if c.TokenURL != "" {
			p.TokenURL = c.TokenURL
		}
		if c.UserinfoURL != "" {
			p.UserURL = c.UserinfoURL
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AuthorizeURL is the provider's authorize endpoint with the client id and
// the scopes the hub needs (and, for Google, response_type=code); the caller
// adds redirect_uri and state.
func (p Provider) AuthorizeURL() string {
	u, err := url.Parse(p.AuthorizeEndpoint)
	if err != nil {
		return p.AuthorizeEndpoint
	}
	q := u.Query()
	q.Set("client_id", p.ClientID)
	q.Set("scope", providerDefaults[p.Name].scope)
	if p.Name == ProviderGoogle {
		q.Set("response_type", "code")
	}
	u.RawQuery = q.Encode()
	return u.String()
}
