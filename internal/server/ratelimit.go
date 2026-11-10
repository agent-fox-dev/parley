package server

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// DefaultRequestsPerMinute is the product's rate limit per credential.
const DefaultRequestsPerMinute = 600

// rateWindow is the length of a fixed window.
const rateWindow = time.Minute

// Limiter is a fixed-window rate limiter keyed by a string (a credential id,
// a client address). Each key's window starts at its first counted request,
// lasts 60 seconds and admits the configured number of requests; requests
// past it are refused until the window ends. Counts live in memory only. It
// is safe for concurrent use; the git server shares it with the REST API.
type Limiter struct {
	perMinute int
	now       func() time.Time

	mu        sync.Mutex
	windows   map[string]*window
	lastSweep time.Time
}

type window struct {
	start time.Time
	count int
}

// NewLimiter builds a limiter admitting perMinute requests per key and
// window; perMinute 0 or less admits everything. now is its clock (nil means
// time.Now).
func NewLimiter(perMinute int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{perMinute: perMinute, now: now, windows: map[string]*window{}}
}

// Allow counts one request for key and reports whether it is admitted; a
// refused request also returns the time until the key's window ends, rounded
// up to a whole second and at least one second. A refused request is counted
// but does not extend the window.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l.perMinute <= 0 {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	w := l.windows[key]
	if w == nil || !now.Before(w.start.Add(rateWindow)) {
		w = &window{start: now}
		l.windows[key] = w
	}
	w.count++
	if w.count <= l.perMinute {
		return true, 0
	}
	retry := w.start.Add(rateWindow).Sub(now)
	secs := (retry + time.Second - 1) / time.Second
	if secs < 1 {
		secs = 1
	}
	return false, secs * time.Second
}

// sweep drops the windows that have ended, at most once a window, so that
// the map does not grow without bound.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < rateWindow {
		return
	}
	l.lastSweep = now
	for key, w := range l.windows {
		if !now.Before(w.start.Add(rateWindow)) {
			delete(l.windows, key)
		}
	}
}

// clientAddress is the IP address of the request's connection; forwarding
// headers are not read.
func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateKey is the key a request is counted under: its credential, else its
// client address.
func rateKey(r *http.Request, caller *Caller) string {
	if caller != nil {
		return "credential:" + caller.CredentialID
	}
	return "address:" + clientAddress(r)
}

// checkRate is the pipeline's rate-limit stage.
func (s *Server) checkRate(w http.ResponseWriter, r *http.Request, caller *Caller) error {
	ok, retry := s.limiter.Allow(rateKey(r, caller))
	if ok {
		return nil
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(retry/time.Second)))
	return RateLimited("too many requests")
}
