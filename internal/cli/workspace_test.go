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

// updateDemo is the command line that updates demo, followed by more.
func updateDemo(more ...string) []string {
	return append([]string{"workspace", "update", "demo"}, more...)
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

func TestWorkspace_PrintsTheHubsRefusalAndFails(t *testing.T) {
	t.Parallel()
	conflict := `{"error":{"code":409,"message":"slug: a workspace with the slug \"demo\" exists","error_type":"conflict"}}`
	notFound := `{"error":{"code":404,"message":"workspace not found","error_type":"not_found"}}`
	tests := []struct {
		name, method, path string
		args               []string
		status             int
		refusal            string
	}{
		{"create", http.MethodPost, workspacesPath, createDemo(), http.StatusConflict, conflict},
		{"create --wait", http.MethodPost, workspacesPath, createDemo("--wait"), http.StatusConflict, conflict},
		{"get", http.MethodGet, demoPath, []string{"workspace", "get", "demo"}, http.StatusNotFound, notFound},
		{"update", http.MethodPatch, demoPath, updateDemo("--description", "x"), http.StatusNotFound, notFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(tt.method, tt.path, tt.status, tt.refusal)

			code, stdout, stderr := runWorkspace(t, hub, tt.args...)

			if code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
			if want := indented(t, tt.refusal); stderr != want {
				t.Errorf("stderr = %q, want the hub's envelope %q", stderr, want)
			}
			if sent := hub.recorded(); len(sent) != 1 || sent[0].method != tt.method || sent[0].rawPath != tt.path {
				t.Errorf("the hub got %v, want one %s %s", sent, tt.method, tt.path)
			}
		})
	}
}

func TestWorkspace_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
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
		{"an update without a flag", updateDemo(), "--description"},
		{"an update of a setting without =", updateDemo("--setting", "sync_enabled"), "--setting"},
		{"an update of a setting without a key", updateDemo("--setting", "=true"), "--setting"},
		{"an update with a bad setting among good flags", updateDemo("--description", "x", "--setting", "novalue"), "--setting"},
		{"a get without a slug", []string{"workspace", "get"}, "<slug>"},
		{"an update without a slug", []string{"workspace", "update", "--description", "x"}, "<slug>"},
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

func TestWorkspace_SendsEachSettingByTheTypeItsValueReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		settings []string
		want     string
	}{
		{[]string{"sync_enabled=false"}, `{"sync_enabled":false}`},
		{[]string{"audit_fetches=true"}, `{"audit_fetches":true}`},
		{[]string{"land.strategy="}, `{"land.strategy":null}`},
		{[]string{"protected_branches=main,release/*"}, `{"protected_branches":["main","release/*"]}`},
		{[]string{"protected_branches=main"}, `{"protected_branches":["main"]}`},
		{[]string{"protected_branches=true"}, `{"protected_branches":["true"]}`},
		{[]string{"protected_branches="}, `{"protected_branches":null}`},
		{[]string{"forge_api_url=https://x.example/api?a=b"}, `{"forge_api_url":"https://x.example/api?a=b"}`},
		{[]string{"land.strategy=True"}, `{"land.strategy":"True"}`},
		{[]string{"a=1"}, `{"a":"1"}`},
		{[]string{"land.strategy=rebase", "land.strategy=squash"}, `{"land.strategy":"squash"}`},
		{[]string{"a=x", "b=true"}, `{"a":"x","b":true}`},
	}
	commands := []struct {
		name, method, path string
		args               []string
	}{
		{"create", http.MethodPost, workspacesPath, createDemo()},
		{"update", http.MethodPatch, demoPath, updateDemo()},
	}
	for _, c := range commands {
		for _, tt := range tests {
			t.Run(c.name+" "+strings.Join(tt.settings, " "), func(t *testing.T) {
				t.Parallel()
				hub := newStubHub(t).answer(c.method, c.path, http.StatusOK, workspaceRecord("pending", ""))
				args := slices.Clone(c.args)
				for _, s := range tt.settings {
					args = append(args, "--setting", s)
				}

				code, _, stderr := runWorkspace(t, hub, args...)

				if code != 0 {
					t.Fatalf("exit status = %d, want 0 (stderr %q)", code, stderr)
				}
				sent := hub.recorded()
				if len(sent) != 1 || sent[0].method != c.method {
					t.Fatalf("the hub got %v, want one %s", sent, c.method)
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal([]byte(sent[0].body), &body); err != nil {
					t.Fatalf("body %q is not a JSON object: %v", sent[0].body, err)
				}
				wantJSONBody(t, string(body["settings"]), tt.want)
			})
		}
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

func TestWorkspaceListGetUpdate_SendExactlyWhatTheArgumentsAndFlagsName(t *testing.T) {
	list := []string{"workspace", "list"}
	tests := []struct {
		args []string
		want sentRequest
	}{
		{list, sentRequest{http.MethodGet, workspacesPath, "", ""}},
		{append(list, "--include-archived"), sentRequest{http.MethodGet, workspacesPath, "include_archived=true", ""}},
		{
			append(list, "--include-forks", "--parent", "demo", "--limit", "10", "--cursor", "abc"),
			sentRequest{http.MethodGet, workspacesPath, "cursor=abc&include_forks=true&limit=10&parent=demo", ""},
		},
		{[]string{"workspace", "get", "demo--t1"}, sentRequest{http.MethodGet, "/api/v1/workspaces/demo--t1", "", ""}},
		{[]string{"workspace", "get", "a/b"}, sentRequest{http.MethodGet, "/api/v1/workspaces/a%2Fb", "", ""}},
		{updateDemo("--description", "x"), sentRequest{http.MethodPatch, demoPath, "", `{"description":"x"}`}},
		{updateDemo("--display-name", "D", "--org", "acme"), sentRequest{http.MethodPatch, demoPath, "", `{"display_name":"D","org_id":"acme"}`}},
		{updateDemo("--clear-description"), sentRequest{http.MethodPatch, demoPath, "", `{"description":null}`}},
		{updateDemo("--display-name", "D", "--clear-display-name"), sentRequest{http.MethodPatch, demoPath, "", `{"display_name":null}`}},
		{updateDemo("--clear-org"), sentRequest{http.MethodPatch, demoPath, "", `{"org_id":null}`}},
		{updateDemo("--display-name", ""), sentRequest{http.MethodPatch, demoPath, "", `{"display_name":""}`}},
		{updateDemo("--setting", "land.strategy=squash"), sentRequest{http.MethodPatch, demoPath, "", `{"settings":{"land.strategy":"squash"}}`}},
		{
			[]string{"workspace", "update", "a/b", "--description", "x"},
			sentRequest{http.MethodPatch, "/api/v1/workspaces/a%2Fb", "", `{"description":"x"}`},
		},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			wantSends(t, tt.args, tt.want)
		})
	}
}
