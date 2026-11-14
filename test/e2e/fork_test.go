package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The tests of this file set environment variables, so they must not run in
// parallel.

// forkHub is a workspace hub holding demo, cloned by its workers from an
// origin whose main has three commits, in the organization acme of the
// developer and of a second member, who has an API key. Its fork sweep runs
// one pass at the start and then one at each tick the test sends on ticks.
type forkHub struct {
	*workspaceHub
	comp      *hub.Composition
	member    string
	memberKey string
	ticks     chan time.Time
}

// startForkHub starts the hub and creates demo, and each of more, as the
// developer in acme, waiting until the hub has cloned them.
func startForkHub(t *testing.T, more ...string) *forkHub {
	t.Helper()
	return startForkHubAt(t, nil, more...)
}

// startForkHubAt is startForkHub with now as the hub's clock, nil for
// time.Now.
func startForkHubAt(t *testing.T, now func() time.Time, more ...string) *forkHub {
	t.Helper()
	origin := newOrigin(t)
	h := &forkHub{ticks: make(chan time.Time)}
	h.workspaceHub = startWorkspaceHubAt(t, origin, "demo", now, func(c *hub.Composition) {
		h.comp = c
		c.AdjustForks(func(d *fork.Deps) {
			d.SweepTicks = func() (<-chan time.Time, func()) { return h.ticks, func() {} }
		})
	})
	acme := h.CreateOrg(t, "acme")
	h.member = h.CreateUser(t, "member")
	h.memberKey = h.IssueKey(t, h.member)
	h.AddMember(t, acme, h.user)
	h.AddMember(t, acme, h.member)
	for _, slug := range append([]string{"demo"}, more...) {
		if status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key,
			map[string]any{"slug": slug, "git_url": origin.URL(), "org_id": "acme"}); status != http.StatusCreated {
			t.Fatalf("create %s = %d %s, want 201", slug, status, body)
		}
		h.waitStatus(t, slug, workspace.StatusReady)
	}
	return h
}

// runAs runs parley against the hub with credential.
func (h *forkHub) runAs(t *testing.T, credential string, args ...string) ran {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), append([]string{"--endpoint-url", h.URL(""), "--api-key", credential}, args...), &stdout, &stderr)
	return ran{code, stdout.String(), stderr.String()}
}

// repo is the directory of the hub's repository of the workspace slug.
func (h *forkHub) repo(slug string) string { return h.comp.Workspaces().RepoPath(slug) }

// objectCounts are the loose objects and the packed ones of the repository
// dir.
func objectCounts(t *testing.T, dir string) (loose, packed string) {
	t.Helper()
	counts := gitrepotest.ObjectCounts(t, dir)
	return counts["count"], counts["in-pack"]
}

// str is the string at the path of members in the JSON object v.
func str(v map[string]any, members ...string) string {
	for _, m := range members[:len(members)-1] {
		v, _ = v[m].(map[string]any)
	}
	s, _ := v[members[len(members)-1]].(string)
	return s
}

func TestForkWithTokenEndToEnd(t *testing.T) {
	h := startForkHub(t, "other")
	agent, agentKey := h.member, h.memberKey
	parentDir := h.repo("demo")
	parentRefs := gitrepotest.Refs(t, parentDir, "")

	r := h.runAs(t, agentKey, "fork", "create", "demo", "--name", "task1", "--token-permissions", "git:read,git:write,workspaces:read")
	if r.code != 0 || r.stderr == "" {
		t.Fatalf("fork create exited %d with stderr %q, want 0 and the shown-once warning", r.code, r.stderr)
	}
	f := r.json(t)
	slug, secret, tokenID := str(f, "workspace", "slug"), str(f, "token", "token"), str(f, "token", "token_id")
	if slug != "demo--task1" || str(f, "workspace", "status") != "ready" || str(f, "workspace", "owner_id") != agent {
		t.Errorf("printed slug %q, status %q, owner %q; want demo--task1, ready, %s", slug, str(f, "workspace", "status"), str(f, "workspace", "owner_id"), agent)
	}
	if remote := str(f, "remote"); remote == "" || remote != str(f, "workspace", "hub_url") {
		t.Errorf("remote = %q, want the fork's hub_url %q", remote, str(f, "workspace", "hub_url"))
	}
	if secret == "" || strings.Contains(r.stderr, secret) {
		t.Fatalf("token %q printed, stderr %q; want a secret on stdout alone", secret, r.stderr)
	}

	for path, want := range map[string]int{"/api/v1/workspaces/" + slug: http.StatusOK, "/api/v1/workspaces/other": http.StatusNotFound} {
		if status, body := h.Call(t, http.MethodGet, path, secret, nil); status != want {
			t.Errorf("GET %s with the fork's token = %d %s, want %d", path, status, body, want)
		}
	}

	dir := h.repo(slug)
	if loose, packed := objectCounts(t, dir); loose != "0" || packed != "0" {
		t.Errorf("the fork's repository holds %s loose and %s packed objects, want none", loose, packed)
	}
	head := str(f, "workspace", "head_sha")
	if commits := strings.Fields(gitrepotest.Git(t, dir, "rev-list", head)); len(commits) != 3 {
		t.Errorf("the fork lists %d commits of %s, want the parent's 3", len(commits), head)
	}
	// A client holding the parent's main commits one changed file on top of
	// it and pushes the commit to the new branch work of the fork.
	gitrepotest.PushCommit(t, parentDir, dir, "work", "work\n")
	if loose, packed := objectCounts(t, dir); loose != "3" || packed != "0" {
		t.Errorf("after the push the fork holds %s loose and %s packed objects, want the pushed commit's 3 (commit, tree, blob)", loose, packed)
	}
	if got := gitrepotest.Refs(t, parentDir, ""); !maps.Equal(got, parentRefs) {
		t.Errorf("the parent's refs = %v, want them unchanged: %v", got, parentRefs)
	}

	events := h.events(t, "parley.workspace.fork", slug)
	if len(events) != 1 {
		t.Fatalf("fork events of %s = %d, want 1", slug, len(events))
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(events[0]), &meta); err != nil || str(meta, "token_id") != tokenID {
		t.Errorf("the fork event's metadata = %s, want token_id %q", events[0], tokenID)
	}

	list := h.must(t, "fork", "list", "demo")
	slugs, access := itemValues(t, list, "slug"), itemValues(t, list, "access")
	if i := slices.Index(slugs, any(slug)); i < 0 || access[i] != "owner" {
		t.Errorf("the developer's fork list has slugs %v with access %v, want %s with owner", slugs, access, slug)
	}
}

func TestMemberForkFollowsParent(t *testing.T) {
	h := startForkHub(t)
	ctx := context.Background()

	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", h.memberKey, map[string]any{})
	if status != http.StatusCreated {
		t.Fatalf("the member's fork of demo = %d %s, want 201", status, body)
	}
	created := decoded(t, body)
	slug, old := str(created, "workspace", "slug"), str(created, "workspace", "fork_point", "sha")
	if str(created, "workspace", "owner_id") != h.member || str(created, "workspace", "access") != "owner" || str(created, "workspace", "org_slug") != "acme" {
		t.Errorf("the fork has owner %q, access %q, org %q; want %s, owner, acme",
			str(created, "workspace", "owner_id"), str(created, "workspace", "access"), str(created, "workspace", "org_slug"), h.member)
	}

	parentDir := h.repo("demo")
	moved := gitrepotest.Git(t, parentDir, "commit-tree", "main^{tree}", "-p", "main", "-m", "later")
	gitrepotest.Git(t, parentDir, "update-ref", "refs/heads/main", moved)

	status, body = h.Call(t, http.MethodGet, "/api/v1/workspaces/"+slug, h.memberKey, nil)
	w := decoded(t, body)
	if status != http.StatusOK || str(w, "head_sha") != moved || str(w, "fork_point", "sha") != old || old == moved {
		t.Errorf("GET %s = %d with head_sha %q, fork_point.sha %q; want 200, %s and %s", slug, status, str(w, "head_sha"), str(w, "fork_point", "sha"), moved, old)
	}

	refs, err := h.comp.Forks().Refs(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := refs.Branch("main"); !ok || !b.Tracking || b.SHA != moved {
		t.Errorf("the fork's main = %+v (found %v), want a tracking branch at %s", b, ok, moved)
	}
	err = h.comp.Forks().CheckBranchWrite(ctx, slug, "main")
	if refusal, ok := errors.AsType[*server.Error](err); !ok || refusal.Type() != "tracking_branch" {
		t.Errorf("a write check of the fork's main = %v, want tracking_branch", err)
	}

	status, body = h.Call(t, http.MethodPatch, "/api/v1/workspaces/demo", h.memberKey, map[string]any{"description": "x"})
	if status != http.StatusForbidden || str(decoded(t, body), "error", "error_type") != "forbidden" {
		t.Errorf("the member's update of demo = %d %s, want 403 forbidden", status, body)
	}
}
