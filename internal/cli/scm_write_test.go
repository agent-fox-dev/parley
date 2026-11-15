package cli

import (
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The writes of these tests go to the workspace w.
const (
	commitsPath = "/api/v1/workspaces/w/scm/commits"
	rebasePath  = "/api/v1/workspaces/w/scm/rebase"
	// headSHA is a branch head the hub reports, or the caller has seen.
	headSHA = "1111111111111111111111111111111111111111"
)

// commitAnswer and rebaseAnswer are the hub's answers to an accepted write,
// with a number whose text decoding would change.
const (
	commitAnswer = `{"commit": {"sha": "2222222222222222222222222222222222222222", "stats": {"ratio": 1.50}}, "branch": {"name": "refs/heads/b"}, "change": null, "change_error": null}`
	rebaseAnswer = `{"status": "rebased", "old_head_sha": "1111111111111111111111111111111111111111", "new_head_sha": "3333333333333333333333333333333333333333", "change": null}`
)

// localFile writes data to a file named name in a directory of the test's
// and returns its path.
func localFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// wantResult fails the test unless parley printed body as its result: exit
// status 0, body indented on stdout, nothing on stderr.
func wantResult(t *testing.T, code int, stdout, stderr, body string) {
	t.Helper()
	if code != 0 || stderr != "" {
		t.Errorf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
	}
	if want := indented(t, body); stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestSCMCommit_SendsTheOperationsInCommandLineOrder(t *testing.T) {
	t.Parallel()
	source := []byte("package api\n")
	binary := []byte{0xff, 0xfe, 0x00, 0x80, 'x'}
	sourcePath := localFile(t, "g.go", source)
	binaryPath := localFile(t, "bin.dat", binary)
	hub := newStubHub(t).answer(http.MethodPost, commitsPath, http.StatusCreated, commitAnswer)

	code, stdout, stderr := runCLI(t, onHub(hub.URL, "scm", "commit", "w", "--branch", "b", "--expected-head", headSHA, "--message", "m",
		"--delete", "old.go", "--put", "api/g.go="+sourcePath, "--move", "a=b", "--put", "bin.dat="+binaryPath)...)

	wantResult(t, code, stdout, stderr, commitAnswer)
	sent := hub.recorded()
	if len(sent) != 1 || sent[0].method != http.MethodPost || sent[0].rawPath != commitsPath {
		t.Fatalf("the hub got %v, want one POST %s", sent, commitsPath)
	}
	wantJSONBody(t, sent[0].body, `{"branch":"b","message":"m","expected_head_sha":"`+headSHA+`","operations":[`+
		`{"op":"delete","path":"old.go"},`+
		`{"op":"put","path":"api/g.go","content_base64":"`+base64.StdEncoding.EncodeToString(source)+`"},`+
		`{"op":"move","from":"a","to":"b"},`+
		`{"op":"put","path":"bin.dat","content_base64":"`+base64.StdEncoding.EncodeToString(binary)+`"}]}`)
}

func TestSCMCommit_ReadsTheBranchHeadFirst(t *testing.T) {
	t.Parallel()
	const refPath = "/api/v1/workspaces/w/scm/refs/feat%2Fx"
	refNotFound := `{"error":{"code":404,"message":"no ref feat/x","error_type":"ref_not_found"}}`
	forbidden := `{"error":{"code":403,"message":"you may not read w","error_type":"forbidden","details":{"slug":"w"}}}`
	content := []byte("f\n")
	putOp := `{"op":"put","path":"f","content_base64":"` + base64.StdEncoding.EncodeToString(content) + `"}`
	tests := []struct {
		name string
		// status and answer are the hub's answer to the ref read.
		status int
		answer string
		extra  []string
		// commit is the commit body sent, "" for none.
		commit string
	}{
		{"an existing branch sends its head", http.StatusOK, `{"name":"refs/heads/feat/x","kind":"branch","sha":"` + headSHA + `"}`, nil,
			`{"branch":"feat/x","message":"m","expected_head_sha":"` + headSHA + `","operations":[` + putOp + `]}`},
		{"a new branch sends no head", http.StatusNotFound, refNotFound, []string{"--base", "main"},
			`{"branch":"feat/x","message":"m","base_ref":"main","operations":[` + putOp + `]}`},
		{"a refused read sends no commit", http.StatusForbidden, forbidden, nil, ""},
		{"a missing workspace sends no commit", http.StatusNotFound, `{"error":{"code":404,"message":"no workspace w","error_type":"not_found"}}`, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := localFile(t, "f", content)
			hub := newStubHub(t).
				answer(http.MethodGet, "/api/v1/workspaces/w/scm/refs/feat/x", tt.status, tt.answer).
				answer(http.MethodPost, commitsPath, http.StatusCreated, commitAnswer)
			args := append([]string{"scm", "commit", "w", "--branch", "feat/x", "--message", "m", "--put", "f=" + path}, tt.extra...)

			code, stdout, stderr := runCLI(t, onHub(hub.URL, args...)...)

			sent := hub.recorded()
			if len(sent) == 0 || sent[0].method != http.MethodGet || sent[0].rawPath != refPath {
				t.Fatalf("the hub got %v, want GET %s first", sent, refPath)
			}
			if tt.commit == "" {
				if len(sent) != 1 {
					t.Errorf("the hub got %d requests, want the ref read alone", len(sent))
				}
				if code != 1 || stdout != "" {
					t.Errorf("exit status %d, stdout %q; want 1 and nothing", code, stdout)
				}
				if want := indented(t, tt.answer); stderr != want {
					t.Errorf("stderr = %q, want the hub's envelope %q", stderr, want)
				}
				return
			}
			wantResult(t, code, stdout, stderr, commitAnswer)
			if len(sent) != 2 || sent[1].method != http.MethodPost || sent[1].rawPath != commitsPath {
				t.Fatalf("the hub got %v, want the ref read, then POST %s", sent, commitsPath)
			}
			wantJSONBody(t, sent[1].body, tt.commit)
		})
	}
}

func TestSCMCommit_SendsASessionAndAChangeOnlyWhenGiven(t *testing.T) {
	t.Parallel()
	tests := []struct {
		flags []string
		// members are the members the body holds besides branch, message,
		// expected_head_sha and operations, as JSON.
		members string
	}{
		{nil, ``},
		{[]string{"--session", "s1"}, `,"session_id":"s1"`},
		{[]string{"--change-target", "main", "--title", "T", "--intent", "I"}, `,"change":{"target_branch":"main","title":"T","intent":"I"}`},
		{[]string{"--draft"}, `,"change":{"draft":true}`},
		{[]string{"--title", "T"}, `,"change":{"title":"T"}`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.flags, " "), func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodPost, commitsPath, http.StatusCreated, commitAnswer)
			args := append([]string{"scm", "commit", "w", "--branch", "b", "--message", "m", "--expected-head", headSHA, "--delete", "x"}, tt.flags...)

			code, stdout, stderr := runCLI(t, onHub(hub.URL, args...)...)

			wantResult(t, code, stdout, stderr, commitAnswer)
			sent := hub.recorded()
			if len(sent) != 1 {
				t.Fatalf("the hub got %d requests, want 1", len(sent))
			}
			wantJSONBody(t, sent[0].body, `{"branch":"b","message":"m","expected_head_sha":"`+headSHA+`","operations":[{"op":"delete","path":"x"}]`+tt.members+`}`)
		})
	}
}

func TestSCMWrite_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	file := localFile(t, "f", []byte("f\n"))
	missing := filepath.Join(t.TempDir(), "missing")
	commit := []string{"scm", "commit", "w", "--branch", "b", "--message", "m"}
	with := func(args ...string) []string { return append(append([]string{}, commit...), args...) }
	putRefused, moveRefused := `for "--put" flag`, `for "--move" flag`
	tests := []struct {
		args []string
		// says is what the usage error's message holds.
		says string
	}{
		{[]string{"scm", "commit", "w", "--message", "m"}, "needs the flag --branch"},
		{[]string{"scm", "commit", "w", "--branch", "b"}, "needs the flag --message"},
		{[]string{"scm", "commit", "--branch", "b", "--message", "m"}, "needs the argument <slug>"},
		{with("--put", "nopath"), putRefused},
		{with("--put", "="+file), putRefused},
		{with("--put", "p="), putRefused},
		{with("--put", "p="+missing), putRefused},
		{with("--put", "p="+t.TempDir()), putRefused},
		{with("--move", "onlyfrom"), moveRefused},
		{with("--move", "=b"), moveRefused},
		{with("--move", "a="), moveRefused},
		{with("--delete", "x", "--put", "p="+file, "--move", "a"), moveRefused},
		{[]string{"scm", "rebase", "w"}, "needs the argument <branch>"},
		{[]string{"scm", "rebase", "w", "topic"}, "needs the flag --onto"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t)

			code, stdout, stderr := runCLI(t, onHub(hub.URL, tt.args...)...)

			if message := wantClientFailure(t, code, stdout, stderr, "usage_error"); !strings.Contains(message, tt.says) {
				t.Errorf("message = %q, want it to say %q", message, tt.says)
			}
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}

func TestSCMWrite_PrintsAHubErrorAsReceivedWithoutRetrying(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, path, answer string
		args               []string
	}{
		{
			"a commit on a moved branch", commitsPath,
			`{"error":{"code":409,"message":"refs/heads/b moved","error_type":"ref_moved","details":{"current_sha":"4444444444444444444444444444444444444444"}}}`,
			[]string{"scm", "commit", "w", "--branch", "b", "--message", "m", "--expected-head", headSHA, "--delete", "x"},
		},
		{
			"a rebase that conflicts", rebasePath,
			`{"error":{"code":409,"message":"commit 3282e695c40b91d83c5ab6b916b3bb6eb1cafbc8 of refs/heads/topic does not apply","error_type":"rebase_conflict",` +
				`"details":{"target_sha":"b68c63f9e524e8f13781aff3a3e31a0c7ab1fe6e","head_sha":"2d165dbfc00967ebdead889de8ba17e45a81f98d","base_sha":"a5bc5c5cd7650975def32e01a71d7011f00bfeb6",` +
				`"commit_sha":"3282e695c40b91d83c5ab6b916b3bb6eb1cafbc8","files":[{"path":"a.txt","kind":"content","base":null,"ours":null,"theirs":null,"landed_by":[]}],"landed_changes":[],"change":null}}}`,
			[]string{"scm", "rebase", "w", "topic", "--onto", "main"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodPost, tt.path, http.StatusConflict, tt.answer)

			code, stdout, stderr := runCLI(t, onHub(hub.URL, tt.args...)...)

			if code != 1 || stdout != "" {
				t.Errorf("exit status %d, stdout %q; want 1 and nothing", code, stdout)
			}
			if want := indented(t, tt.answer); stderr != want {
				t.Errorf("stderr = %q, want the hub's envelope %q", stderr, want)
			}
			if n := len(hub.recorded()); n != 1 || hub.count(http.MethodPost) != 1 {
				t.Errorf("the hub got %d requests, want one POST", n)
			}
		})
	}
}

func TestSCMRebase_SendsOnePostAndPrintsTheAnswer(t *testing.T) {
	t.Parallel()
	upToDate := `{"status": "up_to_date", "old_head_sha": "` + headSHA + `", "new_head_sha": "` + headSHA + `", "change": null}`
	tests := []struct {
		name         string
		args         []string
		body, answer string
	}{
		{"onto a branch", []string{"--onto", "main"}, `{"branch":"topic","onto":"main"}`, rebaseAnswer},
		{"against a head seen", []string{"--onto", "main", "--expected-head", headSHA}, `{"branch":"topic","onto":"main","expected_head_sha":"` + headSHA + `"}`, rebaseAnswer},
		{"already up to date", []string{"--onto", "main"}, `{"branch":"topic","onto":"main"}`, upToDate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodPost, rebasePath, http.StatusOK, tt.answer)

			code, stdout, stderr := runCLI(t, onHub(hub.URL, append([]string{"scm", "rebase", "w", "topic"}, tt.args...)...)...)

			wantResult(t, code, stdout, stderr, tt.answer)
			sent := hub.recorded()
			if len(sent) != 1 || sent[0].method != http.MethodPost || sent[0].rawPath != rebasePath {
				t.Fatalf("the hub got %v, want one POST %s", sent, rebasePath)
			}
			wantJSONBody(t, sent[0].body, tt.body)
		})
	}
}
