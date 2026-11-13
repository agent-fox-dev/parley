package cli

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The test sets environment variables, so it must not run in parallel.
func TestAdminOrgsCommands_SendOneRequestAndPrintTheAnswer(t *testing.T) {
	create := []string{"admin", "orgs", "create", "--name", "Acme", "--slug", "acme"}
	update := []string{"admin", "orgs", "update", "acme"}
	tests := []struct {
		args []string
		want sentRequest
	}{
		{[]string{"admin", "orgs", "list"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/orgs"}},
		{[]string{"admin", "orgs", "list", "--include-blocked"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/orgs", query: "include_blocked=true"}},
		{create, sentRequest{method: http.MethodPost, rawPath: "/api/v1/orgs", body: `{"name":"Acme","slug":"acme"}`}},
		{slices.Concat(create, []string{"--url", "https://acme.example", "--owner", "0f8fad5b-d9cb-469f-a165-70867728950e"}), sentRequest{method: http.MethodPost, rawPath: "/api/v1/orgs",
			body: `{"name":"Acme","slug":"acme","url":"https://acme.example","owner_id":"0f8fad5b-d9cb-469f-a165-70867728950e"}`}},
		{slices.Concat(update, []string{"--name", "Acme Inc"}), sentRequest{method: http.MethodPatch, rawPath: "/api/v1/orgs/acme", body: `{"name":"Acme Inc"}`}},
		{slices.Concat(update, []string{"--url", "https://acme.example"}), sentRequest{method: http.MethodPatch, rawPath: "/api/v1/orgs/acme", body: `{"url":"https://acme.example"}`}},
		{slices.Concat(update, []string{"--url", ""}), sentRequest{method: http.MethodPatch, rawPath: "/api/v1/orgs/acme", body: `{"url":""}`}},
		{slices.Concat(update, []string{"--name", "Acme Inc", "--url", ""}), sentRequest{method: http.MethodPatch, rawPath: "/api/v1/orgs/acme", body: `{"name":"Acme Inc","url":""}`}},
		{[]string{"admin", "orgs", "delete", "acme"}, sentRequest{method: http.MethodDelete, rawPath: "/api/v1/orgs/acme"}},
		{[]string{"admin", "orgs", "block", "acme"}, sentRequest{method: http.MethodPost, rawPath: "/api/v1/orgs/acme/block"}},
		{[]string{"admin", "orgs", "unblock", "acme"}, sentRequest{method: http.MethodPost, rawPath: "/api/v1/orgs/acme/unblock"}},
		{[]string{"admin", "orgs", "members", "list", "acme"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/orgs/acme/members"}},
		{[]string{"admin", "orgs", "members", "add", "acme", "alice"}, sentRequest{method: http.MethodPut, rawPath: "/api/v1/orgs/acme/members/alice"}},
		{[]string{"admin", "orgs", "members", "remove", "acme", "alice"}, sentRequest{method: http.MethodDelete, rawPath: "/api/v1/orgs/acme/members/alice"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			wantSends(t, tt.args, tt.want)
		})
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestIdentityCommands_PrintAnEmptyObjectWhenTheHubAnswersWithoutABody(t *testing.T) {
	tests := []struct {
		args         []string
		method, path string
	}{
		{[]string{"admin", "users", "delete", "alice"}, http.MethodDelete, "/api/v1/users/alice"},
		{[]string{"admin", "orgs", "delete", "acme"}, http.MethodDelete, "/api/v1/orgs/acme"},
		{[]string{"admin", "orgs", "members", "add", "acme", "alice"}, http.MethodPut, "/api/v1/orgs/acme/members/alice"},
		{[]string{"admin", "orgs", "members", "remove", "acme", "alice"}, http.MethodDelete, "/api/v1/orgs/acme/members/alice"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			hub := newStubHub(t).answer(tt.method, tt.path, http.StatusNoContent, "")

			code, stdout, stderr := runAgainst(t, hub, tt.args...)

			if code != 0 {
				t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
			}
			if stdout != "{}\n" {
				t.Errorf("stdout = %q, want %q", stdout, "{}\n")
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want it empty", stderr)
			}
			if n := len(hub.recorded()); n != 1 {
				t.Errorf("the hub got %d requests, want 1", n)
			}
		})
	}
}
