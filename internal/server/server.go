// Package server is parleyd's HTTP server: its router, the unauthenticated
// health, readiness and version endpoints, and serving and shutting down.
package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
)

// Probe reports whether the hub can do its work. A non-nil error means the
// hub is not ready; the error text is never sent to the client.
type Probe func(ctx context.Context) error

// Options configure a Server.
type Options struct {
	// Addr is the listen address, host:port; port 0 asks the operating
	// system for a free port.
	Addr string
	// ShutdownGrace is how long in-flight requests get to finish once the
	// serving context is cancelled.
	ShutdownGrace time.Duration
	// ReadinessProbes are called on every GET /readyz; the hub is ready when
	// all of them return nil, and when none is registered.
	ReadinessProbes []Probe
	// BodyLimit is the largest request body in bytes a REST route accepts
	// unless the route states its own; 0 or less means DefaultBodyLimit.
	BodyLimit int64
}

// Server is parleyd's HTTP server.
type Server struct {
	opts Options
	mux  *http.ServeMux

	routes []*compiledRoute
	auth   Authenticator
	guard  Check
	scope  Check
	access []Check
}

// New builds a server from its options and registers the health, readiness
// and version routes.
func New(opts Options) *Server {
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.HandleFunc("GET /healthz", s.healthz)
	s.HandleFunc("GET /readyz", s.readyz)
	s.HandleFunc("GET /version", s.version)
	return s
}

// Handle registers a handler for a method-and-pattern route outside the REST
// API on the server's router, as net/http.ServeMux reads patterns
// ("GET /healthz"). REST routes are registered with Register.
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// HandleFunc registers a handler function for a method-and-pattern route.
func (s *Server) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	s.mux.HandleFunc(pattern, h)
}

// Handler returns the server's HTTP handler: the REST API under APIPrefix and
// every other surface registered with Handle.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			s.serveAPI(w, r)
			return
		}
		s.serveSurface(w, r)
	})
}

// connTracker records the state of every connection the server holds.
type connTracker struct {
	mu     sync.Mutex
	states map[net.Conn]http.ConnState
}

func (c *connTracker) track(conn net.Conn, state http.ConnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch state {
	case http.StateClosed, http.StateHijacked:
		delete(c.states, conn)
	default:
		c.states[conn] = state
	}
}

// closeUnused closes every connection that has not read a byte of a request.
func (c *connTracker) closeUnused() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for conn, state := range c.states {
		if state == http.StateNew {
			// A failed close leaves a connection Shutdown ends at its grace.
			_ = conn.Close()
		}
	}
}

type statusDoc struct {
	Status string `json:"status"`
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, statusDoc{Status: "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ready := true
	for _, probe := range s.opts.ReadinessProbes {
		if err := probe(r.Context()); err != nil {
			ready = false
		}
	}
	if !ready {
		WriteJSON(w, http.StatusServiceUnavailable, statusDoc{Status: "not ready"})
		return
	}
	WriteJSON(w, http.StatusOK, statusDoc{Status: "ready"})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, buildinfo.Get())
}

// Serve binds the listen address, calls onListen with the address it bound
// (so that port 0 yields the chosen port) and serves until ctx is cancelled.
// It then stops accepting connections and waits for in-flight requests for at
// most the shutdown grace; connections still open when the grace passes are
// closed. A stop by ctx returns nil whether or not the grace passed; an
// address that cannot be bound returns the bind error.
func (s *Server) Serve(ctx context.Context, onListen func(net.Addr)) error {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return err
	}
	if onListen != nil {
		onListen(ln.Addr())
	}

	conns := &connTracker{states: map[net.Conn]http.ConnState{}}
	hs := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 30 * time.Second, ConnState: conns.track}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownGrace)
	defer cancel()
	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- hs.Shutdown(shutdownCtx) }()
	// Shutdown waits for connections that have not read a byte yet (a
	// client's spare keep-alive dial) as if they were requests; they carry
	// none, so close them while it runs.
	sweep := time.NewTicker(10 * time.Millisecond)
	defer sweep.Stop()
	for done := false; !done; {
		conns.closeUnused()
		select {
		case err := <-shutdownErr:
			done = true
			if err != nil {
				// The grace passed with requests still running: close their
				// connections. Close only fails with the listener's close
				// error, and Shutdown already closed the listener.
				_ = hs.Close()
			}
		case <-sweep.C:
		}
	}
	// After Shutdown or Close, Serve returns http.ErrServerClosed.
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
