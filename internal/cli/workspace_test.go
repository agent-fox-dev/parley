package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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
		{"sync", http.MethodPost, syncPath, syncCommand(), http.StatusBadGateway, upstreamError},
		{"sync --wait", http.MethodPost, syncPath, syncCommand("--wait"), http.StatusBadGateway, upstreamError},
		{"push, not a fast-forward", http.MethodPost, pushPath, pushMain(), http.StatusConflict, pushRefusal(409, "non_fast_forward")},
		{"push --force, origin moved", http.MethodPost, pushPath, pushMain("--force"), http.StatusConflict, pushRefusal(409, "origin_moved")},
		{"push --force of a protected branch", http.MethodPost, pushPath, pushMain("--force"), http.StatusForbidden, pushRefusal(403, "protected_branch")},
		{"push past origin's deadline", http.MethodPost, pushPath, pushMain(), http.StatusGatewayTimeout, pushRefusal(504, "origin_timeout")},
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
			// A forced push warns before it sends; nothing else may come
			// before the envelope.
			envelope := lastDocument(stderr)
			if want := indented(t, tt.refusal); envelope != want {
				t.Errorf("stderr = %q, want the hub's envelope %q", stderr, want)
			}
			for _, line := range nonEmptyLines(strings.TrimSuffix(stderr, envelope)) {
				if !strings.HasPrefix(line, "warning: ") {
					t.Errorf("stderr holds %q before the envelope, want only warnings", line)
				}
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
		{"a sync without a slug", []string{"workspace", "sync"}, "<slug>"},
		{"a sync of two slugs", syncCommand("other"), `"other"`},
		{"a sync wait without time", syncCommand("--wait", "--timeout", "0s"), "--timeout"},
		{"a push without a slug", []string{"workspace", "push"}, "<slug>"},
		{"a push without a branch", []string{"workspace", "push", "demo"}, "<branch>"},
		{"a push of two branches", pushMain("extra"), `"extra"`},
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

// The route parley workspace sync my-project posts to, and the one its
// --wait reads the rebuild job j1 at, as sent.
const (
	syncPath = "/api/v1/workspaces/my-project/sync"
	jobPath  = "/api/v1/workspaces/my-project/rebuilds/j1"
)

// The branches entries of a sync answer: one with the branch exp
// diverged, and one with nothing diverged.
const (
	divergedBranches = `[{"name":"exp","result":"diverged","local_sha":"a1","origin_sha":"b1"},{"name":"main","result":"fast_forward","local_sha":"c1","origin_sha":"c1"}]`
	calmBranches     = `[{"name":"main","result":"up_to_date","local_sha":"c1","origin_sha":"c1"}]`
)

// upstreamError is the hub's refusal of a sync that could not fetch origin.
const upstreamError = `{"error":{"code":502,"message":"origin fetch failed: exit status 128","error_type":"upstream_error"}}`

// syncCommand is the command line that syncs my-project, followed by more.
func syncCommand(more ...string) []string {
	return append([]string{"workspace", "sync", "my-project"}, more...)
}

// syncAnswer is the hub's answer to a sync of my-project: the workspace
// with branches, a JSON array, followed by the members more holds.
func syncAnswer(branches, more string) string {
	return `{"slug":"my-project","kind":"repository","status":"ready","last_sync_at":"2026-11-14T10:00:00Z","sync_error":null,"branches":` + branches + more + `}`
}

// rebuildJob is the rebuild job j1 in status as the hub serves it the n-th
// time, so that two records of one status differ.
func rebuildJob(status string, n int) string {
	return `{"id":"j1","workspace_slug":"my-project","status":"` + status + `","poll":` + strconv.Itoa(n) + `}`
}

// withRebuild is answer, a JSON object, with the member rebuild holding job
// added after its own members.
func withRebuild(answer, job string) string {
	return strings.TrimSuffix(answer, "}") + `,"rebuild":` + job + `}`
}

// syncHub is a stub hub that answers the sync of my-project with answer and
// the n-th read of the rebuild job j1 with jobs[n-1], the last of jobs
// answering every later one too.
func syncHub(t *testing.T, answer string, jobs ...http.HandlerFunc) *stubHub {
	t.Helper()
	hub := newStubHub(t).answer(http.MethodPost, syncPath, http.StatusOK, answer)
	if len(jobs) > 0 {
		hub.handle(http.MethodGet, jobPath, inTurn(jobs...))
	}
	return hub
}

// jobsInStatus are the answers of a syncHub's job reads: the n-th is j1
// in statuses[n-1].
func jobsInStatus(statuses ...string) []http.HandlerFunc {
	jobs := make([]http.HandlerFunc, 0, len(statuses))
	for i, status := range statuses {
		jobs = append(jobs, answerWith(http.StatusOK, rebuildJob(status, i+1)))
	}
	return jobs
}

// runSync is runWorkspace that also returns, for each request the command
// sent, how long it had before its deadline.
func runSync(t *testing.T, hub *stubHub, args ...string) (int, string, string, []time.Duration) {
	t.Helper()
	opts := defaultOptions()
	opts.pollInterval = 10 * time.Millisecond
	var mu sync.Mutex
	var left []time.Duration
	opts.transport = func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var d time.Duration
			if at, ok := r.Context().Deadline(); ok {
				d = time.Until(at)
			}
			mu.Lock()
			left = append(left, d)
			mu.Unlock()
			return next.RoundTrip(r)
		})
	}
	code, stdout, stderr := runCLIWith(t, opts, onHub(hub.URL, args...)...)
	return code, stdout, stderr, left
}

// wantErrorType fails the test unless the last document on stderr is an
// error envelope of errorType, and returns its message.
func wantErrorType(t *testing.T, stderr, errorType string) string {
	t.Helper()
	_, message, got := clientEnvelope(t, lastDocument(stderr))
	if got != errorType {
		t.Errorf("stderr = %q, want an envelope with error_type %s", stderr, errorType)
	}
	return message
}

func TestWorkspaceSync_PostsAndPrints(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		args           []string
		rawPath, query string
	}{
		{"a slug", syncCommand(), syncPath, ""},
		{"--reset-to-origin", syncCommand("--reset-to-origin"), syncPath, "reset_to_origin=true"},
		{"a slug with a slash", []string{"workspace", "sync", "a/b"}, "/api/v1/workspaces/a%2Fb/sync", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			answer := syncAnswer(calmBranches, "")
			path, _ := url.PathUnescape(tt.rawPath)
			hub := newStubHub(t).answer(http.MethodPost, path, http.StatusOK, answer)

			code, stdout, stderr, left := runSync(t, hub, tt.args...)

			if code != 0 || stderr != "" {
				t.Errorf("exit status = %d, stderr %q; want 0 and nothing", code, stderr)
			}
			if want := indented(t, answer); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			sent := hub.recorded()
			if len(sent) != 1 {
				t.Fatalf("the hub got %v, want one request", sent)
			}
			if got := sent[0]; got.method != http.MethodPost || got.rawPath != tt.rawPath || got.query != tt.query || got.body != "" {
				t.Errorf("sent %s %s ?%q body %q, want POST %s ?%q and no body", got.method, got.rawPath, got.query, got.body, tt.rawPath, tt.query)
			}
			if len(left) != 1 || left[0] <= 3*time.Minute-10*time.Second || left[0] > 3*time.Minute {
				t.Errorf("the request had %v before its deadline, want 3 minutes", left)
			}
		})
	}
}

func TestWorkspaceSync_FailOnDiverged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		flag   bool
		answer string
		// names are what stderr must name; none means exit 0 and nothing
		// on stderr.
		names []string
	}{
		{"a diverged branch", true, syncAnswer(divergedBranches, ""), []string{"exp"}},
		{"a diverged patch", true, syncAnswer(calmBranches, `,"patches_diverged":["patch/a"]`), []string{"patch/a"}},
		{"a diverged branch and patch", true, syncAnswer(divergedBranches, `,"patches_diverged":["patch/a"]`), []string{"exp", "patch/a"}},
		{"nothing diverged and no patch", true, syncAnswer(calmBranches, `,"patches_diverged":[]`), nil},
		{"nothing diverged", true, syncAnswer(calmBranches, ""), nil},
		{"a diverged branch without the flag", false, syncAnswer(divergedBranches, ""), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := syncHub(t, tt.answer)
			args := syncCommand()
			if tt.flag {
				args = append(args, "--fail-on-diverged")
			}

			code, stdout, stderr, _ := runSync(t, hub, args...)

			if want := indented(t, tt.answer); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if len(tt.names) == 0 {
				if code != 0 || stderr != "" {
					t.Errorf("exit status = %d, stderr %q; want 0 and nothing", code, stderr)
				}
				return
			}
			if code != 3 {
				t.Errorf("exit status = %d, want 3", code)
			}
			message := wantErrorType(t, stderr, "condition_not_met")
			for _, name := range tt.names {
				if !strings.Contains(message, name) {
					t.Errorf("message = %q, want it to name %s", message, name)
				}
			}
		})
	}
}

func TestWorkspaceSync_WaitsForRebuild(t *testing.T) {
	t.Parallel()
	statuses := []string{"queued", "running", "failed", "running", "completed"}
	tests := []struct {
		name, answer, want string
		gets               int
	}{
		{
			"a rebuild job", syncAnswer(divergedBranches, `,"rebuild_job_id":"j1"`),
			withRebuild(syncAnswer(divergedBranches, `,"rebuild_job_id":"j1"`), rebuildJob("completed", 5)), 5,
		},
		{"a null rebuild job", syncAnswer(calmBranches, `,"rebuild_job_id":null`), syncAnswer(calmBranches, `,"rebuild_job_id":null`), 0},
		{"no rebuild job", syncAnswer(calmBranches, ""), syncAnswer(calmBranches, ""), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := syncHub(t, tt.answer, jobsInStatus(statuses...)...)

			code, stdout, stderr, _ := runSync(t, hub, syncCommand("--wait")...)

			if code != 0 {
				t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
			}
			if want := indented(t, tt.want); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if lastDocument(stderr) != "" {
				t.Errorf("stderr holds a JSON document: %q", stderr)
			}
			if tt.gets == 0 && stderr != "" {
				t.Errorf("stderr = %q, want nothing without a rebuild job", stderr)
			}
			if got := hub.count(http.MethodGet); got != tt.gets {
				t.Errorf("the hub got %d job reads, want %d", got, tt.gets)
			}
			sent := hub.recorded()
			if len(sent) == 0 || sent[0].method != http.MethodPost || sent[0].rawPath != syncPath {
				t.Fatalf("the hub got %v, want the sync first", sent)
			}
			for _, r := range sent[1:] {
				if r.method != http.MethodGet || r.rawPath != jobPath {
					t.Errorf("after the sync the hub got %s %s, want only GET %s", r.method, r.rawPath, jobPath)
				}
			}
		})
	}
}

func TestWorkspaceSync_FailedWait(t *testing.T) {
	t.Parallel()
	answer := syncAnswer(divergedBranches, `,"rebuild_job_id":"j1"`)
	jobError := `{"error":{"code":500,"message":"internal error","error_type":"internal"}}`
	tests := []struct {
		name    string
		jobs    []http.HandlerFunc
		timeout string
		// stdout is the document printed, "" for none; errorType the one
		// of the envelope on stderr.
		stdout, errorType string
	}{
		{"dead_letter", jobsInStatus("running", "dead_letter"), "", withRebuild(answer, rebuildJob("dead_letter", 2)), "wait_failed"},
		{"cancelled", jobsInStatus("queued", "cancelled"), "", withRebuild(answer, rebuildJob("cancelled", 2)), "wait_failed"},
		{"running until the timeout", jobsInStatus("running"), "1s", "", "wait_timeout"},
		{"a job read answered 500", []http.HandlerFunc{answerWith(http.StatusInternalServerError, jobError)}, "", "", "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := syncHub(t, answer, tt.jobs...)
			args := syncCommand("--wait", "--fail-on-diverged")
			if tt.timeout != "" {
				args = append(args, "--timeout", tt.timeout)
			}

			code, stdout, stderr, _ := runSync(t, hub, args...)

			if code != 1 {
				t.Errorf("exit status = %d, want 1 (stderr %q)", code, stderr)
			}
			want := ""
			if tt.stdout != "" {
				want = indented(t, tt.stdout)
			}
			if stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			wantErrorType(t, stderr, tt.errorType)
			if tt.errorType == "internal" && lastDocument(stderr) != indented(t, jobError) {
				t.Errorf("stderr = %q, want the hub's envelope as received", stderr)
			}
		})
	}
}

// pushPath is the route parley workspace push demo main posts to, as sent.
const pushPath = "/api/v1/workspaces/demo/branches/main/push"

// pushMain is the command line that pushes the branch main of demo,
// followed by more.
func pushMain(more ...string) []string {
	return append([]string{"workspace", "push", "demo", "main"}, more...)
}

// pushRefusal is the hub's refusal of a push with code and errorType.
func pushRefusal(code int, errorType string) string {
	return `{"error":{"code":` + strconv.Itoa(code) + `,"message":"push refused","error_type":"` + errorType + `"}}`
}

func TestWorkspacePush_PostsTheForceAndPrintsTheAnswer(t *testing.T) {
	t.Parallel()
	answer := `{"branch":"fix/42","sha":"a1b2","remote":"origin","remote_url":"https://example.com/a.git","result":"pushed"}`
	tests := []struct {
		name          string
		args          []string
		rawPath, body string
		// warned is whether stderr holds the warning, written before the
		// request was sent.
		warned bool
	}{
		{"a branch with a slash", []string{"workspace", "push", "demo", "fix/42"}, "/api/v1/workspaces/demo/branches/fix%2F42/push", `{"force":false}`, false},
		{"--force", pushMain("--force"), pushPath, `{"force":true}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			var stderr syncBuffer
			// atRequest is what stderr held when the request arrived.
			atRequest := make(chan string, 1)
			path, _ := url.PathUnescape(tt.rawPath)
			hub := newStubHub(t).handle(http.MethodPost, path, func(w http.ResponseWriter, _ *http.Request) {
				atRequest <- stderr.String()
				answerWith(http.StatusOK, answer)(w, nil)
			})
			var deadline time.Time
			opts := defaultOptions()
			opts.transport = func(next http.RoundTripper) http.RoundTripper {
				return roundTripFunc(func(r *http.Request) (*http.Response, error) {
					deadline, _ = r.Context().Deadline()
					return next.RoundTrip(r)
				})
			}
			start := time.Now()

			code := run(context.Background(), onHub(hub.URL, tt.args...), &stdout, &stderr, opts)

			if code != 0 {
				t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr.String())
			}
			if want := indented(t, answer); stdout.String() != want {
				t.Errorf("stdout = %q, want %q", stdout.String(), want)
			}
			select {
			case held := <-atRequest:
				if strings.Contains(held, "may be replaced") != tt.warned {
					t.Errorf("stderr when the request arrived = %q, want the warning: %v", held, tt.warned)
				}
			default:
				t.Error("the hub got no request")
			}
			if !tt.warned && stderr.String() != "" {
				t.Errorf("stderr = %q, want it empty", stderr.String())
			}
			sent := hub.recorded()
			if len(sent) != 1 || sent[0].method != http.MethodPost || sent[0].rawPath != tt.rawPath {
				t.Fatalf("the hub got %v, want one POST %s", sent, tt.rawPath)
			}
			wantJSONBody(t, sent[0].body, tt.body)
			if left := deadline.Sub(start); left < 179*time.Second || left > 181*time.Second {
				t.Errorf("the request's deadline was %v after the start, want 3 minutes", left)
			}
		})
	}
}
