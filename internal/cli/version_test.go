package cli

import (
	"bytes"
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingHub is a hub that answers GET /version with body and records the
// path and Authorization header of every request.
type recordingHub struct {
	*httptest.Server
	body string

	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	path          string
	authorization []string
}

func newRecordingHub(t *testing.T, body string) *recordingHub {
	t.Helper()
	h := &recordingHub{body: body}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, recordedRequest{path: r.URL.Path, authorization: r.Header.Values("Authorization")})
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(body))
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *recordingHub) recorded() []recordedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.requests)
}

// sortedKeys gives the member names of a JSON object.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// The test sets environment variables and build values, so it must not run
// in parallel.
func TestVersion_PrintsTheClientAndTheHubBuild(t *testing.T) {
	const (
		bodyA = `{"version":"2.0.0","commit":"aaaaaaa","build_time":"2026-02-03T04:05:06Z","go_version":"go1.27.0"}`
		bodyB = `{"version":"3.0.0","commit":"bbbbbbb","build_time":"2026-03-04T05:06:07Z","go_version":"go1.27.1"}`
	)
	tests := []struct {
		name string
		// flag is the value of --endpoint-url, which is absent when flag is
		// nil; "A" and "B" stand for the URLs of the two hubs.
		flag *string
		// env is PARLEY_ENDPOINT_URL, which is unset when env is nil.
		env  *string
		want string
	}{
		{name: "flag only", flag: new("A"), want: "A"},
		{name: "environment only", env: new("A"), want: "A"},
		{name: "flag before environment", flag: new("A"), env: new("B"), want: "A"},
		{name: "empty flag falls back to environment", flag: new(""), env: new("B"), want: "B"},
		{name: "trailing slash", flag: new("A/"), want: "A"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setBuildValues(t, "1.4.0", "3f2a1bc")
			hubs := map[string]*recordingHub{"A": newRecordingHub(t, bodyA), "B": newRecordingHub(t, bodyB)}
			resolve := func(s string) string {
				return strings.NewReplacer("A", hubs["A"].URL, "B", hubs["B"].URL).Replace(s)
			}
			args := []string{"version"}
			if tt.flag != nil {
				args = append(args, "--endpoint-url", resolve(*tt.flag))
			}
			unsetEnv(t, "PARLEY_ENDPOINT_URL")
			if tt.env != nil {
				t.Setenv("PARLEY_ENDPOINT_URL", resolve(*tt.env))
			}

			code, stdout, stderr := runCLI(t, args...)

			if code != 0 {
				t.Errorf("Run returned %d, want 0", code)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want it empty", stderr)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
				t.Fatalf("stdout is not a JSON object: %v\n%s", err, stdout)
			}
			if got := sortedKeys(doc); !slices.Equal(got, []string{"cli", "server"}) {
				t.Errorf("members = %v, want [cli server]", got)
			}
			var cli map[string]string
			if err := json.Unmarshal(doc["cli"], &cli); err != nil {
				t.Fatalf("cli member: %v", err)
			}
			if want := map[string]string{"version": "1.4.0", "commit": "3f2a1bc"}; !maps.Equal(cli, want) {
				t.Errorf("cli = %v, want %v", cli, want)
			}
			var server, answered map[string]any
			if err := json.Unmarshal(doc["server"], &server); err != nil {
				t.Fatalf("server member: %v", err)
			}
			json.Unmarshal([]byte(hubs[tt.want].body), &answered)
			if !reflect.DeepEqual(server, answered) {
				t.Errorf("server = %v, want what hub %s answered, %v", server, tt.want, answered)
			}

			for name, hub := range hubs {
				got := hub.recorded()
				if name != tt.want {
					if len(got) != 0 {
						t.Errorf("hub %s got %d requests, want none", name, len(got))
					}
					continue
				}
				if len(got) != 1 {
					t.Fatalf("hub %s got %d requests, want 1", name, len(got))
				}
				if got[0].path != "/version" {
					t.Errorf("request path = %q, want /version", got[0].path)
				}
				if len(got[0].authorization) != 0 {
					t.Errorf("request has an Authorization header %q, want none", got[0].authorization)
				}
			}

			var compact, indented bytes.Buffer
			json.Compact(&compact, []byte(stdout))
			json.Indent(&indented, compact.Bytes(), "", "  ")
			if want := indented.String() + "\n"; stdout != want {
				t.Errorf("stdout is not one JSON document indented by 2 spaces and a newline:\n got %q\nwant %q", stdout, want)
			}
		})
	}
}

// The test sets environment variables and build values, so it must not run
// in parallel.
func TestVersion_PrintsTheClientBuildAloneWithoutAHubURL(t *testing.T) {
	tests := []struct {
		name string
		args []string
		// env is PARLEY_ENDPOINT_URL, which is unset when env is nil.
		env *string
	}{
		{name: "no flag, environment unset", args: []string{"version"}},
		{name: "no flag, environment empty", args: []string{"version"}, env: new("")},
		{name: "empty flag, environment unset", args: []string{"version", "--endpoint-url", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setBuildValues(t, "1.4.0", "3f2a1bc")
			unsetEnv(t, "PARLEY_ENDPOINT_URL")
			if tt.env != nil {
				t.Setenv("PARLEY_ENDPOINT_URL", *tt.env)
			}

			code, stdout, stderr := runCLI(t, tt.args...)

			if code != 0 {
				t.Errorf("Run returned %d, want 0", code)
			}
			if want := "{\n  \"cli\": {\n    \"version\": \"1.4.0\",\n    \"commit\": \"3f2a1bc\"\n  }\n}\n"; stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want it empty", stderr)
			}
		})
	}
}

func TestVersion_WarnsAndPrintsTheClientBuildWhenTheHubCannotBeRead(t *testing.T) {
	t.Parallel()
	if got := defaultOptions().versionTimeout; got != 5*time.Second {
		t.Errorf("default timeout of the version command = %v, want 5s", got)
	}

	const versionBody = `{"version":"2.0.0","commit":"aaaaaaa","build_time":"2026-02-03T04:05:06Z","go_version":"go1.27.0"}`
	answer := func(status int, body string) string {
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(body))
		}))
		t.Cleanup(hub.Close)
		return hub.URL
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
			return
		}
		w.Write([]byte(versionBody))
	}))
	t.Cleanup(slow.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := "http://" + ln.Addr().String()
	ln.Close()

	tests := []struct {
		name    string
		hub     string
		timeout time.Duration
	}{
		{name: "connection refused", hub: closedPort},
		{name: "no answer within the timeout", hub: slow.URL, timeout: 100 * time.Millisecond},
		{name: "500 with a JSON body", hub: answer(http.StatusInternalServerError, versionBody)},
		{name: "404", hub: answer(http.StatusNotFound, "404 page not found\n")},
		{name: "200 with a body that is not JSON", hub: answer(http.StatusOK, "not json")},
		{name: "200 with a JSON array", hub: answer(http.StatusOK, "[]")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := defaultOptions()
			if tt.timeout != 0 {
				opts.versionTimeout = tt.timeout
			}

			start := time.Now()
			code, stdout, stderr := runCLIWith(t, opts, "version", "--endpoint-url", tt.hub)
			elapsed := time.Since(start)

			if code != 0 {
				t.Errorf("run returned %d, want 0", code)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
				t.Fatalf("stdout is not a JSON object: %v\n%s", err, stdout)
			}
			if got := sortedKeys(doc); !slices.Equal(got, []string{"cli"}) {
				t.Errorf("members = %v, want [cli]", got)
			}
			if lines := nonEmptyLines(stderr); len(lines) != 1 {
				t.Errorf("stderr has %d lines, want 1 warning line: %q", len(lines), stderr)
			}
			if !strings.HasPrefix(stderr, "warning: ") || !strings.Contains(stderr, tt.hub) {
				t.Errorf("stderr = %q, want a line that begins with %q and names %s", stderr, "warning: ", tt.hub)
			}
			if elapsed > 6*time.Second {
				t.Errorf("the command took %v, want at most 6s", elapsed)
			}
		})
	}
}
