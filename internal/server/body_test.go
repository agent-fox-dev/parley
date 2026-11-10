package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// bodyServer has a passing authenticator and POST /things, which counts its
// calls and reports how many body bytes it read.
type bodyServer struct {
	*Server
	calls atomic.Int32
	read  atomic.Int64
}

func newBodyServer(opts Options) *bodyServer {
	b := &bodyServer{Server: New(opts)}
	b.SetAuthenticator(bearerAuth(nil))
	echo := func(w http.ResponseWriter, r *http.Request) error {
		b.calls.Add(1)
		n, _ := io.Copy(io.Discard, r.Body)
		b.read.Store(n)
		WriteJSON(w, 200, map[string]int64{"read": n})
		return nil
	}
	for _, m := range []string{"POST", "PUT", "PATCH"} {
		b.Register(Route{Method: m, Pattern: "/things", Handler: echo})
	}
	b.Register(Route{Method: "POST", Pattern: "/things/:id/block", Handler: echo})
	return b
}

// chunked sends b without a declared length.
func chunked(b []byte) reqOpt {
	return func(r *http.Request) {
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = -1
		r.Header.Set("Content-Type", "application/json")
	}
}

func jsonOfSize(n int) []byte {
	b := bytes.Repeat([]byte(" "), n)
	b[0], b[1] = '{', '}'
	return b
}

func TestBody_OverTheLimitIsPayloadTooLarge(t *testing.T) {
	limit := int(DefaultBodyLimit)
	tests := []struct {
		name   string
		opts   []reqOpt
		status int
	}{
		{"exactly the limit", []reqOpt{withBody(jsonOfSize(limit))}, 200},
		{"one byte over with a length", []reqOpt{withBody(jsonOfSize(limit + 1))}, 413},
		{"one byte over without a length", []reqOpt{chunked(jsonOfSize(limit + 1))}, 413},
		{"one byte over as text/plain", []reqOpt{header("Content-Type", "text/plain"), withBody(jsonOfSize(limit + 1))}, 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newBodyServer(Options{})
			r := do(t, s.Handler(), "POST", "/api/v1/things", append([]reqOpt{bearer("a")}, tt.opts...)...)
			if r.status != tt.status {
				t.Fatalf("status = %d, want %d: %s", r.status, tt.status, r.body)
			}
			if tt.status == 413 {
				if et := r.errorType(t); et != "payload_too_large" {
					t.Errorf("error_type = %q", et)
				}
				if s.calls.Load() != 0 {
					t.Errorf("handler ran")
				}
			} else if s.read.Load() != int64(limit) {
				t.Errorf("handler read %d bytes, want %d", s.read.Load(), limit)
			}
		})
	}
}

func TestBody_ARouteLimitReplacesTheServerLimit(t *testing.T) {
	s := newBodyServer(Options{})
	var bigCalls atomic.Int32
	s.Register(Route{Method: "POST", Pattern: "/big", BodyLimit: 4 << 20, Handler: func(w http.ResponseWriter, r *http.Request) error {
		bigCalls.Add(1)
		WriteJSON(w, 200, map[string]any{})
		return nil
	}})
	h := s.Handler()
	if r := do(t, h, "POST", "/api/v1/big", bearer("a"), withBody(jsonOfSize(2<<20))); r.status != 200 || bigCalls.Load() != 1 {
		t.Errorf("2 MiB on /big = %d, want the handler's 200", r.status)
	}
	if r := do(t, h, "POST", "/api/v1/big", bearer("a"), withBody(jsonOfSize(4<<20+1))); r.status != 413 {
		t.Errorf("4 MiB + 1 on /big = %d, want 413", r.status)
	}
	if r := do(t, h, "POST", "/api/v1/things", bearer("a"), withBody(jsonOfSize(2<<20))); r.status != 413 {
		t.Errorf("2 MiB on /things = %d, want 413", r.status)
	}
}

func TestBody_WrongContentTypeIsUnsupportedMediaType(t *testing.T) {
	types := []struct {
		value  string
		status int
	}{
		{"text/plain", 415},
		{"", 415},
		{"application/x-www-form-urlencoded", 415},
		{"application/json", 200},
		{"application/json; charset=utf-8", 200},
		{"Application/JSON", 200},
	}
	for _, method := range []string{"POST", "PUT", "PATCH"} {
		for _, ct := range types {
			t.Run(method+" "+ct.value, func(t *testing.T) {
				s := newBodyServer(Options{})
				opts := []reqOpt{bearer("a"), withBody([]byte("{}")), func(r *http.Request) {
					if ct.value == "" {
						r.Header.Del("Content-Type")
					} else {
						r.Header.Set("Content-Type", ct.value)
					}
				}}
				r := do(t, s.Handler(), method, "/api/v1/things", opts...)
				if r.status != ct.status {
					t.Fatalf("status = %d, want %d", r.status, ct.status)
				}
				if ct.status == 415 && (r.errorType(t) != "unsupported_media_type" || s.calls.Load() != 0) {
					t.Errorf("error_type = %q, handler calls %d", r.errorType(t), s.calls.Load())
				}
			})
		}
	}
}

func TestBody_ARequestWithoutABodyPassesTheContentTypeCheck(t *testing.T) {
	for _, ct := range []string{"", "text/plain"} {
		s := newBodyServer(Options{})
		opts := []reqOpt{bearer("a")}
		if ct != "" {
			opts = append(opts, header("Content-Type", ct))
		}
		r := do(t, s.Handler(), "POST", "/api/v1/things/42/block", opts...)
		if r.status != 200 || s.calls.Load() != 1 {
			t.Errorf("bodiless POST with Content-Type %q = %d, want the handler's 200", ct, r.status)
		}
	}
}

func TestBody_ARouteStatesItsOwnContentType(t *testing.T) {
	s := newBodyServer(Options{})
	var got string
	s.Register(Route{Method: "PUT", Pattern: "/raw", ContentType: "text/plain", Handler: func(w http.ResponseWriter, r *http.Request) error {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		WriteJSON(w, 200, map[string]any{})
		return nil
	}})
	h := s.Handler()
	if r := do(t, h, "PUT", "/api/v1/raw", bearer("a"), header("Content-Type", "text/plain"), withBody([]byte("hello"))); r.status != 200 || got != "hello" {
		t.Errorf("PUT /raw = %d with %q, want 200 and hello", r.status, got)
	}
	big := []byte(strings.Repeat("x", int(DefaultBodyLimit)+1))
	if r := do(t, h, "PUT", "/api/v1/raw", bearer("a"), header("Content-Type", "text/plain"), withBody(big)); r.status != 413 {
		t.Errorf("over-limit PUT /raw = %d, want 413", r.status)
	}
}

// decodeServer has POST /things whose handler decodes {"name"} through
// DecodeJSON and answers 201.
func decodeServer(saw *string) http.Handler {
	s := New(Options{})
	s.SetAuthenticator(bearerAuth(nil))
	s.Register(Route{Method: "POST", Pattern: "/things", Handler: func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Name string `json:"name"`
		}
		if err := DecodeJSON(r, &in); err != nil {
			return err
		}
		*saw = in.Name
		WriteJSON(w, 201, map[string]string{"name": in.Name})
		return nil
	}})
	return s.Handler()
}

func TestDecodeJSON_RefusesABodyThatIsNotOneObject(t *testing.T) {
	var saw string
	h := decodeServer(&saw)
	for _, body := range []string{"", "{", "nope", "[1,2]", `"text"`, "42", "null", `{"name":"a"} trailing`, `{"name":"a"}{"name":"b"}`} {
		opts := []reqOpt{bearer("a"), header("Content-Type", "application/json")}
		if body != "" {
			opts = append(opts, jsonBody(body))
		}
		r := do(t, h, "POST", "/api/v1/things", opts...)
		if r.status != 400 || r.errorType(t) != "invalid_request" {
			t.Errorf("body %q = %d %s, want 400 invalid_request", body, r.status, r.body)
		}
	}
	if saw != "" {
		t.Errorf("handler used a decoded value %q", saw)
	}
}

func TestDecodeJSON_IgnoresUnknownMembers(t *testing.T) {
	var saw string
	r := do(t, decodeServer(&saw), "POST", "/api/v1/things", bearer("a"), jsonBody("{\"name\":\"a\",\"surprise\":true}  \n"))
	if r.status != 201 || saw != "a" {
		t.Errorf("status = %d, name %q, want 201 and a", r.status, saw)
	}
}
