package forge_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestParseRemote_DerivesHostOwnerAndName(t *testing.T) {
	t.Parallel()
	widgets := forge.Repository{Host: "github.com", Owner: "acme", Name: "widgets"}
	nested := forge.Repository{Host: "gitlab.com", Owner: "group/subgroup", Name: "proj"}
	cases := []struct {
		url  string
		want forge.Repository // the zero value for a URL naming no repository
	}{
		{"https://github.com/acme/widgets.git", widgets},
		{"http://GitHub.com/acme/widgets", widgets},
		{"https://www.github.com:8443/acme/widgets.git", widgets},
		{"https://user:pw@gitlab.example.com/group/subgroup/proj.git",
			forge.Repository{Host: "gitlab.example.com", Owner: "group/subgroup", Name: "proj"}},
		{"ssh://git@github.com:22/acme/widgets.git", widgets},
		{"git://github.com/acme/widgets", widgets},
		{"git@github.com:acme/widgets.git", widgets},
		{"git@gitlab.com:group/subgroup/proj", nested},
		{"https://github.com/acme/widgets.git.git", forge.Repository{Host: "github.com", Owner: "acme", Name: "widgets.git"}},
		{"https://github.com//acme//widgets/", widgets},
		{"", forge.Repository{}},
		{"https://github.com/widgets", forge.Repository{}},
		{"https://github.com//widgets/", forge.Repository{}},
		{"git@github.com:widgets.git", forge.Repository{}},
		{"https://github.com/acme/.git", forge.Repository{}},
		{"ftp://github.com/acme/widgets", forge.Repository{}},
		{"/srv/git/acme/widgets.git", forge.Repository{}},
		{"https:///acme/widgets", forge.Repository{}},
		{"github.com/acme/widgets", forge.Repository{}},
	}
	for _, c := range cases {
		got, ok := forge.ParseRemote(c.url)
		if got != c.want || ok != (c.want != forge.Repository{}) {
			t.Errorf("ParseRemote(%q) = %+v, %v, want %+v", c.url, got, ok, c.want)
		}
	}
}

func TestKind_PrefersASettingThatNamesOneForgeThenTheHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		setting, host string
		want          forge.ForgeKind
	}{
		{"", "github.com", forge.KindGitHub},
		{"", "gitlab.example.com", forge.KindGitLab},
		{"https://code.example.com/gitlab", "github.com", forge.KindGitLab},
		{"https://GitHub.example.com", "code.example.com", forge.KindGitHub},
		{"https://api.example.com", "gitlab.com", forge.KindGitLab},
		{"https://gitlab.example.com/github-mirror", "github.com", forge.KindGitHub},
		{"", "code.example.com", forge.KindUnknown},
		{"https://api.example.com", "github-gitlab.example.com", forge.KindUnknown},
	}
	for _, c := range cases {
		if got := forge.Kind(c.setting, c.host); got != c.want {
			t.Errorf("Kind(%q, %q) = %q, want %q", c.setting, c.host, got, c.want)
		}
	}
}

func TestAPIURL_UsesTheSettingElseTheForgesDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind          forge.ForgeKind
		setting, host string
		want          string
	}{
		{forge.KindGitHub, "", "github.com", "https://api.github.com"},
		{forge.KindGitHub, "", "github.example.com", "https://github.example.com/api/v3"},
		{forge.KindGitLab, "", "gitlab.com", "https://gitlab.com/api/v4"},
		{forge.KindGitHub, "https://ghe.example.com/api/v3/", "any", "https://ghe.example.com/api/v3"},
		{forge.KindGitLab, "https://gitlab.example.com", "any", "https://gitlab.example.com/api/v4"},
		{forge.KindGitLab, "https://gitlab.example.com/api/v4/", "any", "https://gitlab.example.com/api/v4"},
		{forge.KindGitLab, "https://code.example.com/gitlab", "any", "https://code.example.com/gitlab/api/v4"},
	}
	for _, c := range cases {
		if got := forge.APIURL(c.kind, c.setting, c.host); got != c.want {
			t.Errorf("APIURL(%q, %q, %q) = %q, want %q", c.kind, c.setting, c.host, got, c.want)
		}
	}
}

// secretMap is a workspace.SecretReader over the secrets of the workspace
// "w": a name in fail answers an error instead of its value.
type secretMap struct {
	values map[string]string
	fail   map[string]bool
}

var errUnreadable = errors.New("no key opens the secret")

func (s secretMap) Get(_ context.Context, scope secret.Scope, scopeID, name string) (string, bool, error) {
	if scope != secret.ScopeWorkspace || scopeID != "w" {
		return "", false, nil
	}
	if s.fail[name] {
		return "", true, errUnreadable
	}
	v, ok := s.values[name]
	return v, ok, nil
}

func TestResolveCredential_FollowsTheSecretsOrderOfTheTarget(t *testing.T) {
	t.Parallel()
	type secrets = map[string]string
	pat := func(token string) forge.Credential { return forge.Credential{Kind: forge.CredentialPAT, Token: token} }
	basic := func(token string) forge.Credential {
		return forge.Credential{Kind: forge.CredentialBasic, Token: token}
	}
	none := forge.Credential{Kind: forge.CredentialNone}
	cases := []struct {
		target  forge.Target
		secrets secrets
		fail    string // a secret that cannot be read, which fails the call
		want    forge.Credential
	}{
		{forge.TargetOrigin, secrets{"GIT_PAT": "t1"}, "", pat("t1")},
		{forge.TargetOrigin, secrets{"GIT_PAT": "t1", "GIT_USERNAME": "u", "GIT_PASSWORD": "p"}, "", pat("t1")},
		{forge.TargetOrigin, secrets{"GIT_USERNAME": "u", "GIT_PASSWORD": "p"}, "", basic("p")},
		{forge.TargetOrigin, secrets{"GIT_USERNAME": "u"}, "", none},
		{forge.TargetOrigin, secrets{"GIT_PAT": "", "GIT_USERNAME": "u", "GIT_PASSWORD": "p"}, "", basic("p")},
		{forge.TargetOrigin, secrets{"GIT_PAT": ""}, "", none},
		{forge.TargetOrigin, secrets{}, "", none},
		{forge.TargetOrigin, secrets{"UPSTREAM_GIT_PAT": "u1"}, "", none},
		{forge.TargetUpstream, secrets{"UPSTREAM_GIT_PAT": "u1", "GIT_PAT": "o1"}, "", pat("u1")},
		{forge.TargetUpstream, secrets{"UPSTREAM_GIT_USERNAME": "u", "UPSTREAM_GIT_PASSWORD": "up", "GIT_PAT": "o1"}, "", basic("up")},
		{forge.TargetUpstream, secrets{"UPSTREAM_GIT_USERNAME": "u", "GIT_PAT": "o1"}, "", pat("o1")},
		{forge.TargetUpstream, secrets{"UPSTREAM_GIT_PAT": "", "GIT_PAT": "o1"}, "", pat("o1")},
		{forge.TargetUpstream, secrets{"GIT_USERNAME": "u", "GIT_PASSWORD": "p"}, "", basic("p")},
		{forge.TargetUpstream, secrets{}, "", none},
		// A secret late in the order that cannot be read is an error too,
		// never no credential.
		{forge.TargetUpstream, secrets{"GIT_USERNAME": "u"}, "GIT_PASSWORD", forge.Credential{}},
	}
	for _, c := range cases {
		got, err := forge.ResolveCredential(t.Context(), secretMap{values: c.secrets, fail: map[string]bool{c.fail: true}}, "w", c.target)
		if got != c.want || (c.fail == "") != (err == nil) || c.fail != "" && !errors.Is(err, errUnreadable) {
			t.Errorf("ResolveCredential(%s) over %v with %q unreadable = %+v, %v, want %+v", c.target, c.secrets, c.fail, got, err, c.want)
		}
	}
}

func TestResolveCredential_ReadsTheSecretsOnEveryCall(t *testing.T) {
	t.Parallel()
	reader := secretMap{values: map[string]string{"GIT_PAT": "t1"}}
	for _, want := range []string{"t1", "t2"} {
		reader.values["GIT_PAT"] = want
		got, err := forge.ResolveCredential(t.Context(), reader, "w", forge.TargetOrigin)
		if err != nil || got.Token != want {
			t.Fatalf("ResolveCredential after GIT_PAT became %q = %+v, %v", want, got, err)
		}
	}
}

func TestResolve_DerivesTheTargetsRepositoryForgeAPIURLAndCredential(t *testing.T) {
	t.Parallel()
	reader := secretMap{values: map[string]string{"GIT_PAT": "o1", "UPSTREAM_GIT_PAT": "u1"}}
	ws := workspace.Workspace{Slug: "w", GitURL: new("https://github.com/acme/widgets.git"), UpstreamURL: new("git@code.example.com:up/proj.git")}
	cases := []struct {
		name     string
		target   forge.Target
		settings forge.Settings
		want     forge.Resolution
	}{
		{"origin on github.com", forge.TargetOrigin, forge.Settings{},
			forge.Resolution{Target: forge.TargetOrigin, Repository: forge.Repository{Host: "github.com", Owner: "acme", Name: "widgets"},
				Kind: forge.KindGitHub, APIURL: "https://api.github.com", Credential: forge.Credential{Kind: forge.CredentialPAT, Token: "o1"}}},
		{"upstream on a GitLab its setting names", forge.TargetUpstream,
			forge.Settings{APIURL: "http://ignored", UpstreamAPIURL: "https://code.example.com/gitlab/"},
			forge.Resolution{Target: forge.TargetUpstream, Repository: forge.Repository{Host: "code.example.com", Owner: "up", Name: "proj"},
				Kind: forge.KindGitLab, APIURL: "https://code.example.com/gitlab/api/v4", Credential: forge.Credential{Kind: forge.CredentialPAT, Token: "u1"}}},
	}
	for _, c := range cases {
		if got, err := forge.Resolve(t.Context(), reader, ws, c.target, c.settings); err != nil || got != c.want {
			t.Errorf("%s: Resolve = %+v, %v, want %+v", c.name, got, err, c.want)
		}
	}
}
