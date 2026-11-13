package server

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// defaultRequestsPerMinute is the rate limit when Options.RequestsPerMinute
// is 0.
const defaultRequestsPerMinute = 600

// rateWindow is the length of one key's window.
const rateWindow = time.Minute

// Limiter counts requests per key in fixed windows of 60 seconds. A key's
// window starts at its first counted request and admits a set number of
// requests; the next request after the window has ended starts a new one.
// Counts live in memory only, so a new Limiter starts with none. A Limiter
// is safe for concurrent use.
type Limiter struct {
	perMinute int
	now       func() time.Time

	mu      sync.Mutex
	windows map[string]window
	// swept is when ended windows were last dropped.
	swept time.Time
}

type window struct {
	start time.Time
	count int
}

// NewLimiter returns a Limiter that admits perMinute requests per key and
// window, reading the time from now (nil means time.Now). A perMinute of 0
// or less admits every request.
func NewLimiter(perMinute int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{perMinute: perMinute, now: now, windows: make(map[string]window)}
}

// Allow counts one request for key. It reports whether the request is
// admitted and, when it is not, the whole seconds until the key's window
// ends, rounded up and at least 1. A refused request does not change the
// window.
func (l *Limiter) Allow(key string) (ok bool, retryAfter int) {
	if l.perMinute <= 0 {
		return true, 0
	}
	t := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(t)

	w, found := l.windows[key]
	if !found || !t.Before(w.start.Add(rateWindow)) {
		l.windows[key] = window{start: t, count: 1}
		return true, 0
	}
	if w.count < l.perMinute {
		w.count++
		l.windows[key] = w
		return true, 0
	}
	left := w.start.Add(rateWindow).Sub(t)
	return false, max(1, int((left+time.Second-1)/time.Second))
}

// sweep drops the windows that have ended, at most once per window length,
// so the map holds only keys seen in the last two minutes without a scan on
// every request.
func (l *Limiter) sweep(t time.Time) {
	if t.Sub(l.swept) < rateWindow {
		return
	}
	for key, w := range l.windows {
		if !t.Before(w.start.Add(rateWindow)) {
			delete(l.windows, key)
		}
	}
	l.swept = t
}

// newServerLimiter builds the server's limiter from its options: 0 requests
// per minute means the default, and RateLimitDisabled turns the limit off.
func newServerLimiter(opts Options) *Limiter {
	perMinute := opts.RequestsPerMinute
	if perMinute == 0 {
		perMinute = defaultRequestsPerMinute
	}
	if opts.RateLimitDisabled {
		perMinute = 0
	}
	return NewLimiter(perMinute, opts.Now)
}

// RateLimiter returns the server's limiter, so that a surface mounted with
// Handle counts its requests against the same keys as the REST API.
func (s *Server) RateLimiter() *Limiter { return s.limiter }

// rateLimit is the rate-limit stage. The key is the caller's credential when
// authentication reported one with an id, otherwise the client address, so
// callers without a credential id never share one window. Over the limit it
// sets Retry-After and answers 429.
func (s *Server) rateLimit(w http.ResponseWriter, r *http.Request) error {
	key := addressKey(r)
	if c, ok := CallerOf(r.Context()); ok && c.CredentialID != "" {
		key = "credential:" + c.CredentialID
	}
	ok, retryAfter := s.limiter.Allow(key)
	if ok {
		return nil
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	return RateLimited("rate limit exceeded")
}

// countRefused counts a request that authentication refused against its
// client address. The address's count is not the answer: the authenticator's
// refusal comes first in the order of checks.
func (s *Server) countRefused(r *http.Request) {
	s.limiter.Allow(addressKey(r))
}

// addressKey is the limiter key of the connection's remote IP address. Headers
// such as X-Forwarded-For are not read, since any client can set them.
func addressKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "address:" + host
}
