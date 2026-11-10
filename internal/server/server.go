// Package server is parleyd's HTTP server: its router, the unauthenticated
// health, readiness and version endpoints, and serving and shutting down.
package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
)

// Probe reports whether the hub can do its work. A non-nil error means the
// hub is not ready; the error text is never sent to the client.
type Probe func(ctx context.Context) error

// Options configure a Server.
type Options struct {
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
