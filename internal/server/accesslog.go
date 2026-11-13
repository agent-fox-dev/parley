package server

import (
	"log/slog"
	"net/http"
	"time"
)

// logAccess writes the access-log line of a finished request. The path is
// the URL's path alone: a credential sent as a query parameter must never
// reach the log. Health probes arrive every few seconds from load balancers
// and orchestrators, so they are logged only when asked, at debug level.
func (s *Server) logAccess(r *http.Request, status int, d time.Duration) {
	level := slog.LevelInfo
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		if !s.opts.LogHealthProbes {
			return
		}
		level = slog.LevelDebug
	}
	s.logger.LogAttrs(r.Context(), level, "request",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Float64("duration_ms", float64(d.Microseconds())/1000),
		slog.String("request_id", RequestID(r.Context())),
	)
}
