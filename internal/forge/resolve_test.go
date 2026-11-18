package forge_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestParseRemote_DerivesHostOwnerAndName(t *testing.T) {
	t.Parallel()
	widgets := forge.Repository{Host: "github.com", Owner: "acme", Name: "widgets"}
	cases := []struct {
		url  string
		want forge.Repository
	}{
		{"https://github.com/acme/widgets.git", widgets},
		{"http://GitHub.com/acme/widgets", widgets},
		{"https://www.github.com:8443/acme/widgets.git", widgets},
		{"https://user:pw@gitlab.example.com/group/subgroup/proj.git",
			forge.Repository{Host: "gitlab.example.com", Owner: "group/subgroup", Name: "proj"}},
		{"ssh://git@github.com:22/acme/widgets.git", widgets},
		{"git://github.com/acme/widgets", widgets},
		{"git@github.com:acme/widgets.git", widgets},
		{"git@gitlab.com:group/subgroup/proj", forge.Repository{Host: "gitlab.com", Owner: "group/subgroup", Name: "proj"}},
		{"https://github.com/acme/widgets.git.git", forge.Repository{Host: "github.com", Owner: "acme", Name: "widgets.git"}},
		{"https://github.com//acme//widgets/", widgets},
	}
	for _, c := range cases {
		got, ok := forge.ParseRemote(c.url)
		if !ok || got != c.want {
			t.Errorf("ParseRemote(%q) = %+v, %v, want %+v, true", c.url, got, ok, c.want)
		}
	}
}

func TestParseRemote_RefusesAURLNamingNoRepository(t *testing.T) {
	t.Parallel()
	for _, url := range []string{
		"", "https://github.com/widgets", "https://github.com//widgets/", "git@github.com:widgets.git",
		"https://github.com/acme/.git", "ftp://github.com/acme/widgets", "/srv/git/acme/widgets.git",
		"https:///acme/widgets", "github.com/acme/widgets",
	} {
		if got, ok := forge.ParseRemote(url); ok {
			t.Errorf("ParseRemote(%q) = %+v, true, want no repository", url, got)
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

func TestCheckAPIURL_AcceptsOnlyAnAbsoluteHTTPSURLWithAHost(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", "https://ghe.example.com", "https://ghe.example.com:8443/api/v3/"} {
		if err := forge.CheckAPIURL(ok); err != nil {
			t.Errorf("CheckAPIURL(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"http://ghe.example.com", "ghe.example.com/api", "https://", "https://u:p@ghe.example.com",
		"https://ghe.example.com/api?x=1", "https://ghe.example.com/api#f", "https://ghe.example.com:0",
		"https://ghe.example.com/a b",
	} {
		if forge.CheckAPIURL(bad) == nil {
			t.Errorf("CheckAPIURL(%q) = nil, want an error", bad)
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
	basic := func(token string) forge.Credential { return forge.Credential{Kind: forge.CredentialBasic, Token: token} }
	none := forge.Credential{Kind: forge.CredentialNone}
	cases := []struct {
		target  forge.Target
		secrets secrets
		want    forge.Credential
	}{
		{forge.TargetOrigin, secrets{"GIT_PAT": "t1"}, pat("t1")},
		{forge.TargetOrigin, secrets{"GIT_PAT": "t1", "GIT_USERNAME": "u", "GIT_PASSWORD": "p"}, pat("t1")},
		{forge.TargetOrigin, secrets{"GIT_USERNAME": "u", "GIT_PASSWORD": "p"}, basic("p")},
		{forge.TargetOrigin, secrets{"GIT_USERNAME": "u"}, none},
		{forge.TargetOrigin, secrets{"GIT_PAT": "", "GIT_USERNAME": "u", "GIT_PASSWORD": "p"}, basic("p")},
		{forge.TargetOrigin, secrets{"GIT_PAT": ""}, none},
		{forge.TargetOrigin, secrets{}, none},
		{forge.TargetOrigin, secrets{"UPSTREAM_GIT_PAT": "u1"}, none},
		{forge.TargetUpstream, secrets{"UPSTREAM_GIT_PAT": "u1", "GIT_PAT": "o1"}, pat("u1")},
		{forge.TargetUpstream, secrets{"UPSTREAM_GIT_USERNAME": "u", "UPSTREAM_GIT_PASSWORD": "up", "GIT_PAT": "o1"}, basic("up")},
		{forge.TargetUpstream, secrets{"UPSTREAM_GIT_USERNAME": "u", "GIT_PAT": "o1"}, pat("o1")},
		{forge.TargetUpstream, secrets{"UPSTREAM_GIT_PAT": "", "GIT_PAT": "o1"}, pat("o1")},
		{forge.TargetUpstream, secrets{"GIT_USERNAME": "u", "GIT_PASSWORD": "p"}, basic("p")},
		{forge.TargetUpstream, secrets{}, none},
	}
	for _, c := range cases {
		got, err := forge.ResolveCredential(t.Context(), secretMap{values: c.secrets}, "w", c.target)
		if err != nil || got != c.want {
			t.Errorf("ResolveCredential(%s) over %v = %+v, %v, want %+v", c.target, c.secrets, got, err, c.want)
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

func TestResolveCredential_FailsOnASecretItCannotRead(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"GIT_PAT", "GIT_PASSWORD", "UPSTREAM_GIT_PAT", "UPSTREAM_GIT_USERNAME"} {
		reader := secretMap{values: map[string]string{"GIT_USERNAME": "u"}, fail: map[string]bool{name: true}}
		got, err := forge.ResolveCredential(t.Context(), reader, "w", forge.TargetUpstream)
		if !errors.Is(err, errUnreadable) || got != (forge.Credential{}) {
			t.Errorf("ResolveCredential with %s unreadable = %+v, %v, want the read error", name, got, err)
		}
	}
}

// The hub's own environment never stands in for a workspace's secrets.
func TestResolveCredential_IgnoresTheProcessEnvironment(t *testing.T) {
	t.Setenv("GIT_PAT", "from-env")
	t.Setenv("UPSTREAM_GIT_PAT", "from-env")
	got, err := forge.ResolveCredential(t.Context(), secretMap{}, "w", forge.TargetUpstream)
	if err != nil || got != (forge.Credential{Kind: forge.CredentialNone}) {
		t.Fatalf("ResolveCredential with tokens only in the environment = %+v, %v, want none", got, err)
	}
}

func TestResolve_DerivesTheTargetsForgeOrNamesWhatToSet(t *testing.T) {
	t.Parallel()
	reader := secretMap{values: map[string]string{"GIT_PAT": "o1", "UPSTREAM_GIT_PAT": "u1"}}
	ws := func(gitURL, upstreamURL string) workspace.Workspace {
		return workspace.Workspace{Slug: "w", GitURL: &gitURL, UpstreamURL: &upstreamURL}
	}
	cases := []struct {
		name     string
		ws       workspace.Workspace
		target   forge.Target
		settings forge.Settings
		want     forge.Resolution
		errType  string // a refusal instead of want
		errHas   string // in the refusal's message
	}{
		{name: "origin on github.com", ws: ws("https://github.com/acme/widgets.git", ""), target: forge.TargetOrigin,
			want: forge.Resolution{Target: forge.TargetOrigin, Repository: forge.Repository{Host: "github.com", Owner: "acme", Name: "widgets"},
				Kind: forge.KindGitHub, APIURL: "https://api.github.com", Credential: forge.Credential{Kind: forge.CredentialPAT, Token: "o1"}}},
		{name: "upstream on a GitLab its setting names", ws: ws("https://github.com/acme/widgets.git", "git@code.example.com:up/proj.git"),
			target: forge.TargetUpstream, settings: forge.Settings{APIURL: "http://ignored", UpstreamAPIURL: "https://code.example.com/gitlab/"},
			want: forge.Resolution{Target: forge.TargetUpstream, Repository: forge.Repository{Host: "code.example.com", Owner: "up", Name: "proj"},
				Kind: forge.KindGitLab, APIURL: "https://code.example.com/gitlab/api/v4", Credential: forge.Credential{Kind: forge.CredentialPAT, Token: "u1"}}},
		{name: "an invalid setting before a URL naming no repository", ws: ws("https://github.com/widgets", ""), target: forge.TargetOrigin,
			settings: forge.Settings{APIURL: "https://u:p@ghe.example.com"}, errType: forge.ErrorTypeAPIURLInvalid, errHas: "forge_api_url"},
		{name: "an invalid upstream setting", ws: ws("https://github.com/acme/widgets", "https://github.com/up/proj"), target: forge.TargetUpstream,
			settings: forge.Settings{UpstreamAPIURL: "ghe.example.com/api"}, errType: forge.ErrorTypeAPIURLInvalid, errHas: "upstream_forge_api_url"},
		{name: "a URL naming no repository", ws: ws("https://github.com/widgets", ""), target: forge.TargetOrigin,
			errType: forge.ErrorTypeUnknown},
		{name: "an upstream the workspace does not have", ws: workspace.Workspace{Slug: "w", GitURL: new("https://github.com/acme/widgets")},
			target: forge.TargetUpstream, errType: forge.ErrorTypeUnknown},
		{name: "an origin host naming no forge", ws: ws("https://code.example.com/acme/widgets", ""), target: forge.TargetOrigin,
			errType: forge.ErrorTypeUnknown, errHas: "forge_api_url"},
		{name: "an upstream host naming no forge", ws: ws("https://github.com/acme/widgets", "https://code.example.com/up/proj"),
			target: forge.TargetUpstream, errType: forge.ErrorTypeUnknown, errHas: "upstream_forge_api_url"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := forge.Resolve(t.Context(), reader, c.ws, c.target, c.settings)
			if c.errType == "" {
				if err != nil || got != c.want {
					t.Fatalf("Resolve = %+v, %v, want %+v", got, err, c.want)
				}
				return
			}
			var refusal *forge.ResolveError
			if !errors.As(err, &refusal) || refusal.Type != c.errType || !strings.Contains(refusal.Message, c.errHas) {
				t.Fatalf("Resolve = %+v, %v, want a %s refusal naming %q", got, err, c.errType, c.errHas)
			}
		})
	}
}

func TestResolve_FailsWithoutACredentialItCannotRead(t *testing.T) {
	t.Parallel()
	gitURL := "https://github.com/acme/widgets"
	reader := secretMap{fail: map[string]bool{"GIT_PAT": true}}
	_, err := forge.Resolve(t.Context(), reader, workspace.Workspace{Slug: "w", GitURL: &gitURL}, forge.TargetOrigin, forge.Settings{})
	var refusal *forge.ResolveError
	if !errors.Is(err, errUnreadable) || errors.As(err, &refusal) {
		t.Fatalf("Resolve with GIT_PAT unreadable = %v, want the read error and no refusal", err)
	}
}
