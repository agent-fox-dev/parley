package gitserver

import (
	"maps"
	"net/http"
	"sync"
)

// RequestsMetric is the name under which the counter of git requests is
// exported: parley_git_requests_total{service,result}. The git server is
// the one place that counts it; the push processor reports its result and
// never counts.
const RequestsMetric = "parley_git_requests_total"

// The values of the label service, one per route: ref discovery counts as
// info-refs whichever service it asks for.
const (
	LabelInfoRefs    = "info-refs"
	LabelUploadPack  = "upload-pack"
	LabelReceivePack = "receive-pack"
)

// RequestLabels are the labels of one series of the request counter.
type RequestLabels struct {
	// Service is LabelInfoRefs, LabelUploadPack or LabelReceivePack.
	Service string
	// Result is what the request came to.
	Result Result
}

// requestCounter counts git requests by their labels; its zero value is
// ready and safe for concurrent use.
type requestCounter struct {
	mu sync.Mutex
	n  map[RequestLabels]uint64
}

func (c *requestCounter) add(l RequestLabels) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[RequestLabels]uint64{}
	}
	c.n[l]++
}

// Requests returns a snapshot of the request counter: every series counted
// so far with its count. A series no request has reached is absent, which
// reads as zero. The map is the caller's.
func (c *Component) Requests() map[RequestLabels]uint64 {
	c.requests.mu.Lock()
	defer c.requests.mu.Unlock()
	return maps.Clone(c.requests.n)
}

// labelOf is the service label of route.
func labelOf(route Route) string {
	switch route {
	case RouteUploadPack:
		return LabelUploadPack
	case RouteReceivePack:
		return LabelReceivePack
	default:
		return LabelInfoRefs
	}
}

// resultOf is the result of a request whose answer the git server did not
// judge itself, a refusal of a check or the answer of a Service, by its
// status: refused for 401, 403 and 404, the answers that tell a caller it
// may not, ok for a success, and error for every other answer.
func resultOf(status int) Result {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound:
		return ResultRefused
	case status < 300:
		return ResultOK
	default:
		return ResultError
	}
}

// statusWriter records the status of the answer it passes on. Unwrap lets
// http.ResponseController reach the server's writer, to flush and to
// enable full duplex.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 && status >= http.StatusOK {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
