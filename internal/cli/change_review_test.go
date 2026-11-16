package cli

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// reviewPath is the path of change 7 of the workspace w1, as sent.
const reviewPath = "/api/v1/workspaces/w1/changes/7"

// verdictID is the id of a verdict the hub holds.
const verdictID = "0b9c6c1e-5d0a-4f43-9a55-0d3a7c0a1f10"

// reviewHub is a stub hub that answers the verdict and check routes of
// change 7 of w1, and the verdicts of change 7 of team/w, with hubAnswer,
// and refuses a verdict on change 8 of w1, which is closed.
func reviewHub(t *testing.T) *stubHub {
	t.Helper()
	return newStubHub(t).
		answer(http.MethodPost, reviewPath+"/approvals", http.StatusCreated, hubAnswer).
		answer(http.MethodPost, "/api/v1/workspaces/team/w/changes/7/approvals", http.StatusCreated, hubAnswer).
		answer(http.MethodGet, reviewPath+"/approvals", http.StatusOK, hubAnswer).
		answer(http.MethodPut, reviewPath+"/checks/test", http.StatusCreated, hubAnswer).
		answer(http.MethodPut, reviewPath+"/checks/lint.go", http.StatusOK, hubAnswer).
		answer(http.MethodGet, reviewPath+"/checks", http.StatusOK, hubAnswer).
		answer(http.MethodPost, "/api/v1/workspaces/w1/changes/8/approvals", http.StatusConflict, envelope(http.StatusConflict, "change_closed"))
}

func TestChangeReviewCommands_SendTheOneRequestOfTheirRoute(t *testing.T) {
	t.Parallel()
	hub := reviewHub(t)
	tests := []struct {
		args            []string
		method, rawPath string
		query           url.Values
		// body is the JSON body sent, "" for none.
		body string
		// refusal is the hub's error envelope the run prints, "" for a
		// success.
		refusal string
	}{
		{[]string{"approve", "w1", "7"}, http.MethodPost, reviewPath + "/approvals", nil, `{"verdict":"approve"}`, ""},
		{
			[]string{"approve", "w1", "7", "--version", "2", "--comment", "lgtm"}, http.MethodPost, reviewPath + "/approvals", nil,
			`{"verdict":"approve","version":2,"comment":"lgtm"}`, "",
		},
		{[]string{"reject", "w1", "7", "--comment", "no tests"}, http.MethodPost, reviewPath + "/approvals", nil, `{"verdict":"reject","comment":"no tests"}`, ""},
		{[]string{"approvals", "w1", "7"}, http.MethodGet, reviewPath + "/approvals", nil, "", ""},
		{[]string{"approvals", "w1", "7", "--version", "2"}, http.MethodGet, reviewPath + "/approvals", url.Values{"version": {"2"}}, "", ""},
		{[]string{"check", "w1", "7", "test", "--status", "passed"}, http.MethodPut, reviewPath + "/checks/test", nil, `{"status":"passed"}`, ""},
		{
			[]string{"check", "w1", "7", "lint.go", "--status", "failed", "--version", "2", "--summary", "3 errors", "--url", "https://ci.example/9"},
			http.MethodPut, reviewPath + "/checks/lint.go", nil,
			`{"status":"failed","version":2,"summary":"3 errors","url":"https://ci.example/9"}`, "",
		},
		{[]string{"checks", "w1", "7"}, http.MethodGet, reviewPath + "/checks", nil, "", ""},
		{[]string{"checks", "w1", "7", "--version", "2"}, http.MethodGet, reviewPath + "/checks", url.Values{"version": {"2"}}, "", ""},
		{[]string{"approve", "team/w", "7"}, http.MethodPost, "/api/v1/workspaces/team%2Fw/changes/7/approvals", nil, `{"verdict":"approve"}`, ""},
		{
			[]string{"approve", "w1", "8"}, http.MethodPost, "/api/v1/workspaces/w1/changes/8/approvals", nil, `{"verdict":"approve"}`,
			envelope(http.StatusConflict, "change_closed"),
		},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr, sent := runChange(t, hub, "", tt.args...)

			got := wantOnlyRequest(t, sent, tt.method, tt.rawPath)
			wantQuery(t, got.query, tt.query)
			if auth := got.header.Get("Authorization"); auth != "Bearer k" {
				t.Errorf("Authorization = %q, want %q", auth, "Bearer k")
			}
			if tt.body == "" {
				if got.body != "" {
					t.Errorf("body = %q, want none", got.body)
				}
			} else {
				wantJSONBody(t, got.body, tt.body)
			}
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

func TestChangeApprovals_WithdrawSendsOneDeleteOfTheVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		// refusal is the hub's error envelope, "" for a success without a
		// body.
		refusal string
	}{
		{"withdrawn", http.StatusNoContent, ""},
		{"refused", http.StatusForbidden, envelope(http.StatusForbidden, "forbidden")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodDelete, reviewPath+"/approvals/"+verdictID, tt.status, tt.refusal)

			code, stdout, stderr, sent := runChange(t, hub, "", "approvals", "w1", "7", "--withdraw", verdictID)

			got := wantOnlyRequest(t, sent, http.MethodDelete, reviewPath+"/approvals/"+verdictID)
			if got.body != "" {
				t.Errorf("body = %q, want none", got.body)
			}
			if tt.refusal == "" {
				if code != 0 || stdout != "{}\n" || stderr != "" {
					t.Errorf("exit status %d, stdout %q, stderr %q; want 0, {} and nothing", code, stdout, stderr)
				}
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

func TestChangeReviewCommands_RefuseABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	hub := reviewHub(t)
	tests := [][]string{
		{"reject", "w1", "7"},
		{"reject", "w1", "7", "--comment", ""},
		{"check", "w1", "7", "test"},
		{"check", "w1", "7", "test", "--status", "ok"},
		{"check", "w1", "7", "test", "--status", ""},
		{"check", "w1", "7", "--status", "passed"},
		{"check", "w1", "7", "test", "extra", "--status", "passed"},
		{"check", "w1", "7", "test", "--status", "passed", "--version", "latest"},
		{"approve", "w1", "7", "--version", "0"},
		{"approve", "w1", "7", "--version", "x"},
		{"approve", "w1", "7", "8"},
		{"approve", "w1"},
		{"reject", "w1", "x", "--comment", "c"},
		{"reject", "w1", "7", "--comment", "c", "--version", "-1"},
		{"approvals", "w1"},
		{"approvals", "w1", "0"},
		{"approvals", "w1", "7", "--version", "latest"},
		{"approvals", "w1", "7", "--withdraw", "id1", "--version", "2"},
		{"approvals", "w1", "7", "--withdraw", ""},
		{"checks", "w1", "seven"},
		{"checks", "w1", "7", "--version", "1.5"},
		{"checks", "w1", "7", "8"},
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
