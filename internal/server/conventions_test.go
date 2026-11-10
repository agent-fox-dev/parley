package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type liveResponse struct {
	status int
	header http.Header
	doc    map[string]any
}

func liveDo(t *testing.T, method, target string, body string, headers map[string]string) liveResponse {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, target, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	return liveResponse{status: resp.StatusCode, header: resp.Header, doc: doc}
}

func TestConventions_AnUnknownPathOnARunningHub(t *testing.T) {
	opts := DefaultOptions()
	logger, logs := debugLogger()
	opts.Logger = logger
	srv := httptest.NewServer(New(opts).Handler())
	defer srv.Close()

	r := liveDo(t, "GET", srv.URL+"/api/v1/no-such-thing", "", nil)
	if r.status != 404 {
		t.Fatalf("status = %d, want 404", r.status)
	}
	e, _ := r.doc["error"].(map[string]any)
	if e["code"] != float64(404) || e["error_type"] != "not_found" || e["message"] == "" {
		t.Errorf("error = %v, want not_found 404 with a message", e)
	}
	if r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", r.header.Get("Cache-Control"))
	}
	id := r.header.Get("X-Request-ID")
	var matched int
	for _, l := range logs.lines() {
		var line struct {
			Path      string `json:"path"`
			Status    int    `json:"status"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal([]byte(l), &line) == nil && line.Path == "/api/v1/no-such-thing" {
			matched++
			if line.RequestID != id || line.Status != 404 {
				t.Errorf("log line %s, want request id %s and status 404", l, id)
			}
		}
	}
	if matched != 1 {
		t.Errorf("%d log lines for the path, want 1", matched)
	}

	h := liveDo(t, "GET", srv.URL+"/healthz", "", nil)
	if h.status != 200 || h.header.Get("X-Frame-Options") != "DENY" || h.header.Get("Cache-Control") != "no-cache" ||
		h.header.Get("X-Content-Type-Options") != "nosniff" || h.header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("/healthz = %d with headers %v", h.status, h.header)
	}
}

func TestConventions_ARetriedCreateAndAPagedList(t *testing.T) {
	items := newCollection(4)
	opts := DefaultOptions()
	logger, _ := debugLogger()
	opts.Logger = logger
	s := New(opts)
	s.SetAuthenticator(func(r *http.Request) (*Caller, error) {
		if r.Header.Get("Authorization") != "Bearer agent" {
			return nil, Unauthenticated("no")
		}
		return &Caller{CredentialID: "k-agent", UserID: "u-agent"}, nil
	})
	created := 0
	s.Register(Route{Method: "POST", Pattern: "/things", Create: true, ShowOnce: []string{"token"}, Handler: func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Name string `json:"name"`
		}
		if err := DecodeJSON(r, &in); err != nil {
			return err
		}
		created++
		id := fmt.Sprintf("item-%04d", 1000+created)
		items.insert(id)
		WriteJSON(w, 201, map[string]string{"id": id, "name": in.Name, "token": "secret-" + id})
		return nil
	}})
	s.Register(Route{Method: "GET", Pattern: "/things", Handler: items.handler})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	auth := map[string]string{"Authorization": "Bearer agent", "Idempotency-Key": "k"}
	a := liveDo(t, "POST", srv.URL+"/api/v1/things", `{"name":"a"}`, auth)
	if a.status != 201 || a.doc["token"] == "" || a.doc["token"] == nil {
		t.Fatalf("create = %d %v, want 201 with a token", a.status, a.doc)
	}
	b := liveDo(t, "POST", srv.URL+"/api/v1/things", `{"name":"a"}`, auth)
	if _, hasToken := b.doc["token"]; b.status != 200 || b.doc["id"] != a.doc["id"] || hasToken {
		t.Errorf("retry = %d %v, want 200 with the same id and no token", b.status, b.doc)
	}
	c := liveDo(t, "POST", srv.URL+"/api/v1/things", `{"name":"other"}`, auth)
	if e, _ := c.doc["error"].(map[string]any); c.status != 409 || e["error_type"] != "conflict" {
		t.Errorf("other body = %d %v, want 409 conflict", c.status, c.doc)
	}

	var ids []string
	var sizes []int
	next := srv.URL + "/api/v1/things?limit=2"
	for {
		p := liveDo(t, "GET", next, "", map[string]string{"Authorization": "Bearer agent"})
		if p.status != 200 {
			t.Fatalf("list = %d %v", p.status, p.doc)
		}
		page, _ := p.doc["items"].([]any)
		sizes = append(sizes, len(page))
		for _, it := range page {
			ids = append(ids, it.(map[string]any)["id"].(string))
		}
		cur, ok := p.doc["next_cursor"].(string)
		if !ok {
			break
		}
		next = srv.URL + "/api/v1/things?limit=2&cursor=" + url.QueryEscape(cur)
	}
	if fmt.Sprint(sizes) != "[2 2 1]" {
		t.Errorf("page sizes = %v, want [2 2 1]", sizes)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("id %s listed twice", id)
		}
		seen[id] = true
	}
	if len(ids) != 5 || !seen[a.doc["id"].(string)] || created != 1 {
		t.Errorf("listed %v after %d creates, want five ids including %v", ids, created, a.doc["id"])
	}
}

func TestConventions_AScriptRunsIntoTheRateLimitAndRecovers(t *testing.T) {
	clock := newFakeClock()
	opts := DefaultOptions()
	opts.RequestsPerMinute = 5
	opts.Now = clock.Now
	logger, _ := debugLogger()
	opts.Logger = logger
	s := New(opts)
	s.SetAuthenticator(bearerAuth(nil))
	s.Register(Route{Method: "GET", Pattern: "/things", Handler: (&countingHandler{status: 200}).handle})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	cred := map[string]string{"Authorization": "Bearer script"}
	for i := 1; i <= 5; i++ {
		if r := liveDo(t, "GET", srv.URL+"/api/v1/things", "", cred); r.status != 200 {
			t.Fatalf("request %d = %d, want 200", i, r.status)
		}
	}
	r := liveDo(t, "GET", srv.URL+"/api/v1/things", "", cred)
	e, _ := r.doc["error"].(map[string]any)
	if r.status != 429 || e["error_type"] != "rate_limited" {
		t.Fatalf("sixth = %d %v, want 429 rate_limited", r.status, r.doc)
	}
	n, err := strconv.Atoi(r.header.Get("Retry-After"))
	if err != nil || n < 1 || n > 60 {
		t.Fatalf("Retry-After = %q, want 1 to 60", r.header.Get("Retry-After"))
	}
	if h := liveDo(t, "GET", srv.URL+"/healthz", "", nil); h.status != 200 {
		t.Errorf("/healthz while over the limit = %d", h.status)
	}
	clock.advance(time.Duration(n) * time.Second)
	if r := liveDo(t, "GET", srv.URL+"/api/v1/things", "", cred); r.status != 200 {
		t.Errorf("after Retry-After = %d, want 200", r.status)
	}
}
