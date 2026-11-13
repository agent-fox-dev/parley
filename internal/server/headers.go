package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type requestIDKey struct{}

// RequestID returns the id of the request whose context ctx is: the
// X-Request-ID it arrived with when that is a UUID, else one the server
// generated. It is "" for a context that did not come through the server.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// stamp is the outermost layer of the server: it gives every request an id,
// sets the headers every response carries before anything else can write,
// and writes the access-log line once the request is answered. It wraps
// every surface (the built-in routes, the REST API, unknown paths and those
// mounted with Handle), so none of them can leave them out.
func (s *Server) stamp(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-ID")
		if !isUUID(id) {
			id = newUUID()
		}
		h := w.Header()
		h.Set("X-Request-ID", id)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == apiMount || strings.HasPrefix(r.URL.Path, apiMount+"/") {
			// A route that states its own value replaces this once matched.
			h.Set("Cache-Control", "no-store")
		}

		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				// Only a surface outside the REST API gets here: the
				// pipeline recovers its own panics. net/http drops the
				// connection on the panic, so no answer reaches the client.
				s.logAccess(r, http.StatusInternalServerError, time.Since(start))
				panic(p)
			}
			s.logAccess(r, sw.answered(), time.Since(start))
		}()
		next.ServeHTTP(sw, r)
	})
}

// isUUID reports whether v is a UUID in its 36-character hyphenated
// hexadecimal form, of any version and in either letter case.
func isUUID(v string) bool {
	if len(v) != 36 {
		return false
	}
	for i := range len(v) {
		c := v[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// newUUID returns a random (version 4) UUID in lower case.
func newUUID() string {
	var b [16]byte
	// crypto/rand.Read never returns an error; it ends the program when the
	// system's random source fails.
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// statusWriter records the status a handler answers with, for the access log
// and so that a late failure is not written over an answer already sent.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	// An informational (1xx) status is not the answer; the final one follows.
	if w.status == 0 && status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection's writer, so a
// streaming route can still flush or set deadlines through the wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// answered is the status the client got: 200 when the handler wrote nothing,
// as net/http then sends.
func (w *statusWriter) answered() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// headersSent reports whether an answer has already gone out on w, so that
// nothing is written after it.
func headersSent(w http.ResponseWriter) bool {
	for {
		switch v := w.(type) {
		case *statusWriter:
			return v.status != 0
		case interface{ Unwrap() http.ResponseWriter }:
			w = v.Unwrap()
		default:
			return false
		}
	}
}
