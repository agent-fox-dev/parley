package workspace_test

import (
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestPushRoute_TakesForceOnlyFromATrueMember(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	p.pushes.stubWith(gitrepo.PushPushed)
	for _, c := range []struct {
		name  string
		body  any
		force bool
	}{
		{"no body", nil, false},
		{"an empty body", "", false},
		{"an empty object", `{}`, false},
		{"force false", `{"force":false}`, false},
		{"force true", `{"force":true}`, true},
		{"force true and another member", `{"force":true,"extra":1}`, true},
		{"another member alone", `{"extra":1}`, false},
	} {
		before := len(p.pushes.recorded())
		if a := p.push(t, p.uKey, syncSlug, "main", c.body); a.status != http.StatusOK {
			t.Errorf("%s: answer %d %s, want 200", c.name, a.status, a.body)
			continue
		}
		calls := p.pushes.recorded()
		if len(calls) != before+1 || calls[len(calls)-1].Force != c.force {
			t.Errorf("%s: pushes %+v, want one more with force %v", c.name, calls[before:], c.force)
		}
	}
}

func TestPushRoute_RefusesInvalidRequestBeforePushing(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	p.pushes.stubWith(gitrepo.PushPushed)
	copyRefs, originRefs, events := gitrepotest.Refs(t, p.copy, ""), gitrepotest.Refs(t, p.origin.Dir(), ""), p.eventCount(t)
	type request struct {
		name, branch string
		body         any
	}
	var requests []request
	for _, body := range []string{`[]`, `"x"`, `5`, `{`, `{"force":"true"}`, `{"force":1}`, `{"force":null}`} {
		requests = append(requests, request{"body " + body, "main", body})
	}
	for _, name := range []string{"a..b", "-x", "x.lock", "a b", "@", "a//b", ".hidden", "x/"} {
		requests = append(requests, request{"branch " + name, name, nil})
	}
	for _, r := range requests {
		a := p.push(t, p.uKey, syncSlug, url.PathEscape(r.branch), r.body)
		if a.status != http.StatusBadRequest || a.errorType(t) != "invalid_request" {
			t.Errorf("%s: answer %d %s, want 400 invalid_request", r.name, a.status, a.body)
		}
	}
	if calls := p.pushes.recorded(); len(calls) != 0 {
		t.Errorf("refused requests pushed %+v", calls)
	}
	if got := gitrepotest.Refs(t, p.copy, ""); !maps.Equal(got, copyRefs) {
		t.Errorf("the copy's refs changed from %v to %v", copyRefs, got)
	}
	if got := gitrepotest.Refs(t, p.origin.Dir(), ""); !maps.Equal(got, originRefs) {
		t.Errorf("origin's refs changed from %v to %v", originRefs, got)
	}
	if n := p.eventCount(t); n != events {
		t.Errorf("refused requests wrote %d events", n-events)
	}
}

func TestPushRoute_DecodesBranchNameOnce(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	for _, c := range []struct{ segment, branch string }{
		{"fix%2F42", "fix/42"},
		{"a%2525b", "a%25b"},
	} {
		gitrepotest.Git(t, p.copy, "branch", c.branch, "main")
		obj := p.push(t, p.uKey, syncSlug, c.segment, nil).decode(t, http.StatusOK)
		if obj["branch"] != c.branch {
			t.Errorf("segment %s: answer branch %v, want %s", c.segment, obj["branch"], c.branch)
		}
		if head := p.origin.Head(c.branch); head == "" || head != obj["sha"] {
			t.Errorf("segment %s: origin's %s is at %q, want the pushed %v", c.segment, c.branch, head, obj["sha"])
		}
	}
}

func TestPushRoute_RefusesCallersWithoutWriteAccess(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	p.pushes.stubWith(gitrepo.PushPushed)
	p.put(t, workspace.Workspace{Slug: "team", OrgID: p.acme(t), Status: workspace.StatusReady})
	events := p.eventCount(t)
	for _, c := range []struct {
		name, cred, slug string
		status           int
		errorType        string
	}{
		{"an org member", p.vKey, "team", http.StatusForbidden, "forbidden"},
		{"the owner's PAT with only workspaces:read", p.pat(t, p.u, "workspaces:read"), syncSlug, http.StatusForbidden, "insufficient_scope"},
		{"a user outside the org", p.vKey, syncSlug, http.StatusNotFound, "not_found"},
	} {
		// The body is invalid, so a caller refused for the body would be
		// answered 400.
		if a := p.push(t, c.cred, c.slug, "main", `[]`); a.status != c.status || a.errorType(t) != c.errorType {
			t.Errorf("%s: answer %d %s, want %d %s", c.name, a.status, a.body, c.status, c.errorType)
		}
	}
	if calls := p.pushes.recorded(); len(calls) != 0 {
		t.Errorf("refused callers pushed %+v", calls)
	}
	if n := p.eventCount(t); n != events {
		t.Errorf("refused callers wrote %d events", n-events)
	}
	for _, c := range []struct{ name, cred string }{
		{"the owner's API key", p.uKey},
		{"the owner's PAT with workspaces:write", p.pat(t, p.u, "workspaces:write")},
		{"the admin token", p.Token},
	} {
		if a := p.push(t, c.cred, syncSlug, "main", nil); a.status != http.StatusOK {
			t.Errorf("%s: answer %d %s, want 200", c.name, a.status, a.body)
		}
	}
}

func TestPush_FastForwardsOriginsBranch(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	main := ref(t, p.copy, "refs/heads/main")
	dev := ref(t, p.copy, "refs/heads/dev")
	for _, c := range []struct {
		name, branch string
		grow         func() string
	}{
		{"a branch origin lacks", "feature", func() string { return grow(t, p.copy, "feature", main, "hub") }},
		{"a branch two commits ahead of origin's", "dev", func() string {
			return grow(t, p.copy, "dev", grow(t, p.copy, "dev", dev, "hub"), "hub again")
		}},
		{"the protected default branch one commit ahead", "main", func() string { return grow(t, p.copy, "main", main, "hub") }},
	} {
		head := c.grow()
		obj := p.push(t, p.uKey, syncSlug, c.branch, nil).decode(t, http.StatusOK)
		want := map[string]any{"branch": c.branch, "sha": head, "remote": "origin", "remote_url": p.origin.URL(), "result": "pushed"}
		if !reflect.DeepEqual(obj, want) {
			t.Errorf("%s: answer %v, want %v", c.name, obj, want)
		}
		if got := p.origin.Head(c.branch); got != head {
			t.Errorf("%s: origin's %s is at %s, want %s", c.name, c.branch, got, head)
		}
	}
}

func TestPush_AnswersUpToDateWhenOriginHasTheHead(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	dev := p.origin.Head("dev")
	for _, c := range []struct {
		name   string
		body   string
		forget bool
	}{
		{"without force", `{"force":false}`, false},
		{"with force", `{"force":true}`, false},
		{"with force and no recorded origin head", `{"force":true}`, true},
	} {
		if c.forget {
			gitrepotest.Git(t, p.copy, "update-ref", "-d", "refs/remotes/origin/dev")
		}
		obj := p.push(t, p.uKey, syncSlug, "dev", c.body).decode(t, http.StatusOK)
		if obj["result"] != "up_to_date" || obj["sha"] != dev {
			t.Errorf("%s: answer %v, want up_to_date at %s", c.name, obj, dev)
		}
		if got := p.origin.Head("dev"); got != dev {
			t.Errorf("%s: origin's dev moved from %s to %s", c.name, dev, got)
		}
	}
}

func TestPush_RefusesNonFastForward(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	origin := p.origin.Dir()
	for _, c := range []struct {
		name, branch string
		move         func(base string)
	}{
		{"diverged", "dev", func(base string) { grow(t, origin, "dev", base, "origin"); grow(t, p.copy, "dev", base, "hub") }},
		{"the hub one commit behind", "main", func(base string) { grow(t, origin, "main", base, "origin") }},
	} {
		c.move(p.origin.Head(c.branch))
		theirs, recorded := p.origin.Head(c.branch), ref(t, p.copy, "refs/remotes/origin/"+c.branch)
		a := p.push(t, p.uKey, syncSlug, c.branch, nil)
		errorType, msg := a.refusal(t)
		if a.status != http.StatusConflict || errorType != "non_fast_forward" {
			t.Errorf("%s: answer %d %s, want 409 non_fast_forward", c.name, a.status, a.body)
		}
		for _, word := range []string{recorded, "sync", "force"} {
			if !strings.Contains(msg, word) {
				t.Errorf("%s: message %q does not name %q", c.name, msg, word)
			}
		}
		if got := p.origin.Head(c.branch); got != theirs {
			t.Errorf("%s: origin's %s moved from %s to %s", c.name, c.branch, theirs, got)
		}
		if got := ref(t, p.copy, "refs/remotes/origin/"+c.branch); got != recorded {
			t.Errorf("%s: the recorded origin head moved from %s to %s", c.name, recorded, got)
		}
	}
}

func TestPush_AuthenticatesWithTheSecretsOfTheMoment(t *testing.T) {
	t.Parallel()
	const firstToken, user, password, secondToken = "tok-first-7c1e", "pusher", "pass-word-41b9", "tok-second-9d2a"
	p := newPushWorld(t, map[string]string{"GIT_PAT": firstToken}, gitrepotest.WithBasicAuth("x-token-auth", firstToken))
	for _, c := range []struct {
		name string
		// secrets are stored before the push, a secret with the value ""
		// deleted.
		secrets          map[string]string
		accept, password string
	}{
		{"GIT_PAT", nil, "x-token-auth", firstToken},
		{"GIT_USERNAME and GIT_PASSWORD", map[string]string{"GIT_PAT": "", "GIT_USERNAME": user, "GIT_PASSWORD": password}, user, password},
		{"a changed GIT_PAT", map[string]string{"GIT_PAT": secondToken}, "x-token-auth", secondToken},
	} {
		for name, value := range c.secrets {
			if value == "" {
				p.deleteSecret(t, syncSlug, name)
			} else {
				p.putSecret(t, syncSlug, name, value)
			}
		}
		p.origin.Accept(c.accept, c.password)
		head := grow(t, p.copy, "main", ref(t, p.copy, "refs/heads/main"), c.name)
		if a := p.push(t, p.uKey, syncSlug, "main", nil); a.status != http.StatusOK {
			t.Errorf("%s: answer %d %s, want 200", c.name, a.status, a.body)
		}
		if got := p.origin.Head("main"); got != head {
			t.Errorf("%s: origin's main is at %s, want %s", c.name, got, head)
		}
	}
	config := gitrepotest.Git(t, p.copy, "config", "--list")
	args := p.pushes.gitArgs()
	if len(args) == 0 {
		t.Fatal("the pushes ran no git process")
	}
	for _, secret := range []string{firstToken, password, secondToken} {
		if strings.Contains(config, secret) {
			t.Errorf("the copy's configuration holds %s", secret)
		}
		for _, a := range args {
			if strings.Contains(strings.Join(a, " "), secret) {
				t.Errorf("git ran with the arguments %v, which hold %s", a, secret)
			}
		}
	}
}
