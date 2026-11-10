package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// logAccess writes the access-log line of a finished request: method, path
// (the URL path only, never the query string, so that a credential sent as
// a query parameter does not reach the log), status, duration_ms and
// request_id. /healthz and /readyz are logged at debug level, and only
// while health-probe logging is on.
func (s *Server) logAccess(r *http.Request, status int, d time.Duration, id string) {
	level := slog.LevelInfo
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		if !s.opts.LogHealthProbes {
			return
		}
		level = slog.LevelDebug
	}
	s.logger.LogAttrs(context.Background(), level, "request",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Float64("duration_ms", float64(d.Microseconds())/1000),
		slog.String("request_id", id),
	)
}
