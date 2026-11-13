// Package server is the HTTP side of parleyd: the routes every hub answers
// and the readiness probes later parts of the hub register.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
)

// Probe reports whether one part of the hub can do its work: it returns an
// error when that part is not ready.
type Probe func(context.Context) error

// Options configure a Server.
type Options struct {
	// Addr is the address the server listens on, host:port.
	Addr string
	// ShutdownGrace is how long in-flight requests get to finish on stop.
	ShutdownGrace time.Duration
	// ReadinessProbes are called on every GET /readyz; with none, the hub is
	// ready.
	ReadinessProbes []Probe
}

// Server is the hub's HTTP server.
type Server struct {
	opts Options
	mux  *http.ServeMux
}

// New builds a Server from opts and registers its built-in routes. Unknown
// paths and wrong methods get the standard library router's own answers.
func New(opts Options) *Server {
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /readyz", s.readyz)
	s.mux.HandleFunc("GET /version", s.version)
	return s
}

// Handler returns the handler that serves every route of the server.
func (s *Server) Handler() http.Handler {
	return s.mux
}

type statusDocument struct {
	Status string `json:"status"`
}

type versionDocument struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, statusDocument{Status: "ok"})
}

// readyz calls every probe, even after one has failed, so each probe sees
// every request. The probe's error stays out of the body: the route needs no
// credential, and the error may describe the hub's internals.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ready := true
	for _, probe := range s.opts.ReadinessProbes {
		if err := probe(r.Context()); err != nil {
			ready = false
		}
	}
	if !ready {
		writeJSON(w, http.StatusServiceUnavailable, statusDocument{Status: "not ready"})
		return
	}
	writeJSON(w, http.StatusOK, statusDocument{Status: "ready"})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	info := buildinfo.Get()
	writeJSON(w, http.StatusOK, versionDocument{
		Version:   info.Version,
		Commit:    info.Commit,
		BuildTime: info.BuildTime,
		GoVersion: info.GoVersion,
	})
}

// writeJSON answers status with v encoded as JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The status line is already sent; a failed write means the client went
	// away, and there is no one left to tell.
	_ = json.NewEncoder(w).Encode(v)
}
