package carrypatch_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/secret"
)

// secretMap is a workspace.SecretReader over the secrets of one workspace:
// values holds what is stored, and a name in fail answers err instead.
type secretMap struct {
	slug   string
	values map[string]string
	fail   map[string]bool
	err    error
}

func (s secretMap) Get(_ context.Context, scope secret.Scope, scopeID, name string) (string, bool, error) {
	if scope != secret.ScopeWorkspace || scopeID != s.slug {
		return "", false, nil
	}
	if s.fail[name] {
		return "", true, s.err
	}
	v, ok := s.values[name]
	return v, ok, nil
}

func TestResolveUpstreamCredential_Order(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		secrets map[string]string
		want    gitrepo.Credential
	}{
		{"the upstream token before everything else",
			map[string]string{"UPSTREAM_GIT_PAT": "t", "UPSTREAM_GIT_USERNAME": "u", "UPSTREAM_GIT_PASSWORD": "p", "GIT_PAT": "o"},
			gitrepo.Credential{Username: "x-token-auth", Password: "t"}},
		{"an empty upstream token falls to the upstream user name and password",
			map[string]string{"UPSTREAM_GIT_PAT": "", "UPSTREAM_GIT_USERNAME": "u", "UPSTREAM_GIT_PASSWORD": "p"},
			gitrepo.Credential{Username: "u", Password: "p"}},
		{"an upstream user name without a password falls to the origin token",
			map[string]string{"UPSTREAM_GIT_USERNAME": "u", "GIT_PAT": "o"},
			gitrepo.Credential{Username: "x-token-auth", Password: "o"}},
		{"an empty upstream password falls to the origin user name and password",
			map[string]string{"UPSTREAM_GIT_USERNAME": "u", "UPSTREAM_GIT_PASSWORD": "", "GIT_USERNAME": "a", "GIT_PASSWORD": "b"},
			gitrepo.Credential{Username: "a", Password: "b"}},
		{"an upstream password without a user name falls to the origin token",
			map[string]string{"UPSTREAM_GIT_PASSWORD": "p", "GIT_PAT": "o"},
			gitrepo.Credential{Username: "x-token-auth", Password: "o"}},
		{"an upstream user name alone is no credential",
			map[string]string{"UPSTREAM_GIT_USERNAME": "u"},
			gitrepo.Credential{}},
		{"no secret is no credential", nil, gitrepo.Credential{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			reader := secretMap{slug: "acme", values: c.secrets}
			got, err := carrypatch.ResolveUpstreamCredential(context.Background(), reader, "acme")
			if err != nil || got != c.want {
				t.Errorf("ResolveUpstreamCredential = %+v, %v; want %+v", got, err, c.want)
			}
		})
	}

	t.Run("a changed upstream token counts at the next call", func(t *testing.T) {
		t.Parallel()
		values := map[string]string{"UPSTREAM_GIT_PAT": "t1"}
		reader := secretMap{slug: "acme", values: values}
		if got, err := carrypatch.ResolveUpstreamCredential(context.Background(), reader, "acme"); err != nil || got.Password != "t1" {
			t.Fatalf("before the change = %+v, %v; want the token t1", got, err)
		}
		values["UPSTREAM_GIT_PAT"] = "t2"
		if got, err := carrypatch.ResolveUpstreamCredential(context.Background(), reader, "acme"); err != nil || got.Password != "t2" {
			t.Errorf("after the change = %+v, %v; want the token t2", got, err)
		}
	})
}

func TestResolveUpstreamCredential_ReadErrorIsAnError(t *testing.T) {
	t.Parallel()
	broken := errors.New("the secret store cannot be read")
	all := map[string]bool{
		"UPSTREAM_GIT_PAT": true, "UPSTREAM_GIT_USERNAME": true, "UPSTREAM_GIT_PASSWORD": true,
		"GIT_PAT": true, "GIT_USERNAME": true, "GIT_PASSWORD": true,
	}
	cases := []struct {
		name string
		fail map[string]bool
	}{
		{"no secret can be read", all},
		{"the upstream token cannot be read", map[string]bool{"UPSTREAM_GIT_PAT": true}},
		{"the upstream user name cannot be read", map[string]bool{"UPSTREAM_GIT_USERNAME": true}},
		{"the upstream password cannot be read", map[string]bool{"UPSTREAM_GIT_PASSWORD": true}},
		{"the origin token cannot be read", map[string]bool{"GIT_PAT": true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			reader := secretMap{slug: "acme", values: map[string]string{"UPSTREAM_GIT_USERNAME": "u"}, fail: c.fail, err: broken}
			got, err := carrypatch.ResolveUpstreamCredential(context.Background(), reader, "acme")
			if !errors.Is(err, broken) {
				t.Errorf("err = %v, want the read error", err)
			}
			if got != (gitrepo.Credential{}) {
				t.Errorf("credential = %+v, want none", got)
			}
		})
	}
}
