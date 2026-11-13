package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
)

// The two ways an identity read fails. Every error of an OAuthClient is one
// of them, and its text is built from fixed words only: it never holds the
// code, a client secret, an access token or a provider's answer.
var (
	// ErrRejected is a code the provider refused: its token endpoint
	// answered an error below status 500.
	ErrRejected = errors.New("the identity provider rejected the code")
	// ErrUpstream is a provider that failed: it could not be reached, did
	// not answer in time or answered something the hub cannot use.
	ErrUpstream = errors.New("the identity provider failed")
)

// DefaultProviderTimeout bounds each call to a provider when NewOAuthClient
// is given no timeout.
const DefaultProviderTimeout = 15 * time.Second

// maxProviderAnswer is how much of a provider's answer is read; the answers
// the hub uses are a few hundred bytes.
const maxProviderAnswer = 1 << 20

// Identity is a user as an identity provider vouches for them.
type Identity struct {
	// Provider is the provider's name, github or google.
	Provider string
	// ProviderID is the user's stable id at the provider.
	ProviderID string
	// Username is the provider's login or display name, trimmed, or
	// <provider>-<provider id> when that is empty.
	Username string
	// FullName is the provider's display name as given; it may be empty.
	FullName string
	// Email is an address the provider has verified.
	Email string
}

// ProviderClient reads the identity behind an OAuth code from a provider.
type ProviderClient interface {
	// Identity exchanges code, issued for redirectURI, at the provider
	// named provider and reads the user's identity with the access token.
	// Its errors are ErrRejected or ErrUpstream.
	Identity(ctx context.Context, provider, code, redirectURI string) (Identity, error)
}

// OAuthClient is the ProviderClient that talks to the configured providers
// over HTTP.
type OAuthClient struct {
	providers []config.Provider
	hc        *http.Client
	timeout   time.Duration
}

// NewOAuthClient returns a client for providers, whose endpoints are
// resolved, sending its requests with hc (http.DefaultClient when nil). Each
// call to a provider is abandoned, and its connection closed, after timeout;
// 0 is DefaultProviderTimeout.
func NewOAuthClient(providers []config.Provider, hc *http.Client, timeout time.Duration) *OAuthClient {
	if hc == nil {
		hc = http.DefaultClient
	}
	if timeout <= 0 {
		timeout = DefaultProviderTimeout
	}
	return &OAuthClient{providers: slices.Clone(providers), hc: hc, timeout: timeout}
}

// Identity implements ProviderClient.
func (c *OAuthClient) Identity(ctx context.Context, provider, code, redirectURI string) (Identity, error) {
	i := slices.IndexFunc(c.providers, func(p config.Provider) bool { return p.Name == provider })
	if i < 0 {
		return Identity{}, fmt.Errorf("%w: the provider is not configured", ErrUpstream)
	}
	p := c.providers[i]
	token, err := c.exchange(ctx, p, code, redirectURI)
	if err != nil {
		return Identity{}, err
	}
	var id Identity
	switch p.Name {
	case "github":
		id, err = c.github(ctx, p, token)
	case "google":
		id, err = c.google(ctx, p, token)
	default:
		err = fmt.Errorf("%w: the hub cannot log in with the provider", ErrUpstream)
	}
	if err != nil {
		return Identity{}, err
	}
	id.Provider = p.Name
	id.Username = strings.TrimSpace(id.Username)
	if id.Username == "" {
		id.Username = p.Name + "-" + id.ProviderID
	}
	return id, nil
}

// exchange trades code for an access token at p's token endpoint.
func (c *OAuthClient) exchange(ctx context.Context, p config.Provider, code, redirectURI string) (string, error) {
	form := url.Values{
		"client_id":     {p.ClientID},
		"client_secret": {p.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: the token endpoint is not a valid URL", ErrUpstream)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	status, body, err := c.call(req, "token endpoint")
	if err != nil {
		return "", err
	}
	if status >= http.StatusInternalServerError {
		return "", fmt.Errorf("%w: the token endpoint answered status %d", ErrUpstream, status)
	}
	var answer map[string]json.RawMessage
	if json.Unmarshal(body, &answer) != nil || answer == nil {
		return "", fmt.Errorf("%w: the token endpoint did not answer a JSON object", ErrUpstream)
	}
	if _, refused := answer["error"]; refused {
		return "", fmt.Errorf("%w: the token endpoint answered an error", ErrRejected)
	}
	var token string
	if status/100 != 2 || json.Unmarshal(answer["access_token"], &token) != nil || token == "" {
		return "", fmt.Errorf("%w: the token endpoint answered no access token", ErrUpstream)
	}
	return token, nil
}

// github reads the identity from GitHub's user endpoint, and the address
// from its e-mail list when the profile does not show one.
func (c *OAuthClient) github(ctx context.Context, p config.Provider, token string) (Identity, error) {
	var user struct {
		ID    json.Number `json:"id"`
		Login string      `json:"login"`
		Name  string      `json:"name"`
		Email string      `json:"email"`
	}
	if err := c.getJSON(ctx, "user endpoint", p.UserinfoURL, token, &user); err != nil {
		return Identity{}, err
	}
	n, err := strconv.ParseInt(user.ID.String(), 10, 64)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: the user endpoint answered no user id", ErrUpstream)
	}
	id := Identity{ProviderID: strconv.FormatInt(n, 10), Username: user.Login, FullName: user.Name, Email: user.Email}
	if id.Email != "" {
		return id, nil
	}
	emailsURL, err := url.JoinPath(p.UserinfoURL, "emails")
	if err != nil {
		return Identity{}, fmt.Errorf("%w: the user endpoint is not a valid URL", ErrUpstream)
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := c.getJSON(ctx, "e-mail list endpoint", emailsURL, token, &emails); err != nil {
		return Identity{}, err
	}
	for _, e := range emails {
		if e.Primary && e.Verified && e.Email != "" {
			id.Email = e.Email
			return id, nil
		}
	}
	return Identity{}, fmt.Errorf("%w: the provider vouches for no e-mail address", ErrUpstream)
}

// google reads the identity from Google's userinfo endpoint; the address
// counts only when Google has verified it.
func (c *OAuthClient) google(ctx context.Context, p config.Provider, token string) (Identity, error) {
	var user struct {
		Sub           string `json:"sub"`
		Name          string `json:"name"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := c.getJSON(ctx, "user endpoint", p.UserinfoURL, token, &user); err != nil {
		return Identity{}, err
	}
	if user.Sub == "" {
		return Identity{}, fmt.Errorf("%w: the user endpoint answered no user id", ErrUpstream)
	}
	if !user.EmailVerified || user.Email == "" {
		return Identity{}, fmt.Errorf("%w: the provider vouches for no e-mail address", ErrUpstream)
	}
	return Identity{ProviderID: user.Sub, Username: user.Name, FullName: user.Name, Email: user.Email}, nil
}

// getJSON reads target with the access token and decodes its answer into
// v. Anything but a 200 whose body is JSON of v's shape is a failure of the
// provider; what names the endpoint in the error.
func (c *OAuthClient) getJSON(ctx context.Context, what, target, token string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("%w: the %s is not a valid URL", ErrUpstream, what)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	status, body, err := c.call(req, what)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%w: the %s answered status %d", ErrUpstream, what, status)
	}
	if json.Unmarshal(body, v) != nil {
		return fmt.Errorf("%w: the %s did not answer the expected JSON", ErrUpstream, what)
	}
	return nil
}

// call sends req and reads its answer, both within the client's timeout:
// when it passes, the request's context ends, which makes the transport
// close the connection. The underlying error is dropped because its text
// can hold the provider's URL or answer.
func (c *OAuthClient) call(req *http.Request, what string) (status int, body []byte, err error) {
	ctx, cancel := context.WithTimeout(req.Context(), c.timeout)
	defer cancel()
	resp, err := c.hc.Do(req.WithContext(ctx))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: the %s did not answer", ErrUpstream, what)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxProviderAnswer))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: the %s answer could not be read", ErrUpstream, what)
	}
	return resp.StatusCode, body, nil
}
