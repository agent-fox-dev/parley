// Package forgetest is a fake forge for tests: an HTTP server, over TLS or
// plain HTTP, that records every request it receives and answers what the
// test scripts, or the objects and lists the test sets. It serves GitHub's
// API below the prefix /github and GitLab's below /gitlab/api/v4, so a
// workspace whose API URL setting points at GitHubURL or GitLabURL has its
// forge kind detected from the setting. It is imported by test code only.
package forgetest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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
	// entities are what SetObject and SetList set, by the path as sent.
	entities map[string]entity
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
// Path is empty or equal to its path. A request no queued answer fits gets
// the object or list set for its path (SetObject, SetList), else 404 with a
// forge's "Not Found" message.
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
	a, e, ok := f.record(r, body)
	switch {
	case e != nil:
		f.serveEntity(w, r, *e)
		return
	case !ok:
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
// that concurrent requests take the queued answers in their order: a
// queued answer that fits it, else the entity a GET's path names.
func (f *Fake) record(r *http.Request, body []byte) (Answer, *entity, bool) {
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
			return a, nil, true
		}
	}
	if e, ok := f.entities[r.URL.EscapedPath()]; ok && r.Method == http.MethodGet {
		return Answer{}, &e, true
	}
	return Answer{}, nil, false
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

// SetObject makes the fake answer GET path with value in JSON, until the
// path is set again. path is the path as sent, with its escapes, such as
// GitLabPrefix + "/projects/group%2Fsub%2Fproj": a request that does not
// escape it is not answered.
func (f *Fake) SetObject(path string, value any) {
	f.set(path, entity{object: mustJSON(value)})
}

// SetList makes the fake answer GET path, the path as sent as for
// SetObject, with entries as a JSON array, until the path is set again. It
// pages them as both forges do: the query parameter per_page is the page
// size (30 when it is absent), page the page's number from 1, and while
// entries follow the page, the Link header names the next page with
// rel="next". The fake does not filter: every other query parameter is
// recorded and ignored.
func (f *Fake) SetList(path string, entries ...any) {
	list := make([]json.RawMessage, len(entries))
	for i, e := range entries {
		list[i] = mustJSON(e)
	}
	f.set(path, entity{list: list, isList: true})
}

// SetListIn is SetList for a list the forge answers inside an object, as
// the member member, the way GitHub answers check runs.
func (f *Fake) SetListIn(path, member string, entries ...any) {
	list := make([]json.RawMessage, len(entries))
	for i, e := range entries {
		list[i] = mustJSON(e)
	}
	f.set(path, entity{list: list, isList: true, member: member})
}

// entity is what the fake serves at a path: an object, or a list it pages.
type entity struct {
	object json.RawMessage
	list   []json.RawMessage
	isList bool
	// member, when set, is the member of an object the list's page is
	// answered in, beside total_count.
	member string
}

func (f *Fake) set(path string, e entity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entities == nil {
		f.entities = map[string]entity{}
	}
	f.entities[path] = e
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("forgetest: encode %T: %v", v, err))
	}
	return b
}

// serveEntity answers r with e, a page of it when it is a list.
func (f *Fake) serveEntity(w http.ResponseWriter, r *http.Request, e entity) {
	if !e.isList {
		writeAnswer(w, Answer{Body: string(e.object)})
		return
	}
	q := r.URL.Query()
	size, err := strconv.Atoi(q.Get("per_page"))
	if err != nil || size < 1 {
		size = 30
	}
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	start := min(len(e.list), (page-1)*size)
	end := min(len(e.list), start+size)
	entries := append([]json.RawMessage{}, e.list[start:end]...)
	var body []byte
	if e.member != "" {
		body, _ = json.Marshal(map[string]any{"total_count": len(e.list), e.member: entries})
	} else {
		body, _ = json.Marshal(entries)
	}
	a := Answer{Body: string(body)}
	if end < len(e.list) {
		q.Set("page", strconv.Itoa(page+1))
		next := f.URL + r.URL.EscapedPath() + "?" + q.Encode()
		a.Header = http.Header{"Link": {fmt.Sprintf(`<%s>; rel="next", <%s>; rel="first"`, next, f.URL+r.URL.EscapedPath())}}
	}
	writeAnswer(w, a)
}
