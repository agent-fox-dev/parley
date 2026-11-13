// Package server is the HTTP side of parleyd: the routes every hub answers
// and the readiness probes later parts of the hub register.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
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
	// OnListen, when set, is called once by Serve with the address it bound,
	// before it serves; with port 0 in Addr it carries the chosen port.
	OnListen func(net.Addr)
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

// Handle registers h for pattern on the server's router, with the pattern
// syntax of http.ServeMux. Routes are registered before Serve is called.
func (s *Server) Handle(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
}

// newConnWait is how long a stop waits for connections that are accepted but
// whose request has not been read yet: http.Server.Shutdown drops those, so a
// request the client has already sent would get no answer.
const newConnWait = 100 * time.Millisecond

// Serve binds Options.Addr and serves until ctx is cancelled. It returns the
// error when the address cannot be bound. On cancellation it stops accepting
// connections and gives in-flight requests up to Options.ShutdownGrace to
// finish; when the grace passes it closes the connections that remain,
// without waiting for their handlers. A stop returns nil either way.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.opts.Addr, err)
	}
	if s.opts.OnListen != nil {
		s.opts.OnListen(ln.Addr())
	}

	conns := newConnTracker()
	srv := &http.Server{Handler: s.mux, ConnState: conns.track}
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
