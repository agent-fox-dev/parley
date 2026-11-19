package metrics

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// unmatched is the path label of a request no router pattern matched.
const unmatched = "unmatched"

// otherMethod is the method label of a request whose method is outside the
// standard set: net/http accepts any token as a method, and a client could
// otherwise make a series per token it sends, without even authenticating.
const otherMethod = "OTHER"

// standardMethods are the methods labelled as themselves.
var standardMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodOptions: true, http.MethodConnect: true, http.MethodTrace: true,
}

// Instrument returns next counting every request it answers: 1 on
// parley_http_requests_total and the time it took on
// parley_http_request_duration_seconds, by the request's method in upper
// case (OTHER for one outside the standard set) and the path of the router
// pattern that matched, never the concrete URL, which would make a series
// per resource; the counter also by the status sent, 200 when next wrote
// none, 101 when next took the connection over. pattern reads, once next has
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

		labels := map[string]string{"method": methodLabel(r.Method), "path": patternPath(pattern(r))}
		// The method, path and duration always fit these metrics, whose
		// labels are open, so neither report can be refused.
		_ = rec.Observe(HTTPRequestDurationSeconds, labels, elapsed)
		labels["status"] = strconv.Itoa(sw.answered())
		_ = rec.Add(HTTPRequestsTotal, labels, 1)
	})
}

// methodLabel is the method label of a request with the given method.
func methodLabel(method string) string {
	m := strings.ToUpper(method)
	if !standardMethods[m] {
		return otherMethod
	}
	return m
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
	status   int
	hijacked bool
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

// Hijack takes the connection over through the connection's writer and
// notes it: http.ResponseController asks for Hijack before it unwraps, and
// a handler that takes the connection over writes no status here.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, rw, err
}

// answered is the status the client got: the one the handler wrote; 101
// when it wrote none and took the connection over, which the hub does only
// to switch protocols; otherwise 200, as net/http then sends.
func (w *statusWriter) answered() int {
	switch {
	case w.status != 0:
		return w.status
	case w.hijacked:
		return http.StatusSwitchingProtocols
	default:
		return http.StatusOK
	}
}
