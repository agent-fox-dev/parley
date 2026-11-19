package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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
