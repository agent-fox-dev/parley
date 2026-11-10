package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// headerServer has GET /things (which reports its request id), a panicking
// GET /panic and routes with their own Cache-Control, behind bearerAuth.
func headerServer(opts Options, sawID *string) *Server {
	s := New(opts)
	s.SetAuthenticator(bearerAuth(nil))
	s.Register(Route{Method: "GET", Pattern: "/things", Handler: func(w http.ResponseWriter, r *http.Request) error {
		if sawID != nil {
			*sawID = RequestID(r.Context())
		}
		WriteJSON(w, 200, map[string]any{})
		return nil
	}})
	s.Register(Route{Method: "POST", Pattern: "/things", Handler: (&countingHandler{status: 201}).handle})
	s.Register(Route{Method: "GET", Pattern: "/panic", Handler: func(http.ResponseWriter, *http.Request) error { panic("boom") }})
	s.Register(Route{Method: "GET", Pattern: "/cached", CacheControl: "public, max-age=300", Handler: (&countingHandler{status: 200}).handle})
	s.Register(Route{Method: "GET", Pattern: "/revalidated", CacheControl: "no-cache", Handler: (&countingHandler{status: 200}).handle})
	return s
}

func quietOptions() Options {
	opts := DefaultOptions()
	logger, _ := debugLogger()
	opts.Logger = logger
	return opts
}

func TestRequestID_AValidIncomingIDIsReused(t *testing.T) {
	for _, v := range []string{"3f2a1bc4-0000-4000-8000-0123456789ab", "3F2A1BC4-0000-1000-8000-0123456789AB"} {
		var saw string
		r := do(t, headerServer(quietOptions(), &saw).Handler(), "GET", "/api/v1/things", bearer("a"), header("X-Request-ID", v))
		if got := r.header.Get("X-Request-ID"); got != v {
			t.Errorf("X-Request-ID = %q, want %q", got, v)
		}
		if saw != v {
			t.Errorf("handler's request id = %q, want %q", saw, v)
		}
	}
}

func TestRequestID_AnythingElseGetsAFreshRandomID(t *testing.T) {
	h := headerServer(quietOptions(), nil).Handler()
	incoming := []string{"", "abc", "{3f2a1bc4-0000-4000-8000-0123456789ab}", "3f2a1bc400004000800001234567 89ab", strings.Repeat("a", 300), "3f2a1bc4-0000-4000-8000-0123456789zz"}
	for _, v := range append(incoming, "absent") {
		opts := []reqOpt{bearer("a")}
		if v != "absent" {
			opts = append(opts, header("X-Request-ID", v))
		}
		first := do(t, h, "GET", "/api/v1/things", opts...).header.Get("X-Request-ID")
		second := do(t, h, "GET", "/api/v1/things", opts...).header.Get("X-Request-ID")
		if !uuidV4.MatchString(first) || first == v {
			t.Errorf("incoming %q: X-Request-ID = %q, want a fresh version 4 UUID", v, first)
		}
		if first == second {
			t.Errorf("incoming %q: two requests got the same id %q", v, first)
		}
	}
}

func TestHeaders_EveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	limited := quietOptions()
	limited.RequestsPerMinute = 1
	h := headerServer(quietOptions(), nil).Handler()
	lh := headerServer(limited, nil).Handler()
	do(t, lh, "GET", "/api/v1/things", bearer("a"))

	tests := []struct {
		name string
		h    http.Handler
		send func(http.Handler) apiResponse
		want int
	}{
		{"success", h, func(h http.Handler) apiResponse { return do(t, h, "GET", "/api/v1/things", bearer("a")) }, 200},
		{"404", h, func(h http.Handler) apiResponse { return do(t, h, "GET", "/api/v1/nope") }, 404},
		{"405", h, func(h http.Handler) apiResponse { return do(t, h, "DELETE", "/api/v1/things") }, 405},
		{"401", h, func(h http.Handler) apiResponse { return do(t, h, "GET", "/api/v1/things") }, 401},
		{"413", h, func(h http.Handler) apiResponse {
			return do(t, h, "POST", "/api/v1/things", bearer("a"), withBody(jsonOfSize(int(DefaultBodyLimit)+1)))
		}, 413},
		{"429", lh, func(h http.Handler) apiResponse { return do(t, h, "GET", "/api/v1/things", bearer("a")) }, 429},
		{"500", h, func(h http.Handler) apiResponse { return do(t, h, "GET", "/api/v1/panic", bearer("a")) }, 500},
		{"healthz", h, func(h http.Handler) apiResponse { return do(t, h, "GET", "/healthz") }, 200},
		{"readyz", h, func(h http.Handler) apiResponse { return do(t, h, "GET", "/readyz") }, 200},
		{"version", h, func(h http.Handler) apiResponse { return do(t, h, "GET", "/version") }, 200},
		{"unknown surface", h, func(h http.Handler) apiResponse { return do(t, h, "GET", "/nope") }, 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.send(tt.h)
			if r.status != tt.want {
				t.Fatalf("status = %d, want %d", r.status, tt.want)
			}
			want := map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"}
			for k, v := range want {
				if got := r.header.Get(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			if r.header.Get("X-Request-ID") == "" {
				t.Error("X-Request-ID is missing")
			}
		})
	}
}

func TestHeaders_CacheControlBySurfaceAndRoute(t *testing.T) {
	h := headerServer(quietOptions(), nil).Handler()
	tests := []struct {
		method, path string
		opts         []reqOpt
		want         string
	}{
		{"GET", "/api/v1/things", []reqOpt{bearer("a")}, "no-store"},
		{"GET", "/api/v1/nope", nil, "no-store"},
		{"GET", "/api/v1/things", nil, "no-store"},
		{"GET", "/api/v1/cached", []reqOpt{bearer("a")}, "public, max-age=300"},
		{"GET", "/api/v1/revalidated", []reqOpt{bearer("a")}, "no-cache"},
		{"GET", "/healthz", nil, "no-cache"},
		{"GET", "/readyz", nil, "no-cache"},
		{"GET", "/version", nil, "public, max-age=300"},
	}
	for _, tt := range tests {
		r := do(t, h, tt.method, tt.path, tt.opts...)
		if got := r.header.Values("Cache-Control"); len(got) != 1 || got[0] != tt.want {
			t.Errorf("%s %s Cache-Control = %q, want exactly %q", tt.method, tt.path, got, tt.want)
		}
	}
}

func TestAccessLog_OneLinePerRequestWithoutTheQuery(t *testing.T) {
	opts := DefaultOptions()
	logger, buf := debugLogger()
	opts.Logger = logger
	h := headerServer(opts, nil).Handler()

	type line struct {
		Level      string   `json:"level"`
		Method     string   `json:"method"`
		Path       string   `json:"path"`
		Status     int      `json:"status"`
		DurationMS *float64 `json:"duration_ms"`
		RequestID  string   `json:"request_id"`
	}
	newLine := func(before int) (line, string) {
		t.Helper()
		lines := buf.lines()
		if len(lines) != before+1 {
			t.Fatalf("%d new log lines, want 1: %q", len(lines)-before, lines[before:])
		}
		var l line
		if err := json.Unmarshal([]byte(lines[before]), &l); err != nil {
			t.Fatal(err)
		}
		return l, lines[before]
	}

	n := len(buf.lines())
	r := do(t, h, "GET", "/api/v1/nope")
	l, _ := newLine(n)
	if l.Method != "GET" || l.Path != "/api/v1/nope" || l.Status != 404 || l.RequestID != r.header.Get("X-Request-ID") || l.DurationMS == nil || *l.DurationMS < 0 {
		t.Errorf("log line = %+v, want GET /api/v1/nope 404 with the response's request id", l)
	}

	n = len(buf.lines())
	do(t, h, "GET", "/api/v1/nope?token=s3cr3t&x=1")
	l, raw := newLine(n)
	if l.Path != "/api/v1/nope" || strings.Contains(raw, "?") || strings.Contains(raw, "s3cr3t") {
		t.Errorf("log line %s carries the query", raw)
	}

	n = len(buf.lines())
	do(t, h, "GET", "/healthz")
	do(t, h, "GET", "/readyz")
	if got := len(buf.lines()) - n; got != 0 {
		t.Errorf("health probes wrote %d lines with probe logging off", got)
	}

	opts.LogHealthProbes = true
	ph := headerServer(opts, nil).Handler()
	for _, p := range []string{"/healthz", "/readyz"} {
		n = len(buf.lines())
		do(t, ph, "GET", p)
		l, _ := newLine(n)
		if l.Level != "DEBUG" || l.Path != p {
			t.Errorf("probe line = %+v, want one debug line for %s", l, p)
		}
	}
}
