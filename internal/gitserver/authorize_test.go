package gitserver_test

import (
	"context"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/server"
)

const (
	msgNotFound = "repository not found\n"
	msgReadOnly = "org members have read-only access to this workspace; create a task fork to contribute\n"
)

func TestAuthorize_GrantsTheNeedOfTheRouteByCredential(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{})
	writer, _ := w.pat(t, w.owner, "", "git:write")
	credentials := []struct {
		name, password, user string
		admin, push          bool
	}{
		{"admin token", w.Token, "", true, true},
		{"owner api key", w.ownerKey, w.owner, false, true},
		{"member api key", w.memberKey, w.member, false, false},
		{"owner pat git:read", w.ownerPAT, w.owner, false, false},
		{"owner pat git:write", writer, w.owner, false, true},
	}
	for _, c := range credentials {
		for _, g := range gitRequests {
			t.Run(c.name+"/"+g.name, func(t *testing.T) {
				before := w.service.served()
				a := w.call(t, g.method, g.to("acme/demo.git"), basic("x", c.password))
				granted := w.service.served() == before+1
				if want := c.push || !g.push; granted != want {
					t.Fatalf("granted = %v (answer %d %q), want %v", granted, a.status, a.body, want)
				}
				if !granted {
					return
				}
				need := access.NeedRead
				if g.push {
					need = access.NeedWrite
				}
				if x := w.service.last(t); x.Need != need || x.Caller.UserID != c.user || x.Caller.Admin != c.admin {
					t.Errorf("exchange = need %q user %q admin %v, want need %q user %q admin %v",
						x.Need, x.Caller.UserID, x.Caller.Admin, need, c.user, c.admin)
				}
			})
		}
	}
}

func TestAuthorize_RefusesAMissingScopeWhateverTheRepository(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{})
	noGit, _ := w.pat(t, w.owner, "", "changes:read")
	rows := []struct {
		password string
		request  gitRequest
		want     string
	}{
		{noGit, gitRequests[0], "token lacks scope git:read\n"},
		{noGit, gitRequests[1], "token lacks scope git:read\n"},
		{w.ownerPAT, gitRequests[2], "token lacks scope git:write\n"},
		{w.ownerPAT, gitRequests[3], "token lacks scope git:write\n"},
	}
	for _, row := range rows {
		for _, repo := range []string{"acme/demo.git", "acme/nosuch.git", "beta/other.git"} {
			t.Run(row.request.name+"/"+repo, func(t *testing.T) {
				if a := w.call(t, row.request.method, row.request.to(repo), basic("x", row.password)); a.status != http.StatusForbidden || a.body != row.want {
					t.Errorf("answer = %d %q, want 403 %q", a.status, a.body, row.want)
				}
			})
		}
	}
	if n := w.service.served(); n != 0 {
		t.Errorf("%d refused requests reached the service, want none", n)
	}
}

// guardCall is what the binding guard was asked.
type guardCall struct {
	credential, user, slug, method, pattern string
}

// fakeGuard is a binding guard that covers only the workspace covers and
// records what it is asked.
type fakeGuard struct {
	covers string
	mu     sync.Mutex
	calls  []guardCall
}

func (g *fakeGuard) GuardGit(_ context.Context, c server.Caller, slug, method, pattern string) (access.GitDecision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, guardCall{c.CredentialID, c.UserID, slug, method, pattern})
	if slug != g.covers {
		return access.GitDecision{Status: http.StatusNotFound}, nil
	}
	return access.GitDecision{Pass: true, Binding: &access.Binding{TokenID: c.CredentialID, Workspace: slug, Relation: access.RelationBound}}, nil
}

// asked returns the calls since the last asked and forgets them.
func (g *fakeGuard) asked() []guardCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	calls := g.calls
	g.calls = nil
	return calls
}

func TestAuthorize_AsksTheBindingGuardBeforeTheScope(t *testing.T) {
	t.Parallel()
	guard := &fakeGuard{covers: "demo"}
	w := newWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) { d.Guard = guard })
	}})
	for _, perm := range []string{"git:read", "changes:read"} {
		token, id := w.pat(t, w.owner, "demo", perm)
		for _, repo := range []string{"beta/other", "acme/nosuch"} {
			t.Run(perm+"/"+repo, func(t *testing.T) {
				refs := "/git/" + repo + ".git/info/refs?service=git-upload-pack"
				if a := w.call(t, http.MethodGet, refs, basic("x", token)); a.status != http.StatusNotFound || a.body != msgNotFound {
					t.Errorf("answer = %d %q, want 404 %q", a.status, a.body, msgNotFound)
				}
				want := []guardCall{{id, w.owner, path.Base(repo), http.MethodGet, gitserver.PatternInfoRefs}}
				if got := guard.asked(); !slices.Equal(got, want) {
					t.Errorf("guard asked %+v, want %+v", got, want)
				}
			})
		}
	}
	token, _ := w.pat(t, w.owner, "demo", "git:read")
	if a := w.call(t, http.MethodGet, demoRefs, basic("x", token)); a.status != http.StatusOK {
		t.Fatalf("a bound PAT on its workspace = %d %q, want 200", a.status, a.body)
	}
	if b := w.service.last(t).Binding; b == nil || b.Workspace != "demo" {
		t.Errorf("binding = %+v, want the guard's binding to demo", b)
	}

	unguarded := newWorld(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.AdjustGitServer(func(d *gitserver.Deps) { d.Guard = nil })
	}})
	token, _ = unguarded.pat(t, unguarded.owner, "demo", "git:read")
	if a := unguarded.call(t, http.MethodGet, demoRefs, basic("x", token)); a.status != http.StatusNotFound || a.body != msgNotFound {
		t.Errorf("a bound PAT without a guard = %d %q, want 404 %q", a.status, a.body, msgNotFound)
	}
}

func TestResolve_AnswersEveryRepositoryTheCallerCannotSeeAlike(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{})
	cases := []struct {
		name, password, repo string
	}{
		{"no .git", w.ownerKey, "acme/demo"},
		{"unknown slug", w.ownerKey, "acme/nosuch.git"},
		{"no org", w.ownerKey, "acme/solo.git"},
		{"wrong org", w.ownerKey, "beta/demo.git"},
		{"org in other case", w.ownerKey, "ACME/demo.git"},
	}
	var first string
	check := func(t *testing.T, password, repo string) {
		t.Helper()
		for _, g := range gitRequests {
			a := w.call(t, g.method, g.to(repo), basic("x", password))
			if a.status != http.StatusNotFound || a.body != msgNotFound {
				t.Errorf("%s: answer = %d %q, want 404 %q", g.name, a.status, a.body, msgNotFound)
			}
			if first == "" {
				first = a.dump()
			} else if got := a.dump(); got != first {
				t.Errorf("%s: answer =\n%s\nwant the same as the first:\n%s", g.name, got, first)
			}
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { check(t, c.password, c.repo) })
	}
	for _, status := range []string{access.StatusPending, access.StatusCloning, access.StatusReady, access.StatusFailed, access.StatusArchived} {
		t.Run("stranger/"+status, func(t *testing.T) {
			w.setStatus(t, "demo", status)
			check(t, w.strangerKey, "acme/demo.git")
		})
	}
	if n := w.service.served(); n != 0 {
		t.Errorf("%d refused requests reached the service, want none", n)
	}
}

func TestAuthorize_TellsAMemberWhoPushesToFork(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{})
	memberPAT, _ := w.pat(t, w.member, "", "git:write")
	for _, status := range []string{access.StatusReady, access.StatusArchived} {
		w.setStatus(t, "demo", status)
		for _, password := range []string{w.memberKey, memberPAT} {
			for _, g := range gitRequests[2:] {
				t.Run(status+"/"+g.name, func(t *testing.T) {
					if a := w.call(t, g.method, g.to("acme/demo.git"), basic("x", password)); a.status != http.StatusForbidden || a.body != msgReadOnly {
						t.Errorf("answer = %d %q, want 403 %q", a.status, a.body, msgReadOnly)
					}
				})
			}
		}
	}
	if n := w.service.served(); n != 0 {
		t.Errorf("%d refused requests reached the service, want none", n)
	}
}

func TestAuthorize_ReadsTheMembershipOnEveryRequest(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{})
	want := func(status int) {
		t.Helper()
		if a := w.call(t, http.MethodGet, demoRefs, basic("x", w.memberKey)); a.status != status || (status == http.StatusNotFound && a.body != msgNotFound) {
			t.Fatalf("answer = %d %q, want %d", a.status, a.body, status)
		}
	}
	want(http.StatusOK)
	if status, body := w.Call(t, http.MethodDelete, "/api/v1/orgs/"+w.acme+"/members/"+w.member, w.Token, nil); status != http.StatusNoContent {
		t.Fatalf("remove the member = %d %s, want 204", status, body)
	}
	want(http.StatusNotFound)
	w.AddMember(t, w.acme, w.member)
	want(http.StatusOK)
}

// Each row arranges two failures at once, on a hub that admits one request
// a minute per key; a row with twice set sends its request a second time,
// so that the key is over its limit, and is judged by the second answer.
func TestAuthorize_AnswersTheFirstFailingCheck(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{TOML: "[limits]\nrequests_per_minute = 1"})
	blocked := w.CreateUser(t, "blocked")
	blockedKey := w.IssueKey(t, blocked)
	if status, body := w.Call(t, http.MethodPost, "/api/v1/users/"+blocked+"/block", w.Token, nil); status != http.StatusOK {
		t.Fatalf("block the user = %d %s, want 200", status, body)
	}
	w.setStatus(t, "demo", access.StatusArchived)
	boundReader, _ := w.pat(t, w.owner, "demo", "git:read")
	boundNoGit, _ := w.pat(t, w.owner, "demo", "changes:read")
	noGit, _ := w.pat(t, w.owner, "", "changes:read")
	rows := []struct {
		name, password, path string
		twice                bool
		status               int
		want                 string
	}{
		{"blocked user over the limit", blockedKey, demoRefs, true, http.StatusForbidden, "user is blocked"},
		{"outside the binding over the limit", boundReader, "/git/beta/other.git/info/refs?service=git-upload-pack", true, http.StatusTooManyRequests, "rate limit exceeded"},
		{"outside the binding without a git scope", boundNoGit, "/git/beta/other.git/info/refs?service=git-upload-pack", false, http.StatusNotFound, "repository not found"},
		{"no git scope on an unknown slug", noGit, "/git/acme/nosuch.git/info/refs?service=git-upload-pack", false, http.StatusForbidden, "token lacks scope git:read"},
		{"stranger with an invalid service", w.strangerKey, "/git/acme/demo.git/info/refs?service=git-archive", false, http.StatusNotFound, "repository not found"},
		{"member pushes to a workspace not ready", w.memberKey, "/git/acme/demo.git/git-receive-pack", false, http.StatusForbidden, msgReadOnly[:len(msgReadOnly)-1]},
		{"owner with an invalid service on a workspace not ready", w.ownerKey, "/git/acme/demo.git/info/refs?service=git-archive", false, http.StatusBadRequest, "invalid or missing service parameter"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			method := http.MethodGet
			if strings.HasSuffix(row.path, "/git-receive-pack") {
				method = http.MethodPost
			}
			if row.twice {
				w.call(t, method, row.path, basic("x", row.password))
			}
			if a := w.call(t, method, row.path, basic("x", row.password)); a.status != row.status || a.body != row.want+"\n" {
				t.Errorf("answer = %d %q, want %d %q", a.status, a.body, row.status, row.want+"\n")
			}
		})
	}
}
