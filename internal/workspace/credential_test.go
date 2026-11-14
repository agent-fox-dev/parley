package workspace_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestResolveOriginCredential_ReadsTheWorkspaceSecretsOnEveryCall(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// withCredential is valid(slug) with the credential members given, as
// name, value pairs.
func withCredential(slug string, members ...string) map[string]any {
	body := valid(slug)
	for i := 0; i+1 < len(members); i += 2 {
		body[members[i]] = members[i+1]
	}
	return body
}

func TestCreate_ChecksAGivenCredentialAgainstOriginBeforeStoringAnything(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for i, c := range []struct {
		name    string
		members []string
		want    workspace.Credential
	}{
		{"a token", []string{"git_pat", "tok-123"}, workspace.Credential{Username: "x-token-auth", Password: "tok-123"}},
		{"a user name and password", []string{"git_username", "u", "git_password", "p"}, workspace.Credential{Username: "u", Password: "p"}},
	} {
		workspaces, secrets := w.stored()
		w.create(t, w.uKey, withCredential(fmt.Sprintf("demo-%d", i), c.members...)).object(t)
		calls := w.origin.recorded()
		if len(calls) != i+1 {
			t.Fatalf("%s: origin was called %d times in all, want %d", c.name, len(calls), i+1)
		}
		call := calls[i]
		if call.url != "https://example.com/a.git" || call.cred != c.want {
			t.Errorf("%s: origin was asked for %s with %+v, want https://example.com/a.git with %+v", c.name, call.url, call.cred, c.want)
		}
		if call.left <= 9*time.Second || call.left > 10*time.Second {
			t.Errorf("%s: the check had %v left, want a deadline between 9 and 10 seconds away", c.name, call.left)
		}
		if call.workspaces != workspaces || call.secrets != secrets {
			t.Errorf("%s: at the check the store held %d workspaces and %d secrets, want the %d and %d it held before the request",
				c.name, call.workspaces, call.secrets, workspaces, secrets)
		}
	}
}

func TestCreate_ChecksTheCredentialWithGitAgainstAnOriginTheHubTrusts(t *testing.T) {
	t.Parallel()
	origin := gitrepotest.NewOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", "tok-123"))
	origin.Commit("main", "first")
	w := newWorld(t, withGitOrigin(origin))
	for _, c := range []struct {
		token  string
		status int
	}{{"tok-wrong", http.StatusBadRequest}, {"tok-123", http.StatusCreated}} {
		body := valid("demo")
		body["git_url"], body["git_pat"] = origin.URL(), c.token
		if a := w.create(t, w.uKey, body); a.status != c.status {
			t.Errorf("with %s = %d %s, want %d", c.token, a.status, a.body, c.status)
		}
	}
}

func TestCreate_RefusesACredentialOriginDoesNotAcceptAndCreatesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, c := range []struct {
		name string
		err  error
	}{
		{"authentication refused", errors.New("git ls-remote: exit status 128: fatal: Authentication failed for 'https://example.com/a.git/'")},
		{"connection refused", errors.New("git ls-remote: exit status 128: fatal: unable to access 'https://example.com/a.git/': Failed to connect to example.com port 443: Connection refused")},
		{"the deadline passed", fmt.Errorf("git ls-remote: %w", context.DeadlineExceeded)},
		{"an error holding the token", errors.New("fatal: tok-123 refused at https://x-token-auth:tok-123@host/")},
	} {
		w.origin.answer(c.err)
		before := w.footprint(t)
		a := w.create(t, w.uKey, withCredential("demo", "git_pat", "tok-123"))
		if a.status != http.StatusBadRequest || a.errorType(t) != "credential_check_failed" {
			t.Errorf("%s = %d %s, want 400 credential_check_failed", c.name, a.status, a.body)
		}
		if strings.Contains(string(a.body), "tok-123") {
			t.Errorf("%s: the answer %s holds the token", c.name, a.body)
		}
		before.originCalls++
		if after := w.footprint(t); after != before {
			t.Errorf("%s: the hub went from %+v to %+v, want one call to origin and nothing recorded", c.name, before, after)
		}
	}
	if log := w.Log.String(); strings.Contains(log, "tok-123") || !strings.Contains(log, "credential check") {
		t.Errorf("the hub's log holds the token or no refused credential check:\n%s", log)
	}
}

func TestCreate_RefusesACredentialWithoutASecretKeyBeforeAskingOrigin(t *testing.T) {
	t.Parallel()
	w := newWorld(t, withoutSecretKey())
	before := w.footprint(t)
	a := w.create(t, w.uKey, withCredential("demo", "git_pat", "tok-123"))
	if a.status != http.StatusServiceUnavailable || a.errorType(t) != "secrets_unavailable" {
		t.Errorf("with a token = %d %s, want 503 secrets_unavailable", a.status, a.body)
	}
	if after := w.footprint(t); after != before {
		t.Errorf("the hub went from %+v to %+v, want origin never called and nothing recorded", before, after)
	}
	w.create(t, w.uKey, valid("demo")).object(t)
}

func TestCreate_AsksNeitherOriginNorTheSecretsWithoutACredential(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, url := range []string{"https://example.com/a.git", "git@example.com:a/b.git"} {
		body := valid("demo")
		body["git_url"] = url
		w.create(t, w.uKey, body).object(t)
		if calls, asked := w.origin.called(), w.availableAsked(); calls != 0 || asked != 0 {
			t.Errorf("%s: origin called %d times and the secrets asked %d times, want neither", url, calls, asked)
		}
		w.deleteRecord(t, "demo")
	}
}

func TestCreate_StoresTheCredentialAsWorkspaceSecretsAndShowsItNowhere(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, c := range []struct {
		slug    string
		members []string
		secrets map[string]string
		hidden  string
	}{
		{"with-token", []string{"git_pat", "tok-123"}, map[string]string{"GIT_PAT": "tok-123"}, "tok-123"},
		{"with-login", []string{"git_username", "u1x", "git_password", "pw-9z"}, map[string]string{"GIT_PASSWORD": "pw-9z", "GIT_USERNAME": "u1x"}, "pw-9z"},
	} {
		a := w.create(t, w.uKey, withCredential(c.slug, c.members...))
		a.object(t)
		ctx := context.Background()
		items, err := w.secrets.ListPage(ctx, secret.ScopeWorkspace, c.slug, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, it := range items {
			keys = append(keys, it.Key)
		}
		if want := slices.Sorted(maps.Keys(c.secrets)); !slices.Equal(keys, want) {
			t.Errorf("%s: the workspace secrets are %v, want %v", c.slug, keys, want)
		}
		for name, want := range c.secrets {
			if got, found, err := w.secrets.Get(ctx, secret.ScopeWorkspace, c.slug, name); err != nil || !found || got != want {
				t.Errorf("%s: %s = %q (found %v, %v), want %q", c.slug, name, got, found, err, want)
			}
		}
		evs := w.createEvents(t)
		ev, err := json.Marshal(evs[len(evs)-1])
		if err != nil {
			t.Fatal(err)
		}
		for what, out := range map[string]string{"the answer": string(a.body), "the event": string(ev), "the log": w.Log.String()} {
			if strings.Contains(out, c.hidden) {
				t.Errorf("%s: %s holds the credential: %s", c.slug, what, out)
			}
		}
	}
}
