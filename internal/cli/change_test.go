package cli

import (
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// changesPath is the path of the change routes of the workspace demo, as
// sent.
const changesPath = "/api/v1/workspaces/demo/changes"

// changeSHA is a commit id the by-commit lookup is asked for.
const changeSHA = "0123456789abcdef0123456789abcdef01234567"

// changeNotFound is the hub's refusal of a change it does not hold.
const changeNotFound = `{"error":{"code":404,"message":"workspace demo has no change 99","error_type":"change_not_found"}}`

// patchText is the unified diff the hub answers a diff asked for as a
// patch, with bytes JSON framing would change.
const patchText = "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-\"old\"\t\n+new \\u00e9\n"

// changeHub is a stub hub that answers every change route the tests of
// this file call with 200 and hubAnswer: the diff with patchText as
// text/x-diff when asked for format=patch. Change 99 does not exist.
func changeHub(t *testing.T) *stubHub {
	t.Helper()
	return newStubHub(t).
		answer(http.MethodPost, changesPath, http.StatusCreated, hubAnswer).
		answer(http.MethodPost, "/api/v1/workspaces/a/b/changes", http.StatusCreated, hubAnswer).
		answer(http.MethodGet, changesPath, http.StatusOK, hubAnswer).
		answer(http.MethodGet, changesPath+"/12", http.StatusOK, hubAnswer).
		answer(http.MethodGet, changesPath+"/by-commit/"+changeSHA, http.StatusOK, hubAnswer).
		answer(http.MethodGet, changesPath+"/12/versions", http.StatusOK, hubAnswer).
		answer(http.MethodGet, changesPath+"/99", http.StatusNotFound, changeNotFound).
		handle(http.MethodGet, changesPath+"/12/diff", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("format") == "patch" {
				w.Header().Set("Content-Type", "text/x-diff")
				w.Write([]byte(patchText))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(hubAnswer))
		})
}

// runChange runs parley change with args against hub, with stdin as the
// standard input, and returns what it printed and the requests this run
// sent. The runs of one test share its hub, so they run one at a time.
func runChange(t *testing.T, hub *stubHub, stdin string, args ...string) (code int, stdout, stderr string, sent []stubRequest) {
	t.Helper()
	before := len(hub.recorded())
	opts := defaultOptions()
	opts.stdin = strings.NewReader(stdin)
	code, stdout, stderr = runCLIWith(t, opts, onHub(hub.URL, append([]string{"change"}, args...)...)...)
	return code, stdout, stderr, hub.recorded()[before:]
}

// wantOnlyRequest fails the test unless sent is one request of method to
// rawPath, as sent, and returns it.
func wantOnlyRequest(t *testing.T, sent []stubRequest, method, rawPath string) stubRequest {
	t.Helper()
	if len(sent) != 1 || sent[0].method != method || sent[0].rawPath != rawPath {
		t.Fatalf("the hub got %v, want one %s %s", sent, method, rawPath)
	}
	return sent[0]
}

// wantQuery fails the test unless the query string got holds exactly want.
func wantQuery(t *testing.T, got string, want url.Values) {
	t.Helper()
	parsed, err := url.ParseQuery(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(parsed, want) {
		t.Errorf("query = %v, want %v", parsed, want)
	}
}

func TestChangeOpen_SendsAMemberForEachFlagGiven(t *testing.T) {
	t.Parallel()
	hub := changeHub(t)
	tests := []struct {
		name    string
		args    []string
		rawPath string
		body    string
	}{
		{"only the source branch", []string{"open", "demo", "--source", "feat"}, changesPath, `{"source":{"branch":"feat"}}`},
		{
			"every flag",
			[]string{"open", "demo", "--source", "task", "--source-workspace", "demo--m1", "--target", "release", "--title", "T", "--intent", "I",
				"--draft", "--depends-on", "4", "--auto-land", "--label", "a", "--label", "b", "--session", "s1"},
			changesPath,
			`{"source":{"workspace_slug":"demo--m1","branch":"task"},"target_branch":"release","title":"T","intent":"I","draft":true,` +
				`"depends_on":4,"auto_land":true,"labels":["a","b"],"session_id":"s1"}`,
		},
		{"a slug as one segment", []string{"open", "a/b", "--source", "feat"}, "/api/v1/workspaces/a%2Fb/changes", `{"source":{"branch":"feat"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr, sent := runChange(t, hub, "", tt.args...)

			wantResult(t, code, stdout, stderr, hubAnswer)
			got := wantOnlyRequest(t, sent, http.MethodPost, tt.rawPath)
			wantJSONBody(t, got.body, tt.body)
		})
	}
}

func TestChangeOpen_ReadsTheIntentFromAFileOrStdin(t *testing.T) {
	t.Parallel()
	hub := changeHub(t)
	file := localFile(t, "intent.txt", []byte("why\nline 2\n"))
	tests := []struct {
		name, intent, stdin string
		// want is the intent sent, "" for no request.
		want string
	}{
		{"from a file", "@" + file, "", "why\nline 2\n"},
		{"from stdin", "-", "from stdin", "from stdin"},
		{"from a file that cannot be read", "@" + filepath.Join(t.TempDir(), "missing"), "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr, sent := runChange(t, hub, tt.stdin, "open", "demo", "--source", "feat", "--intent", tt.intent)

			if tt.want == "" {
				wantClientFailure(t, code, stdout, stderr, "client_error")
				if len(sent) != 0 {
					t.Errorf("the hub got %v, want no request", sent)
				}
				return
			}
			wantResult(t, code, stdout, stderr, hubAnswer)
			got := wantOnlyRequest(t, sent, http.MethodPost, changesPath)
			wantJSONBody(t, got.body, `{"source":{"branch":"feat"},"intent":`+jsonString(tt.want)+`}`)
		})
	}
}

// jsonString is s as a JSON string.
func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

func TestChangeList_SendsAParameterForEachFlagGiven(t *testing.T) {
	t.Parallel()
	hub := changeHub(t)
	tests := []struct {
		args []string
		want url.Values
	}{
		{[]string{"list", "demo"}, nil},
		{
			[]string{"list", "demo", "--state", "draft", "--state", "landed", "--target", "main", "--author", "me", "--session", "s1",
				"--label", "perf", "--path", "docs/**", "--mergeable", "clean", "--landing-status", "ready", "--stack-of", "4",
				"--limit", "10", "--cursor", "abc"},
			url.Values{
				"state": {"draft", "landed"}, "target_branch": {"main"}, "author": {"me"}, "session_id": {"s1"}, "label": {"perf"},
				"path": {"docs/**"}, "mergeable": {"clean"}, "landing_status": {"ready"}, "stack_of": {"4"}, "limit": {"10"}, "cursor": {"abc"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr, sent := runChange(t, hub, "", tt.args...)

			wantResult(t, code, stdout, stderr, hubAnswer)
			got := wantOnlyRequest(t, sent, http.MethodGet, changesPath)
			wantQuery(t, got.query, tt.want)
			if got.body != "" {
				t.Errorf("body = %q, want none", got.body)
			}
		})
	}
}

func TestChange_ReadsSendOneGetToTheirRoute(t *testing.T) {
	t.Parallel()
	hub := changeHub(t)
	tests := []struct {
		args    []string
		rawPath string
		want    url.Values
		// refusal is the hub's error envelope the run prints, "" for a
		// success.
		refusal string
	}{
		{[]string{"show", "demo", "12"}, changesPath + "/12", nil, ""},
		{[]string{"show", "demo", "12", "--include", "versions,stack"}, changesPath + "/12", url.Values{"include": {"versions,stack"}}, ""},
		{[]string{"find", "demo", changeSHA}, changesPath + "/by-commit/" + changeSHA, nil, ""},
		{[]string{"versions", "demo", "12"}, changesPath + "/12/versions", nil, ""},
		{[]string{"show", "demo", "99"}, changesPath + "/99", nil, changeNotFound},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr, sent := runChange(t, hub, "", tt.args...)

			got := wantOnlyRequest(t, sent, http.MethodGet, tt.rawPath)
			wantQuery(t, got.query, tt.want)
			if tt.refusal == "" {
				wantResult(t, code, stdout, stderr, hubAnswer)
				return
			}
			if code != 1 || stdout != "" {
				t.Errorf("exit status %d, stdout %q; want 1 and nothing", code, stdout)
			}
			if want := indented(t, tt.refusal); stderr != want {
				t.Errorf("stderr = %q, want the hub's envelope %q", stderr, want)
			}
		})
	}
}

func TestChangeDiff_SendsTheFlagsAndPrintsWhatTheHubAnswers(t *testing.T) {
	t.Parallel()
	hub := changeHub(t)
	tests := []struct {
		args []string
		want url.Values
		// patch tells that the run prints patchText, not the JSON answer.
		patch bool
	}{
		{[]string{"diff", "demo", "12"}, nil, false},
		{
			[]string{"diff", "demo", "12", "--version", "3", "--against", "1", "--path", "a/**", "--path", "b.txt"},
			url.Values{"version": {"3"}, "against": {"1"}, "path": {"a/**", "b.txt"}}, false,
		},
		{[]string{"diff", "demo", "12", "--version", "latest"}, url.Values{"version": {"latest"}}, false},
		{[]string{"diff", "demo", "12", "--name-only"}, url.Values{"patches": {"false"}}, false},
		{[]string{"diff", "demo", "12", "--patch"}, url.Values{"format": {"patch"}}, true},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr, sent := runChange(t, hub, "", tt.args...)

			got := wantOnlyRequest(t, sent, http.MethodGet, changesPath+"/12/diff")
			wantQuery(t, got.query, tt.want)
			if !tt.patch {
				wantResult(t, code, stdout, stderr, hubAnswer)
				return
			}
			if code != 0 || stderr != "" {
				t.Errorf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
			}
			if stdout != patchText {
				t.Errorf("stdout = %q, want the patch as received %q", stdout, patchText)
			}
		})
	}
}

func TestChange_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	hub := changeHub(t)
	tests := [][]string{
		{"open", "demo"},
		{"open"},
		{"open", "demo", "extra", "--source", "f"},
		{"open", "demo", "--source", "f", "--depends-on", "z"},
		{"open", "demo", "--source", "f", "--depends-on", "0"},
		{"show", "demo"},
		{"show", "demo", "x"},
		{"show", "demo", "1", "2"},
		{"find", "demo"},
		{"versions", "demo", "0"},
		{"versions", "demo", "-3"},
		{"diff", "demo", "1", "--version", "x"},
		{"diff", "demo", "1", "--against", "latest"},
		{"diff", "demo", "1", "--patch", "--name-only"},
		{"diff", "demo", "+1"},
		{"list", "demo", "--stack-of", "-1"},
		{"list", "demo", "--stack-of", "1.5"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr, sent := runChange(t, hub, "", args...)

			wantClientFailure(t, code, stdout, stderr, "usage_error")
			if len(sent) != 0 {
				t.Errorf("the hub got %v, want no request", sent)
			}
		})
	}
}
