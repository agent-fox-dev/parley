package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
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
	tests := []struct {
		name, answer string
		want         int
	}{
		{"exits 0", `{"exit_code":0,"stdout":"hi\n","stderr":"","truncated":false,"timed_out":false,"duration_ms":4}`, 0},
		{"exits 7", `{"exit_code":7,"stdout":"","stderr":"bad","truncated":false,"timed_out":false,"duration_ms":4}`, 7},
		{"exits 200", `{"exit_code":200,"stdout":"","stderr":"","truncated":false,"timed_out":false,"duration_ms":4}`, 125},
		{"times out", `{"exit_code":null,"stdout":"part","stderr":"","truncated":false,"timed_out":true,"duration_ms":1000}`, 1},
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
