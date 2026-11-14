// Package server is the HTTP side of parleyd: the routes every hub answers
// and the readiness probes later parts of the hub register.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Probe reports whether one part of the hub can do its work: it returns an
// error when that part is not ready.
type Probe func(context.Context) error

// Options configure a Server.
type Options struct {
	// Addr is the address the server listens on, host:port.
	Addr string
	// Listener, when set, is the bound listener Serve serves on instead of
	// binding Addr, so that a caller can bind before it starts work that must
	// not run when the address cannot be bound. Serve closes it on a stop.
	Listener net.Listener
	// ShutdownGrace is how long in-flight requests get to finish on stop.
	ShutdownGrace time.Duration
	// ReadinessProbes are called on every GET /readyz; with none, the hub is
	// ready.
	ReadinessProbes []Probe
	// OnListen, when set, is called once by Serve with the address it bound,
	// before it serves; with port 0 in Addr it carries the chosen port.
	OnListen func(net.Addr)
	// MaxBodySize is the body limit of a REST API request in bytes, for a
	// route that states none of its own; 0 means 1048576 (1 MiB).
	MaxBodySize int64
	// RequestsPerMinute is how many REST API requests one credential, or one
	// client address without a valid credential, may send in a window of 60
	// seconds; 0 means 600.
	RequestsPerMinute int
	// RateLimitDisabled turns the rate limit off, whatever RequestsPerMinute
	// says.
	RateLimitDisabled bool
	// Now is the clock of the rate limit and of the idempotency records' age;
	// nil means time.Now.
	Now func() time.Time
	// Logger receives the access log and the causes of internal errors; nil
	// means slog.Default().
	Logger *slog.Logger
	// LogHealthProbes writes access-log lines for /healthz and /readyz, at
	// debug level; without it they get none.
	LogHealthProbes bool
	// IdempotencyStore keeps the records of creates sent with an
	// Idempotency-Key; nil means an in-memory store, emptied at restart.
	IdempotencyStore IdempotencyStore
	// VersionMembers, when set, is called on every GET /version and its
	// members are added to the document after the build's; a member named
	// like one of the build's is left out.
	VersionMembers func() map[string]any
}

// Server is the hub's HTTP server.
type Server struct {
	opts Options
	mux  *http.ServeMux

	routesMu sync.RWMutex
	// routes holds the REST API routes registered, which dispatch matches
	// itself rather than through mux.
	routes []apiRoute

	posMu sync.RWMutex
	pos   positions

	limiter *Limiter
	logger  *slog.Logger
	now     func() time.Time
	idem    *idempotency
}

// New builds a Server from opts and registers its built-in routes. Unknown
// paths and wrong methods are answered with the error envelope.
func New(opts Options) *Server {
	s := &Server{
		opts:    opts,
		mux:     http.NewServeMux(),
		limiter: newServerLimiter(opts),
		logger:  opts.Logger,
		now:     opts.Now,
		idem:    newIdempotency(opts.IdempotencyStore),
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /readyz", s.readyz)
	s.mux.HandleFunc("GET /version", s.version)
	return s
}

// Handler returns the handler that serves every route of the server. Every
// response it sends carries X-Request-ID and the security headers, and every
// request it answers gets an access-log line.
func (s *Server) Handler() http.Handler {
	return s.stamp(http.HandlerFunc(s.dispatch))
}

// Handle registers h for pattern on the server's router, with the pattern
// syntax of http.ServeMux, for a surface outside the REST API. Its requests
// do not pass the REST API's checks. Routes are registered before Serve is
// called.
func (s *Server) Handle(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
}

// newConnWait is how long a stop waits for connections that are accepted but
// whose request has not been read yet: http.Server.Shutdown drops those, so a
// request the client has already sent would get no answer.
const newConnWait = 100 * time.Millisecond

// Serve binds Options.Addr, or takes Options.Listener when it is set, and
// serves until ctx is cancelled. It returns the error when the address cannot
// be bound. On cancellation it stops accepting connections and gives
// in-flight requests up to Options.ShutdownGrace to finish; when the grace
// passes it closes the connections that remain, without waiting for their
// handlers. A stop returns nil either way.
func (s *Server) Serve(ctx context.Context) error {
	ln := s.opts.Listener
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", s.opts.Addr); err != nil {
			return fmt.Errorf("listen on %s: %w", s.opts.Addr, err)
		}
	}
	if s.opts.OnListen != nil {
		s.opts.OnListen(ln.Addr())
	}

	conns := newConnTracker()
	srv := &http.Server{Handler: s.Handler(), ConnState: conns.track}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(trackingListener{ln, conns}) }()

	select {
	case err := <-served:
		_ = srv.Close()
		return fmt.Errorf("serve on %s: %w", ln.Addr(), err)
	case <-ctx.Done():
	}

	// Closing the listener ends srv.Serve with an accept error, the expected
	// end here and not a failure; served is buffered, so its goroutine ends
	// without a reader.
	_ = ln.Close()
	conns.waitForRequests(newConnWait)

	graceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(graceCtx); errors.Is(err, context.DeadlineExceeded) {
		// Close drops the remaining connections; their handlers are left to
		// end on their own.
		_ = srv.Close()
	}
	return nil
}

// connTracker knows which connections are accepted but have not had a
// request read yet (http.StateNew).
type connTracker struct {
	mu      sync.Mutex
	fresh   map[net.Conn]struct{}
	changed chan struct{}
}

func newConnTracker() *connTracker {
	return &connTracker{fresh: make(map[net.Conn]struct{}), changed: make(chan struct{}, 1)}
}

// trackingListener counts a connection as waiting for its first request as
// soon as Accept returns it, before http.Server reports it as StateNew, so a
// stop cannot miss a connection accepted just before the listener closed.
type trackingListener struct {
	net.Listener
	conns *connTracker
}

func (l trackingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.conns.track(conn, http.StateNew)
	}
	return conn, err
}

// track is the http.Server ConnState hook.
func (c *connTracker) track(conn net.Conn, state http.ConnState) {
	c.mu.Lock()
	if state == http.StateNew {
		c.fresh[conn] = struct{}{}
	} else {
		delete(c.fresh, conn)
	}
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *connTracker) anyFresh() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.fresh) > 0
}

// waitForRequests waits until no connection is waiting for its first request
// to be read, or for at most limit.
func (c *connTracker) waitForRequests(limit time.Duration) {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for c.anyFresh() {
		select {
		case <-c.changed:
		case <-timer.C:
			return
		}
	}
}

type statusDocument struct {
	Status string `json:"status"`
}

// The probes must always see the hub's current state, so no cache may keep
// their answer; the build of a running hub changes only on a restart.
const (
	probeCacheControl   = "no-cache"
	versionCacheControl = "public, max-age=300"
)

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", probeCacheControl)
	WriteJSON(w, http.StatusOK, statusDocument{Status: "ok"})
}

// readyz calls every probe, even after one has failed, so each probe sees
// every request. The probe's error stays out of the body: the route needs no
// credential, and the error may describe the hub's internals.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", probeCacheControl)
	ready := true
	for _, probe := range s.opts.ReadinessProbes {
		if err := probe(r.Context()); err != nil {
			ready = false
		}
	}
	if !ready {
		WriteJSON(w, http.StatusServiceUnavailable, statusDocument{Status: "not ready"})
		return
	}
	WriteJSON(w, http.StatusOK, statusDocument{Status: "ready"})
}
