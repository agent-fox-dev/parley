package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// RequestID returns the id of the request a context belongs to, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(keyRequestID).(string)
	return id
}

// validRequestID is the 36-character hyphenated hexadecimal form of a UUID,
// in either letter case and of any version.
var validRequestID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// newRequestID returns a random version 4 UUID.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails.
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// cacheControl is the default Cache-Control of a path; a REST route may
// state its own.
func cacheControl(path string) string {
	switch {
	case isAPIPath(path):
		return "no-store"
	case path == "/healthz", path == "/readyz":
		return "no-cache"
	case path == "/version":
		return "public, max-age=300"
	}
	return ""
}

// stamp is the outermost middleware, around every surface of the server: it
// gives the request its id, sets the response headers every answer carries,
// answers a panic as 500 internal and writes the access-log line.
func (s *Server) stamp(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-ID")
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		r = r.WithContext(context.WithValue(r.Context(), keyRequestID, id))

		h := w.Header()
		h.Set("X-Request-ID", id)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if cc := cacheControl(r.URL.Path); cc != "" {
			h.Set("Cache-Control", cc)
		}

		tw := &trackingWriter{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				s.logger.Error("internal error", "request_id", id, "error", fmt.Sprint(p))
				if !tw.wrote {
					WriteError(tw, Internal("internal error"))
				}
			}
			s.logAccess(r, tw.statusOrOK(), time.Since(start), id)
		}()
		next.ServeHTTP(tw, r)
	})
}
