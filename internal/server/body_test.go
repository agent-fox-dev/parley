package server

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// bodyServer builds a server with a passing authenticator and recording
// binding guard, scope check and access check, registers routes on it and
// returns its handler and the log the checks write to.
func bodyServer(opts Options, log *callLog, routes ...Route) http.Handler {
	s := New(opts)
	s.SetAuthenticator(func(*http.Request) (Caller, error) { return testCaller, nil })
	s.SetBindingGuard(log.check("binding guard", nil))
	s.SetScopeCheck(log.check("scope check", nil))
	s.AddAccessCheck(log.check("access check", nil))
	for _, rt := range routes {
		s.Register(rt)
	}
	return s.Handler()
}

// echoLength returns a handler that records "handler", reads the whole body
// and answers 200 with {"read": <bytes read>, "body": <the bytes as text>}.
func echoLength(log *callLog) HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		log.add("handler")
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		WriteJSON(w, http.StatusOK, map[string]any{"read": len(b), "body": string(b)})
		return nil
	}
}

// decodeName returns a handler that records "handler", decodes the body with
// DecodeJSON and answers 201 with the decoded name.
func decodeName(log *callLog) HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		log.add("handler")
		var in struct {
			Name string `json:"name"`
		}
		if err := DecodeJSON(r, &in); err != nil {
			return err
		}
		WriteJSON(w, http.StatusCreated, map[string]string{"name": in.Name})
		return nil
	}
}

// echoed decodes what echoLength answered.
func echoed(t *testing.T, body []byte) (read int, text string) {
	t.Helper()
	var out struct {
		Read int    `json:"read"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out.Read, out.Body
}

var jsonHeader = http.Header{"Content-Type": {"application/json"}}

const defaultLimit = 1048576

func TestBodyStage_RefusesABodyOverTheLimitBeforeTheChecksAndTheHandler(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		size        int
		chunked     bool
		contentType string
		wantStatus  int
	}{
		{"exactly the limit with a declared length", defaultLimit, false, "application/json", http.StatusOK},
		{"one byte over with a declared length", defaultLimit + 1, false, "application/json", http.StatusRequestEntityTooLarge},
		{"one byte over without a declared length", defaultLimit + 1, true, "application/json", http.StatusRequestEntityTooLarge},
		{"one byte over as text/plain", defaultLimit + 1, false, "text/plain", http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var log callLog
			h := bodyServer(Options{}, &log, Route{Method: http.MethodPost, Pattern: "/things", Handler: echoLength(&log)})
			r := newRequest(http.MethodPost, "/api/v1/things", http.Header{"Content-Type": {tc.contentType}}, strings.Repeat("a", tc.size))
			if tc.chunked {
				r.ContentLength = -1
			}

			rec := do(h, r)

			if tc.wantStatus == http.StatusRequestEntityTooLarge {
				assertError(t, rec, http.StatusRequestEntityTooLarge, "payload_too_large")
				if calls := log.calls(); len(calls) != 0 {
					t.Errorf("ran %v, want no check or handler", calls)
				}
				return
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.Bytes())
			}
			if read, _ := echoed(t, rec.Body.Bytes()); read != tc.size {
				t.Errorf("handler read %d bytes, want %d", read, tc.size)
			}
		})
	}
}

func TestBodyStage_AppliesTheRoutesLimitInPlaceOfTheServers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		serverMax  int64
		path       string
		size       int
		wantStatus int
	}{
		{"route limit admits a body over the server's", 0, "/api/v1/big", 2097152, http.StatusOK},
		{"route limit refuses one byte over its own", 0, "/api/v1/big", 4194305, http.StatusRequestEntityTooLarge},
		{"other routes keep the server's limit", 0, "/api/v1/things", 2097152, http.StatusRequestEntityTooLarge},
		{"configured server limit admits its size", 8, "/api/v1/things", 8, http.StatusOK},
		{"configured server limit refuses one byte over", 8, "/api/v1/things", 9, http.StatusRequestEntityTooLarge},
		{"route limit wins over a configured server limit", 8, "/api/v1/big", 9, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var log callLog
			h := bodyServer(Options{MaxBodySize: tc.serverMax}, &log,
				Route{Method: http.MethodPost, Pattern: "/big", MaxBodySize: 4194304, Handler: echoLength(&log)},
				Route{Method: http.MethodPost, Pattern: "/things", Handler: echoLength(&log)},
			)

			rec := do(h, newRequest(http.MethodPost, tc.path, jsonHeader, strings.Repeat("a", tc.size)))

			if tc.wantStatus == http.StatusRequestEntityTooLarge {
				assertError(t, rec, http.StatusRequestEntityTooLarge, "payload_too_large")
				if calls := log.calls(); len(calls) != 0 {
					t.Errorf("ran %v, want no check or handler", calls)
				}
				return
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.Bytes())
			}
			if read, _ := echoed(t, rec.Body.Bytes()); read != tc.size {
				t.Errorf("handler read %d bytes, want %d", read, tc.size)
			}
		})
	}
}

func TestBodyStage_RefusesABodyThatIsNotJSONBeforeTheChecksAndTheHandler(t *testing.T) {
	t.Parallel()
	contentTypes := []struct {
		name   string
		header http.Header
		pass   bool
	}{
		{"text/plain", http.Header{"Content-Type": {"text/plain"}}, false},
		{"absent", nil, false},
		{"form", http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, false},
		{"application/json", http.Header{"Content-Type": {"application/json"}}, true},
		{"with charset", http.Header{"Content-Type": {"application/json; charset=utf-8"}}, true},
		{"upper case", http.Header{"Content-Type": {"Application/JSON"}}, true},
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		for _, ct := range contentTypes {
			t.Run(method+" "+ct.name, func(t *testing.T) {
				t.Parallel()
				var log callLog
				h := bodyServer(Options{}, &log, Route{Method: method, Pattern: "/things", Handler: echoLength(&log)})

				rec := do(h, newRequest(method, "/api/v1/things", ct.header, "{}"))

				if !ct.pass {
					assertError(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")
					if calls := log.calls(); len(calls) != 0 {
						t.Errorf("ran %v, want no check or handler", calls)
					}
					return
				}
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.Bytes())
				}
				want := []string{"binding guard", "scope check", "access check", "handler"}
				if calls := log.calls(); !slices.Equal(calls, want) {
					t.Errorf("ran %v, want %v", calls, want)
				}
			})
		}
	}
}

func TestBodyStage_PassesARequestWithoutABodyWhateverItsContentType(t *testing.T) {
	t.Parallel()
	for _, ct := range []struct {
		name   string
		header http.Header
	}{
		{"absent", nil},
		{"text/plain", http.Header{"Content-Type": {"text/plain"}}},
	} {
		t.Run(ct.name, func(t *testing.T) {
			t.Parallel()
			var log callLog
			h := bodyServer(Options{}, &log, Route{Method: http.MethodPost, Pattern: "/things/:id/block", Handler: log.handler("handler", http.StatusOK)})

			rec := do(h, newRequest(http.MethodPost, "/api/v1/things/42/block", ct.header, ""))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.Bytes())
			}
			if calls := log.calls(); !slices.Contains(calls, "handler") {
				t.Errorf("ran %v, want the handler", calls)
			}
		})
	}
}

func TestBodyStage_AcceptsTheContentTypeTheRouteStates(t *testing.T) {
	t.Parallel()
	var log callLog
	h := bodyServer(Options{}, &log, Route{Method: http.MethodPut, Pattern: "/raw", ContentType: "text/plain", Handler: echoLength(&log)})
	plain := http.Header{"Content-Type": {"text/plain"}}

	rec := do(h, newRequest(http.MethodPut, "/api/v1/raw", plain, "hello"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.Bytes())
	}
	if _, text := echoed(t, rec.Body.Bytes()); text != "hello" {
		t.Errorf("handler read %q, want %q", text, "hello")
	}

	rec = do(h, newRequest(http.MethodPut, "/api/v1/raw", plain, strings.Repeat("a", defaultLimit+1)))
	assertError(t, rec, http.StatusRequestEntityTooLarge, "payload_too_large")
}

func TestDecodeJSON_RefusesABodyThatIsNotOneJSONObject(t *testing.T) {
	t.Parallel()
	bodies := []string{
		"",
		"{",
		"nope",
		"[1,2]",
		`"text"`,
		"42",
		"null",
		`{"name":"a"} trailing`,
		`{"name":"a"}{"name":"b"}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			var log callLog
			h := bodyServer(Options{}, &log, Route{Method: http.MethodPost, Pattern: "/things", Handler: decodeName(&log)})

			rec := do(h, newRequest(http.MethodPost, "/api/v1/things", jsonHeader, body))

			assertError(t, rec, http.StatusBadRequest, "invalid_request")
		})
	}
}

func TestDecodeJSON_IgnoresMembersTheHandlerDoesNotKnow(t *testing.T) {
	t.Parallel()
	var log callLog
	h := bodyServer(Options{}, &log, Route{Method: http.MethodPost, Pattern: "/things", Handler: decodeName(&log)})

	rec := do(h, newRequest(http.MethodPost, "/api/v1/things", jsonHeader, "{\"name\":\"a\",\"surprise\":true}  \n"))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.Bytes())
	}
	if got := decodeObject(t, rec.Body.Bytes())["name"]; got != "a" {
		t.Errorf("handler saw name %v, want %q", got, "a")
	}
}
