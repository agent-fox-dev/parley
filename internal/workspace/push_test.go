package workspace_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
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
		// dev, since a forced push of the protected main is refused.
		if a := p.push(t, p.uKey, syncSlug, "dev", c.body); a.status != http.StatusOK {
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

func TestPush_RefusesBeforeContactingOrigin(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	p.svc.RegisterPatchBranchLookup(patchLookup{registered: []string{"patch/a"}, failing: "cp-broken"})
	parent, deploy := syncSlug, "deploy"
	ready := workspace.StatusReady
	for _, rec := range []workspace.Workspace{
		{Slug: "pending", Status: workspace.StatusPending},
		{Slug: "cloning", Status: workspace.StatusCloning},
		{Slug: "recloning", Status: ready, Marker: workspace.MarkerReclone},
		{Slug: "archiving", Status: ready, Marker: workspace.MarkerArchive},
		{Slug: "deleting", Status: ready, Marker: workspace.MarkerDelete},
		{Slug: "failed", Status: workspace.StatusFailed},
		{Slug: "archived", Status: workspace.StatusArchived},
		{Slug: "a-fork", Kind: access.KindFork, ParentSlug: &parent, Status: ready},
		{Slug: "cp", Mode: workspace.ModeCarryPatch, IntegrationBranch: &deploy, Status: ready},
		{Slug: "cp-broken", Mode: workspace.ModeCarryPatch, IntegrationBranch: &deploy, Status: ready},
	} {
		p.put(t, rec)
	}
	requests, copyRefs, originRefs, events := p.origin.Requests(), gitrepotest.Refs(t, p.copy, ""), gitrepotest.Refs(t, p.origin.Dir(), ""), p.eventCount(t)
	for _, c := range []struct {
		name, slug, branch string
		body               any
		status             int
		errorType          string
	}{
		{"a pending workspace", "pending", "main", nil, http.StatusConflict, "workspace_busy"},
		{"a cloning workspace", "cloning", "main", nil, http.StatusConflict, "workspace_busy"},
		{"a running reclone", "recloning", "main", nil, http.StatusConflict, "workspace_busy"},
		{"a running archive", "archiving", "main", nil, http.StatusConflict, "workspace_busy"},
		{"a running delete", "deleting", "main", nil, http.StatusConflict, "workspace_busy"},
		{"a failed workspace", "failed", "main", nil, http.StatusConflict, "workspace_not_ready"},
		{"an archived workspace", "archived", "main", nil, http.StatusConflict, "workspace_archived"},
		{"a fork whose parent is ready", "a-fork", "work", nil, http.StatusBadRequest, "fork_has_no_origin"},
		{"the integration branch", "cp", "deploy", `{"force":false}`, http.StatusForbidden, "integration_branch"},
		{"the integration branch with force", "cp", "deploy", `{"force":true}`, http.StatusForbidden, "integration_branch"},
		{"a registered patch branch", "cp", url.PathEscape("patch/a"), `{"force":false}`, http.StatusForbidden, "patch_branch"},
		{"a registered patch branch with force", "cp", url.PathEscape("patch/a"), `{"force":true}`, http.StatusForbidden, "patch_branch"},
		{"a patch list that cannot be read", "cp-broken", "feature", nil, http.StatusServiceUnavailable, "unavailable"},
		{"a branch the hub's copy lacks", syncSlug, "nope", `{"force":false}`, http.StatusNotFound, "branch_not_found"},
	} {
		if a := p.push(t, p.uKey, c.slug, c.branch, c.body); a.status != c.status || a.errorType(t) != c.errorType {
			t.Errorf("%s: answer %d %s, want %d %s", c.name, a.status, a.body, c.status, c.errorType)
		}
	}
	// Secrets that cannot be read end the push before origin is contacted,
	// never with a push without a credential.
	p.fail(faultSecretRead)
	if a := p.push(t, p.uKey, syncSlug, "main", nil); a.status != http.StatusInternalServerError || a.errorType(t) != "internal" {
		t.Errorf("unreadable secrets: answer %d %s, want 500 internal", a.status, a.body)
	}
	p.fail("")
	if calls := p.pushes.recorded(); len(calls) != 0 {
		t.Errorf("refused pushes reached the push operation: %+v", calls)
	}
	if n := p.origin.Requests(); n != requests {
		t.Errorf("refused pushes sent origin %d requests", n-requests)
	}
	if got := gitrepotest.Refs(t, p.copy, ""); !maps.Equal(got, copyRefs) {
		t.Errorf("the copy's refs changed from %v to %v", copyRefs, got)
	}
	if got := gitrepotest.Refs(t, p.origin.Dir(), ""); !maps.Equal(got, originRefs) {
		t.Errorf("origin's refs changed from %v to %v", originRefs, got)
	}
	if n := p.eventCount(t); n != events {
		t.Errorf("refused pushes wrote %d events", n-events)
	}
}

func TestPush_LeavesBranchesNoRefusalNames(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	p.pushes.stubWith(gitrepo.PushPushed)
	main := ref(t, p.copy, "refs/heads/main")
	for _, b := range []string{"deploy", "patch/a", "patch/gone"} {
		gitrepotest.Git(t, p.copy, "branch", b, main)
	}
	for _, c := range []struct {
		name, branch string
		before       func()
	}{
		{"the branch deploy of a standard workspace", "deploy", func() {}},
		{"a carry-patch workspace's branch with no lookup registered", "patch/a", func() {
			p.setMode(t, workspace.ModeCarryPatch, "deploy")
		}},
		{"a branch the lookup does not report", "patch/gone", func() {
			p.svc.RegisterPatchBranchLookup(patchLookup{registered: []string{"patch/a"}})
		}},
	} {
		c.before()
		for _, body := range []string{`{"force":false}`, `{"force":true}`} {
			before := len(p.pushes.recorded())
			if a := p.push(t, p.uKey, syncSlug, url.PathEscape(c.branch), body); a.status != http.StatusOK {
				t.Errorf("%s, %s: answer %d %s, want 200", c.name, body, a.status, a.body)
			}
			if calls := p.pushes.recorded(); len(calls) != before+1 || calls[before].Branch != c.branch {
				t.Errorf("%s, %s: pushes %+v, want one of %s", c.name, body, calls[before:], c.branch)
			}
		}
	}
}

func TestPush_RefusesForcedPushOfAProtectedBranch(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	p.pushes.stubWith(gitrepo.PushPushed)
	main := ref(t, p.copy, "refs/heads/main")
	for _, b := range []string{"main2", "x/main", "release/1/x", "a/b"} {
		gitrepotest.Git(t, p.copy, "branch", b, main)
	}
	for _, c := range []struct {
		globs     []string // nil: the setting is not stored
		branch    string
		protected bool
	}{
		{[]string{"main"}, "main", true},
		{[]string{"main"}, "main2", false},
		{[]string{"main"}, "x/main", false},
		{[]string{"release/*"}, "release/1.x", true},
		{[]string{"release/*"}, "release/1/x", false},
		{[]string{"release/**"}, "release/1/x", true},
		{[]string{"**"}, "a/b/c", true},
		{[]string{"*"}, "a/b", false},
		{[]string{}, "main", false},
		{nil, "main", true},
	} {
		p.setProtected(t, c.globs)
		before := len(p.pushes.recorded())
		a := p.push(t, p.uKey, syncSlug, url.PathEscape(c.branch), `{"force":true}`)
		pushed := len(p.pushes.recorded()) > before
		switch {
		case c.protected && (a.status != http.StatusForbidden || a.errorType(t) != "protected_branch" || pushed):
			t.Errorf("globs %q, branch %s: answer %d %s (pushed %v), want 403 protected_branch without a push", c.globs, c.branch, a.status, a.body, pushed)
		case !c.protected && (a.status != http.StatusOK || !pushed):
			t.Errorf("globs %q, branch %s: answer %d %s (pushed %v), want 200 after a push", c.globs, c.branch, a.status, a.body, pushed)
		}
	}
}

func TestPush_AnswersTheFirstFailingCheck(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	p.pushes.stubWith(gitrepo.PushPushed)
	parent, deploy := syncSlug, "deploy"
	for _, rec := range []workspace.Workspace{
		{Slug: "archived", Status: workspace.StatusArchived},
		{Slug: "archived-cp", Status: workspace.StatusArchived, Mode: workspace.ModeCarryPatch, IntegrationBranch: &deploy},
		{Slug: "failed-fork", Status: workspace.StatusFailed, Kind: access.KindFork, ParentSlug: &parent},
		{Slug: "cp-protected", Status: workspace.StatusReady, Mode: workspace.ModeCarryPatch, IntegrationBranch: &deploy,
			Settings: map[string]json.RawMessage{"protected_branches": json.RawMessage(`["deploy"]`)}},
	} {
		p.put(t, rec)
	}
	p.setProtected(t, []string{"release/*"})
	for _, c := range []struct {
		name, slug, branch string
		body               any
		status             int
		errorType, message string // message: a word the message holds, "" for any
	}{
		{"an invalid body and branch name", syncSlug, "a..b", `[]`, http.StatusBadRequest, "invalid_request", "request body"},
		{"an invalid branch name on an archived workspace", "archived", "a..b", nil, http.StatusBadRequest, "invalid_request", "branch name"},
		{"the integration branch of an archived workspace", "archived-cp", "deploy", nil, http.StatusConflict, "workspace_archived", ""},
		{"a failed fork", "failed-fork", "work", nil, http.StatusConflict, "workspace_not_ready", ""},
		{"a forced push of a protected integration branch", "cp-protected", "deploy", `{"force":true}`, http.StatusForbidden, "integration_branch", ""},
		{"a forced push of a missing protected branch", syncSlug, url.PathEscape("release/9"), `{"force":true}`, http.StatusForbidden, "protected_branch", ""},
		{"a push of a missing protected branch", syncSlug, url.PathEscape("release/9"), `{"force":false}`, http.StatusNotFound, "branch_not_found", ""},
	} {
		a := p.push(t, p.uKey, c.slug, c.branch, c.body)
		errorType, msg := a.refusal(t)
		if a.status != c.status || errorType != c.errorType || !strings.Contains(msg, c.message) {
			t.Errorf("%s: answer %d %s, want %d %s naming %q", c.name, a.status, a.body, c.status, c.errorType, c.message)
		}
	}
	if calls := p.pushes.recorded(); len(calls) != 0 {
		t.Errorf("refused pushes reached the push operation: %+v", calls)
	}
}

func TestPush_ForcesOnlyUnderTheRecordedOriginHead(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	base := ref(t, p.copy, "refs/heads/main")
	rewrite := func(branch string) { grow(t, p.copy, branch, base, "hub") }
	for _, c := range []struct {
		name, branch string
		// arrange sets origin's branch, its recorded origin head and the
		// hub's branch.
		arrange func(branch string)
		result  string // "": the push is refused as 409 origin_moved
	}{
		{"origin at the recorded head, which the hub's head does not descend from", "replaced", func(b string) {
			p.seen(t, b, base)
			rewrite(b)
		}, "forced"},
		{"a fast-forward from the recorded head", "ahead", func(b string) { grow(t, p.copy, b, p.seen(t, b, base), "hub") }, "pushed"},
		{"a branch origin lacks and no head is recorded for", "created", rewrite, "pushed"},
		{"origin's branch got a commit after the recorded head", "moved", func(b string) {
			grow(t, p.origin.Dir(), b, p.seen(t, b, base), "origin")
			rewrite(b)
		}, ""},
		{"origin has the branch and no head is recorded", "unrecorded", func(b string) {
			p.seen(t, b, base)
			p.setOriginHead(t, b, "")
			rewrite(b)
		}, ""},
		{"a head is recorded and origin deleted the branch", "deleted", func(b string) {
			p.seen(t, b, base)
			gitrepotest.Git(t, p.origin.Dir(), "update-ref", "-d", "refs/heads/"+b)
			rewrite(b)
		}, ""},
	} {
		c.arrange(c.branch)
		hub, theirs, recorded := ref(t, p.copy, "refs/heads/"+c.branch), p.origin.Head(c.branch), ref(t, p.copy, "refs/remotes/origin/"+c.branch)
		a := p.push(t, p.uKey, syncSlug, c.branch, `{"force":true}`)
		if c.result == "" {
			if a.status != http.StatusConflict || a.errorType(t) != "origin_moved" {
				t.Errorf("%s: answer %d %s, want 409 origin_moved", c.name, a.status, a.body)
			}
			if got := p.origin.Head(c.branch); got != theirs {
				t.Errorf("%s: origin's branch moved from %q to %q", c.name, theirs, got)
			}
			if got := ref(t, p.copy, "refs/remotes/origin/"+c.branch); got != recorded {
				t.Errorf("%s: the recorded origin head moved from %q to %q", c.name, recorded, got)
			}
			continue
		}
		obj := a.decode(t, http.StatusOK)
		if obj["result"] != c.result || obj["sha"] != hub {
			t.Errorf("%s: answer %v, want %s at %s", c.name, obj, c.result, hub)
		}
		if got := p.origin.Head(c.branch); got != hub {
			t.Errorf("%s: origin's branch is at %q, want %s", c.name, got, hub)
		}
	}
}

func TestPush_AnswersEachFailureOfThePush(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	head := grow(t, p.copy, "feature", p.seen(t, "feature", ref(t, p.copy, "refs/heads/main")), "hub")
	theirs, recorded := p.origin.Head("feature"), ref(t, p.copy, "refs/remotes/origin/feature")
	gone := gitrepotest.NewOrigin(t)
	gone.Close()
	refuse := func(status int) func() func() {
		return func() func() { p.origin.Refuse(status); return func() { p.origin.Refuse(0) } }
	}
	for _, c := range []struct {
		name string
		// arrange sets the origin's behaviour up and returns what puts it
		// back.
		arrange   func() func()
		status    int
		errorType string
		message   string // a text the message holds, "" for any
	}{
		{"origin's hook refuses every update", func() func() {
			p.origin.Reject("branch is locked")
			return func() { p.origin.Reject("") }
		}, http.StatusUnprocessableEntity, "origin_rejected", "branch is locked"},
		{"origin asks for authentication and the workspace has no credential", func() func() {
			p.origin.Accept("x-token-auth", "tok-taken")
			return func() { p.origin.Accept("", "") }
		}, http.StatusBadGateway, "origin_unauthenticated", "no credential"},
		{"origin does not take the stored GIT_PAT", func() func() {
			p.putSecret(t, syncSlug, "GIT_PAT", "tok-refused")
			p.origin.Accept("x-token-auth", "tok-taken")
			return func() { p.deleteSecret(t, syncSlug, "GIT_PAT"); p.origin.Accept("", "") }
		}, http.StatusBadGateway, "origin_unauthenticated", "rejected"},
		{"origin answers 403", refuse(http.StatusForbidden), http.StatusBadGateway, "origin_forbidden", ""},
		{"origin answers 404", refuse(http.StatusNotFound), http.StatusBadGateway, "origin_not_found", ""},
		{"origin's listener is closed", func() func() {
			p.setGitURL(t, syncSlug, gone.URL())
			return func() { p.setGitURL(t, syncSlug, p.origin.URL()) }
		}, http.StatusBadGateway, "origin_unreachable", ""},
		{"origin serves a certificate the hub does not trust", func() func() {
			p.origin.ServeUntrusted(true)
			return func() { p.origin.ServeUntrusted(false) }
		}, http.StatusBadGateway, "origin_unreachable", ""},
		{"origin answers 500", refuse(http.StatusInternalServerError), http.StatusBadGateway, "upstream_error", ""},
	} {
		undo := c.arrange()
		a := p.push(t, p.uKey, syncSlug, "feature", nil)
		undo()
		errorType, msg := a.refusal(t)
		if a.status != c.status || errorType != c.errorType || !strings.Contains(msg, c.message) {
			t.Errorf("%s: answer %d %s, want %d %s naming %q", c.name, a.status, a.body, c.status, c.errorType, c.message)
		}
		if got := p.origin.Head("feature"); got != theirs {
			t.Errorf("%s: origin's feature moved from %s to %s", c.name, theirs, got)
		}
		if got := ref(t, p.copy, "refs/remotes/origin/feature"); got != recorded {
			t.Errorf("%s: the recorded origin head moved from %s to %s", c.name, recorded, got)
		}
	}
	// The failures leave origin able to take the push.
	if obj := p.push(t, p.uKey, syncSlug, "feature", nil).decode(t, http.StatusOK); obj["sha"] != head {
		t.Errorf("the push after the failures answered %v, want the push of %s", obj, head)
	}
}

func TestPush_EndsAPushThatOutlivesItsDeadline(t *testing.T) {
	t.Parallel()
	const deadline = 300 * time.Millisecond
	p := newPushWorldWith(t, nil, []option{withPushDeadline(deadline)})
	grow(t, p.copy, "dev", ref(t, p.copy, "refs/heads/dev"), "hub")
	recorded := ref(t, p.copy, "refs/remotes/origin/dev")
	p.origin.Block()
	start := time.Now()
	a := p.push(t, p.uKey, syncSlug, "dev", nil)
	took := time.Since(start)
	if a.status != http.StatusGatewayTimeout || a.errorType(t) != "origin_timeout" {
		t.Errorf("answer %d %s, want 504 origin_timeout", a.status, a.body)
	}
	if took < deadline || took > deadline+5*time.Second {
		t.Errorf("the answer came after %v, want it between the deadline %v and 5 s after it", took, deadline)
	}
	if left := processesNaming(t, p.origin.URL()); len(left) != 0 {
		t.Errorf("processes of the push remain: %q", left)
	}
	if got := ref(t, p.copy, "refs/remotes/origin/dev"); got != recorded {
		t.Errorf("the recorded origin head moved from %s to %s", recorded, got)
	}
}
