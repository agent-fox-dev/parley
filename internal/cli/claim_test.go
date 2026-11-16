package cli

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// claimsPath is the claims route of demo, as sent, and claimID the id of
// one of its claims.
const (
	claimsPath = "/api/v1/workspaces/demo/claims"
	claimID    = "0b7e1c52-3f4a-4d8e-9b21-5c6d7e8f9a01"
)

func TestClaim_CommandsSendTheRouteWithTheMembersAndParametersTheirFlagsName(t *testing.T) {
	t.Parallel()
	answer := `{"claim": {"id": "` + claimID + `", "state": "active"}, "activity": {"changes": [], "claims": [], "landed": []}}`
	hub := newStubHub(t).
		answer(http.MethodPost, claimsPath, http.StatusCreated, answer).
		answer(http.MethodGet, claimsPath, http.StatusOK, hubAnswer).
		answer(http.MethodPatch, claimsPath+"/"+claimID, http.StatusOK, answer).
		answer(http.MethodDelete, claimsPath+"/"+claimID, http.StatusNoContent, "")
	tests := []struct {
		args         []string
		method, path string
		query        url.Values
		// body is the JSON document sent, "" for none.
		body string
		// prints is the document parley prints.
		prints string
	}{
		{
			[]string{"create", "demo", "--paths", "api/**,docs/**", "--intent", "rework"},
			http.MethodPost, claimsPath, nil, `{"paths":["api/**","docs/**"],"intent":"rework"}`, answer,
		},
		{
			[]string{"create", "demo", "--paths", "api/**,docs/**", "--intent", "rework", "--target", "release", "--ttl", "30m", "--session", "s1"},
			http.MethodPost, claimsPath, nil,
			`{"paths":["api/**","docs/**"],"intent":"rework","target_branch":"release","ttl":"30m","session_id":"s1"}`, answer,
		},
		{[]string{"list", "demo"}, http.MethodGet, claimsPath, nil, "", hubAnswer},
		{
			[]string{"list", "demo", "--state", "expired", "--session", "s1", "--path", "api/**", "--limit", "10", "--cursor", "c"},
			http.MethodGet, claimsPath,
			url.Values{"state": {"expired"}, "session_id": {"s1"}, "path": {"api/**"}, "limit": {"10"}, "cursor": {"c"}}, "", hubAnswer,
		},
		{[]string{"renew", "demo", claimID}, http.MethodPatch, claimsPath + "/" + claimID, nil, `{"ttl":"1h"}`, answer},
		{[]string{"renew", "demo", claimID, "--ttl", "4h"}, http.MethodPatch, claimsPath + "/" + claimID, nil, `{"ttl":"4h"}`, answer},
		{[]string{"release", "demo", claimID}, http.MethodDelete, claimsPath + "/" + claimID, nil, "", `{}`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr, sent := runOnHub(t, hub, append([]string{"claim"}, tt.args...)...)

			wantResult(t, code, stdout, stderr, tt.prints)
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

func TestClaim_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t)
	tests := []struct {
		args      []string
		inMessage string
	}{
		{[]string{"claim", "create", "demo", "--intent", "x"}, "parley claim create needs the flag --paths"},
		{[]string{"claim", "create", "demo", "--paths", "a"}, "parley claim create needs the flag --intent"},
		{[]string{"claim", "create", "--paths", "a", "--intent", "x"}, "parley claim create needs the argument <slug>"},
		{[]string{"claim", "renew", "demo"}, "parley claim renew needs the argument <id>"},
		{[]string{"claim", "renew", "demo", claimID, "x"}, `got the extra argument "x"`},
		{[]string{"claim", "release", "demo"}, "parley claim release needs the argument <id>"},
		{[]string{"claim", "list"}, "parley claim list needs the argument <slug>"},
		{[]string{"claim", "list", "demo", "--limit", "x"}, `"x" for "--limit" flag`},
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
