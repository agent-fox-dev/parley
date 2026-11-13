package cli

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

// apiPrefix is the path every route of the hub's REST API lies under.
const apiPrefix = "/api/v1"

// defaultRequestTimeout bounds one request to the hub, from connecting to
// reading the whole answer, unless the command or the request states
// another. Runs take it from options.requestTimeout, which tests shorten.
const defaultRequestTimeout = 30 * time.Second

// client sends a command's requests to the hub. Every command that calls
// the hub sends through it, so every request reaches the hub alike: once,
// within its timeout, and with every failure a client_error.
type client struct {
	// hubURL is the hub's URL without trailing slashes; a path it carries
	// comes before /api/v1.
	hubURL string
	// credential is sent as Authorization: Bearer; an empty one sends no
	// Authorization header.
	credential string
	// timeout bounds each request that states none of its own.
	timeout time.Duration
	http    *http.Client
}

// newClient is the client of the hub at hubURL, presenting credential, with
// timeout for each request that states none.
func newClient(hubURL, credential string, timeout time.Duration) *client {
	return &client{
		hubURL:     strings.TrimRight(hubURL, "/"),
		credential: credential,
		timeout:    timeout,
		// The timeout is a context around the whole exchange instead of
		// the client's, so that a request can state its own.
		http: &http.Client{Transport: onceTransport(), CheckRedirect: keepRedirect},
	}
}

// keepRedirect has the client return a redirect as the answer instead of
// following it: following one sends the request a second time, a POST with
// its body again after a 307 or 308, and a redirect is no answer of the
// hub's REST API.
func keepRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// onceTransport is the default transport with connections that serve one
// request each. Go's transport may send a request again on a new
// connection when a reused one fails, and the hub may have received it the
// first time; a connection that is never reused is never retried, so every
// request is sent once.
func onceTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	return t
}

// apiRequest is one request to the hub.
type apiRequest struct {
	method string
	// path is relative to /api/v1, or to the hub URL itself with
	// outsideAPI, as GET /version is. Its segments are encoded already
	// (routePath).
	path string
	// outsideAPI sends path below the hub URL instead of below /api/v1.
	outsideAPI bool
	// query is sent as the query string when it is not empty.
	query url.Values
	// body is sent encoded as JSON when it is not nil.
	body any
	// timeout, when not zero, replaces the client's for this request.
	timeout time.Duration
}

// response is the hub's answer to one request: its status and its whole
// body.
type response struct {
	status int
	body   []byte
}

// request sends method to <hub URL>/api/v1<path>, with query and body as
// send takes them, and returns the hub's answer as send does.
func (c *client) request(ctx context.Context, method, path string, query url.Values, body any) (*response, error) {
	return c.send(ctx, apiRequest{method: method, path: path, query: query, body: body})
}

// send sends r and returns a success of the REST API: a 2xx status whose
// body is empty or JSON. An error envelope the hub answered is a hubError.
// Any other answer is a client_error naming the status, because it did not
// come from the hub's REST API (a proxy, a wrong URL), and so is every
// failure raw reports.
func (c *client) send(ctx context.Context, r apiRequest) (*response, error) {
	resp, err := c.raw(ctx, r)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.status >= 400:
		if isHubEnvelope(resp.body) {
			return nil, &hubError{status: resp.status, body: resp.body}
		}
		return nil, clientError("the hub answered %d without an error envelope", resp.status)
	case resp.status < 200 || resp.status >= 300:
		return nil, clientError("the hub answered %d, which is neither a success nor an error", resp.status)
	case len(bytes.TrimSpace(resp.body)) > 0 && !json.Valid(resp.body):
		return nil, clientError("the hub answered %d with a body that is not JSON", resp.status)
	}
	return resp, nil
}

// raw sends r once and returns the hub's answer whatever its status and
// body hold, for a command whose output is not one JSON document. Failing
// to connect (refused, a host name that does not resolve, a TLS handshake)
// is a client_error naming the hub URL, and so is an answer not read whole
// within the timeout.
func (c *client) raw(ctx context.Context, r apiRequest) (*response, error) {
	target := c.hubURL + r.path
	if !r.outsideAPI {
		target = c.hubURL + apiPrefix + r.path
	}
	if len(r.query) > 0 {
		target += "?" + r.query.Encode()
	}
	var payload io.Reader
	if r.body != nil {
		encoded, err := json.Marshal(r.body)
		if err != nil {
			return nil, clientError("cannot encode the request body: %v", err)
		}
		payload = bytes.NewReader(encoded)
	}

	timeout := c.timeout
	if r.timeout > 0 {
		timeout = r.timeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, r.method, target, payload)
	if err != nil {
		return nil, clientError("cannot build a request to the hub at %s: %v", c.hubURL, err)
	}
	if c.credential != "" {
		req.Header.Set("Authorization", "Bearer "+c.credential)
	}
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.failure(ctx, reqCtx, timeout, "cannot reach", err)
	}
	defer resp.Body.Close()
	// The timeout runs on while the body is read, so a hub that stalls in
	// the middle of its answer is abandoned too.
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, c.failure(ctx, reqCtx, timeout, "cannot read the answer of", err)
	}
	return &response{status: resp.StatusCode, body: answer}, nil
}

// failure is the client_error of a request to the hub that failed with err:
// a timeout when reqCtx ran out while ctx, the caller's, did not; else what
// failed ("cannot reach") and the reason, without the method and URL
// net/http puts before it.
func (c *client) failure(ctx, reqCtx context.Context, timeout time.Duration, what string, err error) error {
	if ctx.Err() == nil && errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
		return clientError("the hub at %s gave no complete answer within %v", c.hubURL, timeout)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return clientError("%s the hub at %s: %v", what, c.hubURL, err)
}
