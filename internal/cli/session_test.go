package cli

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// sessionsPath is the sessions route as sent, and sessionID the id of one
// session, which the commands pass on without judging it.
const (
	sessionsPath = "/api/v1/sessions"
	sessionID    = "S"
)

// sessionAnswer is a session as the hub answers it, compact.
const sessionAnswer = `{"id": "` + sessionID + `", "workspace_slug": "demo", "status": "active", "metadata": {"a": [1]}}`

func TestSession_CommandsSendTheRouteWithTheMembersAndParametersTheirFlagsName(t *testing.T) {
	t.Parallel()
	usageAnswer := `{"id": "U", "session_id": "` + sessionID + `", "model": "m1", "input_tokens": 10}`
	completed := `{"id": "` + sessionID + `", "status": "completed"}`
	tests := []struct {
		args         []string
		method, path string
		// status and answer are what the stub hub answers.
		status int
		answer string
		query  url.Values
		// body is the JSON document sent, "" for none.
		body string
	}{
		{[]string{"start", "demo"}, http.MethodPost, sessionsPath, http.StatusCreated, sessionAnswer, nil, `{"workspace_slug":"demo"}`},
		{[]string{"start", "demo"}, http.MethodPost, sessionsPath, http.StatusOK, sessionAnswer, nil, `{"workspace_slug":"demo"}`},
		{
			[]string{"start", "demo", "--id", "X", "--run-id", "20260102_030405_abcdef", "--node-id", "n1", "--archetype", "coder", "--model", "m1", "--metadata", `{"a":[1]}`},
			http.MethodPost, sessionsPath, http.StatusCreated, sessionAnswer, nil,
			`{"workspace_slug":"demo","id":"X","run_id":"20260102_030405_abcdef","node_id":"n1","archetype":"coder","model":"m1","metadata":{"a":[1]}}`,
		},
		{
			[]string{"start", "demo", "--id", "X", "--metadata", `[1, "two"]`},
			http.MethodPost, sessionsPath, http.StatusOK, sessionAnswer, nil,
			`{"workspace_slug":"demo","id":"X","metadata":[1,"two"]}`,
		},
		{
			[]string{"usage", sessionID, "--model", "m1", "--input", "10", "--output", "5", "--cache-read", "3", "--id", "U"},
			http.MethodPost, sessionsPath + "/" + sessionID + "/usage", http.StatusCreated, usageAnswer, nil,
			`{"model":"m1","input_tokens":10,"output_tokens":5,"cache_read_tokens":3,"id":"U"}`,
		},
		{[]string{"usage", sessionID, "--model", "m1"}, http.MethodPost, sessionsPath + "/" + sessionID + "/usage", http.StatusCreated, usageAnswer, nil, `{"model":"m1"}`},
		{[]string{"complete", sessionID}, http.MethodPost, sessionsPath + "/" + sessionID + "/complete", http.StatusOK, completed, nil, `{"status":"completed"}`},
		{
			[]string{"complete", sessionID, "--status", "failed", "--error", "boom", "--duration-ms", "1200"},
			http.MethodPost, sessionsPath + "/" + sessionID + "/complete", http.StatusOK, completed, nil,
			`{"status":"failed","error_message":"boom","duration_ms":1200}`,
		},
		{[]string{"complete", sessionID, "--status", "timeout"}, http.MethodPost, sessionsPath + "/" + sessionID + "/complete", http.StatusOK, completed, nil, `{"status":"timeout"}`},
		{[]string{"get", sessionID}, http.MethodGet, sessionsPath + "/" + sessionID, http.StatusOK, sessionAnswer, nil, ""},
		{[]string{"list"}, http.MethodGet, sessionsPath, http.StatusOK, hubAnswer, nil, ""},
		{
			[]string{"list", "--workspace", "demo", "--include-forks", "--run-id", "20260102_030405_abcdef", "--status", "active", "--since", "2026-01-01T00:00:00Z", "--limit", "10", "--cursor", "c1"},
			http.MethodGet, sessionsPath, http.StatusOK, hubAnswer,
			url.Values{
				"workspace": {"demo"}, "include_forks": {"true"}, "run_id": {"20260102_030405_abcdef"}, "status": {"active"},
				"since": {"2026-01-01T00:00:00Z"}, "limit": {"10"}, "cursor": {"c1"},
			},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status)+" "+strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(tt.method, tt.path, tt.status, tt.answer)

			code, stdout, stderr, sent := runOnHub(t, hub, append([]string{"session"}, tt.args...)...)

			wantResult(t, code, stdout, stderr, tt.answer)
			got := wantOnlyRequest(t, sent, tt.method, tt.path)
			wantQuery(t, got.query, tt.query)
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

func TestSession_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t)
	tests := []struct {
		args      []string
		inMessage string
	}{
		{[]string{"session", "start"}, "parley session start needs the argument <workspace-slug>"},
		{[]string{"session", "usage"}, "parley session usage needs the argument <session-id>"},
		{[]string{"session", "usage", sessionID}, "parley session usage needs the flag --model"},
		{[]string{"session", "complete"}, "parley session complete needs the argument <session-id>"},
		{[]string{"session", "get"}, "parley session get needs the argument <session-id>"},
		{[]string{"session", "list", "demo"}, `got "demo"`},
		{[]string{"session", "start", "demo", "--metadata", "{oops"}, "parley session start takes --metadata as a JSON document"},
		{[]string{"session", "usage", sessionID, "--model", "m1", "--input", "ten"}, `"ten" for "--input" flag`},
		{[]string{"session", "usage", sessionID, "--model", "m1", "--output", "1.5"}, `"1.5" for "--output" flag`},
		{[]string{"session", "usage", sessionID, "--model", "m1", "--cache-read", "x"}, `"x" for "--cache-read" flag`},
		{[]string{"session", "complete", sessionID, "--duration-ms", "x"}, `"x" for "--duration-ms" flag`},
		{[]string{"session", "list", "--limit", "x"}, `"x" for "--limit" flag`},
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

func TestSession_PrintsTheHubsRefusalAsReceived(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args         []string
		method, path string
		status       int
		errorType    string
	}{
		{[]string{"usage", sessionID, "--model", "m1"}, http.MethodPost, sessionsPath + "/" + sessionID + "/usage", http.StatusConflict, "session_not_active"},
		{[]string{"start", "demo"}, http.MethodPost, sessionsPath, http.StatusConflict, "workspace_archived"},
		{[]string{"complete", sessionID}, http.MethodPost, sessionsPath + "/" + sessionID + "/complete", http.StatusForbidden, "forbidden"},
		{[]string{"get", sessionID}, http.MethodGet, sessionsPath + "/" + sessionID, http.StatusNotFound, "not_found"},
		{[]string{"list", "--status", "nope"}, http.MethodGet, sessionsPath, http.StatusBadRequest, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			refusal := envelope(tt.status, tt.errorType)
			hub := newStubHub(t).answer(tt.method, tt.path, tt.status, refusal)

			code, stdout, stderr, sent := runOnHub(t, hub, append([]string{"session"}, tt.args...)...)

			wantOnlyRequest(t, sent, tt.method, tt.path)
			wantHubFailure(t, code, stdout, stderr, refusal)
		})
	}
}
