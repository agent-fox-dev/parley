// Package server is parleyd's HTTP server: its router, the unauthenticated
// health, readiness and version endpoints, and serving and shutting down.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
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
}

// Server is parleyd's HTTP server.
type Server struct {
	opts Options
	mux  *http.ServeMux
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

// Handle registers a handler for a method-and-pattern route on the server's
// router, as net/http.ServeMux reads patterns ("GET /healthz").
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// HandleFunc registers a handler function for a method-and-pattern route.
func (s *Server) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	s.mux.HandleFunc(pattern, h)
}

// Handler returns the server's HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }

type statusDoc struct {
	Status string `json:"status"`
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, statusDoc{Status: "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ready := true
	for _, probe := range s.opts.ReadinessProbes {
		if err := probe(r.Context()); err != nil {
			ready = false
		}
	}
	if !ready {
		writeJSON(w, http.StatusServiceUnavailable, statusDoc{Status: "not ready"})
		return
	}
	writeJSON(w, http.StatusOK, statusDoc{Status: "ready"})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, buildinfo.Get())
}

// writeJSON writes v as the JSON body of a response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
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

	hs := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 30 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownGrace)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil {
		// The grace passed with requests still running: close their
		// connections. Close only fails with the listener's close error,
		// and the listener is already closed by Shutdown.
		_ = hs.Close()
	}
	// After Shutdown or Close, Serve returns http.ErrServerClosed.
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
