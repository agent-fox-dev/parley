package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
)

// The production limits of an outbound forge request.
const (
	// DefaultDeadline is the time a forge has to answer one request
	// completely, its body included.
	DefaultDeadline = 30 * time.Second
	// DefaultMaxBody is the largest answer body read from a forge, in bytes.
	DefaultMaxBody = 8 << 20
	// maxRedirects is how many redirects one request follows.
	maxRedirects = 10
)

// ClientOptions configure a Client. A zero member takes the production
// value, so tests set only what they replace.
type ClientOptions struct {
	// Transport sends the requests; nil means a copy of
	// http.DefaultTransport. A *http.Transport is copied and given the
	// proxy Env names; another round tripper is used as it is.
	Transport http.RoundTripper
	// Now is the clock that measures a request's time and a rate limit's
	// reset; nil means time.Now.
	Now func() time.Time
	// Sleep waits out a rate limit's stated wait; nil means a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Deadline is the time one request is given; 0 means DefaultDeadline.
	Deadline time.Duration
	// MaxBody is the largest answer body read, in bytes; 0 means
	// DefaultMaxBody.
	MaxBody int64
	// Version follows parleyd/ in the User-Agent; "" means the build's
	// version.
	Version string
	// Env is the hub's environment, read once here for HTTPS_PROXY,
	// HTTP_PROXY and NO_PROXY (or their lower-case names); nil means no
	// proxy. The process environment is never read: the hub has none of
	// its own (ADR 06).
	Env func(name string) (string, bool)
}

// Client sends requests to a forge under the hub's outbound call rules:
// every request names the hub, carries the credential's token, follows a
// redirect only to the same scheme, host and port, has its own deadline and
// a limited body, and waits out a rate limit once when the forge says how
// long. A Client is built once and is safe for concurrent use.
type Client struct {
	http     *http.Client
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	deadline time.Duration
	maxBody  int64
	agent    string
}

// NewClient builds a client from o.
func NewClient(o ClientOptions) *Client {
	c := &Client{now: o.Now, sleep: o.Sleep, deadline: o.Deadline, maxBody: o.MaxBody}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sleep == nil {
		c.sleep = sleep
	}
	if c.deadline <= 0 {
		c.deadline = DefaultDeadline
	}
	if c.maxBody <= 0 {
		c.maxBody = DefaultMaxBody
	}
	version := o.Version
	if version == "" {
		version = buildinfo.Get().Version
	}
	c.agent = "parleyd/" + version

	transport := o.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if t, ok := transport.(*http.Transport); ok {
		t = t.Clone()
		t.Proxy = proxyFunc(o.Env)
		transport = t
	}
	c.http = &http.Client{Transport: transport, CheckRedirect: checkRedirect}
	return c
}

// Deadline is the time the client gives one request.
func (c *Client) Deadline() time.Duration { return c.deadline }

// Request is a request to a forge.
type Request struct {
	Method string
	// URL is the request's absolute URL, below the forge's API URL.
	URL string
	// Kind is the forge the request goes to; GitHub gets its own Accept
	// header.
	Kind ForgeKind
	// Credential is sent as Authorization: Bearer <token> when its kind is
	// pat or basic; a request with CredentialNone is anonymous.
	Credential Credential
}

// Response is a forge's successful answer, its body read in full.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// FailureKind says why a forge request failed.
type FailureKind string

// The kinds of a Failure.
const (
	// FailureRedirect: the forge redirected to another scheme, host or
	// port, or more than ten times; the redirect's target got no request.
	FailureRedirect FailureKind = "redirect"
	// FailureTimeout: the forge did not answer completely within the
	// request's deadline, connecting included.
	FailureTimeout FailureKind = "timeout"
	// FailureRateLimited: the forge answered rate limited, and stated no
	// wait, a wait longer than the request had left, or limited the one
	// retry too. Wait and WaitStated are those of the last answer.
	FailureRateLimited FailureKind = "rate_limited"
	// FailureStatus: the forge answered with a status of 400 or more that
	// is no rate limit. Status, Header and Body are its answer.
	FailureStatus FailureKind = "status"
	// FailureUnreadable: the answer's body could not be read, or is larger
	// than the client's size limit on an answer below 400. An error
	// answer past the limit keeps its kind, its body cut at the limit.
	FailureUnreadable FailureKind = "unreadable"
	// FailureUnreachable: the forge could not be reached: its name did not
	// resolve, the connection or the TLS handshake failed.
	FailureUnreachable FailureKind = "unreachable"
)

// Failure is a forge request that failed. Its message holds no credential.
type Failure struct {
	Kind FailureKind
	// Status, Header and Body are the forge's answer, for FailureStatus
	// and FailureRateLimited; Status and Header are also those of an
	// answer whose body is FailureUnreadable.
	Status int
	Header http.Header
	Body   []byte
	// Wait is the wait the forge stated in its last rate-limited answer,
	// when WaitStated is true.
	Wait       time.Duration
	WaitStated bool
	// Err is the cause, for the kinds that have one.
	Err error
}

func (f *Failure) Error() string {
	var msg string
	switch f.Kind {
	case FailureRedirect:
		msg = "the forge's redirect was not followed"
	case FailureTimeout:
		msg = "the forge did not answer in time"
	case FailureRateLimited:
		msg = fmt.Sprintf("the forge answered %d: rate limited", f.Status)
	case FailureStatus:
		msg = fmt.Sprintf("the forge answered %d", f.Status)
	case FailureUnreadable:
		msg = "the forge's answer could not be read"
	default:
		msg = "the forge could not be reached"
	}
	if f.Err != nil {
		msg += ": " + f.Err.Error()
	}
	return msg
}

func (f *Failure) Unwrap() error { return f.Err }

// Do sends req and returns the forge's answer, or a *Failure. The request
// runs on a context of its own that the caller's cancellation does not end,
// bounded by the client's deadline, so that a write is not cut off half way
// when its caller leaves. A rate-limited answer whose stated wait fits in
// what the request had left of its deadline is waited out and the request
// sent once more, with a deadline of its own.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	ctx = context.WithoutCancel(ctx)
	for retried := false; ; retried = true {
		sent := c.now()
		resp, err := c.send(ctx, req)
		if err != nil {
			return nil, err
		}
		wait, stated, limited := rateLimit(resp, c.now())
		if !limited {
			if resp.Status >= http.StatusBadRequest {
				return nil, &Failure{Kind: FailureStatus, Status: resp.Status, Header: resp.Header, Body: resp.Body}
			}
			return resp, nil
		}
		left := c.deadline - c.now().Sub(sent)
		if retried || !stated || wait > left {
			return nil, &Failure{Kind: FailureRateLimited, Status: resp.Status, Header: resp.Header,
				Body: resp.Body, Wait: wait, WaitStated: stated}
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, &Failure{Kind: FailureTimeout, Err: err}
		}
	}
}

// Decode decodes the JSON body of resp into v. A body that is not the JSON
// v expects is a *Failure of kind FailureUnreadable, which the hub answers
// as the forge's error.
func Decode(resp *Response, v any) error {
	if err := json.Unmarshal(resp.Body, v); err != nil {
		return &Failure{Kind: FailureUnreadable, Status: resp.Status, Header: resp.Header,
			Err: fmt.Errorf("the body is not the JSON expected: %w", err)}
	}
	return nil
}

// send sends req once, under its own deadline, and reads the answer.
func (c *Client) send(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.deadline)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, req.Method, req.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("forge request: %w", err)
	}
	r.Header.Set("User-Agent", c.agent)
	if req.Kind == KindGitHub {
		r.Header.Set("Accept", "application/vnd.github+json")
	}
	if req.Credential.Kind == CredentialPAT || req.Credential.Kind == CredentialBasic {
		r.Header.Set("Authorization", "Bearer "+req.Credential.Token)
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, classify(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		if isTimeout(err) {
			return nil, &Failure{Kind: FailureTimeout, Err: err}
		}
		return nil, &Failure{Kind: FailureUnreadable, Status: resp.StatusCode, Header: resp.Header, Err: err}
	}
	if int64(len(body)) > c.maxBody {
		// An error answer is judged by its status, a rate limit included;
		// its body only lends the message, which the limit's worth of it
		// holds.
		if resp.StatusCode >= http.StatusBadRequest {
			body = body[:c.maxBody]
		} else {
			return nil, &Failure{Kind: FailureUnreadable, Status: resp.StatusCode, Header: resp.Header,
				Err: fmt.Errorf("the body is larger than %d bytes", c.maxBody)}
		}
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// classify turns an error of sending a request into its Failure.
func classify(err error) error {
	var f *Failure
	switch {
	case errors.As(err, &f):
		return f
	case isTimeout(err):
		return &Failure{Kind: FailureTimeout, Err: err}
	default:
		return &Failure{Kind: FailureUnreachable, Err: err}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// checkRedirect follows a redirect only to the scheme, host and port of the
// first request, and at most maxRedirects times: a forge must not send the
// hub, and the credential it carries, anywhere else.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return &Failure{Kind: FailureRedirect, Err: fmt.Errorf("more than %d redirects", maxRedirects)}
	}
	if origin(req.URL) != origin(via[0].URL) {
		return &Failure{Kind: FailureRedirect, Err: errors.New("a redirect to another scheme, host or port")}
	}
	return nil
}

// origin is a URL's scheme, host and port, the port made explicit.
func origin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// rateLimit says whether an answer is rate limited, and the wait it states:
// Retry-After in seconds, else the Unix time of X-RateLimit-Reset or
// RateLimit-Reset less now, not below zero.
func rateLimit(resp *Response, now time.Time) (wait time.Duration, stated, limited bool) {
	h := resp.Header
	_, hasRetryAfter := h["Retry-After"]
	limited = resp.Status == http.StatusTooManyRequests ||
		resp.Status == http.StatusForbidden && (h.Get("X-RateLimit-Remaining") == "0" || hasRetryAfter)
	if !limited {
		return 0, false, false
	}
	if s, ok := seconds(h.Get("Retry-After")); ok {
		return time.Duration(s) * time.Second, true, true
	}
	for _, name := range []string{"X-RateLimit-Reset", "RateLimit-Reset"} {
		if s, ok := seconds(h.Get(name)); ok {
			return max(time.Unix(s, 0).Sub(now), 0), true, true
		}
	}
	return 0, false, true
}

// seconds parses a header's whole number of seconds, capped where a
// time.Duration of it would overflow.
func seconds(v string) (int64, bool) {
	s, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || s < 0 {
		return 0, false
	}
	return min(s, math.MaxInt64/int64(time.Second)), true
}

// proxyFunc is the transport's proxy rule from the proxy variables of env,
// upper-case names first as the standard library reads them.
func proxyFunc(env func(string) (string, bool)) func(*http.Request) (*url.URL, error) {
	if env == nil {
		return nil
	}
	lookup := func(names ...string) string {
		for _, name := range names {
			if v, ok := env(name); ok && v != "" {
				return v
			}
		}
		return ""
	}
	cfg := httpproxy.Config{
		HTTPProxy:  lookup("HTTP_PROXY", "http_proxy"),
		HTTPSProxy: lookup("HTTPS_PROXY", "https_proxy"),
		NoProxy:    lookup("NO_PROXY", "no_proxy"),
	}
	if cfg.HTTPProxy == "" && cfg.HTTPSProxy == "" {
		return nil
	}
	proxy := cfg.ProxyFunc()
	return func(r *http.Request) (*url.URL, error) { return proxy(r.URL) }
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
