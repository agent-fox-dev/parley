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

// apiPrefix is the path every route of the hub's REST API lies under.
const apiPrefix = "/api/v1"

// defaultRequestTimeout bounds one request to the hub, from sending it to
// reading the whole answer.
const defaultRequestTimeout = 30 * time.Second

// client sends a command's requests to the hub. Every command that calls
// the REST API sends through it, so every request reaches the hub alike.
type client struct {
	// hubURL is the hub's URL without trailing slashes; a path it carries
	// comes before /api/v1.
	hubURL string
	// credential is sent as Authorization: Bearer; an empty one sends no
	// Authorization header.
	credential string
	http       *http.Client
}

// newClient is the client of the hub at hubURL, presenting credential.
func newClient(hubURL, credential string) *client {
	return &client{
		hubURL:     strings.TrimRight(hubURL, "/"),
		credential: credential,
		http:       &http.Client{Timeout: defaultRequestTimeout},
	}
}

// response is the hub's answer to one request: its status and its whole
// body.
type response struct {
	status int
	body   []byte
}

// request sends method to <hub URL>/api/v1<path>, with query as the query
// string when it is not empty and body encoded as JSON when it is not nil,
// and returns the hub's answer, whatever its status.
func (c *client) request(ctx context.Context, method, path string, query url.Values, body any) (*response, error) {
	target := c.hubURL + apiPrefix + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, clientError("cannot encode the request body: %v", err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return nil, clientError("cannot build the request to %s: %v", target, err)
	}
	if c.credential != "" {
		req.Header.Set("Authorization", "Bearer "+c.credential)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, clientError("cannot reach the hub at %s: %v", c.hubURL, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, clientError("cannot read the hub's answer: %v", err)
	}
	return &response{status: resp.StatusCode, body: answer}, nil
}
