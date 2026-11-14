package workspace_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestResolveOriginCredential_ReadsTheWorkspaceSecretsOnEveryCall(t *testing.T) {
	w := newWorld(t)
	tokenAuth := func(tok string) workspace.Credential {
		return workspace.Credential{Username: "x-token-auth", Password: tok}
	}
	cases := []struct {
		name    string
		secrets map[string]string
		want    workspace.Credential
	}{
		{"a token", map[string]string{"GIT_PAT": "t"}, tokenAuth("t")},
		{"a token before a user name and password", map[string]string{"GIT_PAT": "t", "GIT_USERNAME": "u", "GIT_PASSWORD": "p"}, tokenAuth("t")},
		{"a user name and password", map[string]string{"GIT_USERNAME": "u", "GIT_PASSWORD": "p"}, workspace.Credential{Username: "u", Password: "p"}},
		{"an empty token and a user name and password", map[string]string{"GIT_PAT": "", "GIT_USERNAME": "u", "GIT_PASSWORD": "p"}, workspace.Credential{Username: "u", Password: "p"}},
		{"a user name alone", map[string]string{"GIT_USERNAME": "u"}, workspace.Credential{}},
		{"a password alone", map[string]string{"GIT_PASSWORD": "p"}, workspace.Credential{}},
		{"an empty token", map[string]string{"GIT_PAT": ""}, workspace.Credential{}},
		{"no secret", nil, workspace.Credential{}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slug := "ws-" + string(rune('a'+i))
			for name, value := range c.secrets {
				w.putSecret(t, slug, name, value)
			}
			got, err := workspace.ResolveOriginCredential(context.Background(), w.secrets, slug)
			if err != nil || got != c.want {
				t.Errorf("ResolveOriginCredential = %+v, %v; want %+v", got, err, c.want)
			}
		})
	}

	t.Run("a changed token at the next call", func(t *testing.T) {
		w.putSecret(t, "rotated", "GIT_PAT", "t")
		if got, err := workspace.ResolveOriginCredential(context.Background(), w.secrets, "rotated"); err != nil || got != tokenAuth("t") {
			t.Fatalf("before the change = %+v, %v; want the token t", got, err)
		}
		w.putSecret(t, "rotated", "GIT_PAT", "t2")
		if got, err := workspace.ResolveOriginCredential(context.Background(), w.secrets, "rotated"); err != nil || got != tokenAuth("t2") {
			t.Errorf("after the change = %+v, %v; want the token t2", got, err)
		}
	})
}

func TestResolveOriginCredential_ReturnsAReadErrorNeverNoCredential(t *testing.T) {
	broken := errors.New("the secret store cannot be read")
	cases := []struct {
		name     string
		failName string
	}{
		{"the token cannot be read", "GIT_PAT"},
		{"the user name cannot be read after no token", "GIT_USERNAME"},
		{"the password cannot be read after a user name", "GIT_PASSWORD"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reader := failingReader{fail: c.failName, err: broken, values: map[string]string{"GIT_USERNAME": "u", "GIT_PASSWORD": "p"}}
			got, err := workspace.ResolveOriginCredential(context.Background(), reader, "acme")
			if !errors.Is(err, broken) {
				t.Errorf("err = %v, want the read error", err)
			}
			if got != (workspace.Credential{}) {
				t.Errorf("credential = %+v, want none", got)
			}
		})
	}
}
