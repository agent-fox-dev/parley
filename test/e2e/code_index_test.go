package e2e

import (
	"context"
	"encoding/base64"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/codeindex"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

// codeIndexOn is the configuration of a hub that keeps code indexes.
const codeIndexOn = "[code_index]\n"

// indexFollows waits until the code index of the workspace slug, whose
// repository is repo, holds the head of its main, and fails t when it does
// not within 30 s after what moved the head.
func indexFollows(t *testing.T, index func() *codeindex.Component, slug, repo, after string) codeindex.Record {
	t.Helper()
	head := gitrepotest.Refs(t, repo, "refs/heads/")["main"]
	var rec codeindex.Record
	waitUntil(30*time.Second, func() bool {
		rec, _, _ = index().Record(context.Background(), slug)
		return rec.IndexedSHA == head
	})
	if rec.IndexedSHA != head || rec.Error != "" {
		t.Fatalf("after %s the code index of %s holds %q (%+v), want the head of main, %s", after, slug, rec.IndexedSHA, rec, head)
	}
	return rec
}

// The index of a workspace follows the head of its default branch whatever
// moves it, and nothing else.
func TestCodeIndex_FollowsTheHeadWhateverMovesIt(t *testing.T) {
	parallel(t)
	t.Run("a push, a land and the source control API", func(t *testing.T) {
		h := startLifecycleHubTOML(t, codeIndexOn)
		index, repo := h.comp.CodeIndex, h.repo("demo")
		// The hub's set-up pushed to main already.
		indexFollows(t, index, "demo", repo, "the first clone and a push")

		agent := cloneAs(t, h.forkHub, h.agent, "demo--t1")
		agent("checkout", "--quiet", "-b", "feat")
		commitFile(t, agent, "b.txt", "b\n", "Add b")
		agent("push", "--quiet", "origin", "feat")
		n := h.open(t, h.agent, "demo--t1", "feat", map[string]any{"intent": "why"})
		if r := h.runAs(t, h.agent, "change", "land", "demo", n, "--wait"); r.code != 0 {
			t.Fatalf("change land --wait exited %d: %s", r.code, r.stderr)
		}
		indexFollows(t, index, "demo", repo, "a land")

		owner := cloneAs(t, h.forkHub, h.key, "demo")
		commitFile(t, owner, "d.txt", "d\n", "Add d")
		owner("push", "--quiet", "origin", "main")
		indexFollows(t, index, "demo", repo, "a git push to main")

		commits := "/api/v1/workspaces/demo/scm/commits"
		put := func(path string) []map[string]any {
			return []map[string]any{{"op": "put", "path": path, "content_base64": base64.StdEncoding.EncodeToString([]byte(path + "\n"))}}
		}
		head := refHead(t, h.workspaceHub, "demo", "main", h.key)
		if a := scmWrite(t, h.workspaceHub, http.MethodPost, commits, h.key, map[string]any{
			"branch": "main", "expected_head_sha": head, "message": "e", "operations": put("e.txt")}); a.status != http.StatusCreated {
			t.Fatalf("POST scm/commits on main = %d %s, want 201", a.status, a.body)
		}
		built := indexFollows(t, index, "demo", repo, "a commit through the API")

		// A write to another branch moves no head the index follows.
		if a := scmWrite(t, h.workspaceHub, http.MethodPost, commits, h.key, map[string]any{
			"branch": "side", "base_ref": "main", "message": "f", "operations": put("f.txt")}); a.status != http.StatusCreated {
			t.Fatalf("POST scm/commits on side = %d %s, want 201", a.status, a.body)
		}
		var pending int
		if err := h.Store.Main().QueryRow(`SELECT count(*) FROM jobs WHERE type = 'code_index' AND status IN ('queued', 'running', 'failed')`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if rec, _, _ := index().Record(context.Background(), "demo"); pending != 0 || !rec.BuiltAt.Equal(*built.BuiltAt) {
			t.Errorf("after a commit to another branch %d builds are pending and the index was built at %s, want none and %s", pending, rec.BuiltAt, built.BuiltAt)
		}

		main, side := refHead(t, h.workspaceHub, "demo", "main", h.key), refHead(t, h.workspaceHub, "demo", "side", h.key)
		if a := scmWrite(t, h.workspaceHub, http.MethodPatch, "/api/v1/workspaces/demo/scm/refs/refs/heads/main", h.key,
			map[string]any{"sha": side, "expected_sha": main}); a.status != http.StatusOK {
			t.Fatalf("PATCH scm/refs/refs/heads/main = %d %s, want 200", a.status, a.body)
		}
		if rec := indexFollows(t, index, "demo", repo, "a ref move through the API"); rec.IndexedSHA != side {
			t.Errorf("the index holds %s, want the commit main was moved to, %s", rec.IndexedSHA, side)
		}
	})

	t.Run("a sync and a reclone", func(t *testing.T) {
		origin := pushOrigin(t)
		var comp *hub.Composition
		h := startWorkspaceHubTOML(t, origin, "demo", nil, codeIndexOn, func(c *hub.Composition) { comp = c })
		index, repo := func() *codeindex.Component { return comp.CodeIndex() }, comp.Workspaces().RepoPath("demo")
		h.createReady(t, "demo", origin.URL())
		indexFollows(t, index, "demo", repo, "the first clone")

		moved := origin.Commit("main", "origin moves")
		if r := h.run(t, "workspace", "sync", "demo"); r.code != 0 {
			t.Fatalf("workspace sync exited %d: %s", r.code, r.stderr)
		}
		if rec := indexFollows(t, index, "demo", repo, "a sync"); rec.IndexedSHA != moved {
			t.Errorf("the index holds %s, want the commit the sync brought, %s", rec.IndexedSHA, moved)
		}

		again := origin.Commit("main", "origin moves again")
		h.must(t, "workspace", "reclone", "demo", "--confirm", "--wait", "--poll-interval", "1s")
		if rec := indexFollows(t, index, "demo", repo, "a reclone"); rec.IndexedSHA != again {
			t.Errorf("the index holds %s, want the head of the new copy, %s", rec.IndexedSHA, again)
		}
	})

	t.Run("a rebuild of a carry-patch workspace", func(t *testing.T) {
		var comp *hub.Composition
		var mu sync.Mutex
		var told []string
		cfg := hubtest.Config{TOML: codeIndexOn, Compose: func(c *hub.Composition) {
			comp = c
			// After the hub's own, so that it sees what the code index is told.
			c.AdjustCarryPatch(func(d *carrypatch.Deps) {
				next := d.AfterRebuildPublish
				d.AfterRebuildPublish = func(ctx context.Context, slug, branch string) {
					mu.Lock()
					told = append(told, slug+" "+branch)
					mu.Unlock()
					if next != nil {
						next(ctx, slug, branch)
					}
				}
			})
		}}
		h, up, _, repo := startForkOfUpstreamOn(t, cfg, true, map[string]string{"README": "upstream\n"}, nil,
			[]forkPatch{{"patch/one", "patch one", map[string]string{"one.txt": "one\n"}}})
		index := func() *codeindex.Component { return comp.CodeIndex() }
		indexFollows(t, index, "acme-fork", repo, "the first clone")

		up.Branch("main", gitrepotest.Commit(t, up.Dir(), "u2", map[string]string{"u2.txt": "u2\n"}, up.Head("main")))
		if status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/acme-fork/patches", h.key, []map[string]any{{"branch_name": "patch/one"}}); status >= 300 {
			t.Fatalf("add the patch = %d %s", status, body)
		}
		if r := h.run(t, "rebuild", "submit", "acme-fork", "--wait", "--poll-interval", "1s"); r.code != 0 {
			t.Fatalf("rebuild submit --wait exited %d: %s", r.code, r.stderr)
		}
		// A rebuild moves the integration branch, which the index is told
		// of; it follows the default branch, wherever a sync left it.
		mu.Lock()
		got := append([]string(nil), told...)
		mu.Unlock()
		if len(got) == 0 || got[len(got)-1] != "acme-fork deploy" {
			t.Errorf("the rebuild told the code index of %q, want the move of acme-fork's deploy", got)
		}
		indexFollows(t, index, "acme-fork", repo, "a rebuild")
	})
}
