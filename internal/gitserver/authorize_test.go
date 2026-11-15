package gitserver_test

import (
	"context"
	"net/http"
	"path"
	"slices"
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

// The credential is the Basic password alone: each credential is sent with
// another user name on each request, and a request it is not granted is
// refused 403, never challenged.
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
	names := []string{"", "x-token-auth", "git", "member"}
	for _, c := range credentials {
		for i, g := range gitRequests {
			t.Run(c.name+"/"+g.name, func(t *testing.T) {
				before := w.service.served()
				a := w.call(t, g.method, g.to("acme/demo.git"), basic(names[i], c.password))
				granted := w.service.served() == before+1
				if want := c.push || !g.push; granted != want || (!granted && a.status != http.StatusForbidden) {
					t.Fatalf("granted = %v (answer %d %q), want %v, or a 403", granted, a.status, a.body, want)
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
