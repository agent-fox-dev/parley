package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// idempotencyCallers are the callers the fixture's authenticator reports, by
// Authorization header.
var idempotencyCallers = map[string]Caller{
	"Bearer u1":   {CredentialID: "c1", UserID: "u1"},
	"Bearer u2":   {CredentialID: "c2", UserID: "u2"},
	"Bearer root": {CredentialID: "admin", Admin: true},
	"Bearer svcA": {CredentialID: "svc-a"},
	"Bearer svcB": {CredentialID: "svc-b"},
	"Bearer anon": {},
}

// createFixture is a server with a clock the test moves, an in-memory record
// store the test reads, a log the test reads and a handler that counts its
// calls.
type createFixture struct {
	s     *Server
	store *MemoryIdempotencyStore
	clock *testClock
	log   *logBuffer
	calls atomic.Int64
}

func newCreateFixture() *createFixture {
	f := &createFixture{store: NewMemoryIdempotencyStore(), clock: newTestClock(), log: &logBuffer{}}
	f.s = New(Options{Now: f.clock.Now, IdempotencyStore: f.store, Logger: f.log.logger()})
	f.s.SetAuthenticator(func(r *http.Request) (Caller, error) {
		c, ok := idempotencyCallers[r.Header.Get("Authorization")]
		if !ok {
			return Caller{}, Unauthenticated("unknown credential")
		}
		return c, nil
	})
	return f
}

// create is a create handler: it answers 201 with a fresh id and a secret
// token for each call.
func (f *createFixture) create(w http.ResponseWriter, _ *http.Request) error {
	n := f.calls.Add(1)
	WriteJSON(w, http.StatusCreated, map[string]string{
		"id":    strconv.FormatInt(n, 10),
		"token": "secret-" + strconv.FormatInt(n, 10),
	})
	return nil
}

// createRoute declares POST pattern as a create route served by f.create.
func (f *createFixture) createRoute(pattern string, showOnce ...string) Route {
	return Route{Method: http.MethodPost, Pattern: pattern, Handler: f.create, Create: true, ShowOnce: showOnce}
}

// post sends a JSON body to path as the caller of auth. With no key the
// request has no Idempotency-Key header; an empty key sends the header empty.
func (f *createFixture) post(path, auth, body string, key ...string) *httptest.ResponseRecorder {
	header := http.Header{"Content-Type": {"application/json"}, "Authorization": {auth}}
	if len(key) > 0 {
		header["Idempotency-Key"] = []string{key[0]}
	}
	return do(f.s.Handler(), newRequest(http.MethodPost, path, header, body))
}

// records returns every record the store holds.
func (f *createFixture) records() []IdempotencyRecord {
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	var recs []IdempotencyRecord
	for _, rec := range f.store.records {
		recs = append(recs, rec)
	}
	return recs
}

// gateRefs returns how many requests hold or await the gate slot of id.
func (f *createFixture) gateRefs(id recordKey) int {
	g := &f.s.idem.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if slot := g.held[id]; slot != nil {
		return slot.refs
	}
	return 0
}

// recordKeys returns the keys of every record the store holds.
func (f *createFixture) recordKeys() map[string]bool {
	keys := make(map[string]bool)
	for _, rec := range f.records() {
		keys[rec.Key] = true
	}
	return keys
}

func TestIdempotency_RecordsASuccessfulCreateWithoutItsShowOnceMembers(t *testing.T) {
	t.Parallel()
	f := newCreateFixture()
	f.s.Register(f.createRoute("/things", "token"))

	rec := f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "k1")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.Bytes())
	}
	first := decodeObject(t, rec.Body.Bytes())
	if first["token"] != "secret-1" || first["id"] != "1" {
		t.Errorf("first response = %v, want the handler's id 1 and token secret-1", first)
	}

	stored, ok, err := f.store.Find(t.Context(), "k1", idempotencyCaller(idempotencyCallers["Bearer u1"]), "POST /api/v1/things")
	if err != nil || !ok {
		t.Fatalf("Find(k1, u1, POST /api/v1/things) = %t, %v; want the record", ok, err)
	}
	if n := len(f.records()); n != 1 {
		t.Errorf("store holds %d records, want 1", n)
	}
	body := decodeObject(t, stored.Body)
	if _, has := body["token"]; has {
		t.Errorf("stored body %s has the show-once member token", stored.Body)
	}
	if bytes.Contains(stored.Body, []byte("secret-1")) {
		t.Errorf("stored body %s holds the secret", stored.Body)
	}
	if body["id"] != "1" {
		t.Errorf("stored id = %v, want 1", body["id"])
	}
	if stored.RequestHash != sha256.Sum256([]byte(`{"name":"a"}`)) {
		t.Errorf("stored request hash is not the SHA-256 of the request body")
	}
	if !stored.WrittenAt.Equal(f.clock.Now()) {
		t.Errorf("written at %v, want %v", stored.WrittenAt, f.clock.Now())
	}
}

func TestIdempotency_ReplaysTheRecordedBodyWithoutRunningTheHandler(t *testing.T) {
	t.Parallel()
	f := newCreateFixture()
	f.s.Register(f.createRoute("/things", "token"))
	first := f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "k1")

	f.clock.Advance(23*time.Hour + 59*time.Minute)
	replay := f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "k1")

	if replay.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200 (body %s)", replay.Code, replay.Body.Bytes())
	}
	if ct := replay.Header().Get("Content-Type"); ct != jsonContentType {
		t.Errorf("replay Content-Type = %q, want %q", ct, jsonContentType)
	}
	if got, want := decodeObject(t, replay.Body.Bytes())["id"], decodeObject(t, first.Body.Bytes())["id"]; got != want {
		t.Errorf("replayed id = %v, want the first response's %v", got, want)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("handler ran %d times, want 1", n)
	}
}

func TestIdempotency_OmitsShowOnceMembersFromAReplay(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		showOnce string
		body     string
		secret   string
		want     string
	}{
		{"top-level member", "token", `{"id":"1","token":"secret-1"}`, "secret-1", `{"id":"1"}`},
		{
			"nested member", "token.token",
			`{"workspace":{"slug":"w","origin":{"url":"u"}},"token":{"token_id":"t","token":"secret-2"}}`, "secret-2",
			`{"workspace":{"slug":"w","origin":{"url":"u"}},"token":{"token_id":"t"}}`,
		},
		{"member the body does not have", "password", `{"id":"1","count":12,"tags":["a"]}`, "", `{"id":"1","count":12,"tags":["a"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCreateFixture()
			f.s.Register(Route{
				Method: http.MethodPost, Pattern: "/things", Create: true, ShowOnce: []string{tc.showOnce},
				Handler: func(w http.ResponseWriter, _ *http.Request) error {
					f.calls.Add(1)
					w.Header().Set("Content-Type", contentTypeJSON)
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(tc.body))
					return nil
				},
			})

			first := f.post("/api/v1/things", "Bearer u1", `{}`, "k")
			replay := f.post("/api/v1/things", "Bearer u1", `{}`, "k")

			if first.Body.String() != tc.body {
				t.Errorf("first response = %s, want the handler's body %s unchanged", first.Body.Bytes(), tc.body)
			}
			if replay.Code != http.StatusOK {
				t.Fatalf("replay status = %d, want 200 (body %s)", replay.Code, replay.Body.Bytes())
			}
			if got, want := decodeObject(t, replay.Body.Bytes()), decodeObject(t, []byte(tc.want)); !reflect.DeepEqual(got, want) {
				t.Errorf("replay = %s, want %s", replay.Body.Bytes(), tc.want)
			}
			if tc.secret != "" && strings.Contains(replay.Body.String(), tc.secret) {
				t.Errorf("replay %s holds the secret %q", replay.Body.Bytes(), tc.secret)
			}
			if n := f.calls.Load(); n != 1 {
				t.Errorf("handler ran %d times, want 1", n)
			}
		})
	}
}

func TestIdempotency_ReplaysTheHeadersTheHandlerSet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		showOnce    string
		contentType string
		body        string
		wantBody    string
	}{
		{"JSON answer with a Location", "", contentTypeJSON, `{"id":"1"}`, `{"id":"1"}`},
		{"answer that is not JSON", "", "text/plain; charset=utf-8", "created 1", "created 1"},
		{"answer whose show-once member is removed", "token", contentTypeJSON, `{"id":"1","token":"secret-1"}`, "{\"id\":\"1\"}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCreateFixture()
			rt := Route{
				Method: http.MethodPost, Pattern: "/things", Create: true,
				Handler: func(w http.ResponseWriter, _ *http.Request) error {
					f.calls.Add(1)
					w.Header().Set("Content-Type", tc.contentType)
					w.Header().Set("Location", "/api/v1/things/1")
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)))
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(tc.body))
					w.Header().Set("X-Too-Late", "not sent")
					return nil
				},
			}
			if tc.showOnce != "" {
				rt.ShowOnce = []string{tc.showOnce}
			}
			f.s.Register(rt)

			first := f.post("/api/v1/things", "Bearer u1", `{}`, "k")
			replay := f.post("/api/v1/things", "Bearer u1", `{}`, "k")

			if first.Code != http.StatusCreated {
				t.Fatalf("first status = %d, want 201 (body %s)", first.Code, first.Body.Bytes())
			}
			if replay.Code != http.StatusOK {
				t.Fatalf("replay status = %d, want 200 (body %s)", replay.Code, replay.Body.Bytes())
			}
			if got := replay.Body.String(); got != tc.wantBody {
				t.Errorf("replay body = %q, want %q", got, tc.wantBody)
			}
			h := replay.Header()
			if got := h.Get("Content-Type"); got != tc.contentType {
				t.Errorf("replay Content-Type = %q, want the handler's %q", got, tc.contentType)
			}
			if got := h.Get("Location"); got != "/api/v1/things/1" {
				t.Errorf("replay Location = %q, want the handler's /api/v1/things/1", got)
			}
			if got := h.Get("Content-Length"); got != "" && got != strconv.Itoa(len(tc.wantBody)) {
				t.Errorf("replay Content-Length = %s, want none or the replayed body's %d", got, len(tc.wantBody))
			}
			if got := h.Get("X-Too-Late"); got != "" {
				t.Errorf("replay X-Too-Late = %q, want none: the handler set it after answering", got)
			}
			if got, orig := h.Get("X-Request-ID"), first.Header().Get("X-Request-ID"); got == "" || got == orig {
				t.Errorf("replay X-Request-ID = %q, want its own id, not the first request's %q", got, orig)
			}
			if n := f.calls.Load(); n != 1 {
				t.Errorf("handler ran %d times, want 1", n)
			}
		})
	}
}

func TestIdempotency_RefusesTheSameKeyWithAnotherBody(t *testing.T) {
	t.Parallel()
	f := newCreateFixture()
	f.s.Register(f.createRoute("/things", "token"))
	f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "k")

	rec := f.post("/api/v1/things", "Bearer u1", `{"name":"b"}`, "k")

	assertError(t, rec, http.StatusConflict, "conflict")
	if n := f.calls.Load(); n != 1 {
		t.Errorf("handler ran %d times, want 1", n)
	}
	recs := f.records()
	if len(recs) != 1 || recs[0].RequestHash != sha256.Sum256([]byte(`{"name":"a"}`)) {
		t.Errorf("store holds %d records; want the original record unchanged", len(recs))
	}
	if again := f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "k"); again.Code != http.StatusOK {
		t.Errorf("the original body after the conflict: status = %d, want 200", again.Code)
	}
}

func TestIdempotency_RefusesAnEmptyOrTooLongKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		key         []string
		wantStatus  int
		wantRecords int
	}{
		{"empty", []string{""}, http.StatusBadRequest, 0},
		{"256 characters", []string{strings.Repeat("a", 256)}, http.StatusBadRequest, 0},
		{"one character", []string{"a"}, http.StatusCreated, 1},
		{"255 characters", []string{strings.Repeat("a", 255)}, http.StatusCreated, 1},
		{"absent", nil, http.StatusCreated, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCreateFixture()
			f.s.Register(f.createRoute("/things"))

			rec := f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, tc.key...)

			if tc.wantStatus == http.StatusBadRequest {
				assertError(t, rec, http.StatusBadRequest, "invalid_request")
				if n := f.calls.Load(); n != 0 {
					t.Errorf("handler ran %d times, want 0", n)
				}
			} else if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.Bytes())
			}
			if n := len(f.records()); n != tc.wantRecords {
				t.Errorf("store holds %d records, want %d", n, tc.wantRecords)
			}
		})
	}
}

func TestIdempotency_RecordsNothingWhenTheRequestFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		handlerErr error
		checkErr   error
		wantStatus int
		wantCalls  int64
	}{
		{"handler answers 400", InvalidRequest("name is required"), nil, http.StatusBadRequest, 2},
		{"handler fails with a plain error", errors.New("disk full"), nil, http.StatusInternalServerError, 2},
		{"handler answers 409", Conflict("name taken"), nil, http.StatusConflict, 2},
		{"access check refuses", nil, Forbidden("not yours"), http.StatusForbidden, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCreateFixture()
			f.s.Register(Route{
				Method: http.MethodPost, Pattern: "/things", Create: true,
				Handler: func(w http.ResponseWriter, r *http.Request) error {
					if f.calls.Load() == 0 && tc.handlerErr != nil {
						f.calls.Add(1)
						return tc.handlerErr
					}
					return f.create(w, r)
				},
			})
			var checks atomic.Int64
			f.s.AddAccessCheck(func(*http.Request) error {
				if checks.Add(1) == 1 && tc.checkErr != nil {
					return tc.checkErr
				}
				return nil
			})

			first := f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "k")
			second := f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "k")
			third := f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "k")

			if first.Code != tc.wantStatus {
				t.Errorf("first status = %d, want %d", first.Code, tc.wantStatus)
			}
			if second.Code != http.StatusCreated {
				t.Errorf("second status = %d, want 201 (body %s)", second.Code, second.Body.Bytes())
			}
			if third.Code != http.StatusOK {
				t.Errorf("third status = %d, want 200", third.Code)
			}
			if n := f.calls.Load(); n != tc.wantCalls {
				t.Errorf("handler ran %d times, want %d", n, tc.wantCalls)
			}
		})
	}
}

func TestIdempotency_LetsOneOfConcurrentRequestsCreate(t *testing.T) {
	t.Parallel()
	const n = 20
	for _, failFirst := range []bool{false, true} {
		t.Run("first run fails "+strconv.FormatBool(failFirst), func(t *testing.T) {
			t.Parallel()
			f := newCreateFixture()
			f.s.Register(Route{
				Method: http.MethodPost, Pattern: "/things", Create: true,
				Handler: func(w http.ResponseWriter, _ *http.Request) error {
					call := f.calls.Add(1)
					time.Sleep(20 * time.Millisecond)
					if failFirst && call == 1 {
						return errors.New("store unavailable")
					}
					WriteJSON(w, http.StatusCreated, map[string]string{"id": strconv.FormatInt(call, 10)})
					return nil
				},
			})

			results := make([]*httptest.ResponseRecorder, n)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range n {
				wg.Go(func() {
					<-start
					results[i] = f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "k")
				})
			}
			close(start)
			wg.Wait()

			statuses := make(map[int]int)
			ids := make(map[any]bool)
			for _, rec := range results {
				statuses[rec.Code]++
				if rec.Code != http.StatusInternalServerError {
					ids[decodeObject(t, rec.Body.Bytes())["id"]] = true
				}
			}
			want := map[int]int{http.StatusCreated: 1, http.StatusOK: n - 1}
			wantCalls := int64(1)
			if failFirst {
				want = map[int]int{http.StatusInternalServerError: 1, http.StatusCreated: 1, http.StatusOK: n - 2}
				wantCalls = 2
			}
			if !reflect.DeepEqual(statuses, want) {
				t.Errorf("statuses = %v, want %v", statuses, want)
			}
			if c := f.calls.Load(); c != wantCalls {
				t.Errorf("handler ran %d times, want %d", c, wantCalls)
			}
			if len(ids) != 1 {
				t.Errorf("responses carry the ids %v, want one id", ids)
			}
		})
	}
}

func TestIdempotency_DoesNotHoldARequestWithAnotherKeyOrCaller(t *testing.T) {
	t.Parallel()
	f := newCreateFixture()
	entered := make(chan struct{})
	release := make(chan struct{})
	f.s.Register(Route{
		Method: http.MethodPost, Pattern: "/things", Create: true,
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			if r.Header.Get("Idempotency-Key") == "held" && r.Header.Get("Authorization") == "Bearer u1" {
				close(entered)
				<-release
			}
			return f.create(w, r)
		},
	})
	held := make(chan int, 1)
	go func() { held <- f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "held").Code }()
	waitFor(t, entered, 5*time.Second, "held handler entered")

	others := make(chan int, 2)
	go func() { others <- f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "other").Code }()
	go func() { others <- f.post("/api/v1/things", "Bearer u2", `{"name":"a"}`, "held").Code }()
	for range 2 {
		if code := waitFor(t, others, 5*time.Second, "a request with another key or caller answered"); code != http.StatusCreated {
			t.Errorf("status = %d, want 201", code)
		}
	}
	close(release)
	if code := waitFor(t, held, 5*time.Second, "held request answered"); code != http.StatusCreated {
		t.Errorf("held request status = %d, want 201", code)
	}
}

func TestIdempotency_LeavesARequestWhoseClientLeftTheGateUnanswered(t *testing.T) {
	t.Parallel()
	f := newCreateFixture()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.s.Register(Route{
		Method: http.MethodPost, Pattern: "/things", Create: true,
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			once.Do(func() {
				close(entered)
				<-release
			})
			return f.create(w, r)
		},
	})
	held := make(chan int, 1)
	go func() { held <- f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "k").Code }()
	waitFor(t, entered, 5*time.Second, "held handler entered")

	ctx, cancel := context.WithCancel(context.Background())
	header := http.Header{"Content-Type": {"application/json"}, "Authorization": {"Bearer u1"}, "Idempotency-Key": {"k"}}
	r := newRequest(http.MethodPost, "/api/v1/things", header, `{"name":"a"}`).WithContext(ctx)
	waiter := make(chan *httptest.ResponseRecorder, 1)
	go func() { waiter <- do(f.s.Handler(), r) }()
	deadline := time.Now().Add(5 * time.Second)
	for f.gateRefs(recordKey{key: "k", caller: "user:u1", route: "POST /api/v1/things"}) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the second request never waited at the gate")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	rec := waitFor(t, waiter, 5*time.Second, "the waiting request returned")
	if rec.Code == http.StatusInternalServerError || rec.Body.Len() != 0 {
		t.Errorf("the request whose client left was answered %d %q, want no answer", rec.Code, rec.Body.String())
	}
	for _, line := range f.log.lines() {
		if doc := decodeLine(t, line); doc["level"] == "ERROR" {
			t.Errorf("logged %s, want no error line for a client that left", line)
		}
	}

	close(release)
	if code := waitFor(t, held, 5*time.Second, "held request answered"); code != http.StatusCreated {
		t.Errorf("held request status = %d, want 201", code)
	}
	if c := f.calls.Load(); c != 1 {
		t.Errorf("handler ran %d times, want 1", c)
	}
}

func TestIdempotency_TreatsAKeyRecordedElsewhereOrExpiredAsNew(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		firstPath string
		path      string
		auth      string
		body      string
		advance   time.Duration
	}{
		{"another caller", "/api/v1/things", "/api/v1/things", "Bearer u2", `{"name":"a"}`, 0},
		{"another create route and body", "/api/v1/things", "/api/v1/others", "Bearer u1", `{"name":"b"}`, 0},
		{"another request path of the same route", "/api/v1/owners/a/things", "/api/v1/owners/b/things", "Bearer u1", `{"name":"a"}`, 0},
		{"the admin token", "/api/v1/things", "/api/v1/things", "Bearer root", `{"name":"a"}`, 0},
		{"a record exactly 24 hours old", "/api/v1/things", "/api/v1/things", "Bearer u1", `{"name":"a"}`, 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCreateFixture()
			f.s.Register(f.createRoute("/things"))
			f.s.Register(f.createRoute("/others"))
			f.s.Register(f.createRoute("/owners/:owner/things"))
			if first := f.post(tc.firstPath, "Bearer u1", `{"name":"a"}`, "k"); first.Code != http.StatusCreated {
				t.Fatalf("recording request: status = %d, want 201", first.Code)
			}

			f.clock.Advance(tc.advance)
			rec := f.post(tc.path, tc.auth, tc.body, "k")

			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want the handler's 201 (body %s)", rec.Code, rec.Body.Bytes())
			}
			if id := decodeObject(t, rec.Body.Bytes())["id"]; id != "2" {
				t.Errorf("id = %v, want the handler's new id 2", id)
			}
			if n := f.calls.Load(); n != 2 {
				t.Errorf("handler ran %d times, want 2", n)
			}
		})
	}

	for _, key := range []string{"k", ""} {
		t.Run("route that is not a create route, key "+strconv.Quote(key), func(t *testing.T) {
			t.Parallel()
			f := newCreateFixture()
			f.s.Register(Route{
				Method: http.MethodPost, Pattern: "/things/:id/block",
				Handler: func(w http.ResponseWriter, _ *http.Request) error {
					WriteJSON(w, http.StatusOK, map[string]int64{"calls": f.calls.Add(1)})
					return nil
				},
			})
			for i := range 2 {
				rec := f.post("/api/v1/things/42/block", "Bearer u1", `{"reason":"spam"}`, key)
				if rec.Code != http.StatusOK {
					t.Errorf("request %d: status = %d, want the handler's 200 (body %s)", i+1, rec.Code, rec.Body.Bytes())
				}
			}
			if n := f.calls.Load(); n != 2 {
				t.Errorf("handler ran %d times, want 2", n)
			}
			if n := len(f.records()); n != 0 {
				t.Errorf("store holds %d records, want 0", n)
			}
		})
	}
}

func TestIdempotency_RemovesExpiredRecordsWhenANewOneIsWritten(t *testing.T) {
	t.Parallel()
	f := newCreateFixture()
	f.s.Register(f.createRoute("/things"))

	f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "k1")
	f.clock.Advance(12 * time.Hour)
	f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "k2")
	f.clock.Advance(12 * time.Hour)
	f.post("/api/v1/things", "Bearer u1", `{"name":"a"}`, "k3")

	if got, want := f.recordKeys(), map[string]bool{"k2": true, "k3": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("store holds the keys %v, want %v", got, want)
	}
}

func TestIdempotency_IgnoresTheKeyOnAPublicCreateRoute(t *testing.T) {
	t.Parallel()
	f := newCreateFixture()
	rt := f.createRoute("/signups")
	rt.Public = true
	f.s.Register(rt)

	for i, key := range []string{"k", "k", ""} {
		if rec := f.post("/api/v1/signups", "", `{"name":"a"}`, key); rec.Code != http.StatusCreated {
			t.Errorf("request %d with key %q: status = %d, want the handler's 201", i+1, key, rec.Code)
		}
	}
	if n := f.calls.Load(); n != 3 {
		t.Errorf("handler ran %d times, want 3", n)
	}
	if n := len(f.records()); n != 0 {
		t.Errorf("store holds %d records, want 0", n)
	}
}

func TestIdempotency_KeepsCallersWithoutAUserApart(t *testing.T) {
	t.Parallel()
	f := newCreateFixture()
	f.s.Register(f.createRoute("/things", "token"))

	a := f.post("/api/v1/things", "Bearer svcA", `{"name":"a"}`, "k")
	b := f.post("/api/v1/things", "Bearer svcB", `{"name":"a"}`, "k")
	if a.Code != http.StatusCreated || b.Code != http.StatusCreated {
		t.Fatalf("two credentials without a user, same key: statuses %d and %d, want 201 and 201 (bodies %s %s)",
			a.Code, b.Code, a.Body.Bytes(), b.Body.Bytes())
	}
	if again := f.post("/api/v1/things", "Bearer svcA", `{"name":"a"}`, "k"); again.Code != http.StatusOK ||
		decodeObject(t, again.Body.Bytes())["id"] != decodeObject(t, a.Body.Bytes())["id"] {
		t.Errorf("replay for the first credential = %d %s, want 200 with its own id", again.Code, again.Body.Bytes())
	}

	for range 2 {
		if r := f.post("/api/v1/things", "Bearer anon", `{"name":"a"}`, "k"); r.Code != http.StatusCreated {
			t.Errorf("caller with neither user nor credential: status %d, want the handler's 201", r.Code)
		}
	}
	if got := f.calls.Load(); got != 4 {
		t.Errorf("handler ran %d times, want 4", got)
	}
	if got := len(f.records()); got != 2 {
		t.Errorf("store holds %d records, want 2 (none for a caller with no identity)", got)
	}
}
