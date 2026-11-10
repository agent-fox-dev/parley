package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultProviderTimeout bounds each call to a provider.
const DefaultProviderTimeout = 15 * time.Second

// maxProviderBody bounds what is read of a provider's answer.
const maxProviderBody = 1 << 20

// Identity is what a provider vouches for about a user.
type Identity struct {
	Provider   string
	ProviderID string
	Username   string
	FullName   string
	Email      string
}

// ErrorKind tells a code the provider rejected from a provider that failed.
type ErrorKind int

// The kinds of a provider error.
const (
	KindNone ErrorKind = iota
	// KindRejected: the provider rejected the code (401 at the callback).
	KindRejected
	// KindUpstream: the provider failed, timed out or gave no usable
	// identity (502 at the callback).
	KindUpstream
)

// ProviderError is an error of the identity read. Its text is a fixed
// phrase: never the code, a client secret, an access token or a provider's
// body.
type ProviderError struct {
	Kind ErrorKind
	text string
}

func (e *ProviderError) Error() string { return e.text }

func rejected(text string) error { return &ProviderError{Kind: KindRejected, text: text} }
func upstream(text string) error { return &ProviderError{Kind: KindUpstream, text: text} }

// KindOf returns the kind of a provider error; KindNone for any other error.
func KindOf(err error) ErrorKind {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return KindNone
}

// IdentityReader reads a user's identity from a provider with an OAuth code;
// the login callback depends on this interface so tests can set what it
// returns.
type IdentityReader interface {
	Identity(ctx context.Context, provider, code, redirectURI string) (Identity, error)
}

// OAuthClient reads identities from the configured providers with the
// standard library alone.
type OAuthClient struct {
	http      *http.Client
	timeout   time.Duration
	providers map[string]Provider
}

// NewOAuthClient builds a client over the configured providers; timeout
// bounds each call (DefaultProviderTimeout in production).
func NewOAuthClient(providers []Provider, httpClient *http.Client, timeout time.Duration) *OAuthClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	c := &OAuthClient{http: httpClient, timeout: timeout, providers: map[string]Provider{}}
	for _, p := range providers {
		c.providers[p.Name] = p
	}
	return c
}

// Identity exchanges the code at the provider's token URL and reads the
// user's identity with the access token it gets.
func (c *OAuthClient) Identity(ctx context.Context, provider, code, redirectURI string) (Identity, error) {
	p, ok := c.providers[provider]
	if !ok {
		return Identity{}, upstream("the provider is not configured")
	}
	token, err := c.exchange(ctx, p, code, redirectURI)
	if err != nil {
		return Identity{}, err
	}
	var id Identity
	switch provider {
	case ProviderGitHub:
		id, err = c.githubIdentity(ctx, p, token)
	case ProviderGoogle:
		id, err = c.googleIdentity(ctx, p, token)
	default:
		err = upstream("the provider is not supported")
	}
	if err != nil {
		return Identity{}, err
	}
	id.Provider = provider
	id.Username = strings.TrimSpace(id.Username)
	if id.Username == "" {
		id.Username = provider + "-" + id.ProviderID
	}
	return id, nil
}

// call sends one request bounded by the timeout and returns the status and
// at most maxProviderBody bytes of the body.
func (c *OAuthClient) call(ctx context.Context, req *http.Request) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.http.Do(req.WithContext(ctx))
	if err != nil {
		return 0, nil, upstream("the provider could not be reached")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderBody))
	if err != nil {
		return 0, nil, upstream("the provider's answer could not be read")
	}
	return resp.StatusCode, body, nil
}

// jsonObject decodes a body that must be one JSON object.
func jsonObject(body []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// exchange trades the code for an access token: one form-encoded POST with
// Accept: application/json.
func (c *OAuthClient) exchange(ctx context.Context, p Provider, code, redirectURI string) (string, error) {
	form := url.Values{
		"client_id":     {p.ClientID},
		"client_secret": {p.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequest(http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", upstream("the provider's token URL is not usable")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	status, body, err := c.call(ctx, req)
	if err != nil {
		return "", err
	}
	if status >= 500 {
		return "", upstream("the provider failed to exchange the code")
	}
	obj, ok := jsonObject(body)
	if !ok {
		return "", upstream("the provider's token answer is not a JSON object")
	}
	if _, has := obj["error"]; has {
		return "", rejected("the provider rejected the code")
	}
	token, _ := obj["access_token"].(string)
	if token == "" {
		return "", upstream("the provider gave no access token")
	}
	return token, nil
}

// getJSON reads a URL with the access token and decodes the answer into v.
func (c *OAuthClient) getJSON(ctx context.Context, rawURL, token string, v any) error {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return upstream("the provider's user URL is not usable")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	status, body, err := c.call(ctx, req)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return upstream("the provider failed to read the user")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return upstream("the provider's user answer is not readable")
	}
	return nil
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	return ""
}

// githubIdentity reads id, login, name and email; when the profile hides
// the address, the primary verified entry of the e-mail list.
func (c *OAuthClient) githubIdentity(ctx context.Context, p Provider, token string) (Identity, error) {
	var user map[string]any
	if err := c.getJSON(ctx, p.UserURL, token, &user); err != nil {
		return Identity{}, err
	}
	if user == nil {
		return Identity{}, upstream("the provider's user answer is not a JSON object")
	}
	id := Identity{ProviderID: str(user["id"]), Username: str(user["login"]), FullName: str(user["name"]), Email: str(user["email"])}
	if id.ProviderID == "" {
		return Identity{}, upstream("the provider gave no user id")
	}
	if id.Email == "" {
		var emails []struct {
			Email    string `json:"email"`
			Primary  bool   `json:"primary"`
			Verified bool   `json:"verified"`
		}
		if err := c.getJSON(ctx, strings.TrimRight(p.UserURL, "/")+"/emails", token, &emails); err != nil {
			return Identity{}, err
		}
		for _, e := range emails {
			if e.Primary && e.Verified {
				id.Email = e.Email
			}
		}
		if id.Email == "" {
			return Identity{}, upstream("the provider gave no verified e-mail")
		}
	}
	return id, nil
}

// googleIdentity reads sub, name and email, the last only when
// email_verified is true.
func (c *OAuthClient) googleIdentity(ctx context.Context, p Provider, token string) (Identity, error) {
	var user map[string]any
	if err := c.getJSON(ctx, p.UserURL, token, &user); err != nil {
		return Identity{}, err
	}
	if user == nil {
		return Identity{}, upstream("the provider's user answer is not a JSON object")
	}
	id := Identity{ProviderID: str(user["sub"]), Username: str(user["name"]), FullName: str(user["name"])}
	if id.ProviderID == "" {
		return Identity{}, upstream("the provider gave no user id")
	}
	if verified, _ := user["email_verified"].(bool); verified {
		id.Email = str(user["email"])
	}
	if id.Email == "" {
		return Identity{}, upstream("the provider gave no verified e-mail")
	}
	return id, nil
}
