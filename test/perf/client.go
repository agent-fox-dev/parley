package perf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// apiMount is the path the hub serves its REST API below.
const apiMount = "/api/v1"

// redacted stands for the token wherever text the harness writes would
// otherwise hold it.
const redacted = "[redacted]"

// detailMax is the most bytes of a result's detail, and of any text taken
// from a hub's response.
const detailMax = 200

// client is the harness's HTTP client of one hub. Every request carries
// the bearer token; nothing it returns holds the token.
type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(cfg Config) *client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// The concurrent scenarios hold many requests open at once; reusing
	// their connections keeps connection set-up out of what is measured.
	tr.MaxIdleConnsPerHost = 256
	return &client{base: strings.TrimRight(cfg.URL, "/"), token: cfg.Token, http: &http.Client{Transport: tr}}
}

// response is a hub's answer, read to its end, and how long it took from
// sending the request to reading the last byte of the body.
type response struct {
	status   int
	body     []byte
	duration time.Duration
}

// requestError is a request that was answered with a status of 400 or
// more (status and errorType from the error envelope, when there is one)
// or that failed in transport (status 0, err the cause).
type requestError struct {
	method, path string
	status       int
	errorType    string
	message      string
	err          error
}

func (e *requestError) Error() string {
	if e.status == 0 {
		return fmt.Sprintf("%s %s: transport error: %s", e.method, e.path, e.message)
	}
	s := fmt.Sprintf("%s %s: %d", e.method, e.path, e.status)
	if e.errorType != "" {
		s += " " + e.errorType
	}
	if e.message != "" {
		s += ": " + e.message
	}
	return s
}

func (e *requestError) Unwrap() error { return e.err }

// do sends method to path below the REST API's mount (path may hold a
// query string) with body encoded as JSON unless it is nil, and reads the
// whole answer. An answer of 400 or more is returned with a *requestError.
func (c *client) do(ctx context.Context, method, path string, body any) (response, error) {
	return c.send(ctx, method, apiMount+path, body)
}

// json is do with a successful answer's body decoded into out.
func (c *client) json(ctx context.Context, method, path string, body, out any) (response, error) {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return resp, err
	}
	if err := json.Unmarshal(resp.body, out); err != nil {
		return resp, &requestError{method: method, path: urlPath(apiMount + path), status: resp.status,
			message: "the answer is not the JSON expected: " + c.scrub(err.Error()), err: err}
	}
	return resp, nil
}

// send is do for a path from the hub's root.
func (c *client) send(ctx context.Context, method, path string, body any) (response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return response{}, fmt.Errorf("encode the body of %s %s: %w", method, urlPath(path), err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return response{}, fmt.Errorf("%s %s: %s", method, urlPath(path), c.scrub(err.Error()))
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return response{}, c.transportError(method, path, err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	ans := response{status: resp.StatusCode, body: b, duration: time.Since(start)}
	if err != nil {
		return ans, c.transportError(method, path, err)
	}
	if resp.StatusCode >= 400 {
		return ans, c.refusal(method, path, ans)
	}
	return ans, nil
}

func (c *client) transportError(method, path string, err error) *requestError {
	return &requestError{method: method, path: urlPath(path), message: c.scrub(err.Error()), err: err}
}

// refusal is the error of an answer of 400 or more: the error envelope's
// error_type and message, or the body's text when it is no envelope.
func (c *client) refusal(method, path string, ans response) *requestError {
	e := &requestError{method: method, path: urlPath(path), status: ans.status}
	var env struct {
		Error struct {
			Message   string `json:"message"`
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if json.Unmarshal(ans.body, &env) == nil && env.Error.ErrorType != "" {
		e.errorType, e.message = env.Error.ErrorType, env.Error.Message
	} else {
		e.message = strings.TrimSpace(string(ans.body))
	}
	e.errorType, e.message = cut(c.scrub(e.errorType), detailMax), cut(c.scrub(e.message), detailMax)
	return e
}

// scrub is s without the token.
func (c *client) scrub(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, redacted)
}

// detail is s as a detail of the report: without the token and at most
// detailMax bytes long.
func (c *client) detail(s string) string { return cut(c.scrub(s), detailMax) }

// urlPath is p without its query string, which can be long and says
// nothing about which route failed.
func urlPath(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// cut is s cut to at most n bytes, at a character boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}
