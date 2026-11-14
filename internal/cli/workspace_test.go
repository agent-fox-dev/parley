package cli

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The route parley workspace create posts to, and the one its --wait polls
// for the workspace demo, as sent.
const (
	workspacesPath = "/api/v1/workspaces"
	demoPath       = "/api/v1/workspaces/demo"
)

// workspaceRecord is the workspace demo in status as the hub answers it,
// with cloneError as its clone_error, null when empty.
func workspaceRecord(status, cloneError string) string {
	ce := "null"
	if cloneError != "" {
		ce = `"` + cloneError + `"`
	}
	return `{"slug":"demo","kind":"repository","status":"` + status + `","clone_error":` + ce + `,"settings":{},"access":"owner"}`
}

// inTurn answers the n-th request with answers[n-1], the last of answers
// answering every later one too.
func inTurn(answers ...http.HandlerFunc) http.HandlerFunc {
	var mu sync.Mutex
	n := 0
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := min(n, len(answers)-1)
		n++
		mu.Unlock()
		answers[i](w, r)
	}
}

// createDemo is the command line that registers demo, followed by more.
func createDemo(more ...string) []string {
	return append([]string{"workspace", "create", "--slug", "demo", "--git-url", "https://example.com/a.git"}, more...)
}

// runWorkspace runs args against hub, polling every 10 ms in place of
// --poll-interval, and returns the exit status, stdout and stderr.
func runWorkspace(t *testing.T, hub *stubHub, args ...string) (int, string, string) {
	t.Helper()
	opts := defaultOptions()
	opts.pollInterval = 10 * time.Millisecond
	return runCLIWith(t, opts, onHub(hub.URL, args...)...)
}

func TestWorkspaceCreate_PostsAMemberForEachFlagGivenAndPrintsTheWorkspace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		body string
	}{
		{"slug and git URL alone", createDemo(), `{"slug":"demo","git_url":"https://example.com/a.git"}`},
		{
			"every flag, with a user name and a password",
			createDemo("--default-branch", "main", "--org", "acme", "--display-name", "Demo", "--description", "a demo",
				"--workspace-mode", "carry_patch", "--upstream-url", "https://example.com/up.git", "--integration-branch", "deploy",
				"--git-username", "u", "--git-password", "p", "--setting", "a=true"),
			`{"slug":"demo","git_url":"https://example.com/a.git","default_branch":"main","org_id":"acme","display_name":"Demo",
			"description":"a demo","workspace_mode":"carry_patch","upstream_url":"https://example.com/up.git","integration_branch":"deploy",
			"git_username":"u","git_password":"p","settings":{"a":true}}`,
		},
		{"a token", createDemo("--git-pat", "t"), `{"slug":"demo","git_url":"https://example.com/a.git","git_pat":"t"}`},
		{
			"flags given empty are sent empty",
			createDemo("--default-branch", "", "--description", "", "--workspace-mode", ""),
			`{"slug":"demo","git_url":"https://example.com/a.git","default_branch":"","description":"","workspace_mode":""}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			created := workspaceRecord("pending", "")
			hub := newStubHub(t).answer(http.MethodPost, workspacesPath, http.StatusCreated, created)

			code, stdout, stderr := runWorkspace(t, hub, tt.args...)

			if code != 0 {
				t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
			}
			if want := indented(t, created); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want it empty", stderr)
			}
			sent := hub.recorded()
			if len(sent) != 1 || sent[0].method != http.MethodPost || sent[0].rawPath != workspacesPath {
				t.Fatalf("the hub got %v, want one POST %s", sent, workspacesPath)
			}
			wantJSONBody(t, sent[0].body, tt.body)
		})
	}
}

func TestWorkspaceCreate_PrintsTheHubsRefusalAndFails(t *testing.T) {
	t.Parallel()
	refusal := `{"error":{"code":409,"message":"slug: a workspace with the slug \"demo\" exists","error_type":"conflict"}}`
	for _, wait := range []bool{false, true} {
		t.Run(map[bool]string{false: "without --wait", true: "with --wait"}[wait], func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodPost, workspacesPath, http.StatusConflict, refusal)
			args := createDemo()
			if wait {
				args = append(args, "--wait")
			}

			code, stdout, stderr := runWorkspace(t, hub, args...)

			if code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
			if want := indented(t, refusal); stderr != want {
				t.Errorf("stderr = %q, want the hub's envelope %q", stderr, want)
			}
			if n := len(hub.recorded()); n != 1 {
				t.Errorf("the hub got %d requests, want 1", n)
			}
		})
	}
}

func TestWorkspaceCreate_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	url := []string{"--git-url", "https://example.com/a.git"}
	slug := []string{"--slug", "demo"}
	tests := []struct {
		name string
		args []string
		// says is what the message must name.
		says string
	}{
		{"no --slug", slices.Concat([]string{"workspace", "create"}, url), "--slug"},
		{"no --git-url", slices.Concat([]string{"workspace", "create"}, slug), "--git-url"},
		{"a token with a user name", createDemo("--git-pat", "t", "--git-username", "u"), "--git-pat"},
		{"a token with a password", createDemo("--git-pat", "t", "--git-password", "p"), "--git-pat"},
		{"a user name alone", createDemo("--git-username", "u"), "--git-password"},
		{"a password alone", createDemo("--git-password", "p"), "--git-username"},
		{"an empty token", createDemo("--git-pat", ""), "--git-pat"},
		{"an empty user name", createDemo("--git-username", "", "--git-password", "p"), "--git-username"},
		{"an empty password", createDemo("--git-username", "u", "--git-password", ""), "--git-password"},
		{"a setting without =", createDemo("--setting", "abc"), "--setting"},
		{"a setting without a key", createDemo("--setting", "=v"), "--setting"},
		{"an empty setting", createDemo("--setting", ""), "--setting"},
		{"a wait without time", createDemo("--wait", "--timeout", "0s"), "--timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t)

			code, stdout, stderr := runWorkspace(t, hub, tt.args...)

			if message := wantClientFailure(t, code, stdout, stderr, "usage_error"); !strings.Contains(message, tt.says) {
				t.Errorf("message = %q, want it to name %s", message, tt.says)
			}
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}

func TestWorkspaceCreate_SendsEachSettingByTheTypeItsValueReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		settings []string
		want     string
	}{
		{[]string{"a=true"}, `{"a":true}`},
		{[]string{"a=false"}, `{"a":false}`},
		{[]string{"a="}, `{"a":null}`},
		{[]string{"protected_branches=main,release/*"}, `{"protected_branches":["main","release/*"]}`},
		{[]string{"protected_branches=main"}, `{"protected_branches":["main"]}`},
		{[]string{"protected_branches=true"}, `{"protected_branches":["true"]}`},
		{[]string{"protected_branches="}, `{"protected_branches":null}`},
		{[]string{"u=https://x/y?a=b"}, `{"u":"https://x/y?a=b"}`},
		{[]string{"a=1"}, `{"a":"1"}`},
		{[]string{"a=x", "a=y"}, `{"a":"y"}`},
		{[]string{"a=x", "b=true"}, `{"a":"x","b":true}`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.settings, " "), func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodPost, workspacesPath, http.StatusCreated, workspaceRecord("pending", ""))
			args := createDemo()
			for _, s := range tt.settings {
				args = append(args, "--setting", s)
			}

			code, _, stderr := runWorkspace(t, hub, args...)

			if code != 0 {
				t.Fatalf("exit status = %d, want 0 (stderr %q)", code, stderr)
			}
			sent := hub.recorded()
			if len(sent) != 1 {
				t.Fatalf("the hub got %d requests, want 1", len(sent))
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal([]byte(sent[0].body), &body); err != nil {
				t.Fatalf("body %q is not a JSON object: %v", sent[0].body, err)
			}
			wantJSONBody(t, string(body["settings"]), tt.want)
		})
	}
}

func TestWorkspaceCreate_WaitsUntilTheWorkspaceIsReadyOrFailed(t *testing.T) {
	t.Parallel()
	notFound := `{"error":{"code":404,"message":"workspace not found","error_type":"not_found"}}`
	tests := []struct {
		name    string
		polls   []http.HandlerFunc
		timeout string
		// code is the exit status; stdout the record printed, "" for none;
		// errorType the envelope's on stderr, "" for none.
		code      int
		stdout    string
		errorType string
		// gets is how many polls the hub must get, -1 for any number.
		gets int
	}{
		{
			name: "ready",
			polls: []http.HandlerFunc{
				answerWith(http.StatusOK, workspaceRecord("pending", "")),
				answerWith(http.StatusOK, workspaceRecord("cloning", "")),
				answerWith(http.StatusOK, workspaceRecord("ready", "")),
			},
			code: 0, stdout: workspaceRecord("ready", ""), gets: 3,
		},
		{
			name: "failed",
			polls: []http.HandlerFunc{
				answerWith(http.StatusOK, workspaceRecord("cloning", "")),
				answerWith(http.StatusOK, workspaceRecord("failed", "x")),
			},
			code: 1, stdout: workspaceRecord("failed", "x"), errorType: "wait_failed", gets: 2,
		},
		{
			name:    "pending until the timeout",
			polls:   []http.HandlerFunc{answerWith(http.StatusOK, workspaceRecord("pending", ""))},
			timeout: "1s",
			code:    1, errorType: "wait_timeout", gets: -1,
		},
		{
			name:  "a poll answered 404",
			polls: []http.HandlerFunc{answerWith(http.StatusNotFound, notFound)},
			code:  1, errorType: "not_found", gets: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).
				answer(http.MethodPost, workspacesPath, http.StatusCreated, workspaceRecord("pending", "")).
				handle(http.MethodGet, demoPath, inTurn(tt.polls...))
			args := createDemo("--wait")
			if tt.timeout != "" {
				args = append(args, "--timeout", tt.timeout)
			}

			code, stdout, stderr := runWorkspace(t, hub, args...)

			if code != tt.code {
				t.Errorf("exit status = %d, want %d (stderr %q)", code, tt.code, stderr)
			}
			want := ""
			if tt.stdout != "" {
				want = indented(t, tt.stdout)
			}
			if stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			last := lastDocument(stderr)
			switch {
			case tt.errorType == "" && last != "":
				t.Errorf("stderr holds a JSON document: %q", stderr)
			case tt.errorType != "":
				var doc struct {
					Error struct {
						ErrorType string `json:"error_type"`
					} `json:"error"`
				}
				if err := json.Unmarshal([]byte(last), &doc); err != nil || doc.Error.ErrorType != tt.errorType {
					t.Errorf("stderr = %q, want an envelope with error_type %s", stderr, tt.errorType)
				}
			}
			sent := hub.recorded()
			if len(sent) == 0 || sent[0].method != http.MethodPost {
				t.Fatalf("the hub got %v, want the create first", sent)
			}
			for _, r := range sent[1:] {
				if r.method != http.MethodGet || r.rawPath != demoPath {
					t.Errorf("after the create the hub got %s %s, want only GET %s", r.method, r.rawPath, demoPath)
				}
			}
			if gets := len(sent) - 1; tt.gets >= 0 && gets != tt.gets {
				t.Errorf("the hub got %d polls, want %d", gets, tt.gets)
			}
		})
	}
}
