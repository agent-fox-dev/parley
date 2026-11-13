package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestClient_SendsTheRequestToTheAPIPathOfTheHub(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		suffix        string // appended to the hub's URL
		method, path  string
		query         url.Values
		body          any
		wantPathStart string
	}{
		{name: "GET", method: http.MethodGet, path: "/things/x"},
		{name: "trailing slash and a query", suffix: "/", method: http.MethodGet, path: "/things/x", query: url.Values{"limit": {"5"}}},
		{name: "two trailing slashes", suffix: "//", method: http.MethodDelete, path: "/things/x"},
		{name: "a path in the hub URL", suffix: "/prefix", method: http.MethodGet, path: "/things/x", wantPathStart: "/prefix"},
		{name: "POST with a body", method: http.MethodPost, path: "/things", body: map[string]any{"name": "x", "n": 1}},
		{name: "PATCH with an empty object", method: http.MethodPatch, path: "/things/x", body: map[string]any{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t)

			_, err := newClient(hub.URL+tt.suffix, "k", defaultRequestTimeout).raw(context.Background(), apiRequest{method: tt.method, path: tt.path, query: tt.query, body: tt.body})

			if err != nil {
				t.Fatalf("raw() = %v, want no error", err)
			}
			got := hub.recorded()
			if len(got) != 1 {
				t.Fatalf("the hub got %d requests, want 1", len(got))
			}
			r := got[0]
			if want := tt.wantPathStart + "/api/v1" + tt.path; r.method != tt.method || r.rawPath != want {
				t.Errorf("request = %s %s, want %s %s", r.method, r.rawPath, tt.method, want)
			}
			if want := tt.query.Encode(); r.query != want {
				t.Errorf("query = %q, want %q", r.query, want)
			}
			if auth := r.header.Get("Authorization"); auth != "Bearer k" {
				t.Errorf("Authorization = %q, want %q", auth, "Bearer k")
			}
			if tt.body == nil {
				if ct, ok := r.header["Content-Type"]; ok {
					t.Errorf("Content-Type = %q, want none without a body", ct)
				}
				if r.contentLength != 0 || r.body != "" {
					t.Errorf("content length %d, body %q, want no body", r.contentLength, r.body)
				}
				return
			}
			if ct := r.header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var sent, want any
			if err := json.Unmarshal([]byte(r.body), &sent); err != nil {
				t.Fatalf("body %q is not JSON: %v", r.body, err)
			}
			encoded, _ := json.Marshal(tt.body)
			json.Unmarshal(encoded, &want)
			if !reflect.DeepEqual(sent, want) {
				t.Errorf("body = %s, want %s", strings.TrimSpace(r.body), encoded)
			}
		})
	}
}

// pathTokens are what the arguments of the path segment property are drawn
// from: letters, digits, the characters that mean something in a URL, dot
// segments and letters outside ASCII.
var pathTokens = []string{
	"a", "Z", "q", "7", "0", "/", "?", "#", "%", " ", "+", ".", "..", "&", "=",
	"ü", "ö", "é", "ß", "ж", "日",
}

// randomSegment draws 1 to 64 tokens of pathTokens.
func randomSegment(r *rand.Rand) string {
	var b strings.Builder
	for range 1 + r.IntN(64) {
		b.WriteString(pathTokens[r.IntN(len(pathTokens))])
	}
	return b.String()
}

// The test sets environment variables, so it must not run in parallel.
func TestClient_EncodesEachArgumentAsOnePathSegment(t *testing.T) {
	hub := newStubHub(t)
	probeEnv(t, hub.URL)
	seed := uint64(time.Now().UnixNano())
	t.Logf("seed %d", seed)
	r := rand.New(rand.NewPCG(seed, 0))
	values := []string{"a/b", "feature/x y", "100%", "..", ".", "a?b#c", "ü/ö"}
	for range 200 {
		values = append(values, randomSegment(r))
	}

	for i, value := range values {
		runProbes(t, "probe", "get", value)

		got := hub.recorded()
		if len(got) != i+1 {
			t.Fatalf("argument %q: the hub got %d requests in all, want %d", value, len(got), i+1)
		}
		req := got[i]
		seg, ok := strings.CutPrefix(req.rawPath, "/api/v1/things/")
		if !ok {
			t.Errorf("argument %q: raw path %q, want it under /api/v1/things/", value, req.rawPath)
			continue
		}
		if strings.ContainsAny(seg, "/?# ") {
			t.Errorf("argument %q: segment %q holds one of / ? # or a space", value, seg)
		}
		// A dot segment is a step of the path, not a segment of it.
		if seg == "." || seg == ".." {
			t.Errorf("argument %q: segment %q is a dot segment", value, seg)
		}
		if back, err := url.PathUnescape(seg); err != nil || back != value {
			t.Errorf("argument %q: segment %q decodes to %q (%v)", value, seg, back, err)
		}
		if req.query != "" {
			t.Errorf("argument %q: query %q, want none", value, req.query)
		}
	}
}

// slowProbe builds "probe slow <name>", "probe get" that states a request
// timeout of 300 ms.
func slowProbe() *cobra.Command {
	return declare(command{
		Name:        "slow",
		Description: "Get a thing within 300 ms",
		Method:      http.MethodGet,
		Path:        "/things/:name",
		Args:        []argSpec{{Name: "name", Required: true}},
		Auth:        authUser,
		Timeout:     300 * time.Millisecond,
	})
}

// halfBodyThenStall is a handler that announces a body of 20 bytes, sends
// 10 of them and then stalls until the client gives up, or 2 s.
func halfBodyThenStall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Length", "20")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"a":"bcd`))
	http.NewResponseController(w).Flush()
	select {
	case <-time.After(2 * time.Second):
	case <-r.Context().Done():
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestClient_AbandonsARequestWithoutACompleteAnswerInTime(t *testing.T) {
	if got := defaultOptions().requestTimeout; got != 30*time.Second {
		t.Errorf("default request timeout = %v, want 30s", got)
	}
	tests := []struct {
		name string
		// shortened, when set, replaces the default timeout.
		shortened time.Duration
		command   string
		handler   http.HandlerFunc
		// timeout is the one the request runs under; 0: it succeeds.
		timeout time.Duration
	}{
		{name: "the default, shortened, before a late answer", shortened: 100 * time.Millisecond, command: "get", handler: answerAfter(2 * time.Second), timeout: 100 * time.Millisecond},
		{name: "a command's own timeout, answered in time", command: "slow", handler: answerAfter(200 * time.Millisecond)},
		{name: "a command's own timeout, before a late answer", command: "slow", handler: answerAfter(2 * time.Second), timeout: 300 * time.Millisecond},
		{name: "half a body, then nothing", shortened: 100 * time.Millisecond, command: "get", handler: halfBodyThenStall, timeout: 100 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newStubHub(t).handle(http.MethodGet, "/api/v1/things/x", tt.handler)
			probeEnv(t, hub.URL)
			opts := withProbes(defaultOptions(), slowProbe)
			if tt.shortened != 0 {
				opts.requestTimeout = tt.shortened
			}

			start := time.Now()
			code, stdout, stderr := runCLIWith(t, opts, "probe", tt.command, "x")
			took := time.Since(start)

			if tt.timeout == 0 {
				if code != 0 || stdout != "{}\n" {
					t.Errorf("exit status %d, stdout %q, want 0 and {}; stderr %q", code, stdout, stderr)
				}
				return
			}
			wantClientFailure(t, code, stdout, stderr, "client_error")
			if limit := tt.timeout + time.Second; took >= limit {
				t.Errorf("the command took %v, want less than %v", took, limit)
			}
			if n := len(hub.recorded()); n != 1 {
				t.Errorf("the hub got %d requests, want 1", n)
			}
		})
	}

	t.Run("a single request's own timeout", func(t *testing.T) {
		hub := newStubHub(t).handle(http.MethodGet, "/api/v1/things/x", answerAfter(2*time.Second))
		start := time.Now()

		_, err := newClient(hub.URL, "k", defaultRequestTimeout).send(context.Background(), apiRequest{method: http.MethodGet, path: "/things/x", timeout: 100 * time.Millisecond})

		wantClientError(t, err, "")
		if took := time.Since(start); took >= 1100*time.Millisecond {
			t.Errorf("the request took %v, want less than 1.1s", took)
		}
	})
}

// The test sets environment variables, so it must not run in parallel.
func TestClient_FailsNamingTheHostWhenItCannotConnect(t *testing.T) {
	tlsHub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("{}")) }))
	t.Cleanup(tlsHub.Close)
	for name, hubURL := range map[string]string{
		"connection refused":              closedPortURL(t),
		"host name that does not resolve": "http://no-such-host.invalid",
		"untrusted certificate":           tlsHub.URL,
	} {
		t.Run(name, func(t *testing.T) {
			loginEnv(t)

			code, stdout, stderr := runProbes(t, "probe", "get", "x", "--endpoint-url", hubURL, "--api-key", "k")

			message := wantClientFailure(t, code, stdout, stderr, "client_error")
			if u, _ := url.Parse(hubURL); !strings.Contains(message, u.Host) {
				t.Errorf("message %q does not name the host %s", message, u.Host)
			}
		})
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestClient_FailsNamingTheStatusOfAnAnswerItCannotDecode(t *testing.T) {
	tests := []struct {
		status int
		body   string
		// location, when set, makes the answer a redirect to it.
		location string
	}{
		{200, "<html>", ""},
		{200, `{"a":`, ""},
		{502, "<html>Bad Gateway</html>", ""},
		{404, "not found", ""},
		{500, "{}", ""},
		{500, `{"error":"boom"}`, ""},
		{403, `{"error":{"message":"x"}}`, ""},
		{302, "", "/api/v1/things/y"},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.status)+" "+tt.body+tt.location, func(t *testing.T) {
			hub := newStubHub(t).answer(http.MethodGet, "/api/v1/things/x", tt.status, tt.body)
			if tt.location != "" {
				hub.handle(http.MethodGet, "/api/v1/things/x", redirectTo(tt.status, tt.location)).
					answer(http.MethodGet, tt.location, http.StatusOK, "{}")
			}
			probeEnv(t, hub.URL)

			code, stdout, stderr := runProbes(t, "probe", "get", "x")

			message := wantClientFailure(t, code, stdout, stderr, "client_error")
			if !strings.Contains(message, strconv.Itoa(tt.status)) {
				t.Errorf("message %q does not name the status %d", message, tt.status)
			}
		})
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestClient_RefusesAnAnswerLargerThanItsBound(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		handler http.HandlerFunc
		// fails says the command fails naming the status.
		fails bool
	}{
		{name: "a JSON answer of exactly the bound", status: 200, handler: jsonStringOf(maxAnswerSize)},
		{name: "a JSON answer one byte over the bound", status: 200, handler: jsonStringOf(maxAnswerSize + 1), fails: true},
		{name: "a success that never ends", status: 200, handler: endlessBody(200), fails: true},
		{name: "an error that never ends", status: 502, handler: endlessBody(502), fails: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newStubHub(t).handle(http.MethodGet, "/api/v1/things/x", tt.handler)
			probeEnv(t, hub.URL)
			opts := withProbes(defaultOptions())
			opts.requestTimeout = 5 * time.Second

			start := time.Now()
			code, stdout, stderr := runCLIWith(t, opts, "probe", "get", "x")
			took := time.Since(start)

			if !tt.fails {
				if code != 0 || len(stdout) != maxAnswerSize+1 {
					t.Errorf("exit status %d, %d bytes on stdout, want 0 and %d; stderr %q", code, len(stdout), maxAnswerSize+1, stderr)
				}
				return
			}
			if code == 0 {
				t.Fatalf("exit status 0 with %d bytes on stdout, want a client_error", len(stdout))
			}
			message := wantClientFailure(t, code, stdout, stderr, "client_error")
			if !strings.Contains(message, strconv.Itoa(tt.status)) {
				t.Errorf("message %q does not name the status %d", message, tt.status)
			}
			if took >= opts.requestTimeout {
				t.Errorf("the command took %v, want it to stop reading before the %v timeout", took, opts.requestTimeout)
			}
		})
	}
}

// jsonStringOf is a handler that answers 200 with a JSON string of n bytes,
// quotes included.
func jsonStringOf(n int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`"` + strings.Repeat("a", n-2) + `"`))
	}
}

// endlessBody is a handler that answers status and writes a body until the
// client goes away.
func endlessBody(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		chunk := []byte(strings.Repeat(" ", 64<<10))
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestClient_SendsEachRequestOnce(t *testing.T) {
	envelope := func(status int, errorType string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"code":%d,"message":"no","error_type":%q}}`, status, errorType)
		}
	}
	tests := []struct {
		name    string
		handler http.HandlerFunc
		// status is the envelope's code, 0 for a CLI error.
		status   int
		wantCode int
		wantType string
	}{
		{"429", envelope(429, "rate_limited"), 429, 1, "rate_limited"},
		{"500", envelope(500, "internal"), 500, 1, "internal"},
		{"502", envelope(502, "upstream_error"), 502, 1, "upstream_error"},
		{"503", envelope(503, "unavailable"), 503, 1, "unavailable"},
		{"a connection closed without an answer", closeWithoutAnswer, 0, 2, "client_error"},
		{"a 307 to another path", redirectTo(307, "/api/v1/things/y"), 0, 2, "client_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newStubHub(t).handle(http.MethodGet, "/api/v1/things/x", tt.handler)
			probeEnv(t, hub.URL)

			code, _, stderr := runProbes(t, "probe", "get", "x")
			if tt.status == http.StatusTooManyRequests {
				// Long enough for a client that honoured Retry-After to
				// have asked again.
				time.Sleep(1500 * time.Millisecond)
			}

			if n := len(hub.recorded()); n != 1 {
				t.Errorf("the hub got %d requests, want 1", n)
			}
			if gotCode, _, gotType := clientEnvelope(t, stderr); code != tt.wantCode || int(gotCode) != tt.status || gotType != tt.wantType {
				t.Errorf("exit status %d, envelope %v %q, want %d and %d %q", code, gotCode, gotType, tt.wantCode, tt.status, tt.wantType)
			}
		})
	}
}

func TestClient_SendsARequestOnceWhenTheHubDropsTheConnectionOfAnEarlierOne(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t).
		answer(http.MethodGet, "/api/v1/things/x", http.StatusOK, "{}").
		handle(http.MethodGet, "/api/v1/things/y", closeWithoutAnswer)
	c := newClient(hub.URL, "k", defaultRequestTimeout)

	if _, err := c.request(context.Background(), http.MethodGet, "/things/x", nil, nil); err != nil {
		t.Fatalf("the first request: %v", err)
	}
	_, err := c.request(context.Background(), http.MethodGet, "/things/y", nil, nil)

	wantClientError(t, err, "")
	var paths []string
	for _, r := range hub.recorded() {
		paths = append(paths, r.path)
	}
	if want := []string{"/api/v1/things/x", "/api/v1/things/y"}; !slices.Equal(paths, want) {
		t.Errorf("the hub got %v, want %v", paths, want)
	}
}

func TestClient_GivesTheRawAnswerWhateverItHolds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status int
		body   string
	}{
		{200, "line 1\nline 2\n"},
		{200, ""},
		{502, "<html>Bad Gateway</html>"},
		{404, `{"error":{"code":404,"message":"not found","error_type":"not_found"}}`},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.status)+" "+tt.body, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodGet, "/logs", tt.status, tt.body)

			resp, err := newClient(hub.URL+"/", "", defaultRequestTimeout).raw(context.Background(), apiRequest{method: http.MethodGet, path: "/logs", outsideAPI: true})

			if err != nil {
				t.Fatalf("raw() = %v, want no error", err)
			}
			if resp.status != tt.status || string(resp.body) != tt.body {
				t.Errorf("answer = %d %q, want %d %q", resp.status, resp.body, tt.status, tt.body)
			}
			if r := hub.recorded(); len(r) != 1 || r[0].header.Get("Authorization") != "" {
				t.Errorf("the hub got %v, want one request without Authorization", r)
			}
		})
	}
}
