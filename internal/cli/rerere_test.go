package cli

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// rererePath is the route of the remembered resolutions of demo, as sent.
const rererePath = "/api/v1/workspaces/demo/rerere"

func TestRerereList_PrintsTheResolutionsOfAWorkspace(t *testing.T) {
	tests := []struct {
		name, slug, rawPath string
	}{
		{"a slug", "demo", rererePath},
		{"a slug sent as one segment", "a/b", "/api/v1/workspaces/a%2Fb/rerere"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantSends(t, []string{"rerere", "list", tt.slug}, sentRequest{method: http.MethodGet, rawPath: tt.rawPath})
		})
	}
}

func TestRerereForget_SendsEachPartOfTheArgumentAsItsOwnSegment(t *testing.T) {
	t.Parallel()
	id := "3f2a" + strings.Repeat("0", 36)
	tests := []struct {
		arg, rawPath string
	}{
		{id, rererePath + "/" + id},
		{"src/net/dial.go", rererePath + "/src/net/dial.go"},
		{"docs/read me.md", rererePath + "/docs/read%20me.md"},
		{"a/b?c#d", rererePath + "/a/b%3Fc%23d"},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			t.Parallel()
			path, err := url.PathUnescape(tt.rawPath)
			if err != nil {
				t.Fatal(err)
			}
			hub := newStubHub(t).answer(http.MethodDelete, path, http.StatusNoContent, "")

			code, stdout, stderr, sent := runOnHub(t, hub, "rerere", "forget", "demo", tt.arg)

			if got := wantOnlyRequest(t, sent, http.MethodDelete, tt.rawPath); got.body != "" {
				t.Errorf("body = %q, want none", got.body)
			}
			pathspec, err := json.Marshal(tt.arg)
			if err != nil {
				t.Fatal(err)
			}
			wantResult(t, code, stdout, stderr, `{"status": "forgotten", "pathspec": `+string(pathspec)+`}`)
		})
	}
}

// The -- row puts the global flags first, so that the word after -- is the
// command's argument; runAgainst sets the environment, so the test does not
// run in parallel.
func TestRerereAndPatchStatus_PrintTheHubsErrorAsReceived(t *testing.T) {
	envelope := func(code int, errorType, message string) string {
		return `{"error": {"code": ` + strconv.Itoa(code) + `, "message": "` + message + `", "error_type": "` + errorType + `"}}`
	}
	tests := []struct {
		args      []string
		method    string
		rawPath   string
		status    int
		errorType string
		message   string
	}{
		{[]string{"rerere", "forget", "demo", "nosuch"}, http.MethodDelete, rererePath + "/nosuch", http.StatusNotFound, "not_found", "resolution not found"},
		{[]string{"rerere", "list", "plain"}, http.MethodGet, "/api/v1/workspaces/plain/rerere", http.StatusBadRequest, "workspace_mode_mismatch", "the workspace is not a carry-patch workspace"},
		{[]string{"workspace", "patch-status", "demo"}, http.MethodGet, "/api/v1/workspaces/demo/patch-status", http.StatusForbidden, "insufficient_scope", "the token lacks the scope workspaces:read"},
		{[]string{"rerere", "forget", "demo", "--", "-x"}, http.MethodDelete, rererePath + "/-x", http.StatusBadRequest, "invalid_request", "the pathspec must not begin with -"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			answer := envelope(tt.status, tt.errorType, tt.message)
			path, err := url.PathUnescape(tt.rawPath)
			if err != nil {
				t.Fatal(err)
			}
			hub := newStubHub(t).answer(tt.method, path, tt.status, answer)

			code, stdout, stderr := runAgainst(t, hub, tt.args...)

			wantOnlyRequest(t, hub.recorded(), tt.method, tt.rawPath)
			wantHubFailure(t, code, stdout, stderr, answer)
			var got struct {
				Error struct {
					Code      int    `json:"code"`
					ErrorType string `json:"error_type"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stderr), &got); err != nil {
				t.Fatalf("stderr %q is not JSON: %v", stderr, err)
			}
			if got.Error.Code != tt.status || got.Error.ErrorType != tt.errorType {
				t.Errorf("stderr names code %d and %q, want %d and %q", got.Error.Code, got.Error.ErrorType, tt.status, tt.errorType)
			}
		})
	}
}

func TestRerereAndPatchStatus_RefuseAWrongNumberOfArgumentsBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t)
	tests := []struct {
		args      []string
		inMessage string
	}{
		{[]string{"rerere", "list"}, "parley rerere list needs the argument <slug>"},
		{[]string{"rerere", "list", "a", "b"}, `got the extra argument "b"`},
		{[]string{"rerere", "forget", "demo"}, "parley rerere forget needs the argument <id-or-path>"},
		{[]string{"rerere", "forget", "demo", "a", "b"}, `got the extra argument "b"`},
		{[]string{"workspace", "patch-status"}, "parley workspace patch-status needs the argument <slug>"},
		{[]string{"workspace", "patch-status", "a", "b"}, `got the extra argument "b"`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr, _ := runOnHub(t, hub, tt.args...)

			if message := wantClientFailure(t, code, stdout, stderr, "usage_error"); !strings.Contains(message, tt.inMessage) {
				t.Errorf("message = %q, want it to contain %q", message, tt.inMessage)
			}
		})
	}
	if sent := hub.recorded(); len(sent) != 0 {
		t.Errorf("the hub got %v, want no request", sent)
	}
}
