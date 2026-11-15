package scm_test

import (
	"net/http"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestRead_AForkShowsItsOwnBranchesAndItsParentsCurrentHeads(t *testing.T) {
	w := newWorld(t)
	forkRepo := w.workspaces.RepoPath("demo--t1")
	gitrepotest.Git(t, forkRepo, "update-ref", "refs/heads/work", w.id["c1"])
	next := commit(t, w.repo, w.id["c2"], "f.txt", "next\n")
	gitrepotest.Git(t, w.repo, "update-ref", "refs/heads/main", next, w.id["c2"])

	r := w.get(t, scmPath("demo--t1", "/refs/main"), w.ownerKey)
	if r.status != http.StatusOK || r.json["sha"] != next || r.json["tracking"] != true {
		t.Errorf("the fork's main: %d %s, want sha %s and tracking true", r.status, r.body, next)
	}
	if f := w.get(t, scmPath("demo--t1", "/file?ref=main&path=f.txt"), w.ownerKey); f.status != http.StatusOK || f.header.Get("X-Parley-Resolved-Sha") != next || string(f.body) != "next\n" {
		t.Errorf("the fork's f.txt on main: %d %q from %q, want 200 %q from %s", f.status, f.body, f.header.Get("X-Parley-Resolved-Sha"), "next\n", next)
	}
	list := w.get(t, scmPath("demo--t1", "/refs"), w.ownerKey)
	if want := []string{"refs/heads/main", "refs/heads/work"}; !slices.Equal(list.names(), want) {
		t.Errorf("the fork's branches = %q, want %q", list.names(), want)
	}
	items, _ := list.json["items"].([]any)
	for _, it := range items {
		if o := it.(map[string]any); o["name"] == "refs/heads/work" && o["tracking"] != false {
			t.Errorf("the fork's own branch work has tracking %v, want false", o["tracking"])
		}
	}
	if v2 := w.get(t, scmPath("demo--t1", "/refs/refs/tags/v2"), w.ownerKey); v2.json["sha"] != w.id["c2"] {
		t.Errorf("the fork's tag v2 has sha %v, want the commit it peels to, %s", v2.json["sha"], w.id["c2"])
	}
	if names := w.get(t, scmPath("demo", "/refs"), w.ownerKey).names(); slices.Contains(names, "refs/heads/work") {
		t.Errorf("the parent's branches %q hold the fork's own branch work", names)
	}
}
