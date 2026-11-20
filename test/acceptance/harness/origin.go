package harness

import (
	"net/http"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// Origin is a git origin outside the hub, served over TLS: a bare
// repository a scenario writes commits to directly.
type Origin struct {
	*gitrepotest.Origin
	t testing.TB
	// Main is the head of main after the seeding.
	Main string
}

// WithRepoPath serves an origin at /<path>, such as acme/widgets.git, for a
// forge that reads the owner and the repository from the URL.
var WithRepoPath = gitrepotest.WithRepoPath

// Origin returns an origin seeded with main (three commits), dev and the
// tag v1, which git reads but cannot push to.
func (h *Hub) Origin(opts ...gitrepotest.Option) *Origin {
	h.t.Helper()
	return seed(h.t, gitrepotest.NewOrigin(h.t, opts...))
}

// PushableOrigin is Origin served by git's smart transport, which takes
// pushes, such as those of a sync or an archive.
func (h *Hub) PushableOrigin(opts ...gitrepotest.Option) *Origin {
	h.t.Helper()
	return seed(h.t, gitrepotest.SmartOrigin(h.t, opts...))
}

func seed(tb testing.TB, o *gitrepotest.Origin) *Origin {
	main, _, _ := o.History()
	return &Origin{Origin: o, t: tb, Main: main}
}

// CommitFiles adds on top of branch a commit with msg whose tree is the
// branch's with the files, at the top level, and returns its id.
func (o *Origin) CommitFiles(branch, msg string, files map[string]string) string {
	o.t.Helper()
	var parents []string
	if head := o.Head(branch); head != "" {
		parents = append(parents, head)
	}
	id := gitrepotest.Commit(o.t, o.Dir(), msg, files, parents...)
	gitrepotest.SetRef(o.t, o.Dir(), "refs/heads/"+branch, id)
	o.Publish()
	return id
}

// Workspace is a workspace registered on a hub.
type Workspace struct {
	Slug string
	// Remote is the workspace's git URL on the hub.
	Remote string
	Owner  *User
	Origin *Origin
	// Body is the workspace object once it was ready.
	Body map[string]any
}

// Workspace registers slug, owned by owner, from a new seeded origin, and
// waits until it is ready.
func (h *Hub) Workspace(owner *User, slug string) *Workspace {
	h.t.Helper()
	return h.Register(owner, slug, h.Origin(), nil)
}

// Register registers slug, owned by owner, from the origin o, with the
// members of extra added to the create's body, and waits until it is
// ready.
func (h *Hub) Register(owner *User, slug string, o *Origin, extra map[string]any) *Workspace {
	h.t.Helper()
	body := map[string]any{"slug": slug, "git_url": o.URL()}
	for k, v := range extra {
		body[k] = v
	}
	h.Call(http.MethodPost, "/api/v1/workspaces", owner.Key, body).Want(h.t, http.StatusCreated)
	ws := h.WaitReady(owner.Key, slug)
	return &Workspace{Slug: slug, Remote: str(ws["hub_url"]), Owner: owner, Origin: o, Body: ws}
}

// WaitReady polls the workspace slug until it is ready and returns it; a
// workspace that fails fails the test.
func (h *Hub) WaitReady(credential, slug string) map[string]any {
	h.t.Helper()
	var r Response
	Eventually(h.t, "workspace "+slug+" to be ready", func() bool {
		r = h.Call(http.MethodGet, "/api/v1/workspaces/"+slug, credential, nil)
		if r.Body["status"] == "failed" {
			h.t.Fatalf("workspace %s failed: %s", slug, r.Raw)
		}
		return r.Body["status"] == "ready"
	})
	return r.Body
}

// Fork is a task fork created through the API.
type Fork struct {
	Slug, Remote string
	// Token is the token minted with the fork, "" when none was asked for.
	Token string
	// Body is the create's answer.
	Body map[string]any
}

// Fork creates a fork of parent with credential, minting a token bound to
// it with permissions unless they are nil, with the members of extra added
// to the body (a token member there replaces the harness's).
func (h *Hub) Fork(credential, parent string, permissions []string, extra map[string]any) *Fork {
	h.t.Helper()
	body := map[string]any{}
	if permissions != nil {
		body["token"] = map[string]any{"permissions": permissions}
	}
	for k, v := range extra {
		body[k] = v
	}
	r := h.Call(http.MethodPost, "/api/v1/workspaces/"+parent+"/forks", credential, body).Want(h.t, http.StatusCreated)
	ws, _ := r.Body["workspace"].(map[string]any)
	token, _ := r.Body["token"].(map[string]any)
	return &Fork{Slug: str(ws["slug"]), Remote: str(r.Body["remote"]), Token: str(token["token"]), Body: r.Body}
}
