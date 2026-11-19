package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/net/websocket"
)

// profilesAnswer is a profile list as a hub with sandboxes answers it.
const profilesAnswer = `{"items":[{"name":"agent","ttl":"4h","ttl_seconds":14400,"env_keys":["A"]}],"next_cursor":null}`

func TestSandboxProfiles_PrintsTheHubsProfileList(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t).answer(http.MethodGet, "/api/v1/sandbox-profiles", http.StatusOK, profilesAnswer)

	code, stdout, stderr := runCLI(t, onHub(hub.URL, "sandbox", "profiles")...)

	if code != 0 {
		t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
	}
	if want := indented(t, profilesAnswer); stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	var list struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil || len(list.Items) == 0 || list.Items[0].Name != "agent" {
		t.Errorf("stdout = %q, want a list whose first item is agent", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want it empty", stderr)
	}
	sent := hub.recorded()
	if len(sent) != 1 {
		t.Fatalf("the hub got %d requests, want 1", len(sent))
	}
	if got := sent[0]; got.method != http.MethodGet || got.rawPath != "/api/v1/sandbox-profiles" || got.query != "" || got.body != "" {
		t.Errorf("sent %s %s ?%q with body %q, want GET /api/v1/sandbox-profiles without a query or body", got.method, got.rawPath, got.query, got.body)
	}
	if auth := sent[0].header.Get("Authorization"); auth != "Bearer k" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer k")
	}
}

func TestSandboxProfiles_PrintsTheRefusalOfAHubWithoutSandboxesOnStderr(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t).answer(http.MethodGet, "/api/v1/sandbox-profiles", http.StatusNotFound,
		`{"error":{"code":404,"message":"sandboxes are not enabled on this hub","error_type":"sandboxes_disabled"}}`)

	code, stdout, stderr := runCLI(t, onHub(hub.URL, "sandbox", "profiles")...)

	if code != 1 {
		t.Errorf("exit status = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want it empty", stdout)
	}
	var env struct {
		Error struct {
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &env); err != nil || env.Error.ErrorType != "sandboxes_disabled" {
		t.Errorf("stderr = %q, want the envelope with error_type sandboxes_disabled", stderr)
	}
	if n := len(hub.recorded()); n != 1 {
		t.Errorf("the hub got %d requests, want 1", n)
	}
}

func TestSandbox_WithoutACommandPrintsItsHelpWithoutSettings(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := runCLI(t, "sandbox")

	if code != 0 {
		t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "profiles") {
		t.Errorf("stdout = %q, want the help listing profiles", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want it empty", stderr)
	}
}

// sandboxesPath is the route parley sandbox create posts to and parley
// sandbox list reads for the workspace demo, as sent.
const sandboxesPath = "/api/v1/workspaces/demo/sandboxes"

// sandboxRecord is the sandbox sbx_1 in state as the hub serves it the
// n-th time (0 for the answer to the create).
func sandboxRecord(state string, n int) string {
	return fmt.Sprintf(`{"id":"sbx_1","workspace":"demo","state":%q,"poll":%d}`, state, n)
}

// sandboxHub is a stub hub that creates sbx_1 in provisioning on POST to
// demo's sandboxes and answers the n-th GET of sbx_1 with the record in
// polls[n-1], the last of polls answering every later one too.
func sandboxHub(t *testing.T, polls ...string) *stubHub {
	t.Helper()
	var mu sync.Mutex
	n := 0
	return newStubHub(t).
		answer(http.MethodPost, sandboxesPath, http.StatusCreated, sandboxRecord("provisioning", 0)).
		handle(http.MethodGet, sandboxesPath+"/sbx_1", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			n++
			i := min(n, len(polls)) - 1
			mu.Unlock()
			answerWith(http.StatusOK, sandboxRecord(polls[i], i+1))(w, r)
		})
}

// runSandboxWait runs parley sandbox args against hub, the global flags
// first so that words after a -- stay the command's, polling every 10 ms.
func runSandboxWait(t *testing.T, hub *stubHub, args ...string) (int, string, string) {
	t.Helper()
	opts := defaultOptions()
	opts.pollInterval = 10 * time.Millisecond
	return runCLIWith(t, opts, slices.Concat([]string{"--endpoint-url", hub.URL, "--api-key", "k", "sandbox"}, args)...)
}

// wantWaitEnd checks the end of a wait over polls: the create came first,
// the polls went to sbx_1,
// and either exit 0 with the last record alone on stdout, or exit 1 with
// the last record on stdout and errorType on stderr; for wait_timeout
// stdout is empty.
func wantWaitEnd(t *testing.T, hub *stubHub, code int, stdout, stderr string, polls []string, errorType string) {
	t.Helper()
	sent := hub.recorded()
	if len(sent) == 0 || sent[0].method != http.MethodPost || sent[0].rawPath != sandboxesPath {
		t.Fatalf("the hub got %v (exit status %d, stderr %q); want a POST %s first", sent, code, stderr, sandboxesPath)
	}
	for _, r := range sent[1:] {
		if r.method != http.MethodGet || r.rawPath != sandboxesPath+"/sbx_1" {
			t.Errorf("polled %s %s, want GET %s/sbx_1", r.method, r.rawPath, sandboxesPath)
		}
	}
	if errorType == "" {
		if code != 0 || lastDocument(stderr) != "" {
			t.Errorf("exit status %d, stderr %q; want 0 and no envelope", code, stderr)
		}
	} else {
		if code != 1 {
			t.Errorf("exit status = %d, want 1 (stderr %q)", code, stderr)
		}
		if _, _, got := clientEnvelope(t, lastDocument(stderr)); got != errorType {
			t.Errorf("error_type = %q, want %q", got, errorType)
		}
	}
	if errorType == "wait_timeout" {
		if stdout != "" {
			t.Errorf("stdout = %q, want it empty", stdout)
		}
		return
	}
	n := len(polls)
	if want := indented(t, sandboxRecord(polls[n-1], n)); stdout != want {
		t.Errorf("stdout = %q, want the last record %q", stdout, want)
	}
}

// The tests that use wantSends set environment variables, so they do not
// run in parallel.

func TestSandboxCreate_FlagsToBody(t *testing.T) {
	const session = "6f1c2a3b-4d5e-4f60-8a9b-0c1d2e3f4a5b"
	tests := []struct {
		args          []string
		rawPath, body string
	}{
		{[]string{"demo"}, sandboxesPath, `{}`},
		{
			[]string{"demo", "--profile", "agent", "--ttl", "1h", "--purpose", "p", "--session", session, "--restore-from", "sbx_x"},
			sandboxesPath, `{"profile":"agent","ttl":"1h","purpose":"p","session_id":"` + session + `","restore_from":"sbx_x"}`,
		},
		{
			[]string{"demo", "--env", "A=1", "--env", "B=b=c", "--label", "team=core"},
			sandboxesPath, `{"env":{"A":"1","B":"b=c"},"labels":{"team":"core"}}`,
		},
		{[]string{"demo", "--no-checkout"}, sandboxesPath, `{"checkout":false}`},
		{[]string{"demo", "--ref", "release"}, sandboxesPath, `{"checkout":{"ref":"release"}}`},
		{[]string{"demo", "--ref", "main", "--branch", "work"}, sandboxesPath, `{"checkout":{"ref":"main","branch":"work"}}`},
		{[]string{"demo", "--", "make", "test", "--flag"}, sandboxesPath, `{"command":["make","test","--flag"]}`},
		{[]string{"a/b"}, "/api/v1/workspaces/a%2Fb/sandboxes", `{}`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			wantSends(t, append([]string{"sandbox", "create"}, tt.args...), sentRequest{http.MethodPost, tt.rawPath, "", tt.body})
		})
	}
}

func TestSandboxCreate_Wait(t *testing.T) {
	t.Parallel()
	tests := []struct {
		polls     []string
		errorType string
	}{
		{[]string{"provisioning", "provisioning", "ready"}, ""},
		{[]string{"completed"}, ""},
		{[]string{"provisioning", "failed"}, "wait_failed"},
		{[]string{"stopped"}, "wait_failed"},
		{[]string{"deleted"}, "wait_failed"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.polls, ","), func(t *testing.T) {
			t.Parallel()
			hub := sandboxHub(t, tt.polls...)

			code, stdout, stderr := runSandboxWait(t, hub, "create", "demo", "--wait")

			wantWaitEnd(t, hub, code, stdout, stderr, tt.polls, tt.errorType)
			if got := hub.count(http.MethodGet); got != len(tt.polls) {
				t.Errorf("the hub got %d polls, want %d", got, len(tt.polls))
			}
		})
	}
	t.Run("provisioning past the timeout", func(t *testing.T) {
		t.Parallel()
		hub := sandboxHub(t, "provisioning")

		// 1s is the shortest --timeout the duration form allows.
		code, stdout, stderr := runSandboxWait(t, hub, "create", "demo", "--wait", "--timeout", "1s")

		wantWaitEnd(t, hub, code, stdout, stderr, nil, "wait_timeout")
	})
}

func TestSandboxCreate_WaitExit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		polls     []string
		wait      bool
		errorType string
	}{
		{[]string{"provisioning", "ready", "ready", "completed"}, false, ""},
		{[]string{"provisioning", "ready", "ready", "completed"}, true, ""},
		{[]string{"ready", "failed"}, false, "wait_failed"},
		{[]string{"ready", "stopped"}, false, "wait_failed"},
		{[]string{"deleted"}, false, "wait_failed"},
	}
	for _, tt := range tests {
		args := []string{"create", "demo", "--wait-exit"}
		if tt.wait {
			args = append(args, "--wait")
		}
		args = append(args, "--", "make")
		t.Run(strings.Join(args, " ")+" "+strings.Join(tt.polls, ","), func(t *testing.T) {
			t.Parallel()
			hub := sandboxHub(t, tt.polls...)

			code, stdout, stderr := runSandboxWait(t, hub, args...)

			wantWaitEnd(t, hub, code, stdout, stderr, tt.polls, tt.errorType)
			if got := hub.count(http.MethodGet); got != len(tt.polls) {
				t.Errorf("the hub got %d polls, want %d", got, len(tt.polls))
			}
			wantJSONBody(t, hub.recorded()[0].body, `{"command":["make"]}`)
		})
	}
}

func TestSandbox_UsageErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		// says is what the message must name.
		says string
	}{
		{[]string{"create"}, "<workspace-slug>"},
		{[]string{"create", "a", "b"}, `"b"`},
		{[]string{"create", "a", "b", "--", "make"}, `"b"`},
		{[]string{"create", "--", "make"}, "<workspace-slug>"},
		{[]string{"create", "demo", "--wait-exit"}, "after --"},
		{[]string{"create", "demo", "--no-checkout", "--ref", "main"}, "--ref"},
		{[]string{"create", "demo", "--no-checkout", "--branch", "w"}, "--branch"},
		{[]string{"create", "demo", "--env", "A"}, `"A"`},
		{[]string{"create", "demo", "--label", "=v"}, `"=v"`},
		{[]string{"list"}, "<workspace-slug>"},
		{[]string{"list", "a", "b"}, `"b"`},
		{[]string{"get", "demo"}, "<id>"},
		{[]string{"get", "demo", "id", "x"}, `"x"`},
		{[]string{"exec", "demo", "sbx_1"}, "after --"},
		{[]string{"exec", "demo", "sbx_1", "--"}, "after --"},
		{[]string{"exec", "demo", "sbx_1", "make"}, `"make"`},
		{[]string{"exec", "demo", "--", "make"}, "<id>"},
		{[]string{"exec", "demo", "sbx_1", "--env", "NOEQ", "--", "make"}, `"NOEQ"`},
		{[]string{"shell", "demo"}, "<id>"},
		{[]string{"logs", "demo"}, "<id>"},
		{[]string{"logs", "demo", "sbx_1", "--source", "x"}, `"x"`},
		{[]string{"logs", "demo", "sbx_1", "--tail", "0"}, "--tail"},
		{[]string{"upload", "demo", "sbx_1", "f"}, "<sandbox-path>"},
		{[]string{"download", "demo", "sbx_1", "out.txt"}, "<local-path>"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t)

			code, stdout, stderr := runSandboxWait(t, hub, tt.args...)

			if message := wantClientFailure(t, code, stdout, stderr, "usage_error"); !strings.Contains(message, tt.says) {
				t.Errorf("message = %q, want it to name %s", message, tt.says)
			}
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}

func TestSandboxListAndGet(t *testing.T) {
	t.Parallel()
	const session = "6f1c2a3b-4d5e-4f60-8a9b-0c1d2e3f4a5b"
	wantWrites(t, []sentWrite{
		{args: []string{"sandbox", "list", "demo"}, method: http.MethodGet, path: "demo/sandboxes"},
		{
			args: []string{"sandbox", "list", "demo", "--state", "ready", "--state", "failed", "--session", session,
				"--label", "team=core", "--label", "tier=1", "--include-forks", "--limit", "10", "--cursor", "c1"},
			method: http.MethodGet, path: "demo/sandboxes",
			query: url.Values{
				"state": {"ready", "failed"}, "session_id": {session}, "label": {"team=core", "tier=1"},
				"include_forks": {"true"}, "limit": {"10"}, "cursor": {"c1"},
			},
		},
		{args: []string{"sandbox", "get", "demo", "sbx_1"}, method: http.MethodGet, path: "demo/sandboxes/sbx_1"},
		{args: []string{"sandbox", "get", "a/b", "x/y"}, method: http.MethodGet, path: "a%2Fb/sandboxes/x%2Fy"},
	})

	t.Run("a refusal of the hub", func(t *testing.T) {
		t.Parallel()
		const refusal = `{"error":{"code":404,"message":"no such sandbox","error_type":"sandbox_not_found"}}`
		for _, args := range [][]string{{"sandbox", "get", "demo", "sbx_9"}, {"sandbox", "list", "demo"}} {
			hub := newStubHub(t).
				answer(http.MethodGet, sandboxesPath+"/sbx_9", http.StatusNotFound, refusal).
				answer(http.MethodGet, sandboxesPath, http.StatusNotFound, refusal)

			code, stdout, stderr, _ := runOnHub(t, hub, args...)

			if code != 1 || stdout != "" {
				t.Errorf("%v: exit status %d, stdout %q; want 1 and nothing", args, code, stdout)
			}
			if stderr != indented(t, refusal) {
				t.Errorf("%v: stderr = %q, want the hub's envelope unchanged", args, stderr)
			}
		}
	})
}

// execPath is the route parley sandbox exec demo sbx_1 posts to.
const execPath = sandboxesPath + "/sbx_1/exec"

// sseFrame is one frame of an exec's event stream: its event name and data.
type sseFrame struct{ event, data string }

// outputFrame is the frame of output b on stream, stdout or stderr, as the
// hub sends it: the bytes as data when they are UTF-8, else as data_base64.
func outputFrame(stream string, b []byte) sseFrame {
	member, value := "data", string(b)
	if !utf8.Valid(b) {
		member, value = "data_base64", base64.StdEncoding.EncodeToString(b)
	}
	data, _ := json.Marshal(map[string]string{member: value})
	return sseFrame{stream, string(data)}
}

// exitFrame is the frame that ends the stream of a command that exited with
// code.
func exitFrame(code int) sseFrame {
	return sseFrame{"exit", fmt.Sprintf(`{"exit_code":%d,"timed_out":false,"duration_ms":12}`, code)}
}

// timedOutFrame is the frame that ends the stream of a command that was
// killed at its timeout.
var timedOutFrame = sseFrame{"exit", `{"exit_code":null,"timed_out":true,"duration_ms":1000}`}

// execStream is a handler that answers an exec with an event stream of
// frames, after delay, flushing after each frame.
func execStream(delay time.Duration, frames ...sseFrame) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.event, f.data)
			http.NewResponseController(w).Flush()
		}
	}
}

// runExec runs parley sandbox exec demo sbx_1 args against hub with opts,
// the global flags first so that the words after a -- stay the command's.
func runExec(t *testing.T, hub *stubHub, opts options, args ...string) (int, string, string) {
	t.Helper()
	return runCLIWith(t, opts, slices.Concat([]string{"--endpoint-url", hub.URL, "--api-key", "k", "sandbox", "exec", "demo", "sbx_1"}, args)...)
}

// wantOneExec fails the test unless hub got exactly one exec request, and
// returns it.
func wantOneExec(t *testing.T, hub *stubHub) stubRequest {
	t.Helper()
	return wantOnlyRequest(t, hub.recorded(), http.MethodPost, execPath)
}

func TestSandboxExec_WritesTheStreamedOutputAndExitsWithTheCommandsCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		stdout, stderr []byte
		exit, want     int
	}{
		{"output on both streams", []byte("out\n"), []byte("err\n"), 0, 0},
		{"no output", nil, nil, 3, 3},
		{"the highest code kept", []byte("x"), nil, 125, 125},
		{"126 as 125", nil, nil, 126, 125},
		{"255 as 125", nil, nil, 255, 125},
		{"bytes that are not text", []byte{0xff, 0x00}, nil, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var frames []sseFrame
			if tt.stdout != nil {
				frames = append(frames, outputFrame("stdout", tt.stdout))
			}
			if tt.stderr != nil {
				frames = append(frames, outputFrame("stderr", tt.stderr))
			}
			hub := newStubHub(t).handle(http.MethodPost, execPath, execStream(0, append(frames, exitFrame(tt.exit))...))

			code, stdout, stderr := runExec(t, hub, defaultOptions(), "--", "prog", "arg")

			if code != tt.want {
				t.Errorf("exit status = %d, want %d (stderr %q)", code, tt.want, stderr)
			}
			if stdout != string(tt.stdout) || stderr != string(tt.stderr) {
				t.Errorf("stdout %q, stderr %q; want %q and %q", stdout, stderr, tt.stdout, tt.stderr)
			}
			got := wantOneExec(t, hub)
			if accept := got.header.Get("Accept"); accept != "text/event-stream" {
				t.Errorf("Accept = %q, want text/event-stream", accept)
			}
			wantJSONBody(t, got.body, `{"command":["prog","arg"]}`)
		})
	}
	t.Run("a command that outlasts the request timeout", func(t *testing.T) {
		t.Parallel()
		hub := newStubHub(t).handle(http.MethodPost, execPath, execStream(200*time.Millisecond, outputFrame("stdout", []byte("late")), exitFrame(0)))
		opts := defaultOptions()
		opts.requestTimeout = 50 * time.Millisecond

		code, stdout, stderr := runExec(t, hub, opts, "--", "sleep")

		if code != 0 || stdout != "late" || stderr != "" {
			t.Errorf("exit status %d, stdout %q, stderr %q; want 0, %q and nothing", code, stdout, stderr, "late")
		}
	})
}

func TestSandboxExec_SendsOnlyTheMembersOfTheFlagsGiven(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t).handle(http.MethodPost, execPath, execStream(0, exitFrame(0)))
	opts := defaultOptions()
	opts.stdin = strings.NewReader("in\x00\xff")

	code, _, stderr := runExec(t, hub, opts, "--workdir", "/tmp", "--env", "A=1", "--env", "B=x=y", "--timeout", "90s", "--stdin", "--", "cat")

	if code != 0 {
		t.Fatalf("exit status = %d, want 0 (stderr %q)", code, stderr)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(wantOneExec(t, hub).body), &body); err != nil {
		t.Fatal(err)
	}
	stdin, err := base64.StdEncoding.DecodeString(fmt.Sprint(body["stdin_base64"]))
	if err != nil || string(stdin) != "in\x00\xff" {
		t.Errorf("stdin_base64 decodes to %q (%v), want %q", stdin, err, "in\x00\xff")
	}
	delete(body, "stdin_base64")
	want := map[string]any{"command": []any{"cat"}, "workdir": "/tmp", "env": map[string]any{"A": "1", "B": "x=y"}, "timeout": "90s"}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body without stdin_base64 = %v, want %v", body, want)
	}

	t.Run("without flags", func(t *testing.T) {
		t.Parallel()
		hub := newStubHub(t).handle(http.MethodPost, execPath, execStream(0, exitFrame(0)))

		code, _, stderr := runExec(t, hub, defaultOptions(), "--", "cat")

		if code != 0 {
			t.Fatalf("exit status = %d, want 0 (stderr %q)", code, stderr)
		}
		wantJSONBody(t, wantOneExec(t, hub).body, `{"command":["cat"]}`)
	})
}

func TestSandboxExec_JSONPrintsTheCollectedAnswer(t *testing.T) {
	t.Parallel()
	// A NUL byte is escaped to six, so a full stdout and part of a stderr
	// make an answer past 8 MiB.
	nuls := func(n int) string { return strings.Repeat(`\u0000`, n) }
	escaped := fmt.Sprintf(`{"exit_code":0,"stdout":"%s","stderr":"%s","truncated":true,"timed_out":false,"duration_ms":4}`, nuls(1<<20), nuls(400<<10))
	tests := []struct {
		name, answer string
		want         int
	}{
		{"exits 0", `{"exit_code":0,"stdout":"hi\n","stderr":"","truncated":false,"timed_out":false,"duration_ms":4}`, 0},
		{"exits 7", `{"exit_code":7,"stdout":"","stderr":"bad","truncated":false,"timed_out":false,"duration_ms":4}`, 7},
		{"exits 200", `{"exit_code":200,"stdout":"","stderr":"","truncated":false,"timed_out":false,"duration_ms":4}`, 125},
		{"times out", `{"exit_code":null,"stdout":"part","stderr":"","truncated":false,"timed_out":true,"duration_ms":1000}`, 1},
		{"output escaped past 8 MiB", escaped, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodPost, execPath, http.StatusOK, tt.answer)

			code, stdout, stderr := runExec(t, hub, defaultOptions(), "--json", "--", "prog")

			if code != tt.want {
				t.Errorf("exit status = %d, want %d (stderr %q)", code, tt.want, stderr)
			}
			if want := indented(t, tt.answer); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
				t.Fatal(err)
			}
			for _, member := range []string{"exit_code", "stdout", "stderr", "truncated", "timed_out", "duration_ms"} {
				if _, ok := doc[member]; !ok {
					t.Errorf("stdout lacks %s: %q", member, stdout)
				}
			}
			if tt.want == 1 {
				if _, _, errorType := clientEnvelope(t, stderr); errorType != "wait_timeout" {
					t.Errorf("error_type = %q, want wait_timeout", errorType)
				}
			} else if stderr != "" {
				t.Errorf("stderr = %q, want it empty", stderr)
			}
			got := wantOneExec(t, hub)
			if accept := got.header.Get("Accept"); strings.Contains(accept, "text/event-stream") {
				t.Errorf("Accept = %q, want one that does not name text/event-stream", accept)
			}
			wantJSONBody(t, got.body, `{"command":["prog"]}`)
		})
	}
}

func TestSandboxExec_ReportsAFailureAsAnErrorEnvelope(t *testing.T) {
	t.Parallel()
	const refusal = `{"error":{"code":409,"message":"the sandbox is stopped","error_type":"sandbox_not_ready"}}`
	tests := []struct {
		name      string
		stdin     string
		handler   http.HandlerFunc
		code      int
		errorType string
		stdout    string
		requests  int
	}{
		{name: "standard input over 4 MiB", stdin: strings.Repeat("x", 4194305), code: 2, errorType: "client_error"},
		{name: "a refusal of the hub", handler: answerWith(http.StatusConflict, refusal), code: 1, errorType: "sandbox_not_ready", requests: 1},
		{
			name: "a command that times out", handler: execStream(0, outputFrame("stdout", []byte("part")), timedOutFrame),
			code: 1, errorType: "wait_timeout", stdout: "part", requests: 1,
		},
		{
			name: "a stream that ends without an exit frame", handler: execStream(0, outputFrame("stdout", []byte("part"))),
			code: 2, errorType: "client_error", stdout: "part", requests: 1,
		},
		{
			name: "a stream that breaks", handler: func(w http.ResponseWriter, r *http.Request) {
				execStream(0, outputFrame("stdout", []byte("part")))(w, r)
				panic(http.ErrAbortHandler)
			},
			code: 2, errorType: "client_error", stdout: "part", requests: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t)
			if tt.handler != nil {
				hub.handle(http.MethodPost, execPath, tt.handler)
			}
			opts := defaultOptions()
			args := []string{"--", "prog"}
			if tt.stdin != "" {
				opts.stdin = strings.NewReader(tt.stdin)
				args = append([]string{"--stdin"}, args...)
			}

			code, stdout, stderr := runExec(t, hub, opts, args...)

			if code != tt.code || stdout != tt.stdout {
				t.Errorf("exit status %d, stdout %q; want %d and %q (stderr %q)", code, stdout, tt.code, tt.stdout, stderr)
			}
			envelopeCode, _, errorType := clientEnvelope(t, stderr)
			if errorType != tt.errorType {
				t.Errorf("error_type = %q, want %q", errorType, tt.errorType)
			}
			if tt.errorType == "sandbox_not_ready" {
				if envelopeCode != 409 || stderr != indented(t, refusal) {
					t.Errorf("stderr = %q, want the hub's envelope unchanged", stderr)
				}
			} else if envelopeCode != 0 {
				t.Errorf("envelope code = %v, want 0", envelopeCode)
			}
			if n := len(hub.recorded()); n != tt.requests {
				t.Errorf("the hub got %d requests, want %d", n, tt.requests)
			}
		})
	}
}

// logsPath is the route parley sandbox logs demo sbx_1 reads.
const logsPath = sandboxesPath + "/sbx_1/logs"

// logLine is the n-th stored line of sbx_1 as the hub serves it in a page,
// with spaces that parley does not print.
func logLine(n int) string {
	return fmt.Sprintf(`{"time": "2026-11-19T10:00:00Z", "source": "process", "stream": "stdout", "line": "line %d"}`, n)
}

// logLines are the lines 1 to n.
func logLines(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = logLine(i + 1)
	}
	return lines
}

// logPages is a handler that serves lines in pages of 50, the cursor being
// the index of the page's first line.
func logPages(lines []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		end := min(start+50, len(lines))
		next := "null"
		if end < len(lines) {
			next = strconv.Quote(strconv.Itoa(end))
		}
		answerWith(http.StatusOK, `{"items": [`+strings.Join(lines[start:end], ", ")+`], "next_cursor": `+next+`}`)(w, r)
	}
}

// compactLine is the JSON document line compacted.
func compactLine(t *testing.T, line string) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, []byte(line)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// compactLines is lines as parley prints them: each compacted, on a line of
// its own.
func compactLines(t *testing.T, lines []string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range lines {
		out.WriteString(compactLine(t, line) + "\n")
	}
	return out.String()
}

// eventually fails the test unless cond holds within 5 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 5 s for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// background runs parley with args and opts in a goroutine; the channel
// gives its exit status.
func background(opts options, stdout, stderr io.Writer, args ...string) <-chan int {
	done := make(chan int, 1)
	go func() { done <- run(context.Background(), args, stdout, stderr, opts) }()
	return done
}

// exitOf is the exit status done gives within 5 seconds.
func exitOf(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("parley did not end within 5 s")
		return -1
	}
}

func TestSandboxLogs_PrintsEachLineAsOneCompactJSONObject(t *testing.T) {
	t.Parallel()
	t.Run("every page, in the order the hub returned them", func(t *testing.T) {
		t.Parallel()
		lines := logLines(120)
		hub := newStubHub(t).handle(http.MethodGet, logsPath, logPages(lines))

		code, stdout, stderr, sent := runOnHub(t, hub, "sandbox", "logs", "demo", "sbx_1")

		if code != 0 || stderr != "" {
			t.Errorf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
		}
		if want := compactLines(t, lines); stdout != want {
			t.Errorf("stdout = %q, want the 120 lines compacted, one per line", stdout)
		}
		for _, line := range nonEmptyLines(stdout) {
			var item map[string]any
			if err := json.Unmarshal([]byte(line), &item); err != nil || !reflect.DeepEqual(sortedKeys(item), []string{"line", "source", "stream", "time"}) {
				t.Errorf("line %q is not an object with time, source, stream and line (%v)", line, err)
			}
		}
		if len(sent) != 3 {
			t.Fatalf("the hub got %d requests, want 3", len(sent))
		}
		for i, cursor := range []string{"", "50", "100"} {
			want := url.Values{}
			if cursor != "" {
				want.Set("cursor", cursor)
			}
			wantQuery(t, sent[i].query, want)
		}
	})
	t.Run("with the filters given on every page", func(t *testing.T) {
		t.Parallel()
		hub := newStubHub(t).handle(http.MethodGet, logsPath, logPages(logLines(60)))

		code, stdout, stderr, sent := runOnHub(t, hub, "sandbox", "logs", "demo", "sbx_1",
			"--source", "policy", "--since", "2026-01-02T03:04:05Z", "--tail", "5")

		if code != 0 || stderr != "" || len(nonEmptyLines(stdout)) != 60 {
			t.Errorf("exit status %d, stderr %q, %d lines; want 0, nothing and 60", code, stderr, len(nonEmptyLines(stdout)))
		}
		filters := url.Values{"source": {"policy"}, "since": {"2026-01-02T03:04:05Z"}, "tail": {"5"}}
		if len(sent) != 2 {
			t.Fatalf("the hub got %d requests, want 2", len(sent))
		}
		wantQuery(t, sent[0].query, filters)
		filters.Set("cursor", "50")
		wantQuery(t, sent[1].query, filters)
	})
	t.Run("followed until the hub ends the stream", func(t *testing.T) {
		t.Parallel()
		stored := make(chan struct{})
		first, second := compactLine(t, logLine(1)), compactLine(t, logLine(2))
		hub := newStubHub(t).handle(http.MethodGet, logsPath, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", first)
			http.NewResponseController(w).Flush()
			select {
			case <-stored:
			case <-r.Context().Done():
				return
			}
			// Returning ends the stream, as the hub does once the sandbox
			// is deleted.
			fmt.Fprintf(w, ": heartbeat\n\nevent: log\ndata: %s\n\n", second)
		})
		opts := defaultOptions()
		var deadlines atomic.Int32
		opts.transport = func(next http.RoundTripper) http.RoundTripper {
			return roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if _, ok := r.Context().Deadline(); ok {
					deadlines.Add(1)
				}
				return next.RoundTrip(r)
			})
		}
		var stdout, stderr syncBuffer

		done := background(opts, &stdout, &stderr, onHub(hub.URL, "sandbox", "logs", "demo", "sbx_1", "--follow", "--tail", "1")...)
		eventually(t, "the first line", func() bool { return len(nonEmptyLines(stdout.String())) == 1 })
		close(stored)
		code := exitOf(t, done)

		if code != 0 || stderr.String() != "" {
			t.Errorf("exit status %d, stderr %q; want 0 and nothing", code, stderr.String())
		}
		if want := compactLines(t, logLines(2)); stdout.String() != want {
			t.Errorf("stdout = %q, want %q", stdout.String(), want)
		}
		got := wantOnlyRequest(t, hub.recorded(), http.MethodGet, logsPath)
		wantQuery(t, got.query, url.Values{"follow": {"true"}, "tail": {"1"}})
		if accept := got.header.Get("Accept"); accept != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", accept)
		}
		if n := deadlines.Load(); n != 0 {
			t.Errorf("the stream was opened under a deadline")
		}
	})
	t.Run("a refusal of the hub, printed as received", func(t *testing.T) {
		t.Parallel()
		const refusal = `{"error":{"code":400,"message":"since is not a time","error_type":"invalid_request"}}`
		for _, follow := range []bool{false, true} {
			hub := newStubHub(t).answer(http.MethodGet, logsPath, http.StatusBadRequest, refusal)
			args := []string{"sandbox", "logs", "demo", "sbx_1", "--since", "soon"}
			if follow {
				args = append(args, "--follow")
			}

			code, stdout, stderr, sent := runOnHub(t, hub, args...)

			if code != 1 || stdout != "" || stderr != indented(t, refusal) {
				t.Errorf("follow %v: exit status %d, stdout %q, stderr %q; want 1, nothing and the hub's envelope", follow, code, stdout, stderr)
			}
			if len(sent) != 1 {
				t.Errorf("follow %v: the hub got %d requests, want 1", follow, len(sent))
			}
		}
	})
}

// attachPath is the route parley sandbox shell demo sbx_1 opens.
const attachPath = sandboxesPath + "/sbx_1/attach"

// wsFrame is a WebSocket frame a stub terminal received: its type and its
// payload.
type wsFrame struct {
	kind byte
	data string
}

// receiveFrames receives one frame into a *wsFrame, keeping its type.
var receiveFrames = websocket.Codec{Unmarshal: func(data []byte, kind byte, v any) error {
	*v.(*wsFrame) = wsFrame{kind, string(data)}
	return nil
}}

// echoTerminal is the attach of a stub hub: a WebSocket on which every
// binary frame is sent back, as a terminal echoes what is typed, and every
// text frame is passed to texts. Closing exit closes the connection, as the
// hub does when the shell exits.
type echoTerminal struct {
	opened, exit chan struct{}
	texts        chan string
}

func newEchoTerminal() *echoTerminal {
	return &echoTerminal{opened: make(chan struct{}), exit: make(chan struct{}), texts: make(chan string, 8)}
}

func (e *echoTerminal) serve(w http.ResponseWriter, r *http.Request) {
	websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			close(e.opened)
			go func() {
				<-e.exit
				ws.Close()
			}()
			for {
				var f wsFrame
				if err := receiveFrames.Receive(ws, &f); err != nil {
					return
				}
				if f.kind == websocket.TextFrame {
					e.texts <- f.data
					continue
				}
				if err := websocket.Message.Send(ws, []byte(f.data)); err != nil {
					return
				}
			}
		},
	}.ServeHTTP(w, r)
}

// wait fails the test unless ch is closed within 5 seconds.
func wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("waited 5 s for %s", what)
	}
}

func TestSandboxShell_RelaysATerminalOverAWebSocket(t *testing.T) {
	t.Parallel()
	t.Run("copies standard input to the terminal and its output to stdout", func(t *testing.T) {
		t.Parallel()
		term := newEchoTerminal()
		hub := newStubHub(t).handle(http.MethodGet, attachPath, term.serve)
		stdin, typed := io.Pipe()
		t.Cleanup(func() { typed.Close() })
		opts := defaultOptions()
		opts.stdin = stdin
		var stdout, stderr syncBuffer

		done := background(opts, &stdout, &stderr, onHub(hub.URL, "sandbox", "shell", "demo", "sbx_1")...)
		wait(t, term.opened, "the WebSocket")
		got := wantOnlyRequest(t, hub.recorded(), http.MethodGet, attachPath)
		wantQuery(t, got.query, url.Values{"cols": {"80"}, "rows": {"24"}})
		if auth := got.header.Get("Authorization"); auth != "Bearer k" {
			t.Errorf("Authorization = %q, want Bearer k", auth)
		}
		if _, err := typed.Write([]byte("ls\n")); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the echo on stdout", func() bool { return stdout.String() == "ls\n" })
		close(term.exit)

		if code := exitOf(t, done); code != 0 || stderr.String() != "" {
			t.Errorf("exit status %d, stderr %q; want 0 and nothing", code, stderr.String())
		}
	})
	t.Run("prints the hub's refusal before the upgrade", func(t *testing.T) {
		t.Parallel()
		const refusal = `{"error":{"code":409,"message":"the sandbox is stopped","error_type":"sandbox_not_ready"}}`
		hub := newStubHub(t).handle(http.MethodGet, attachPath, func(w http.ResponseWriter, r *http.Request) {
			// As the hub does, a request that asks for no upgrade is
			// refused before the sandbox's state is judged.
			if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
				r.Header.Get("Sec-WebSocket-Key") == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
				answerWith(http.StatusBadRequest, `{"error":{"code":400,"message":"not a WebSocket upgrade","error_type":"invalid_request"}}`)(w, r)
				return
			}
			answerWith(http.StatusConflict, refusal)(w, r)
		})

		code, stdout, stderr, sent := runOnHub(t, hub, "sandbox", "shell", "demo", "sbx_1")

		if code != 1 || stdout != "" || stderr != indented(t, refusal) {
			t.Errorf("exit status %d, stdout %q, stderr %q; want 1, nothing and the hub's envelope", code, stdout, stderr)
		}
		if len(sent) != 2 {
			t.Errorf("the hub got %d requests, want the upgrade and its repetition", len(sent))
		}
		for _, r := range sent {
			if r.path != attachPath || r.header.Get("Authorization") != "Bearer k" {
				t.Errorf("sent %s %s with Authorization %q, want the attach route with Bearer k", r.method, r.path, r.header.Get("Authorization"))
			}
		}
	})
	t.Run("a hub it cannot reach", func(t *testing.T) {
		t.Parallel()
		code, stdout, stderr := runCLI(t, "--endpoint-url", closedPortURL(t), "--api-key", "k", "sandbox", "shell", "demo", "sbx_1")

		wantClientFailure(t, code, stdout, stderr, "client_error")
	})
	t.Run("sends a resize frame", func(t *testing.T) {
		t.Parallel()
		term := newEchoTerminal()
		srv := httptest.NewServer(http.HandlerFunc(term.serve))
		t.Cleanup(srv.Close)
		config, err := websocket.NewConfig("ws"+strings.TrimPrefix(srv.URL, "http"), srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		ws, err := config.DialContext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ws.Close() })

		if err := sendResize(ws, 100, 30); err != nil {
			t.Fatal(err)
		}

		select {
		case text := <-term.texts:
			if text != `{"resize":{"cols":100,"rows":30}}` {
				t.Errorf("text frame = %q, want the resize to 100x30", text)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("waited 5 s for a text frame")
		}
	})
}

// filesPath is the route parley sandbox upload and download demo sbx_1 send
// to.
const filesPath = sandboxesPath + "/sbx_1/files"

// runFiles runs parley with args against hub and returns the exit status,
// stdout, stderr, the requests the hub got and how many of them were sent
// under a deadline.
func runFiles(t *testing.T, hub *stubHub, args ...string) (code int, stdout, stderr string, sent []stubRequest, deadlines int32) {
	t.Helper()
	opts := defaultOptions()
	var n atomic.Int32
	opts.transport = func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if _, ok := r.Context().Deadline(); ok {
				n.Add(1)
			}
			return next.RoundTrip(r)
		})
	}
	code, stdout, stderr = runCLIWith(t, opts, onHub(hub.URL, args...)...)
	return code, stdout, stderr, hub.recorded(), n.Load()
}

// tarEntry is one entry of a tar stream a test builds or reads: a name
// ending in / is a directory.
type tarEntry struct {
	name    string
	mode    int64
	content string
}

// tarOf is the tar stream of entries.
func tarOf(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: tar.TypeReg, Size: int64(len(e.content))}
		if strings.HasSuffix(e.name, "/") {
			hdr.Typeflag, hdr.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// tarEntriesOf reads the tar stream b.
func tarEntriesOf(t *testing.T, b string) []tarEntry {
	t.Helper()
	var entries []tarEntry
	tr := tar.NewReader(strings.NewReader(b))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatalf("the body is not a tar stream: %v", err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, tarEntry{hdr.Name, hdr.Mode, string(content)})
	}
}

// writeLocal writes content to path with mode, whatever the umask.
func writeLocal(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxUpload_SendsAFileOrADirectoryAndPrintsTheHubsAnswer(t *testing.T) {
	t.Parallel()
	t.Run("a file, with its mode", func(t *testing.T) {
		t.Parallel()
		const answer = `{"path":"/workspace/bin/f.sh","bytes":4,"entries":1}`
		hub := newStubHub(t).answer(http.MethodPut, filesPath, http.StatusOK, answer)
		local := filepath.Join(t.TempDir(), "f.sh")
		writeLocal(t, local, "echo", 0o750)

		code, stdout, stderr, sent, deadlines := runFiles(t, hub, "sandbox", "upload", "demo", "sbx_1", local, "bin/f.sh")

		if code != 0 || stdout != indented(t, answer) || stderr != "" {
			t.Errorf("exit status %d, stdout %q, stderr %q; want 0, the hub's answer and nothing", code, stdout, stderr)
		}
		got := wantOnlyRequest(t, sent, http.MethodPut, filesPath)
		wantQuery(t, got.query, url.Values{"path": {"bin/f.sh"}, "mode": {"0750"}})
		if got.body != "echo" || got.contentLength != 4 || got.header.Get("Content-Type") != "application/octet-stream" {
			t.Errorf("sent body %q with length %d and Content-Type %q, want echo, 4 and application/octet-stream", got.body, got.contentLength, got.header.Get("Content-Type"))
		}
		if deadlines != 0 {
			t.Errorf("the upload was sent under a deadline")
		}
	})
	t.Run("a directory, as a tar stream without its links", func(t *testing.T) {
		t.Parallel()
		const answer = `{"path":"/workspace/proj","bytes":2,"entries":2}`
		hub := newStubHub(t).answer(http.MethodPut, filesPath, http.StatusOK, answer)
		dir := filepath.Join(t.TempDir(), "d")
		writeLocal(t, filepath.Join(dir, "a", "b.txt"), "hi", 0o640)
		if err := os.Symlink(filepath.Join("a", "b.txt"), filepath.Join(dir, "l")); err != nil {
			t.Fatal(err)
		}

		code, stdout, stderr, sent, deadlines := runFiles(t, hub, "sandbox", "upload", "demo", "sbx_1", dir, "proj")

		if code != 0 || stdout != indented(t, answer) {
			t.Errorf("exit status %d, stdout %q; want 0 and the hub's answer (stderr %q)", code, stdout, stderr)
		}
		if !strings.HasPrefix(stderr, "warning: ") || !strings.Contains(stderr, filepath.Join(dir, "l")) || len(nonEmptyLines(stderr)) != 1 {
			t.Errorf("stderr = %q, want one warning line naming the link", stderr)
		}
		got := wantOnlyRequest(t, sent, http.MethodPut, filesPath)
		wantQuery(t, got.query, url.Values{"path": {"proj"}, "archive": {"tar"}})
		if ct := got.header.Get("Content-Type"); ct != "application/x-tar" || got.contentLength != -1 {
			t.Errorf("Content-Type %q, length %d; want application/x-tar streamed without a length", ct, got.contentLength)
		}
		dirMode := int64(0o755)
		if info, err := os.Stat(filepath.Join(dir, "a")); err == nil {
			dirMode = int64(info.Mode().Perm())
		}
		want := []tarEntry{{"a/", dirMode, ""}, {"a/b.txt", 0o640, "hi"}}
		if entries := tarEntriesOf(t, got.body); !reflect.DeepEqual(entries, want) {
			t.Errorf("tar entries = %v, want %v", entries, want)
		}
		if deadlines != 0 {
			t.Errorf("the upload was sent under a deadline")
		}
	})
	t.Run("a refusal of the hub, printed as received", func(t *testing.T) {
		t.Parallel()
		const refusal = `{"error":{"code":409,"message":"the sandbox is stopped","error_type":"sandbox_not_ready"}}`
		dir := t.TempDir()
		writeLocal(t, filepath.Join(dir, "f"), "x", 0o644)
		for _, local := range []string{filepath.Join(dir, "f"), dir} {
			hub := newStubHub(t).answer(http.MethodPut, filesPath, http.StatusConflict, refusal)

			code, stdout, stderr, sent, _ := runFiles(t, hub, "sandbox", "upload", "demo", "sbx_1", local, "x")

			if code != 1 || stdout != "" || stderr != indented(t, refusal) || len(sent) != 1 {
				t.Errorf("%s: exit status %d, stdout %q, stderr %q, %d requests; want 1, nothing, the hub's envelope and 1", local, code, stdout, stderr, len(sent))
			}
		}
	})
	for _, tt := range []struct {
		name  string
		local func(t *testing.T) string
	}{
		{"a local path that does not exist", func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "no", "such")
		}},
		{"a directory holding a file it cannot read", func(t *testing.T) string {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file of mode 000")
			}
			dir := t.TempDir()
			writeLocal(t, filepath.Join(dir, "a"), "x", 0o644)
			writeLocal(t, filepath.Join(dir, "sub", "locked"), "x", 0o000)
			return dir
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t)

			code, stdout, stderr, sent, _ := runFiles(t, hub, "sandbox", "upload", "demo", "sbx_1", tt.local(t), "x")

			wantClientFailure(t, code, stdout, stderr, "client_error")
			if len(sent) != 0 {
				t.Errorf("the hub got %d requests, want none", len(sent))
			}
		})
	}
}

// projTar is the tar stream a hub sends for the directory proj.
var projTar = []tarEntry{{"src/", 0o755, ""}, {"src/main.go", 0o644, "package main"}, {"run.sh", 0o755, "#!/bin/sh\n"}}

// filesHub is a stub hub whose sandbox sbx_1 holds the file out.txt and
// the directory proj, and whose tar stream of the directory cut is cut
// off by a dropped connection.
func filesHub(t *testing.T) *stubHub {
	t.Helper()
	proj := tarOf(t, projTar...)
	return newStubHub(t).handle(http.MethodGet, filesPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch p, tarred := q.Get("path"), q.Get("archive") == "tar"; {
		case p == "out.txt" && !tarred:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", "6")
			w.Write([]byte("result"))
		case (p == "proj" || p == "cut") && !tarred:
			answerWith(http.StatusBadRequest, `{"error":{"code":400,"message":"path is a directory","error_type":"path_is_directory"}}`)(w, r)
		case p == "proj":
			w.Header().Set("Content-Type", "application/x-tar")
			w.Write(proj)
		case p == "cut":
			w.Header().Set("Content-Type", "application/x-tar")
			w.Write(proj[:len(proj)/2])
			http.NewResponseController(w).Flush()
			panic(http.ErrAbortHandler)
		default:
			answerWith(http.StatusNotFound, `{"error":{"code":404,"message":"the sandbox has no such path","error_type":"path_not_found"}}`)(w, r)
		}
	})
}

// transferResult is the document parley sandbox download prints.
func transferResult(t *testing.T, path string, n int) string {
	t.Helper()
	b, err := json.Marshal(struct {
		Path  string `json:"path"`
		Bytes int    `json:"bytes"`
	}{path, n})
	if err != nil {
		t.Fatal(err)
	}
	return indented(t, string(b))
}

// wantNothingAt fails the test unless path does not exist.
func wantNothingAt(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (%v), want nothing there", path, err)
	}
}

func TestSandboxDownload_WritesAFileOrUnpacksADirectory(t *testing.T) {
	t.Parallel()
	t.Run("a file, through a temporary file beside it", func(t *testing.T) {
		t.Parallel()
		hub := filesHub(t)
		tmp := t.TempDir()
		local := filepath.Join(tmp, "o.txt")

		code, stdout, stderr, sent, deadlines := runFiles(t, hub, "sandbox", "download", "demo", "sbx_1", "out.txt", local)

		if code != 0 || stdout != transferResult(t, local, 6) || stderr != "" {
			t.Errorf("exit status %d, stdout %q, stderr %q; want 0, the path and 6 bytes, and nothing", code, stdout, stderr)
		}
		if b, err := os.ReadFile(local); err != nil || string(b) != "result" {
			t.Errorf("%s holds %q (%v), want result", local, b, err)
		}
		if names, _ := os.ReadDir(tmp); len(names) != 1 {
			t.Errorf("the directory holds %v, want o.txt alone", names)
		}
		got := wantOnlyRequest(t, sent, http.MethodGet, filesPath)
		wantQuery(t, got.query, url.Values{"path": {"out.txt"}})
		if deadlines != 0 {
			t.Errorf("the download was sent under a deadline")
		}
	})
	t.Run("a file to stdout", func(t *testing.T) {
		t.Parallel()
		code, stdout, stderr, _, _ := runFiles(t, filesHub(t), "sandbox", "download", "demo", "sbx_1", "out.txt", "-")

		if code != 0 || stdout != "result" || stderr != "" {
			t.Errorf("exit status %d, stdout %q, stderr %q; want 0, result and nothing", code, stdout, stderr)
		}
	})
	t.Run("a directory, unpacked with its modes", func(t *testing.T) {
		t.Parallel()
		local := filepath.Join(t.TempDir(), "p")

		code, stdout, stderr, sent, deadlines := runFiles(t, filesHub(t), "sandbox", "download", "demo", "sbx_1", "proj", local)

		if code != 0 || stdout != transferResult(t, local, len("package main")+len("#!/bin/sh\n")) || stderr != "" {
			t.Errorf("exit status %d, stdout %q, stderr %q; want 0, the path and the sum of the file sizes, and nothing", code, stdout, stderr)
		}
		for _, e := range projTar {
			info, err := os.Stat(filepath.Join(local, e.name))
			if err != nil {
				t.Errorf("%s: %v", e.name, err)
				continue
			}
			if perm := int64(info.Mode().Perm()); perm != e.mode {
				t.Errorf("%s has mode %04o, want %04o", e.name, perm, e.mode)
			}
			if !info.IsDir() {
				if b, _ := os.ReadFile(filepath.Join(local, e.name)); string(b) != e.content {
					t.Errorf("%s holds %q, want %q", e.name, b, e.content)
				}
			}
		}
		if len(sent) != 2 {
			t.Fatalf("the hub got %d requests, want 2", len(sent))
		}
		wantQuery(t, sent[0].query, url.Values{"path": {"proj"}})
		wantQuery(t, sent[1].query, url.Values{"path": {"proj"}, "archive": {"tar"}})
		if deadlines != 0 {
			t.Errorf("the download was sent under a deadline")
		}
	})
	t.Run("a refusal of the hub, leaving nothing", func(t *testing.T) {
		t.Parallel()
		local := filepath.Join(t.TempDir(), "m")

		code, stdout, stderr, sent, _ := runFiles(t, filesHub(t), "sandbox", "download", "demo", "sbx_1", "missing", local)

		if code != 1 || stdout != "" {
			t.Errorf("exit status %d, stdout %q; want 1 and nothing", code, stdout)
		}
		if _, _, errorType := clientEnvelope(t, stderr); errorType != "path_not_found" {
			t.Errorf("error_type = %q, want path_not_found", errorType)
		}
		if len(sent) != 1 {
			t.Errorf("the hub got %d requests, want 1", len(sent))
		}
		wantNothingAt(t, local)
	})
	t.Run("a tar stream cut off", func(t *testing.T) {
		t.Parallel()
		local := filepath.Join(t.TempDir(), "c")

		code, stdout, stderr, _, _ := runFiles(t, filesHub(t), "sandbox", "download", "demo", "sbx_1", "cut", local)

		wantClientFailure(t, code, stdout, stderr, "client_error")
		wantNothingAt(t, local)
	})
}

func TestUnpackTar_RefusesAnEntryOutsideItsTarget(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"../x", "/x", "a/../../x"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			target := filepath.Join(tmp, "t")
			stream := tarOf(t, tarEntry{"ok.txt", 0o644, "ok"}, tarEntry{name, 0o644, "out"})

			_, err := unpackTar(bytes.NewReader(stream), target)

			var ce *cliError
			if !errors.As(err, &ce) || ce.errorType != clientErrorType {
				t.Errorf("err = %v, want a client_error", err)
			}
			wantNothingAt(t, filepath.Join(tmp, "x"))
			if names, _ := os.ReadDir(tmp); len(names) > 1 || (len(names) == 1 && names[0].Name() != "t") {
				t.Errorf("the target's parent holds %v, want at most the target", names)
			}
		})
	}
}
