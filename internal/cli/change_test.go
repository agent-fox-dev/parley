package cli

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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

// labelledChange is change 13 as the hub answers it, with two labels.
const labelledChange = `{"number":13,"labels":["a","b"],"title":"T"}`

// changeHub is a stub hub that answers every change route the tests of
// this file call with 200 and hubAnswer: the diff with patchText as
// text/x-diff when asked for format=patch, and a GET of change 13 with
// labelledChange. Change 99 does not exist.
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
		answer(http.MethodPatch, changesPath+"/12", http.StatusOK, hubAnswer).
		answer(http.MethodPatch, "/api/v1/workspaces/a/b/changes/12", http.StatusOK, hubAnswer).
		answer(http.MethodPost, changesPath+"/12/abandon", http.StatusOK, hubAnswer).
		answer(http.MethodPost, changesPath+"/12/restore", http.StatusOK, hubAnswer).
		answer(http.MethodGet, changesPath+"/12/stack", http.StatusOK, hubAnswer).
		answer(http.MethodGet, "/api/v1/workspaces/a/b/changes/12/stack", http.StatusOK, hubAnswer).
		answer(http.MethodGet, changesPath+"/13", http.StatusOK, labelledChange).
		answer(http.MethodPatch, changesPath+"/13", http.StatusOK, hubAnswer).
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
		{[]string{"stack", "demo", "12"}, changesPath + "/12/stack", nil, ""},
		{[]string{"stack", "a/b", "12"}, "/api/v1/workspaces/a%2Fb/changes/12/stack", nil, ""},
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

// conflictAnswer is the hub's answer to a read of change 2's conflict,
// mergeable as given and a report only for conflict.
func conflictAnswer(mergeable string) string {
	if mergeable != "conflict" {
		return `{"mergeable":"` + mergeable + `","conflict":null}`
	}
	return `{"mergeable":"conflict","conflict":{"target_sha":"` + changeSHA + `","head_sha":"` + changeSHA + `","base_sha":null,` +
		`"commit_sha":"` + changeSHA + `","files":[{"path":"app.txt","kind":"content"}],"landed_changes":[{"number":1}],"change":null}}`
}

func TestChangeConflict_PrintsTheChangesCurrentReport(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mergeable string
		flags     []string
		want      int
	}{
		{"clean", nil, 0},
		{"conflict", nil, 0},
		{"clean", []string{"--fail-on-conflict"}, 0},
		{"conflict", []string{"--fail-on-conflict"}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.mergeable+" "+strings.Join(tt.flags, " "), func(t *testing.T) {
			t.Parallel()
			answer := conflictAnswer(tt.mergeable)
			hub := newStubHub(t).answer(http.MethodGet, changesPath+"/2/conflict", http.StatusOK, answer)

			code, stdout, stderr, sent := runChange(t, hub, "", append([]string{"conflict", "demo", "2"}, tt.flags...)...)

			got := wantOnlyRequest(t, sent, http.MethodGet, changesPath+"/2/conflict")
			wantQuery(t, got.query, nil)
			if tt.want == 0 {
				wantResult(t, code, stdout, stderr, answer)
				return
			}
			if code != tt.want {
				t.Errorf("exit status = %d, want %d (stderr %q)", code, tt.want, stderr)
			}
			if want := indented(t, answer); stdout != want {
				t.Errorf("stdout = %q, want the report %q", stdout, want)
			}
			if gotCode, message, errorType := clientEnvelope(t, stderr); gotCode != 0 || errorType != "condition_not_met" || message == "" {
				t.Errorf("envelope code %v, error_type %q, message %q; want 0, condition_not_met and a message", gotCode, errorType, message)
			}
		})
	}
}

func TestChangeConflict_WritesAConflictedFileAsTheHubSentIt(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, text string }{
		{"without a final newline", "a\n<<<<<<< target\nb\n=======\nc\n>>>>>>> change"},
		{"with a final newline", "a\n<<<<<<< target\n\"b\"\t\n=======\nc\n>>>>>>> change\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).handle(http.MethodGet, changesPath+"/2/conflict/file", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Write([]byte(tt.text))
			})

			code, stdout, stderr, sent := runChange(t, hub, "", "conflict", "demo", "2", "--file", "dir/a b&c.txt")

			got := wantOnlyRequest(t, sent, http.MethodGet, changesPath+"/2/conflict/file")
			wantQuery(t, got.query, url.Values{"path": {"dir/a b&c.txt"}})
			if code != 0 || stderr != "" {
				t.Errorf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
			}
			if stdout != tt.text {
				t.Errorf("stdout = %q, want the file as received %q", stdout, tt.text)
			}
		})
	}
}

func TestChangeConflict_PrintsTheHubsRefusalAsReceived(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		flags     []string
		rawPath   string
		status    int
		errorType string
	}{
		{"an unknown change", nil, changesPath + "/2/conflict", http.StatusNotFound, "change_not_found"},
		{"a file of another kind", []string{"--file", "a"}, changesPath + "/2/conflict/file", http.StatusConflict, "not_text_conflict"},
		{"a path of no conflicted file", []string{"--file", "a"}, changesPath + "/2/conflict/file", http.StatusNotFound, "path_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			refusal := envelope(tt.status, tt.errorType)
			hub := newStubHub(t).answer(http.MethodGet, tt.rawPath, tt.status, refusal)

			code, stdout, stderr, sent := runChange(t, hub, "", append([]string{"conflict", "demo", "2"}, tt.flags...)...)

			wantOnlyRequest(t, sent, http.MethodGet, tt.rawPath)
			if code != 1 || stdout != "" {
				t.Errorf("exit status %d, stdout %q; want 1 and nothing", code, stdout)
			}
			if want := indented(t, refusal); stderr != want {
				t.Errorf("stderr = %q, want the hub's envelope %q", stderr, want)
			}
		})
	}
}

func TestChange_LifecycleCommandsSendOneRequestWithTheMembersTheirFlagsName(t *testing.T) {
	t.Parallel()
	hub := changeHub(t)
	file := localFile(t, "intent.txt", []byte("from file\n"))
	edit := func(flags ...string) []string { return append([]string{"edit", "demo", "12"}, flags...) }
	abandon := func(flags ...string) []string { return append([]string{"abandon", "demo", "12"}, flags...) }
	tests := []struct {
		name, stdin string
		args        []string
		method      string
		rawPath     string
		// body is the JSON document sent, "" for none.
		body string
	}{
		{"edit a title", "", edit("--title", "T"), http.MethodPatch, changesPath + "/12", `{"title":"T"}`},
		{"edit an empty title", "", edit("--title", ""), http.MethodPatch, changesPath + "/12", `{"title":""}`},
		{"edit an intent", "", edit("--intent", "why"), http.MethodPatch, changesPath + "/12", `{"intent":"why"}`},
		{"edit an intent from a file", "", edit("--intent", "@"+file), http.MethodPatch, changesPath + "/12", `{"intent":"from file\n"}`},
		{"edit an intent from stdin", "from stdin", edit("--intent", "-"), http.MethodPatch, changesPath + "/12", `{"intent":"from stdin"}`},
		{"set auto-land", "", edit("--auto-land"), http.MethodPatch, changesPath + "/12", `{"auto_land":true}`},
		{"clear auto-land", "", edit("--no-auto-land"), http.MethodPatch, changesPath + "/12", `{"auto_land":false}`},
		{"retarget", "", edit("--target", "release"), http.MethodPatch, changesPath + "/12", `{"target_branch":"release"}`},
		{"edit a title and retarget", "", edit("--title", "T", "--target", "release"), http.MethodPatch, changesPath + "/12", `{"title":"T","target_branch":"release"}`},
		{"ready", "", []string{"ready", "demo", "12"}, http.MethodPatch, changesPath + "/12", `{"state":"open"}`},
		{"draft", "", []string{"draft", "demo", "12"}, http.MethodPatch, changesPath + "/12", `{"state":"draft"}`},
		{"ready a slug as one segment", "", []string{"ready", "a/b", "12"}, http.MethodPatch, "/api/v1/workspaces/a%2Fb/changes/12", `{"state":"open"}`},
		{"abandon", "", abandon(), http.MethodPost, changesPath + "/12/abandon", `{}`},
		{"abandon for a reason", "", abandon("--reason", "old"), http.MethodPost, changesPath + "/12/abandon", `{"reason":"old"}`},
		{"abandon reparenting", "", abandon("--dependants", "reparent"), http.MethodPost, changesPath + "/12/abandon", `{"dependants":"reparent"}`},
		{
			"abandon the stack above for a reason", "", abandon("--reason", "x", "--dependants", "abandon"),
			http.MethodPost, changesPath + "/12/abandon", `{"reason":"x","dependants":"abandon"}`,
		},
		{"restore", "", []string{"restore", "demo", "12"}, http.MethodPost, changesPath + "/12/restore", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr, sent := runChange(t, hub, tt.stdin, tt.args...)

			wantResult(t, code, stdout, stderr, hubAnswer)
			got := wantOnlyRequest(t, sent, tt.method, tt.rawPath)
			if tt.body == "" {
				if got.body != "" {
					t.Errorf("body = %q, want none", got.body)
				}
				return
			}
			wantJSONBody(t, got.body, tt.body)
		})
	}
}

func TestChangeEdit_SendsTheChangesLabelsWithTheLabelFlagsApplied(t *testing.T) {
	t.Parallel()
	hub := changeHub(t)
	tests := []struct {
		flags []string
		body  string
	}{
		{[]string{"--label", "c"}, `{"labels":["a","b","c"]}`},
		{[]string{"--remove-label", "a"}, `{"labels":["b"]}`},
		{[]string{"--label", "a"}, `{"labels":["a","b"]}`},
		{[]string{"--remove-label", "zz"}, `{"labels":["a","b"]}`},
		{[]string{"--remove-label", "a", "--label", "a"}, `{"labels":["b","a"]}`},
		{[]string{"--remove-label", "a", "--remove-label", "b"}, `{"labels":[]}`},
		{[]string{"--label", "c", "--label", "c", "--title", "T"}, `{"labels":["a","b","c"],"title":"T"}`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.flags, " "), func(t *testing.T) {
			code, stdout, stderr, sent := runChange(t, hub, "", append([]string{"edit", "demo", "13"}, tt.flags...)...)

			wantResult(t, code, stdout, stderr, hubAnswer)
			if len(sent) != 2 || sent[0].method != http.MethodGet || sent[0].rawPath != changesPath+"/13" ||
				sent[1].method != http.MethodPatch || sent[1].rawPath != changesPath+"/13" {
				t.Fatalf("the hub got %v, want GET then PATCH of %s/13", sent, changesPath)
			}
			wantJSONBody(t, sent[1].body, tt.body)
		})
	}
}

func TestChangeRestack_SendsAnEmptyObjectAndPrintsTheOutcome(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"restacked", "up_to_date"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			answer := `{"status":"` + status + `","change":{"number":12,"needs_restack":false}}`
			hub := newStubHub(t).answer(http.MethodPost, changesPath+"/12/restack", http.StatusOK, answer)

			code, stdout, stderr, sent := runChange(t, hub, "", "restack", "demo", "12")

			got := wantOnlyRequest(t, sent, http.MethodPost, changesPath+"/12/restack")
			if ct := got.header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			wantJSONBody(t, got.body, `{}`)
			wantResult(t, code, stdout, stderr, answer)
		})
	}
}

func TestChangeResolve_SendsAResolutionForEachFlagAndPrintsTheOutcome(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	merged := write("m.txt", []byte("merged\n"))
	// The local file's name holds an =, which only the first = splits from
	// the repository path.
	binary := write("v=1.bin", []byte{0xff, 0x00})
	empty := write("empty.txt", nil)
	tests := []struct {
		name  string
		flags []string
		body  string
	}{
		{
			"files and a delete, in the order given",
			[]string{"--file", "app.txt=" + merged, "--file", "logo.bin=" + binary, "--delete", "old.txt"},
			`{"resolutions":[{"path":"app.txt","content_base64":"bWVyZ2VkCg=="},{"path":"logo.bin","content_base64":"/wA="},{"path":"old.txt","delete":true}]}`,
		},
		{
			"a delete before a file",
			[]string{"--delete", "old.txt", "--file", "dir/app.txt=" + merged},
			`{"resolutions":[{"path":"old.txt","delete":true},{"path":"dir/app.txt","content_base64":"bWVyZ2VkCg=="}]}`,
		},
		{"an empty file", []string{"--file", "e.txt=" + empty}, `{"resolutions":[{"path":"e.txt","content_base64":""}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			answer := `{"status":"restacked","change":{"number":2,"version":3,"needs_restack":false}}`
			hub := newStubHub(t).answer(http.MethodPost, changesPath+"/2/restack", http.StatusOK, answer)

			code, stdout, stderr, sent := runChange(t, hub, "", append([]string{"resolve", "demo", "2"}, tt.flags...)...)

			got := wantOnlyRequest(t, sent, http.MethodPost, changesPath+"/2/restack")
			wantQuery(t, got.query, nil)
			// The comparison is exact, so no entry holds a content member.
			wantJSONBody(t, got.body, tt.body)
			wantResult(t, code, stdout, stderr, answer)
		})
	}
}

func TestChange_LifecycleCommandsPrintTheHubsRefusal(t *testing.T) {
	t.Parallel()
	conflict := `{"error":{"code":409,"message":"demo#12 conflicts with what it must sit on","error_type":"rebase_conflict",` +
		`"details":{"target_sha":"` + changeSHA + `","files":[{"path":"f.txt","kind":"content"}]}}}`
	resolved := localFile(t, "f.txt", []byte("both\n"))
	tests := []struct {
		args         []string
		method, path string
		status       int
		errorType    string
		// refusal is the envelope the hub answers, "" for the one envelope
		// builds from the status and the error type.
		refusal string
	}{
		{[]string{"edit", "demo", "12", "--title", "T"}, http.MethodPatch, changesPath + "/12", http.StatusConflict, "change_closed", ""},
		{[]string{"edit", "demo", "12", "--label", "c"}, http.MethodGet, changesPath + "/12", http.StatusNotFound, "change_not_found", ""},
		{[]string{"ready", "demo", "12"}, http.MethodPatch, changesPath + "/12", http.StatusBadRequest, "intent_required", ""},
		{[]string{"abandon", "demo", "12"}, http.MethodPost, changesPath + "/12/abandon", http.StatusConflict, "has_dependants", ""},
		{[]string{"restore", "demo", "12"}, http.MethodPost, changesPath + "/12/restore", http.StatusConflict, "source_workspace_gone", ""},
		{[]string{"restack", "demo", "12"}, http.MethodPost, changesPath + "/12/restack", http.StatusConflict, "rebase_conflict", conflict},
		{[]string{"resolve", "demo", "12", "--file", "f.txt=" + resolved}, http.MethodPost, changesPath + "/12/restack", http.StatusConflict, "rebase_conflict", conflict},
		{[]string{"resolve", "demo", "12", "--delete", "f.txt"}, http.MethodPost, changesPath + "/12/restack", http.StatusNotFound, "change_not_found", ""},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			refusal := tt.refusal
			if refusal == "" {
				refusal = envelope(tt.status, tt.errorType)
			}
			hub := newStubHub(t).answer(tt.method, tt.path, tt.status, refusal)

			code, stdout, stderr, sent := runChange(t, hub, "", tt.args...)

			wantOnlyRequest(t, sent, tt.method, tt.path)
			if code != 1 || stdout != "" {
				t.Errorf("exit status %d, stdout %q; want 1 and nothing", code, stdout)
			}
			if want := indented(t, refusal); stderr != want {
				t.Errorf("stderr = %q, want the hub's envelope %q", stderr, want)
			}
		})
	}
}

func TestChange_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	hub := changeHub(t)
	local := localFile(t, "a.txt", []byte("a\n"))
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
		{"edit", "demo", "12"},
		{"edit", "demo", "12", "--auto-land", "--no-auto-land"},
		{"edit", "demo", "x", "--title", "T"},
		{"abandon", "demo", "12", "--dependants", "delete"},
		{"abandon", "demo"},
		{"ready", "demo"},
		{"draft", "demo", "12", "13"},
		{"restore"},
		{"restack", "demo"},
		{"restack", "demo", "0"},
		{"stack", "demo", "x"},
		{"stack", "demo", "1", "2"},
		{"conflict", "demo"},
		{"conflict", "demo", "x"},
		{"conflict", "demo", "2", "3"},
		{"conflict", "demo", "2", "--file", "a", "--fail-on-conflict"},
		{"resolve", "demo", "2"},
		{"resolve", "demo", "2", "--file", "a.txt"},
		{"resolve", "demo", "2", "--file", "=" + local},
		{"resolve", "demo", "2", "--file", "a.txt="},
		{"resolve", "demo", "2", "--file", "a.txt=/no/such/file"},
		{"resolve", "demo", "2", "--file", "a.txt=" + filepath.Dir(local)},
		{"resolve", "demo", "2", "--delete", ""},
		{"resolve", "demo", "2", "--file", "a.txt=" + local, "--delete", "a.txt"},
		{"resolve", "demo", "2", "--delete", "a.txt", "--delete", "a.txt"},
		{"resolve", "demo", "2", "--file", "a.txt=" + local, "--file", "a.txt=" + local},
		{"resolve", "demo", "--delete", "a.txt"},
		{"resolve", "demo", "2", "3", "--delete", "a.txt"},
		{"resolve", "demo", "x", "--delete", "a.txt"},
		{"wait", "demo"},
		{"wait", "demo", "12", "--for", "done"},
		{"wait", "demo", "12", "--timeout", "0s"},
		{"wait", "demo", "12", "--poll-interval", "-1s"},
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

// polledChange is change 12 as the hub answers it in state, landing status
// and mergeable, given as "state/landing/mergeable"; a state alone, such
// as "landed", gives the state alone.
func polledChange(t *testing.T, given string) string {
	t.Helper()
	parts := strings.Split(given, "/")
	switch len(parts) {
	case 1:
		return `{"number":12,"state":` + jsonString(parts[0]) + `}`
	case 3:
		return `{"number":12,"state":` + jsonString(parts[0]) + `,"landing":{"status":` + jsonString(parts[1]) +
			`},"mergeable":` + jsonString(parts[2]) + `}`
	}
	t.Fatalf("a polled change is state or state/landing/mergeable, got %q", given)
	return ""
}

// runChangeWait runs parley change wait demo 12 with flags, polling every
// interval, against a stub hub that answers its polls with answers in
// order, the last one again once they run out, and returns what it printed
// and the requests it sent.
func runChangeWait(t *testing.T, interval time.Duration, answers []stubAnswer, flags ...string) (code int, stdout, stderr string, sent []stubRequest) {
	t.Helper()
	var mu sync.Mutex
	polls := 0
	hub := newStubHub(t).handle(http.MethodGet, changesPath+"/12", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		a := answers[min(polls, len(answers)-1)]
		polls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		w.Write([]byte(a.body))
	})
	opts := defaultOptions()
	opts.pollInterval = interval
	code, stdout, stderr = runCLIWith(t, opts, onHub(hub.URL, append([]string{"change", "wait", "demo", "12"}, flags...)...)...)
	return code, stdout, stderr, hub.recorded()
}

func TestChangeWait_StopsAtThePollThatMeetsOrFailsTheCondition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		flags []string
		// polls are the answers to the polls, in order: a change as
		// polledChange takes it, or the status of a hub error.
		polls []string
		// want is the error type the wait ends with, "" for a success.
		want string
	}{
		{"landed by default", nil, []string{"open/pending/unknown", "open/ready/clean", "landed"}, ""},
		{"landed", []string{"--for", "landed"}, []string{"landed"}, ""},
		{"ready", []string{"--for", "ready"}, []string{"open/pending/unknown", "open/ready/unknown", "open/ready/clean"}, ""},
		{"ready by landing", []string{"--for", "ready"}, []string{"landed"}, ""},
		{"mergeable while a blocked draft", []string{"--for", "mergeable"}, []string{"draft/blocked/unknown", "draft/blocked/clean"}, ""},
		{"mergeable by landing", []string{"--for", "mergeable"}, []string{"landed"}, ""},
		{"landed but abandoned", []string{"--for", "landed"}, []string{"open/pending/unknown", "abandoned"}, "wait_failed"},
		{"ready but abandoned", []string{"--for", "ready"}, []string{"abandoned"}, "wait_failed"},
		{"mergeable but abandoned", []string{"--for", "mergeable"}, []string{"abandoned"}, "wait_failed"},
		{"ready but blocked", []string{"--for", "ready"}, []string{"open/pending/unknown", "open/blocked/clean"}, "wait_failed"},
		{"ready but a draft", []string{"--for", "ready"}, []string{"draft/blocked/unknown"}, "wait_failed"},
		{"ready but conflicting", []string{"--for", "ready"}, []string{"open/ready/conflict"}, "wait_failed"},
		{"mergeable but conflicting", []string{"--for", "mergeable"}, []string{"open/ready/unknown", "open/ready/conflict"}, "wait_failed"},
		{"landed but gone", nil, []string{"open/pending/unknown", "404"}, "change_not_found"},
		{"landed but unauthenticated", nil, []string{"401"}, "unauthenticated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var answers []stubAnswer
			for _, p := range tt.polls {
				if status, err := strconv.Atoi(p); err == nil {
					answers = append(answers, stubAnswer{status, envelope(status, tt.want)})
				} else {
					answers = append(answers, stubAnswer{http.StatusOK, polledChange(t, p)})
				}
			}
			last := answers[len(answers)-1]

			code, stdout, stderr, sent := runChangeWait(t, 10*time.Millisecond, answers, tt.flags...)

			if len(sent) != len(answers) {
				t.Errorf("the hub got %d requests, want %d polls", len(sent), len(answers))
			}
			for _, r := range sent {
				if r.method != http.MethodGet || r.rawPath != changesPath+"/12" {
					t.Errorf("the hub got %s %s, want only GET %s/12", r.method, r.rawPath, changesPath)
				}
			}
			progress := nonEmptyLines(strings.TrimSuffix(stderr, lastDocument(stderr)))
			if tt.want == "" {
				if code != 0 || len(progress) != len(answers) {
					t.Errorf("exit status %d, stderr %q; want 0 and one progress line per poll", code, stderr)
				}
				if want := indented(t, last.body); stdout != want {
					t.Errorf("stdout = %q, want the last change %q", stdout, want)
				}
				return
			}
			if code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
			gotCode, _, gotType := clientEnvelope(t, lastDocument(stderr))
			if gotType != tt.want {
				t.Errorf("stderr = %q, want an envelope of %s", stderr, tt.want)
			}
			if last.status != http.StatusOK {
				if stdout != "" || gotCode != float64(last.status) {
					t.Errorf("stdout %q, envelope code %v; want nothing and the hub's envelope", stdout, gotCode)
				}
				return
			}
			if want := indented(t, last.body); stdout != want || gotCode != 0 {
				t.Errorf("stdout %q, envelope code %v; want the last change %q and code 0", stdout, gotCode, want)
			}
			if len(progress) != len(answers) {
				t.Errorf("progress = %q, want one line per poll", progress)
			}
		})
	}
}

func TestChangeWait_GivesUpAtTheTimeoutWithoutChangingAnything(t *testing.T) {
	t.Parallel()
	start := time.Now()

	code, stdout, stderr, sent := runChangeWait(t, 20*time.Millisecond, []stubAnswer{{http.StatusOK, polledChange(t, "open/pending/unknown")}},
		"--timeout", "1s")

	// The flag's smallest timeout is 1s; the wait may outlast it by one
	// poll's request time.
	if elapsed := time.Since(start); elapsed < time.Second || elapsed >= 2*time.Second {
		t.Errorf("the wait took %v, want 1s and less than 2s", elapsed)
	}
	if code != 1 || stdout != "" {
		t.Errorf("exit status %d, stdout %q; want 1 and nothing", code, stdout)
	}
	if gotCode, _, gotType := clientEnvelope(t, lastDocument(stderr)); gotCode != 0 || gotType != "wait_timeout" {
		t.Errorf("stderr = %q, want a wait_timeout envelope with code 0", stderr)
	}
	if len(sent) < 2 {
		t.Errorf("the hub got %d polls, want at least 2", len(sent))
	}
	for _, r := range sent {
		if r.method != http.MethodGet {
			t.Errorf("the hub got %s %s, want only GETs", r.method, r.rawPath)
		}
	}
}
