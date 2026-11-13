package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// headersServer builds a server over opts with an authenticator that admits
// "Bearer ok" and refuses anything else with 401, and these routes:
// GET and POST /things answer 200 with {"request_id": <the handler's request
// id>}; GET /panic panics; GET /cached states Cache-Control
// "public, max-age=300" and GET /fresh states "no-cache".
func headersServer(opts Options) http.Handler {
	s := New(opts)
	s.SetAuthenticator(func(r *http.Request) (Caller, error) {
		if r.Header.Get("Authorization") != "Bearer ok" {
			return Caller{}, Unauthenticated("credential not valid")
		}
		return testCaller, nil
	})
	echoID := func(w http.ResponseWriter, r *http.Request) error {
		WriteJSON(w, http.StatusOK, map[string]string{"request_id": RequestID(r.Context())})
		return nil
	}
	s.Register(Route{Method: http.MethodGet, Pattern: "/things", Handler: echoID})
	s.Register(Route{Method: http.MethodPost, Pattern: "/things", Handler: echoID})
	s.Register(Route{Method: http.MethodGet, Pattern: "/panic", Handler: func(http.ResponseWriter, *http.Request) error {
		panic("boom")
	}})
	s.Register(Route{Method: http.MethodGet, Pattern: "/cached", CacheControl: "public, max-age=300", Handler: echoID})
	s.Register(Route{Method: http.MethodGet, Pattern: "/fresh", CacheControl: "no-cache", Handler: echoID})
	return s.Handler()
}

var withCredential = http.Header{"Authorization": {"Bearer ok"}}

// requestIDOf decodes the request id a /things handler answered.
func requestIDOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.Bytes(), err)
	}
	return out.RequestID
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestRequestID_ReusesAValidIncomingIDAsReceived(t *testing.T) {
	t.Parallel()
	h := headersServer(Options{})
	for _, id := range []string{
		"3f2a1bc4-0000-4000-8000-0123456789ab",
		"3F2A1BC4-0000-1000-8000-0123456789AB",
	} {
		t.Run(id, func(t *testing.T) {
			header := http.Header{"Authorization": {"Bearer ok"}, "X-Request-Id": {id}}
			rec := do(h, newRequest(http.MethodGet, "/api/v1/things", header, ""))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.Bytes())
			}
			if got := rec.Header().Get("X-Request-ID"); got != id {
				t.Errorf("X-Request-ID = %q, want %q", got, id)
			}
			if got := requestIDOf(t, rec); got != id {
				t.Errorf("handler's request id = %q, want %q", got, id)
			}
		})
	}
}

func TestRequestID_GeneratesARandomIDWhenNoneOrAnInvalidOneArrives(t *testing.T) {
	t.Parallel()
	h := headersServer(Options{})
	cases := []struct {
		name   string
		header []string // nil: no X-Request-ID header at all
	}{
		{name: "absent"},
		{name: "empty", header: []string{""}},
		{name: "not a uuid", header: []string{"abc"}},
		{name: "braces", header: []string{"{3f2a1bc4-0000-4000-8000-0123456789ab}"}},
		{name: "space for hyphens", header: []string{"3f2a1bc400004000800001234567 89ab"}},
		{name: "300 characters", header: []string{strings.Repeat("a", 300)}},
		{name: "not hexadecimal", header: []string{"3f2a1bc4-0000-4000-8000-0123456789zz"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{"Authorization": {"Bearer ok"}}
			if tc.header != nil {
				header["X-Request-Id"] = tc.header
			}
			var ids []string
			for range 2 {
				rec := do(h, newRequest(http.MethodGet, "/api/v1/things", header, ""))
				id := rec.Header().Get("X-Request-ID")
				if !uuidV4.MatchString(id) {
					t.Errorf("X-Request-ID = %q, want a random (version 4) UUID", id)
				}
				if tc.header != nil && id == tc.header[0] {
					t.Errorf("X-Request-ID = %q, the value received", id)
				}
				if got := requestIDOf(t, rec); got != id {
					t.Errorf("handler's request id = %q, want the response's %q", got, id)
				}
				ids = append(ids, id)
			}
			if ids[0] == ids[1] {
				t.Errorf("two requests got the same id %q", ids[0])
			}
		})
	}
}

func TestHeaders_SetOnEveryResponse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		opts       Options
		before     *http.Request // sent first, its answer ignored
		request    *http.Request
		wantStatus int
	}{
		{name: "success", request: newRequest(http.MethodGet, "/api/v1/things", withCredential, ""), wantStatus: 200},
		{name: "unknown api path", request: newRequest(http.MethodGet, "/api/v1/nope", nil, ""), wantStatus: 404},
		{name: "wrong method", request: newRequest(http.MethodDelete, "/api/v1/things", nil, ""), wantStatus: 405},
		{name: "no credential", request: newRequest(http.MethodGet, "/api/v1/things", nil, ""), wantStatus: 401},
		{
			name:       "body over the limit",
			request:    newRequest(http.MethodPost, "/api/v1/things", http.Header{"Authorization": {"Bearer ok"}, "Content-Type": {"application/json"}}, strings.Repeat("x", 2<<20)),
			wantStatus: 413,
		},
		{
			name:       "over the rate limit",
			opts:       Options{RequestsPerMinute: 1, Now: newTestClock().Now},
			before:     newRequest(http.MethodGet, "/api/v1/things", withCredential, ""),
			request:    newRequest(http.MethodGet, "/api/v1/things", withCredential, ""),
			wantStatus: 429,
		},
		{name: "handler panics", request: newRequest(http.MethodGet, "/api/v1/panic", withCredential, ""), wantStatus: 500},
		{name: "healthz", request: newRequest(http.MethodGet, "/healthz", nil, ""), wantStatus: 200},
		{name: "readyz", request: newRequest(http.MethodGet, "/readyz", nil, ""), wantStatus: 200},
		{name: "version", request: newRequest(http.MethodGet, "/version", nil, ""), wantStatus: 200},
		{name: "unknown path outside the api", request: newRequest(http.MethodGet, "/nope", nil, ""), wantStatus: 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := tc.opts
			opts.Logger = (&logBuffer{}).logger()
			h := headersServer(opts)
			if tc.before != nil {
				do(h, tc.before)
			}
			rec := do(h, tc.request)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.Bytes())
			}
			assertSecurityHeaders(t, "the answer", rec.Header())
			if rec.Header().Get("X-Request-ID") == "" {
				t.Error("X-Request-ID is missing")
			}
		})
	}
}

func TestHeaders_SetCacheControlByRoute(t *testing.T) {
	t.Parallel()
	h := headersServer(Options{})
	cases := []struct {
		name    string
		request *http.Request
		want    string
	}{
		{name: "api success", request: newRequest(http.MethodGet, "/api/v1/things", withCredential, ""), want: "no-store"},
		{name: "api unknown path", request: newRequest(http.MethodGet, "/api/v1/nope", nil, ""), want: "no-store"},
		{name: "api without credential", request: newRequest(http.MethodGet, "/api/v1/things", nil, ""), want: "no-store"},
		{name: "route states a public value", request: newRequest(http.MethodGet, "/api/v1/cached", withCredential, ""), want: "public, max-age=300"},
		{name: "route states no-cache", request: newRequest(http.MethodGet, "/api/v1/fresh", withCredential, ""), want: "no-cache"},
		{name: "healthz", request: newRequest(http.MethodGet, "/healthz", nil, ""), want: "no-cache"},
		{name: "readyz", request: newRequest(http.MethodGet, "/readyz", nil, ""), want: "no-cache"},
		{name: "version", request: newRequest(http.MethodGet, "/version", nil, ""), want: "public, max-age=300"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(h, tc.request)

			if got := rec.Header().Values("Cache-Control"); len(got) != 1 || got[0] != tc.want {
				t.Errorf("Cache-Control = %q, want exactly [%q]", got, tc.want)
			}
		})
	}
}

func TestAccessLog_WritesOneLinePerRequestWithoutTheQuery(t *testing.T) {
	t.Parallel()
	var logs logBuffer
	h := headersServer(Options{Logger: logs.logger()})

	rec := do(h, newRequest(http.MethodGet, "/api/v1/nope", nil, ""))
	lines := logs.lines()
	if len(lines) != 1 {
		t.Fatalf("logged %d lines, want 1: %q", len(lines), lines)
	}
	line := decodeLine(t, lines[0])
	want := map[string]any{
		"method":     "GET",
		"path":       "/api/v1/nope",
		"status":     float64(http.StatusNotFound),
		"request_id": rec.Header().Get("X-Request-ID"),
	}
	for name, v := range want {
		if line[name] != v {
			t.Errorf("%s = %v, want %v (line %s)", name, line[name], v, lines[0])
		}
	}
	if d, ok := line["duration_ms"].(float64); !ok || d < 0 {
		t.Errorf("duration_ms = %v, want a non-negative number", line["duration_ms"])
	}

	do(h, newRequest(http.MethodGet, "/api/v1/nope?token=s3cr3t&x=1", nil, ""))
	lines = logs.lines()
	if len(lines) != 2 {
		t.Fatalf("logged %d lines after the second request, want 2: %q", len(lines), lines)
	}
	if got := decodeLine(t, lines[1])["path"]; got != "/api/v1/nope" {
		t.Errorf("path = %v, want /api/v1/nope", got)
	}
	if strings.ContainsAny(lines[1], "?") || strings.Contains(lines[1], "s3cr3t") {
		t.Errorf("line %s holds the query string", lines[1])
	}
}

func TestAccessLog_LogsHealthProbesOnlyWhenAskedAtDebugLevel(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{false, true} {
		var logs logBuffer
		h := headersServer(Options{Logger: logs.logger(), LogHealthProbes: on})
		for _, p := range []string{"/healthz", "/readyz"} {
			before := len(logs.lines())
			rec := do(h, newRequest(http.MethodGet, p, nil, ""))
			added := logs.lines()[before:]

			switch {
			case !on && len(added) != 0:
				t.Errorf("health-probe logging off: GET %s logged %q, want nothing", p, added)
			case on && len(added) != 1:
				t.Errorf("health-probe logging on: GET %s logged %d lines, want 1: %q", p, len(added), added)
			case on:
				line := decodeLine(t, added[0])
				if line["level"] != "DEBUG" || line["path"] != p || line["request_id"] != rec.Header().Get("X-Request-ID") {
					t.Errorf("health-probe logging on: GET %s logged %s, want a DEBUG line for the path and request id", p, added[0])
				}
			}
		}
	}
}

func TestHeaders_LeaveTheResponseFlushable(t *testing.T) {
	t.Parallel()
	s := New(Options{})
	var flushErr error
	s.Handle("GET /stream", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("first"))
		flushErr = http.NewResponseController(w).Flush()
	}))

	rec := do(s.Handler(), newRequest(http.MethodGet, "/stream", nil, ""))

	if flushErr != nil {
		t.Errorf("Flush through the server's writer: %v", flushErr)
	}
	if !rec.Flushed {
		t.Error("the response was not flushed")
	}
}
