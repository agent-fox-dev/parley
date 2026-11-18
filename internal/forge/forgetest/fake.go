// Package forgetest is a fake forge for tests: an HTTP server, over TLS or
// plain HTTP, that records every request it receives and answers what the
// test scripts. It serves GitHub's API below the prefix /github and
// GitLab's below /gitlab/api/v4, so a workspace whose API URL setting
// points at GitHubURL or GitLabURL has its forge kind detected from the
// setting. It is imported by test code only.
package forgetest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

// The path prefixes the two forges' APIs are served below.
const (
	GitHubPrefix = "/github"
	GitLabPrefix = "/gitlab/api/v4"
)

// Request is a request the fake received.
type Request struct {
	Method string
	// Host is the host the request named; a request sent to a proxy names
	// the forge it is for.
	Host string
	// Path is the decoded path and RawPath the path as sent, which differ
	// when the path holds an escaped character such as %2F.
	Path     string
	RawPath  string
	RawQuery string
	Header   http.Header
	Body     []byte
}

// Answer is a scripted answer. The zero Answer is 200 with an empty JSON
// object.
type Answer struct {
	// Path, when set, restricts the answer to a request for this path (the
	// decoded path); otherwise the next request takes it, whatever its path.
	Path string
	// Status is the answer's status; 0 means 200, or 302 with Redirect.
	Status int
	Header http.Header
	// Body is the answer's body; "" means "{}" for a 2xx answer.
	Body string
	// Redirect, when set, is the Location the answer redirects to: an
	// absolute URL or a path on the fake.
	Redirect string
	// Delay holds the answer back for this long before it is written.
	Delay time.Duration
	// Hold holds the answer back until the test ends or the client leaves;
	// nothing is written.
	Hold bool
}

// Fake is a fake forge. It is safe for concurrent use.
type Fake struct {
	// URL is the base URL of the fake, without a trailing slash.
	URL string

	server  *httptest.Server
	release chan struct{}

	mu       sync.Mutex
	requests []Request
	answers  []Answer
}

// New starts a fake forge over TLS until the test ends. Transport is the
// transport that trusts its certificate.
func New(t testing.TB) *Fake {
	return start(t, httptest.NewTLSServer)
}

// NewPlain starts a fake forge over plain HTTP until the test ends.
func NewPlain(t testing.TB) *Fake {
	return start(t, httptest.NewServer)
}

func start(t testing.TB, serve func(http.Handler) *httptest.Server) *Fake {
	t.Helper()
	f := &Fake{release: make(chan struct{})}
	f.server = serve(http.HandlerFunc(f.serveHTTP))
	f.URL = f.server.URL
	// Cleanups run last first: held answers are let go before the server
	// waits for its handlers.
	t.Cleanup(f.server.Close)
	t.Cleanup(func() { close(f.release) })
	return f
}

// Transport is a transport that reaches the fake, trusting its
// certificate when it serves TLS.
func (f *Fake) Transport() http.RoundTripper {
	return f.server.Client().Transport
}

// GitHubURL is the API URL of the fake's GitHub.
func (f *Fake) GitHubURL() string { return f.URL + GitHubPrefix }

// GitLabURL is the API URL of the fake's GitLab.
func (f *Fake) GitLabURL() string { return f.URL + GitLabPrefix }

// Script queues answers: each request takes the first queued answer whose
// Path is empty or equal to its path. A request no queued answer fits is
// answered 404 with a forge's "Not Found" message.
func (f *Fake) Script(answers ...Answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, answers...)
}

// Requests are the requests the fake received, oldest first.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// Count is how many requests the fake received.
func (f *Fake) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// Last is the latest request the fake received; it fails the test when
// there is none.
func (f *Fake) Last(t testing.TB) Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("the fake forge received no request")
	}
	return f.requests[len(f.requests)-1]
}

func (f *Fake) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a, ok := f.record(r, body)
	if !ok {
		writeAnswer(w, Answer{Status: http.StatusNotFound, Body: `{"message":"Not Found"}`})
		return
	}
	if a.Hold {
		select {
		case <-f.release:
		case <-r.Context().Done():
		}
		return
	}
	if a.Delay > 0 {
		select {
		case <-time.After(a.Delay):
		case <-f.release:
			return
		case <-r.Context().Done():
			return
		}
	}
	writeAnswer(w, a)
}

// record keeps the request and takes the answer it gets, in one step, so
// that concurrent requests take the queued answers in their order.
func (f *Fake) record(r *http.Request, body []byte) (Answer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, Request{
		Method:   r.Method,
		Host:     r.Host,
		Path:     r.URL.Path,
		RawPath:  r.URL.EscapedPath(),
		RawQuery: r.URL.RawQuery,
		Header:   r.Header.Clone(),
		Body:     body,
	})
	for i, a := range f.answers {
		if a.Path == "" || a.Path == r.URL.Path {
			f.answers = slices.Delete(f.answers, i, i+1)
			return a, true
		}
	}
	return Answer{}, false
}

func writeAnswer(w http.ResponseWriter, a Answer) {
	for name, values := range a.Header {
		w.Header()[http.CanonicalHeaderKey(name)] = values
	}
	status := a.Status
	if a.Redirect != "" {
		w.Header().Set("Location", a.Redirect)
		if status == 0 {
			status = http.StatusFound
		}
	}
	if status == 0 {
		status = http.StatusOK
	}
	body := a.Body
	if body == "" && status >= 200 && status < 300 {
		body = "{}"
	}
	if body != "" && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	w.Write([]byte(body))
}
