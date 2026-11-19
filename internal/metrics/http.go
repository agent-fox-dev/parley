package metrics

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// unmatched is the path label of a request no router pattern matched.
const unmatched = "unmatched"

// Instrument returns next counting every request it answers: 1 on
// parley_http_requests_total and the time it took on
// parley_http_request_duration_seconds, by the request's method in upper
// case and the path of the router pattern that matched, never the concrete
// URL, which would make a series per resource; the counter also by the
// status sent, 200 when next wrote none. pattern reads, once next has
// served the request, the pattern that matched in http.ServeMux syntax, ""
// for none; nil reads Request.Pattern, which http.ServeMux sets on the
// request it is given. The time is taken from now, the hub's clock.
//
// The writer next gets unwraps to the one Instrument was given, so that a
// streaming handler can still flush or take over the connection through
// http.ResponseController.
func Instrument(rec Recorder, now func() time.Time, pattern func(*http.Request) string, next http.Handler) http.Handler {
	if pattern == nil {
		pattern = func(r *http.Request) string { return r.Pattern }
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		elapsed := max(now().Sub(start).Seconds(), 0)

		labels := map[string]string{"method": strings.ToUpper(r.Method), "path": patternPath(pattern(r))}
		// The method, path and duration always fit these metrics, whose
		// labels are open, so neither report can be refused.
		_ = rec.Observe(HTTPRequestDurationSeconds, labels, elapsed)
		labels["status"] = strconv.Itoa(sw.answered())
		_ = rec.Add(HTTPRequestsTotal, labels, 1)
	})
}

// patternPath is the path of an http.ServeMux pattern ("[METHOD ][HOST]/PATH"),
// or unmatched for no pattern.
func patternPath(p string) string {
	i := strings.IndexByte(p, '/')
	if i < 0 {
		return unmatched
	}
	return p[i:]
}

// statusWriter records the status the handler answers with.
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

// Unwrap lets http.ResponseController reach the connection's writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// answered is the status the client got: 200 when the handler wrote
// nothing, as net/http then sends.
func (w *statusWriter) answered() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
