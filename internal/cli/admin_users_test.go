package cli

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// The test sets environment variables, so it must not run in parallel.
func TestAdminUsersCommands_SendOneRequestAndPrintTheAnswer(t *testing.T) {
	create := []string{"admin", "users", "create", "--username", "carol", "--email", "carol@example.com", "--provider", "github", "--provider-id", "1"}
	tests := []struct {
		args []string
		want sentRequest
	}{
		{[]string{"admin", "users", "list"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/users"}},
		{[]string{"admin", "users", "list", "--include-blocked"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/users", query: "include_blocked=true"}},
		{[]string{"admin", "users", "show", "alice"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/users/alice"}},
		{create, sentRequest{method: http.MethodPost, rawPath: "/api/v1/users",
			body: `{"username":"carol","email":"carol@example.com","provider":"github","provider_id":"1"}`}},
		{slices.Concat(create, []string{"--full-name", "Carol C"}), sentRequest{method: http.MethodPost, rawPath: "/api/v1/users",
			body: `{"username":"carol","email":"carol@example.com","provider":"github","provider_id":"1","full_name":"Carol C"}`}},
		{[]string{"admin", "users", "update", "alice", "--full-name", "Alice A."}, sentRequest{method: http.MethodPatch, rawPath: "/api/v1/users/alice", body: `{"full_name":"Alice A."}`}},
		{[]string{"admin", "users", "delete", "alice"}, sentRequest{method: http.MethodDelete, rawPath: "/api/v1/users/alice"}},
		{[]string{"admin", "users", "promote", "alice"}, sentRequest{method: http.MethodPost, rawPath: "/api/v1/users/alice/promote"}},
		{[]string{"admin", "users", "demote", "alice"}, sentRequest{method: http.MethodPost, rawPath: "/api/v1/users/alice/demote"}},
		{[]string{"admin", "users", "block", "alice"}, sentRequest{method: http.MethodPost, rawPath: "/api/v1/users/alice/block"}},
		{[]string{"admin", "users", "unblock", "alice"}, sentRequest{method: http.MethodPost, rawPath: "/api/v1/users/alice/unblock"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			wantSends(t, tt.args, tt.want)
		})
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestIdentityCommands_RefuseABadCommandLineAndSendNothing(t *testing.T) {
	createWithout := func(flag string) []string {
		args := []string{"admin", "users", "create"}
		for _, f := range []string{"--username", "--email", "--provider", "--provider-id"} {
			if f != flag {
				args = append(args, f, "v")
			}
		}
		return args
	}
	tests := []struct {
		args []string
		// command is the command the usage error names, and mentions what
		// it says is wrong.
		command, mentions string
	}{
		{[]string{"user", "update"}, "parley user update", "--full-name"},
		{[]string{"admin", "users", "update", "alice"}, "parley admin users update", "--full-name"},
		{createWithout("--username"), "parley admin users create", "--username"},
		{createWithout("--email"), "parley admin users create", "--email"},
		{createWithout("--provider"), "parley admin users create", "--provider"},
		{createWithout("--provider-id"), "parley admin users create", "--provider-id"},
		{[]string{"orgs", "show"}, "parley orgs show", "<org>"},
		{[]string{"orgs", "show", "a", "b"}, "parley orgs show", `"b"`},
		{[]string{"admin", "users", "delete"}, "parley admin users delete", "<user>"},
		{[]string{"admin", "users", "promote", "a", "b"}, "parley admin users promote", `"b"`},
		{[]string{"admin", "orgs", "create", "--name", "A"}, "parley admin orgs create", "--slug"},
		{[]string{"admin", "orgs", "create", "--slug", "a"}, "parley admin orgs create", "--name"},
		{[]string{"admin", "orgs", "update", "acme"}, "parley admin orgs update", "--name or --url"},
		{[]string{"admin", "orgs", "members", "add", "acme"}, "parley admin orgs members add", "<user>"},
		{[]string{"admin", "keys", "list"}, "parley admin keys list", "<user>"},
		{[]string{"admin", "keys", "revoke", "alice"}, "parley admin keys revoke", "<key_id>"},
		{[]string{"keys", "list", "x"}, "parley keys list", `"x"`},
		{[]string{"keys", "refresh", "x"}, "parley keys refresh", `"x"`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			hub := newStubHub(t)

			code, stdout, stderr := runAgainst(t, hub, tt.args...)

			message := wantClientFailure(t, code, stdout, stderr, usageErrorType)
			if !strings.Contains(message, tt.command) || !strings.Contains(message, tt.mentions) {
				t.Errorf("message = %q, want it to name %q and %s", message, tt.command, tt.mentions)
			}
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestIdentityCommands_PrintTheHubsErrorEnvelopeAsReceived(t *testing.T) {
	tests := []struct {
		args         []string
		method, path string
		status       int
		envelope     string
	}{
		{[]string{"admin", "users", "demote", "root"}, http.MethodPost, "/api/v1/users/root/demote", http.StatusConflict,
			`{"error":{"code":409,"message":"the user is the only active admin","error_type":"conflict"}}`},
		{[]string{"admin", "users", "show", "nobody"}, http.MethodGet, "/api/v1/users/nobody", http.StatusNotFound,
			`{"error":{"code":404,"message":"user not found","error_type":"not_found"}}`},
		{[]string{"admin", "users", "list"}, http.MethodGet, "/api/v1/users", http.StatusForbidden,
			`{"error":{"code":403,"message":"this route needs an admin","error_type":"forbidden"}}`},
		{[]string{"user", "show"}, http.MethodGet, "/api/v1/user", http.StatusUnauthorized,
			`{"error":{"code":401,"message":"invalid credentials","error_type":"unauthenticated"}}`},
		{[]string{"admin", "orgs", "create", "--name", "A", "--slug", "a"}, http.MethodPost, "/api/v1/orgs", http.StatusBadRequest,
			`{"error":{"code":400,"message":"slug must be 1 to 128 characters of a-z, 0-9, _ and -, starting and ending with a letter or digit","error_type":"invalid_request"}}`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			hub := newStubHub(t).answer(tt.method, tt.path, tt.status, tt.envelope)

			code, stdout, stderr := runAgainst(t, hub, tt.args...)

			if code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
			if want := indented(t, tt.envelope); stderr != want {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
		})
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestIdentityCommands_SendAUserOrOrgArgumentAsOnePathSegment(t *testing.T) {
	commands := []struct {
		// words come before the argument; the request is method to prefix
		// followed by the argument, a path of segments segments.
		words          []string
		method, prefix string
		segments       int
	}{
		{[]string{"admin", "users", "show"}, http.MethodGet, "/api/v1/users/", 5},
		{[]string{"orgs", "show"}, http.MethodGet, "/api/v1/orgs/", 5},
		{[]string{"admin", "orgs", "members", "add", "acme"}, http.MethodPut, "/api/v1/orgs/acme/members/", 7},
	}
	args := []string{"a/b", "a+b@example.com", "a b", "x?y#z", "0f8fad5b-d9cb-469f-a165-70867728950e"}
	for _, cmd := range commands {
		for _, arg := range args {
			t.Run(strings.Join(cmd.words, " ")+" "+arg, func(t *testing.T) {
				hub := newStubHub(t).answer(cmd.method, cmd.prefix+arg, http.StatusOK, "{}")

				code, _, stderr := runAgainst(t, hub, append(slices.Clone(cmd.words), arg)...)

				if code != 0 {
					t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
				}
				sent := hub.recorded()
				if len(sent) != 1 {
					t.Fatalf("the hub got %d requests, want 1", len(sent))
				}
				segs := strings.Split(sent[0].rawPath, "/")
				if len(segs) != cmd.segments {
					t.Fatalf("path %q has %d segments, want %d", sent[0].rawPath, len(segs), cmd.segments)
				}
				last := segs[len(segs)-1]
				if got, err := url.PathUnescape(last); err != nil || got != arg {
					t.Errorf("last segment %q decodes to %q (%v), want %q", last, got, err, arg)
				}
				if arg == "a/b" && last != "a%2Fb" {
					t.Errorf("last segment = %q, want %q", last, "a%2Fb")
				}
			})
		}
	}
}
