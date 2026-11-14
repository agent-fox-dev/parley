package workspace_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/server"
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
func TestPushRoute_JudgesTheBodyItIsHanded(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	i := slices.IndexFunc(w.svc.Routes(), func(rt server.Route) bool { return strings.HasSuffix(rt.Pattern, "/push") })
	if i < 0 {
		t.Fatal("the service has no push route")
	}
	// A request that did not pass the platform's body stage, so that no
	// copy of the body waits in its context.
	err := w.svc.Routes()[i].Handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`[]`)))
	var answer *server.Error
	if !errors.As(err, &answer) || answer.Type() != "invalid_request" || answer.Message() != "request body must be a JSON object" {
		t.Errorf("the body [] answered %v, want 400 invalid_request: request body must be a JSON object", err)
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

func TestPush_MovesOriginsBranchOnlyForwardOrUnderTheLease(t *testing.T) {
	t.Parallel()
	p := newPushWorld(t, nil)
	origin, base, dev := p.origin.Dir(), ref(t, p.copy, "refs/heads/main"), ref(t, p.copy, "refs/heads/dev")
	rewrite := func(b string) { grow(t, p.copy, b, base, "hub") }
	for _, c := range []struct {
		name, branch string
		force        bool
		// arrange sets the hub's branch, origin's branch and the branch's
		// recorded origin head.
		arrange func(branch string)
		outcome string // the answer's result, or its error type
	}{
		// Without force origin's branch only moves forward.
		{"a branch origin lacks", "feature", false, rewrite, "pushed"},
		{"a branch two commits ahead of origin's", "dev", false, func(b string) { grow(t, p.copy, b, grow(t, p.copy, b, dev, "hub"), "hub again") }, "pushed"},
		{"the protected default branch one commit ahead", "main", false, rewrite, "pushed"},
		{"origin at the hub's head", "dev", false, func(string) {}, "up_to_date"},
		{"origin at the hub's head, with force", "dev", true, func(string) {}, "up_to_date"},
		{"origin at the hub's head, with force and no recorded origin head", "dev", true, func(b string) { p.setOriginHead(t, b, "") }, "up_to_date"},
		{"diverged", "dev", false, func(b string) {
			theirs := p.origin.Head(b)
			grow(t, origin, b, theirs, "origin")
			grow(t, p.copy, b, theirs, "hub")
		}, "non_fast_forward"},
		{"the hub one commit behind", "main", false, func(b string) { grow(t, origin, b, p.origin.Head(b), "origin") }, "non_fast_forward"},
		// With force origin's branch is replaced only while it is where the
		// hub last saw it.
		{"origin at the recorded head, which the hub's head does not descend from", "replaced", true, func(b string) {
			p.seen(t, b, base)
			rewrite(b)
		}, "forced"},
		{"a forced fast-forward from the recorded head", "ahead", true, func(b string) { grow(t, p.copy, b, p.seen(t, b, base), "hub") }, "pushed"},
		{"a forced push of a branch origin lacks and no head is recorded for", "created", true, rewrite, "pushed"},
		{"origin's branch got a commit after the recorded head", "moved", true, func(b string) {
			grow(t, origin, b, p.seen(t, b, base), "origin")
			rewrite(b)
		}, "origin_moved"},
		{"origin has the branch and no head is recorded", "unrecorded", true, func(b string) {
			p.seen(t, b, base)
			p.setOriginHead(t, b, "")
			rewrite(b)
		}, "origin_moved"},
		{"a head is recorded and origin deleted the branch", "deleted", true, func(b string) {
			p.seen(t, b, base)
			gitrepotest.Git(t, origin, "update-ref", "-d", "refs/heads/"+b)
			rewrite(b)
		}, "origin_moved"},
	} {
		c.arrange(c.branch)
		hub, theirs, recorded := ref(t, p.copy, "refs/heads/"+c.branch), p.origin.Head(c.branch), ref(t, p.copy, "refs/remotes/origin/"+c.branch)
		n, runs := len(p.eventsOf(t, pushEvent)), len(p.pushes.gitArgs())
		var body any
		if c.force {
			body = `{"force":true}`
		}
		a := p.push(t, p.uKey, syncSlug, c.branch, body)
		p.checkRecords(t, c.name, n, c.branch, hub, c.force, c.outcome)
		if a.status == http.StatusOK {
			want := map[string]any{"branch": c.branch, "sha": hub, "remote": "origin", "remote_url": p.origin.URL(), "result": c.outcome}
			if obj := a.decode(t, http.StatusOK); !reflect.DeepEqual(obj, want) {
				t.Errorf("%s: answer %v, want %v", c.name, obj, want)
			}
			if got := p.origin.Head(c.branch); got != hub {
				t.Errorf("%s: origin's branch is at %q, want %s", c.name, got, hub)
			}
		} else {
			if errorType, msg := a.refusal(t); a.status != http.StatusConflict || errorType != c.outcome {
				t.Errorf("%s: answer %d %s, want 409 %s", c.name, a.status, a.body, c.outcome)
			} else if c.outcome == "non_fast_forward" {
				for _, word := range []string{recorded, "sync", "force"} {
					if !strings.Contains(msg, word) {
						t.Errorf("%s: message %q does not name %q", c.name, msg, word)
					}
				}
			}
			if got := p.origin.Head(c.branch); got != theirs {
				t.Errorf("%s: origin's branch moved from %q to %q", c.name, theirs, got)
			}
			if got := ref(t, p.copy, "refs/remotes/origin/"+c.branch); got != recorded {
				t.Errorf("%s: the recorded origin head moved from %q to %q", c.name, recorded, got)
			}
		}
		// A forced push names the recorded origin head, or "" for none, as
		// its explicit lease, and is never a force origin applies whatever
		// its branch holds.
		args := slices.Concat(p.pushes.gitArgs()[runs:]...)
		if lease := "--force-with-lease=refs/heads/" + c.branch + ":" + recorded; c.force && !slices.Contains(args, lease) {
			t.Errorf("%s: git ran with %q, without the lease %s", c.name, args, lease)
		}
		for _, arg := range args {
			if arg == "--force" || arg == "-f" || strings.HasPrefix(arg, "+") {
				t.Errorf("%s: git ran with %q, which holds the plain force %q", c.name, args, arg)
			}
		}
	}
}

func TestPush_AuthenticatesWithTheSecretsOfTheMoment(t *testing.T) {
	t.Parallel()
	const firstToken, user, password, secondToken = "tok-first-7c1e", "pusher", "pass-word-41b9", "tok-second-9d2a"
	p := newPushWorld(t, map[string]string{"GIT_PAT": firstToken}, gitrepotest.WithBasicAuth("x-token-auth", firstToken))
	for _, c := range []struct {
		name             string
		secrets          map[string]string // stored before the push (see storeSecrets)
		accept, password string
	}{
		{"GIT_PAT", nil, "x-token-auth", firstToken},
		{"GIT_USERNAME and GIT_PASSWORD", map[string]string{"GIT_PAT": "", "GIT_USERNAME": user, "GIT_PASSWORD": password}, user, password},
		{"a changed GIT_PAT", map[string]string{"GIT_PAT": secondToken}, "x-token-auth", secondToken},
	} {
		p.storeSecrets(t, c.secrets)
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
	p.setProtected(t, []string{"main", "release/*"})
	parent, deploy := syncSlug, "deploy"
	ready, failed, archived := workspace.StatusReady, workspace.StatusFailed, workspace.StatusArchived
	for _, rec := range []workspace.Workspace{
		{Slug: "pending", Status: workspace.StatusPending},
		{Slug: "cloning", Status: workspace.StatusCloning},
		{Slug: "recloning", Status: ready, Marker: workspace.MarkerReclone},
		{Slug: "archiving", Status: ready, Marker: workspace.MarkerArchive},
		{Slug: "deleting", Status: ready, Marker: workspace.MarkerDelete},
		{Slug: "failed", Status: failed},
		{Slug: "archived", Status: archived},
		{Slug: "a-fork", Kind: access.KindFork, ParentSlug: &parent, Status: ready},
		{Slug: "failed-fork", Kind: access.KindFork, ParentSlug: &parent, Status: failed},
		{Slug: "cp", Mode: workspace.ModeCarryPatch, IntegrationBranch: &deploy, Status: ready},
		{Slug: "cp-broken", Mode: workspace.ModeCarryPatch, IntegrationBranch: &deploy, Status: ready},
		{Slug: "archived-cp", Mode: workspace.ModeCarryPatch, IntegrationBranch: &deploy, Status: archived},
		{Slug: "cp-protected", Mode: workspace.ModeCarryPatch, IntegrationBranch: &deploy, Status: ready,
			Settings: map[string]json.RawMessage{"protected_branches": json.RawMessage(`["deploy"]`)}},
	} {
		p.put(t, rec)
	}
	type request struct {
		name, slug, segment string
		body                any
		status              int
		errorType, message  string // message: a text the message holds, "" for any
	}
	var requests []request
	for _, body := range []string{`[]`, `"x"`, `5`, `{`, `{"force":"true"}`, `{"force":1}`, `{"force":null}`} {
		requests = append(requests, request{"the body " + body, syncSlug, "main", body, http.StatusBadRequest, "invalid_request", "request body"})
	}
	for _, name := range []string{"a..b", "-x", "x.lock", "a b", "@", "a//b", ".hidden", "x/"} {
		requests = append(requests, request{"the branch name " + name, syncSlug, url.PathEscape(name), nil, http.StatusBadRequest, "invalid_request", "branch name"})
	}
	requests = append(requests, []request{
		{"a pending workspace", "pending", "main", nil, http.StatusConflict, "workspace_busy", ""},
		{"a cloning workspace", "cloning", "main", nil, http.StatusConflict, "workspace_busy", ""},
		{"a running reclone", "recloning", "main", nil, http.StatusConflict, "workspace_busy", ""},
		{"a running archive", "archiving", "main", nil, http.StatusConflict, "workspace_busy", ""},
		{"a running delete", "deleting", "main", nil, http.StatusConflict, "workspace_busy", ""},
		{"a failed workspace", "failed", "main", nil, http.StatusConflict, "workspace_not_ready", ""},
		{"an archived workspace", "archived", "main", nil, http.StatusConflict, "workspace_archived", ""},
		{"a fork whose parent is ready", "a-fork", "work", nil, http.StatusBadRequest, "fork_has_no_origin", ""},
		{"the integration branch", "cp", "deploy", `{"force":false}`, http.StatusForbidden, "integration_branch", ""},
		{"the integration branch with force", "cp", "deploy", `{"force":true}`, http.StatusForbidden, "integration_branch", ""},
		{"a registered patch branch", "cp", url.PathEscape("patch/a"), `{"force":false}`, http.StatusForbidden, "patch_branch", ""},
		{"a registered patch branch with force", "cp", url.PathEscape("patch/a"), `{"force":true}`, http.StatusForbidden, "patch_branch", ""},
		{"a patch list that cannot be read", "cp-broken", "feature", nil, http.StatusServiceUnavailable, "unavailable", ""},
		{"a forced push of a protected branch", syncSlug, "main", `{"force":true}`, http.StatusForbidden, "protected_branch", ""},
		// A request that fails two adjacent checks gets the answer of the
		// first.
		{"an invalid body and branch name", syncSlug, "a..b", `[]`, http.StatusBadRequest, "invalid_request", "request body"},
		{"an invalid branch name on an archived workspace", "archived", "a..b", nil, http.StatusBadRequest, "invalid_request", "branch name"},
		{"the integration branch of an archived workspace", "archived-cp", "deploy", nil, http.StatusConflict, "workspace_archived", ""},
		{"a failed fork", "failed-fork", "work", nil, http.StatusConflict, "workspace_not_ready", ""},
		{"a forced push of a protected integration branch", "cp-protected", "deploy", `{"force":true}`, http.StatusForbidden, "integration_branch", ""},
		{"a forced push of a missing protected branch", syncSlug, url.PathEscape("release/9"), `{"force":true}`, http.StatusForbidden, "protected_branch", ""},
		{"a push of a branch the hub's copy lacks", syncSlug, url.PathEscape("release/9"), `{"force":false}`, http.StatusNotFound, "branch_not_found", ""},
	}...)
	requested, copyRefs, originRefs, events := p.origin.Requests(), gitrepotest.Refs(t, p.copy, ""), gitrepotest.Refs(t, p.origin.Dir(), ""), p.eventCount(t)
	for _, c := range requests {
		a := p.push(t, p.uKey, c.slug, c.segment, c.body)
		if errorType, msg := a.refusal(t); a.status != c.status || errorType != c.errorType || !strings.Contains(msg, c.message) {
			t.Errorf("%s: answer %d %s, want %d %s naming %q", c.name, a.status, a.body, c.status, c.errorType, c.message)
		}
	}
	// Secrets that cannot be read end the push before origin is contacted,
	// never with a push without a credential.
	p.fail(faultSecretRead)
	if a := p.push(t, p.uKey, syncSlug, "dev", nil); a.status != http.StatusInternalServerError || a.errorType(t) != "internal" {
		t.Errorf("unreadable secrets: answer %d %s, want 500 internal", a.status, a.body)
	}
	p.fail("")
	if calls := p.pushes.recorded(); len(calls) != 0 {
		t.Errorf("refused pushes reached the push operation: %+v", calls)
	}
	if n := p.origin.Requests(); n != requested {
		t.Errorf("refused pushes sent origin %d requests", n-requested)
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
		n, undo := len(p.eventsOf(t, pushEvent)), c.arrange()
		a := p.push(t, p.uKey, syncSlug, "feature", nil)
		p.checkRecords(t, c.name, n, "feature", head, false, a.outcome(t))
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

func TestPush_ScrubsAndCutsOriginsMessage(t *testing.T) {
	t.Parallel()
	const token, user, password = "tok-SECRET-123", "pusher", "pass-SECRET-456"
	p := newPushWorld(t, nil)
	head := grow(t, p.copy, "feature", ref(t, p.copy, "refs/heads/main"), "hub")
	for _, c := range []struct {
		name    string
		secrets map[string]string // stored before the push (see storeSecrets)
		// said is what origin's hook says; secret is what neither the answer
		// nor the event may hold, and holds what the message holds.
		said, secret string
		holds        []string
	}{
		{"GIT_PAT", map[string]string{"GIT_PAT": token},
			"denied for " + token + " at https://x-token-auth:" + token + "@git.example/r.git",
			token, []string{"[redacted]", "https://git.example/r.git"}},
		{"GIT_USERNAME and GIT_PASSWORD", map[string]string{"GIT_PAT": "", "GIT_USERNAME": user, "GIT_PASSWORD": password},
			"denied for " + password + " at https://" + user + ":" + password + "@git.example/r.git",
			password, []string{"[redacted]", "https://git.example/r.git"}},
		{"a text of 5000 characters", nil, strings.Repeat("über ", 1000), "", nil},
	} {
		p.storeSecrets(t, c.secrets)
		p.origin.Reject(c.said)
		n := len(p.eventsOf(t, pushEvent))
		a := p.push(t, p.uKey, syncSlug, "feature", nil)
		p.checkRecords(t, c.name, n, "feature", head, false, "origin_rejected")
		errorType, msg := a.refusal(t)
		if a.status != http.StatusUnprocessableEntity || errorType != "origin_rejected" {
			t.Errorf("%s: answer %d %s, want 422 origin_rejected", c.name, a.status, a.body)
		}
		if utf8.RuneCountInString(msg) > 1024 || !utf8.ValidString(msg) {
			t.Errorf("%s: message of %d characters, valid UTF-8 %v; want at most 1024 and valid", c.name,
				utf8.RuneCountInString(msg), utf8.ValidString(msg))
		}
		for _, s := range c.holds {
			if !strings.Contains(msg, s) {
				t.Errorf("%s: message %q does not hold %q", c.name, msg, s)
			}
		}
		ev, err := json.Marshal(p.eventsOf(t, pushEvent)[n:])
		if err != nil {
			t.Fatal(err)
		}
		if c.secret != "" && (strings.Contains(string(a.body), c.secret) || strings.Contains(string(ev), c.secret)) {
			t.Errorf("%s: the answer %s or the event %s holds %s", c.name, a.body, ev, c.secret)
		}
	}
}

func TestPush_GivesTheOriginURLWithoutUserinfo(t *testing.T) {
	t.Parallel()
	const token = "tok-in-the-path-5e1"
	p := newPushWorld(t, nil)
	p.pushes.stubWith(gitrepo.PushPushed)
	p.putSecret(t, syncSlug, "GIT_PAT", token)
	for _, c := range []struct{ stored, want string }{
		{"https://u:p@git.example/a.git", "https://git.example/a.git"},
		{"https://git.example/a.git", "https://git.example/a.git"},
		{"git@git.example:org/a.git", "git@git.example:org/a.git"},
		{"https://git.example/" + token + "/a.git", "https://git.example/[redacted]/a.git"},
	} {
		p.setGitURL(t, syncSlug, c.stored)
		obj := p.push(t, p.uKey, syncSlug, "dev", nil).decode(t, http.StatusOK)
		evs := p.eventsOf(t, pushEvent)
		var meta struct {
			RemoteURL string `json:"remote_url"`
		}
		if err := json.Unmarshal(evs[len(evs)-1].Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		if obj["remote_url"] != c.want || meta.RemoteURL != c.want {
			t.Errorf("%s: remote_url %v in the answer and %q in the event, want %q", c.stored, obj["remote_url"], meta.RemoteURL, c.want)
		}
	}
}

func TestPush_EndsAPushThatOutlivesItsDeadline(t *testing.T) {
	t.Parallel()
	const deadline = 300 * time.Millisecond
	p := newPushWorldWith(t, nil, []option{withPushDeadline(deadline)})
	head := grow(t, p.copy, "dev", ref(t, p.copy, "refs/heads/dev"), "hub")
	recorded := ref(t, p.copy, "refs/remotes/origin/dev")
	for _, c := range []struct {
		name      string
		arrange   func()
		status    int
		errorType string
	}{
		{"origin stalls", p.origin.Block, http.StatusGatewayTimeout, "origin_timeout"},
		// What git reported when it ended on its own is the answer, also as
		// the deadline passes.
		{"git reports a refusal as the deadline passes", func() {
			p.origin.Release()
			p.pushes.answerWith(func(ctx context.Context) (gitrepo.PushResult, error) {
				<-ctx.Done()
				return "", fmt.Errorf("git push of dev: %w", gitrepo.ErrNonFastForward)
			})
		}, http.StatusConflict, "non_fast_forward"},
	} {
		c.arrange()
		n, start := len(p.eventsOf(t, pushEvent)), time.Now()
		a := p.push(t, p.uKey, syncSlug, "dev", nil)
		took := time.Since(start)
		if a.status != c.status || a.errorType(t) != c.errorType {
			t.Errorf("%s: answer %d %s, want %d %s", c.name, a.status, a.body, c.status, c.errorType)
		}
		if took < deadline || took > deadline+5*time.Second {
			t.Errorf("%s: the answer came after %v, want it between the deadline %v and 5 s after it", c.name, took, deadline)
		}
		if left := processesNaming(t, p.origin.URL()); len(left) != 0 {
			t.Errorf("%s: processes of the push remain: %q", c.name, left)
		}
		if got := ref(t, p.copy, "refs/remotes/origin/dev"); got != recorded {
			t.Errorf("%s: the recorded origin head moved from %s to %s", c.name, recorded, got)
		}
		p.checkRecords(t, c.name, n, "dev", head, false, c.errorType)
	}
}

func TestPush_CompletesAfterTheCallerLeaves(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		deadline time.Duration // 0: the hub's own
		outcome  string
	}{
		{"origin answers after the caller left", 0, "pushed"},
		{"origin stalls past the push's deadline", 300 * time.Millisecond, "origin_timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var opts []option
			if c.deadline > 0 {
				opts = append(opts, withPushDeadline(c.deadline))
			}
			p := newPushWorldWith(t, nil, opts)
			head, n := grow(t, p.copy, "feature", ref(t, p.copy, "refs/heads/main"), "hub"), len(p.eventsOf(t, pushEvent))
			p.origin.Block()
			p.postAndLeave(t, p.uKey, "/api/v1/workspaces/"+syncSlug+"/branches/feature/push", p.asked())
			if c.outcome == "pushed" {
				p.origin.Release()
			}
			eventually(t, "the push is recorded", func() bool { return len(p.eventsOf(t, pushEvent)) == n+1 })
			want := ""
			if c.outcome == "pushed" {
				want = head
			}
			if got := p.origin.Head("feature"); got != want {
				t.Errorf("origin's feature is at %q, want %q", got, want)
			}
			p.checkRecords(t, "a caller who left", n, "feature", head, false, c.outcome)
		})
	}
}

func TestPush_PutsTheOriginHeadBackWhenTheRecordsFail(t *testing.T) {
	t.Parallel()
	meanwhile := &beforeEmit{}
	p := newPushWorldWith(t, nil, []option{withCompose(func(c *hub.Composition) {
		c.AdjustWorkspace(func(d *workspace.Deps) { meanwhile.Events, d.Events = d.Events, meanwhile })
	})})
	p.pushes.stubWith(gitrepo.PushPushed)
	main := ref(t, p.copy, "refs/heads/main")
	grow(t, p.copy, "main", main, "hub")
	gitrepotest.Git(t, p.copy, "branch", "feature", main)
	synced := grow(t, p.copy, "synced", main, "sync")
	p.fail(faultEvent)
	for _, c := range []struct {
		name, branch string
		// moveTo is the origin head a sync records for the branch while the
		// push's records are written, "" for none.
		moveTo, want string
	}{
		{"a branch with a recorded origin head", "main", "", main},
		{"a branch without one", "feature", "", ""},
		{"a branch whose origin head a sync moved meanwhile", "main", synced, synced},
	} {
		meanwhile.set(func() {
			if c.moveTo != "" {
				// A failure shows as the origin head checked below.
				repo, err := gitrepo.Git{}.Open(context.Background(), p.copy)
				if err == nil {
					_ = repo.SetOriginHead(context.Background(), c.branch, c.moveTo)
				}
			}
		})
		events := p.eventCount(t)
		rec, _ := p.workspace(t, syncSlug)
		if a := p.push(t, p.uKey, syncSlug, c.branch, nil); a.status != http.StatusInternalServerError || a.errorType(t) != "internal" {
			t.Errorf("%s: answer %d %s, want 500 internal", c.name, a.status, a.body)
		}
		if got := ref(t, p.copy, "refs/remotes/origin/"+c.branch); got != c.want {
			t.Errorf("%s: the recorded origin head is %q, want %q", c.name, got, c.want)
		}
		if after, _ := p.workspace(t, syncSlug); deref(after.OriginHeadSHA) != deref(rec.OriginHeadSHA) {
			t.Errorf("%s: origin_head_sha moved from %q to %q", c.name, deref(rec.OriginHeadSHA), deref(after.OriginHeadSHA))
		}
		if n := p.eventCount(t); n != events {
			t.Errorf("%s: %d events written", c.name, n-events)
		}
	}
}
