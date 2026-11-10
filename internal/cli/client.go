package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// requestTimeout bounds every request unless a command states another;
// tests shorten it.
var requestTimeout = 30 * time.Second

// Client sends a command's requests to <hub URL>/api/v1<path> with the
// credential as Authorization: Bearer. Every command sends its requests
// through it.
type Client struct {
	base string
	cred string
	http *http.Client
	host string
}

// Response is a hub's answer: its status and its body as received.
type Response struct {
	Status int
	Body   []byte
}

// newClient builds a client for a hub URL, which must be an absolute http or
// https URL with a host; every trailing slash is removed and a path it
// carries is kept.
func newClient(hubURL, credential string) (*Client, error) {
	u, err := url.Parse(hubURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, ClientError("the hub URL %q is not an absolute http or https URL", hubURL)
	}
	return &Client{base: strings.TrimRight(hubURL, "/"), cred: credential, http: &http.Client{}, host: u.Host}, nil
}

// Send sends one request: method, a path relative to /api/v1, an optional
// query and an optional body sent as JSON with Content-Type:
// application/json. It returns the status and body as received.
func (c *Client) Send(ctx context.Context, method, path string, query url.Values, body any) (*Response, error) {
	target := c.base + "/api/v1" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, ClientError("cannot encode the request body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, ClientError("cannot build the request to %s: %v", c.host, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ClientError("cannot reach the hub at %s: %v", c.host, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ClientError("cannot read the hub's answer from %s: %v", c.host, err)
	}
	return &Response{Status: resp.StatusCode, Body: data}, nil
}
