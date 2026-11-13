package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests run the conventions end to end: the real server, its real
// handler and every stage it installs, served on a loopback listener and
// asked over HTTP, as a client of a running hub would.

// served is one answer read off the wire.
type served struct {
	status int
	header http.Header
	body   []byte
}

// send sends method target to the server at base with header and body and
// reads the whole answer.
func send(t *testing.T, base, method, target string, header http.Header, body string) served {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, base+target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, target, err)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read the answer to %s %s: %v", method, target, err)
	}
	return served{status: res.StatusCode, header: res.Header, body: b}
}

func TestConventions_AnswerAnUnknownPathOnARunningServer(t *testing.T) {
	t.Parallel()
	var logs logBuffer
	srv := httptest.NewServer(New(Options{Logger: logs.logger()}).Handler())
	defer srv.Close()

	r := send(t, srv.URL, http.MethodGet, "/api/v1/no-such-thing", nil, "")
	if r.status != http.StatusNotFound {
		t.Fatalf("GET /api/v1/no-such-thing status = %d, want 404 (body %s)", r.status, r.body)
	}
	members := errorMembers(t, r.body)
	if members["code"] != float64(http.StatusNotFound) || members["error_type"] != "not_found" {
		t.Errorf("error = %v, want code 404 and error_type not_found", members)
	}
	if msg, _ := members["message"].(string); msg == "" {
		t.Errorf("error.message = %v, want a non-empty string", members["message"])
	}
	if got := r.header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	assertSecurityHeaders(t, "the 404", r.header)
	id := r.header.Get("X-Request-ID")
	if id == "" {
		t.Fatal("the 404 has no X-Request-ID")
	}

	var lines []map[string]any
	for _, line := range logs.lines() {
		if doc := decodeLine(t, line); doc["path"] == "/api/v1/no-such-thing" {
			lines = append(lines, doc)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("access log holds %d lines for the path, want 1: %v", len(lines), logs.lines())
	}
	if lines[0]["request_id"] != id || lines[0]["status"] != float64(http.StatusNotFound) {
		t.Errorf("access-log line = %v, want request_id %q and status 404", lines[0], id)
	}

	h := send(t, srv.URL, http.MethodGet, "/healthz", nil, "")
	if h.status != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200", h.status)
	}
	if got := h.header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("/healthz Cache-Control = %q, want no-cache", got)
	}
	assertSecurityHeaders(t, "/healthz", h.header)
}

// shelf is the ordered collection behind the create and list routes of the
// retry-and-page flow; items are ordered by id.
type shelf struct {
	mu      sync.Mutex
	ids     []int
	created int
}

func (s *shelf) add() (id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id = s.ids[len(s.ids)-1] + 1
	s.ids = append(s.ids, id)
	s.created++
	return id
}

// after returns up to n ids greater than pos, in order.
func (s *shelf) after(pos int, n int) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for _, id := range s.ids {
		if id > pos && len(out) < n {
			out = append(out, id)
		}
	}
	return out
}

// register puts the create route POST /things, whose token is shown once,
// and the list route GET /things on srv.
func (s *shelf) register(srv *Server) {
	srv.Register(Route{
		Method: http.MethodPost, Pattern: "/things", Create: true, ShowOnce: []string{"token"},
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			var in struct {
				Name string `json:"name"`
			}
			if err := DecodeJSON(r, &in); err != nil {
				return err
			}
			id := s.add()
			WriteJSON(w, http.StatusCreated, map[string]any{
				"id": id, "name": in.Name, "token": fmt.Sprintf("secret-%d", id),
			})
			return nil
		},
	})
	srv.Register(Route{
		Method: http.MethodGet, Pattern: "/things",
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			page, err := ReadPage(r)
			if err != nil {
				return err
			}
			var pos int
			if _, err := page.After(&pos); err != nil {
				return err
			}
			items := []map[string]int{}
			for _, id := range s.after(pos, page.Limit+1) {
				items = append(items, map[string]int{"id": id})
			}
			return WritePage(w, page, items, func(item map[string]int) any { return item["id"] })
		},
	})
}

func TestConventions_ReplayARetriedCreateAndPageThroughTheResult(t *testing.T) {
	t.Parallel()
	agent := Caller{CredentialID: "pat-1", UserID: "u1"}
	s := New(Options{})
	s.SetAuthenticator(func(r *http.Request) (Caller, error) {
		if r.Header.Get("Authorization") != "Bearer agent" {
			return Caller{}, Unauthenticated("unknown credential")
		}
		return agent, nil
	})
	things := &shelf{ids: []int{1, 2, 3, 4}}
	things.register(s)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	create := http.Header{
		"Authorization":   {"Bearer agent"},
		"Content-Type":    {"application/json"},
		"Idempotency-Key": {"k"},
	}
	a := send(t, srv.URL, http.MethodPost, "/api/v1/things", create, `{"name":"a"}`)
	if a.status != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201 (body %s)", a.status, a.body)
	}
	first := decodeObject(t, a.body)
	if tok, _ := first["token"].(string); tok == "" {
		t.Errorf("first create body %s has no token", a.body)
	}

	b := send(t, srv.URL, http.MethodPost, "/api/v1/things", create, `{"name":"a"}`)
	if b.status != http.StatusOK {
		t.Fatalf("retried create status = %d, want 200 (body %s)", b.status, b.body)
	}
	replay := decodeObject(t, b.body)
	if replay["id"] != first["id"] {
		t.Errorf("replayed id = %v, want %v", replay["id"], first["id"])
	}
	if _, ok := replay["token"]; ok {
		t.Errorf("replay %s carries the show-once token", b.body)
	}

	c := send(t, srv.URL, http.MethodPost, "/api/v1/things", create, `{"name":"other"}`)
	if c.status != http.StatusConflict || errorMembers(t, c.body)["error_type"] != "conflict" {
		t.Errorf("create with the key and another body = %d %s, want 409 conflict", c.status, c.body)
	}
	if things.created != 1 {
		t.Errorf("the create ran %d times, want 1", things.created)
	}

	var ids []int
	var sizes []int
	cursor := ""
	for range 10 {
		target := "/api/v1/things?limit=2"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		p := send(t, srv.URL, http.MethodGet, target, http.Header{"Authorization": {"Bearer agent"}}, "")
		if p.status != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200 (body %s)", target, p.status, p.body)
		}
		var page struct {
			Items      []struct{ ID int }
			NextCursor *string `json:"next_cursor"`
		}
		if err := json.Unmarshal(p.body, &page); err != nil {
			t.Fatalf("page %s is not a list document: %v", p.body, err)
		}
		sizes = append(sizes, len(page.Items))
		for _, item := range page.Items {
			ids = append(ids, item.ID)
		}
		if page.NextCursor == nil {
			cursor = ""
			break
		}
		cursor = *page.NextCursor
	}
	if cursor != "" {
		t.Fatalf("the list did not end within 10 pages; pages held %v", sizes)
	}
	if !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Errorf("pages held %v items, want [2 2 1]", sizes)
	}
	if want := []int{1, 2, 3, 4, 5}; !slices.Equal(ids, want) {
		t.Errorf("the pages listed %v, want each of %v once", ids, want)
	}
	if !slices.Contains(ids, int(first["id"].(float64))) {
		t.Errorf("the created id %v is not in the list %v", first["id"], ids)
	}
}

func TestConventions_RefuseAScriptOverTheLimitUntilTheWindowEnds(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	s := New(Options{RequestsPerMinute: 5, Now: clock.Now})
	s.SetAuthenticator(func(r *http.Request) (Caller, error) {
		if r.Header.Get("Authorization") != "Bearer script" {
			return Caller{}, Unauthenticated("unknown credential")
		}
		return Caller{CredentialID: "pat-script", UserID: "dev"}, nil
	})
	s.Register(Route{Method: http.MethodGet, Pattern: "/things", Handler: func(w http.ResponseWriter, _ *http.Request) error {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return nil
	}})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	cred := http.Header{"Authorization": {"Bearer script"}}

	for i := 1; i <= 5; i++ {
		if r := send(t, srv.URL, http.MethodGet, "/api/v1/things", cred, ""); r.status != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200 (body %s)", i, r.status, r.body)
		}
	}
	r := send(t, srv.URL, http.MethodGet, "/api/v1/things", cred, "")
	if r.status != http.StatusTooManyRequests || errorMembers(t, r.body)["error_type"] != "rate_limited" {
		t.Fatalf("request 6 = %d %s, want 429 rate_limited", r.status, r.body)
	}
	wait, err := strconv.Atoi(r.header.Get("Retry-After"))
	if err != nil || wait < 1 || wait > 60 {
		t.Fatalf("Retry-After = %q, want whole seconds from 1 to 60", r.header.Get("Retry-After"))
	}

	if h := send(t, srv.URL, http.MethodGet, "/healthz", nil, ""); h.status != http.StatusOK {
		t.Errorf("GET /healthz while over the limit = %d, want 200", h.status)
	}

	clock.Advance(time.Duration(wait) * time.Second)
	if r := send(t, srv.URL, http.MethodGet, "/api/v1/things", cred, ""); r.status != http.StatusOK {
		t.Errorf("request after waiting Retry-After = %d, want 200 (body %s)", r.status, r.body)
	}
}
