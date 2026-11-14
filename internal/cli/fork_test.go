package cli

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// forksPath is the route parley fork create posts to and parley fork list
// reads for the workspace demo, as sent.
const forksPath = "/api/v1/workspaces/demo/forks"

// forkCommand is parley fork sub followed by args.
func forkCommand(sub string, args ...string) []string {
	return append([]string{"fork", sub}, args...)
}

// The tests of this file set environment variables, so they must not run in
// parallel.

func TestForkCreate_FlagsToBody(t *testing.T) {
	const session = "6f1c2a3b-4d5e-4f60-8a9b-0c1d2e3f4a5b"
	tests := []struct {
		args          []string
		rawPath, body string
	}{
		{[]string{"demo"}, forksPath, `{}`},
		{
			[]string{"demo", "--name", "t1", "--ref", "release", "--ttl", "2h", "--purpose", "fix", "--session", session},
			forksPath, `{"name":"t1","ref":"release","ttl":"2h","purpose":"fix","session_id":"` + session + `"}`,
		},
		{[]string{"demo", "--no-auto-change"}, forksPath, `{"auto_change":false}`},
		{
			[]string{"demo", "--token-permissions", "git:read,git:write", "--token-paths", "src/**,docs/**"},
			forksPath, `{"token":{"permissions":["git:read","git:write"],"paths":["src/**","docs/**"]}}`,
		},
		{[]string{"demo", "--token-permissions", "git:read"}, forksPath, `{"token":{"permissions":["git:read"]}}`},
		{
			[]string{"demo", "--sandbox-profile", "default", "--", "make", "test"},
			forksPath, `{"sandbox":{"profile":"default","command":["make","test"]}}`,
		},
		{[]string{"demo", "--sandbox-profile", "default"}, forksPath, `{"sandbox":{"profile":"default"}}`},
		{[]string{"demo", "--name", ""}, forksPath, `{"name":""}`},
		{[]string{"a/b"}, "/api/v1/workspaces/a%2Fb/forks", `{}`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			wantSends(t, forkCommand("create", tt.args...), sentRequest{http.MethodPost, tt.rawPath, "", tt.body})
		})
	}
}

func TestForkCreate_WarnsAboutSecret(t *testing.T) {
	const secret = "parley_pat_SECRET"
	withToken := `{"workspace":{"slug":"demo--t1"},"remote":null,"token":{"token_id":"k","token":"` + secret + `"}}`
	withoutToken := `{"workspace":{"slug":"demo--t1"},"remote":null}`
	tests := []struct {
		name, answer string
		args         []string
		warns        bool
	}{
		{"a token with its secret", withToken, []string{"demo", "--token-permissions", "git:read"}, true},
		{"no token", withoutToken, []string{"demo"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newStubHub(t).answer(http.MethodPost, forksPath, http.StatusCreated, tt.answer)

			code, stdout, stderr := runAgainst(t, hub, forkCommand("create", tt.args...)...)

			if code != 0 {
				t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
			}
			if want := indented(t, tt.answer); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			switch {
			case tt.warns && (stderr == "" || strings.Contains(stderr, secret)):
				t.Errorf("stderr = %q, want a warning without the secret", stderr)
			case !tt.warns && stderr != "":
				t.Errorf("stderr = %q, want it empty", stderr)
			}
		})
	}
}

func TestForkCreate_UsageErrors(t *testing.T) {
	tests := []struct {
		args []string
		// says is what the message must name.
		says string
	}{
		{[]string{"demo", "--token-paths", "src/**"}, "--token-permissions"},
		{[]string{"demo", "--", "make"}, "--sandbox-profile"},
		{[]string{}, "<parent>"},
		{[]string{"demo", "extra"}, `"extra"`},
		{[]string{"--sandbox-profile", "default", "--", "make"}, "<parent>"},
		{[]string{"demo", "extra", "--sandbox-profile", "default", "--", "make"}, `"extra"`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			hub := newStubHub(t)

			code, stdout, stderr := runAgainst(t, hub, forkCommand("create", tt.args...)...)

			if message := wantClientFailure(t, code, stdout, stderr, "usage_error"); !strings.Contains(message, tt.says) {
				t.Errorf("message = %q, want it to name %s", message, tt.says)
			}
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}

func TestForkList_FlagsToQuery(t *testing.T) {
	const session = "6f1c2a3b-4d5e-4f60-8a9b-0c1d2e3f4a5b"
	tests := []struct {
		args           []string
		rawPath, query string
	}{
		{[]string{"demo"}, forksPath, ""},
		{[]string{"demo", "--mine"}, forksPath, "owner=me"},
		{[]string{"demo", "--session", session, "--limit", "10", "--cursor", "abc"}, forksPath, "cursor=abc&limit=10&session_id=" + session},
		{[]string{"a/b"}, "/api/v1/workspaces/a%2Fb/forks", ""},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			wantSends(t, forkCommand("list", tt.args...), sentRequest{http.MethodGet, tt.rawPath, tt.query, ""})
		})
	}

	t.Run("a refusal of the hub", func(t *testing.T) {
		hub := newStubHub(t)

		code, stdout, stderr := runAgainst(t, hub, forkCommand("list", "nope")...)

		var env struct {
			Error struct {
				ErrorType string `json:"error_type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(stderr), &env); code != 1 || err != nil || env.Error.ErrorType != "not_found" || stdout != "" {
			t.Errorf("exit status %d, stdout %q, stderr %q; want 1, nothing and the hub's not_found", code, stdout, stderr)
		}
		if sent := hub.recorded(); len(sent) != 1 || sent[0].rawPath != "/api/v1/workspaces/nope/forks" {
			t.Errorf("the hub got %v, want one GET /api/v1/workspaces/nope/forks", sent)
		}
	})
}

func TestFork_WithoutACommandPrintsItsHelp(t *testing.T) {
	code, stdout, stderr := runCLI(t, "fork")

	if code != 0 || stderr != "" {
		t.Errorf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
	}
	for _, sub := range []string{"create", "list"} {
		if !slices.ContainsFunc(nonEmptyLines(stdout), func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), sub+" ") }) {
			t.Errorf("stdout does not list %s:\n%s", sub, stdout)
		}
	}
}
