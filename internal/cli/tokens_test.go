package cli

import (
	"net/http"
	"strings"
	"testing"
)

// mintedToken is a Token as the hub answers its creation, with its value.
const mintedToken = `{"token":"parley_pat_k3x9q2mz_0123456789abcdefghijklmnopqrstuv","token_id":"k3x9q2mz","name":"ci",` +
	`"permissions":["git:read","changes:write"],"paths":null,"workspace":null,` +
	`"expires_at":"2027-02-11T10:00:00Z","revoked_at":null,"created_at":"2026-11-13T10:00:00Z"}`

// tokenRevokedAt is a Token after a change of its permissions, revoked at
// revokedAt, a JSON value.
func tokenRevokedAt(revokedAt string) string {
	return `{"token_id":"k3x9q2mz","name":"ci","permissions":[],"paths":null,"workspace":null,` +
		`"expires_at":null,"revoked_at":` + revokedAt + `,"created_at":"2026-11-13T10:00:00Z"}`
}

// The test sets environment variables, so it must not run in parallel.
func TestTokensCreate_SendsTheTokenAndWarnsItsValueIsShownOnce(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		body  string
	}{
		{"with every flag",
			[]string{"--name", "ci", "--permissions", "git:read,changes:write", "--paths", "docs/**,src/*.go", "--paths", "README.md", "--expires", "30"},
			`{"name":"ci","permissions":["git:read","changes:write"],"paths":["docs/**","src/*.go","README.md"],"expires":30}`},
		{"without --paths and --expires",
			[]string{"--name", "ci", "--permissions", "git:read"},
			`{"name":"ci","permissions":["git:read"],"expires":90}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newStubHub(t).answer(http.MethodPost, "/api/v1/user/tokens", http.StatusCreated, mintedToken)

			code, stdout, stderr := runAgainst(t, hub, append([]string{"tokens", "create"}, tt.flags...)...)

			if code != 0 {
				t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
			}
			if want := indented(t, mintedToken); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if !strings.Contains(stderr, showOnceWarning) {
				t.Errorf("stderr = %q, want the show-once warning", stderr)
			}
			sent := hub.recorded()
			if len(sent) != 1 {
				t.Fatalf("the hub got %d requests, want 1", len(sent))
			}
			if sent[0].method != http.MethodPost || sent[0].rawPath != "/api/v1/user/tokens" {
				t.Errorf("sent %s %s, want POST /api/v1/user/tokens", sent[0].method, sent[0].rawPath)
			}
			if ct := sent[0].header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			wantJSONBody(t, sent[0].body, tt.body)
		})
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestKeyAndTokenCommands_SendOneRequestAndPrintTheAnswer(t *testing.T) {
	tests := []struct {
		args []string
		want sentRequest
	}{
		{[]string{"keys", "list"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/user/keys"}},
		{[]string{"tokens", "list"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/user/tokens"}},
		{[]string{"tokens", "show", "k3x9q2mz"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/user/tokens/k3x9q2mz"}},
		{[]string{"tokens", "revoke", "k3x9q2mz"}, sentRequest{method: http.MethodDelete, rawPath: "/api/v1/user/tokens/k3x9q2mz"}},
		{[]string{"tokens", "replace", "k3x9q2mz", "--permissions", ""},
			sentRequest{method: http.MethodPut, rawPath: "/api/v1/user/tokens/k3x9q2mz/permissions", body: `{"permissions":[]}`}},
		{[]string{"admin", "keys", "list", "alice"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/users/alice/keys"}},
		{[]string{"admin", "keys", "list", "a/b"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/users/a%2Fb/keys"}},
		{[]string{"admin", "keys", "revoke", "alice", "Ab3dE6gH"}, sentRequest{method: http.MethodDelete, rawPath: "/api/v1/users/alice/keys/Ab3dE6gH"}},
		{[]string{"admin", "keys", "revoke", "a/b", "k?1"}, sentRequest{method: http.MethodDelete, rawPath: "/api/v1/users/a%2Fb/keys/k%3F1"}},
		{[]string{"admin", "tokens", "list", "alice"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/users/alice/tokens"}},
		{[]string{"admin", "tokens", "list", "a/b"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/users/a%2Fb/tokens"}},
		{[]string{"admin", "tokens", "revoke", "alice", "k3x9q2mz"}, sentRequest{method: http.MethodDelete, rawPath: "/api/v1/users/alice/tokens/k3x9q2mz"}},
		{[]string{"admin", "tokens", "revoke", "a/b", "k?1"}, sentRequest{method: http.MethodDelete, rawPath: "/api/v1/users/a%2Fb/tokens/k%3F1"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			wantSends(t, tt.args, tt.want)
		})
	}
}

// The test sets environment variables, so it must not run in parallel.
func TestTokensPermissions_SendTheListAndNoteARevocation(t *testing.T) {
	commands := []struct {
		name, method string
		// notes tells whether the command notes a revocation the change
		// caused.
		notes bool
	}{
		{"replace", http.MethodPut, true},
		{"add", http.MethodPatch, false},
		{"remove", http.MethodDelete, true},
	}
	for _, cmd := range commands {
		for _, revokedAt := range []string{"null", `"2026-11-13T11:00:00Z"`} {
			t.Run(cmd.name+" revoked_at "+revokedAt, func(t *testing.T) {
				answer := tokenRevokedAt(revokedAt)
				hub := newStubHub(t).answer(cmd.method, "/api/v1/user/tokens/k3x9q2mz/permissions", http.StatusOK, answer)

				code, stdout, stderr := runAgainst(t, hub, "tokens", cmd.name, "k3x9q2mz", "--permissions", "git:read,vars:read")

				if code != 0 {
					t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
				}
				if want := indented(t, answer); stdout != want {
					t.Errorf("stdout = %q, want %q", stdout, want)
				}
				noted := strings.Contains(stderr, "the token was revoked")
				if want := cmd.notes && revokedAt != "null"; noted != want {
					t.Errorf("stderr = %q, want a note of the revocation: %v", stderr, want)
				}
				sent := hub.recorded()
				if len(sent) != 1 {
					t.Fatalf("the hub got %d requests, want 1", len(sent))
				}
				if sent[0].method != cmd.method || sent[0].rawPath != "/api/v1/user/tokens/k3x9q2mz/permissions" {
					t.Errorf("sent %s %s, want %s /api/v1/user/tokens/k3x9q2mz/permissions", sent[0].method, sent[0].rawPath, cmd.method)
				}
				wantJSONBody(t, sent[0].body, `{"permissions":["git:read","vars:read"]}`)
			})
		}
	}
}
