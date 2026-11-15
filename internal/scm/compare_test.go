package scm_test

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// compareRepo is the cmp workspace's repository, whose refs the tests
// compare before and after each request.
type compareRepo struct{ dir string }

// id is the full id of the revision rev.
func (c compareRepo) id(t *testing.T, rev string) string {
	t.Helper()
	return strings.TrimSpace(gitrepotest.Git(t, c.dir, "rev-parse", rev))
}

// refs is every ref with its id, as git for-each-ref lists them.
func (c compareRepo) refs(t *testing.T) string {
	t.Helper()
	return gitrepotest.Git(t, c.dir, "for-each-ref")
}

// side is the file path of the revision rev as a conflict report shows
// one side, nil when rev holds no such file.
func (c compareRepo) side(t *testing.T, rev, path string) any {
	t.Helper()
	if f := strings.Fields(gitrepotest.Git(t, c.dir, "ls-tree", rev, "--", path)); len(f) == 4 {
		return map[string]any{"sha": f[2], "mode": f[0], "path": path}
	}
	return nil
}

// commitShas are the shas of the commits of a compare answer.
func (r reply) commitShas() []string {
	list, _ := r.json["commits"].([]any)
	shas := []string{}
	for _, it := range list {
		o, _ := it.(map[string]any)
		s, _ := o["sha"].(string)
		shas = append(shas, s)
	}
	return shas
}

func TestCompare_ReportsHowTwoRevisionsRelateWhenHeadApplies(t *testing.T) {
	w := newWorld(t)
	c := compareRepo{w.addCompare(t)}
	before := c.refs(t)

	r := w.get(t, scmPath("cmp", "/compare?base=main&head=feature"), w.ownerKey)
	if r.status != http.StatusOK || r.json["base_sha"] != c.id(t, "main") || r.json["head_sha"] != c.id(t, "feature") ||
		r.json["merge_base_sha"] != c.id(t, "main~1") || r.json["ahead"] != float64(2) || r.json["behind"] != float64(1) {
		t.Fatalf("%d %s, want main and feature by their ids, the merge base main~1, ahead 2 and behind 1", r.status, r.body)
	}
	if shas := r.commitShas(); !slices.Equal(shas, []string{c.id(t, "feature~1"), c.id(t, "feature")}) {
		t.Fatalf("commits %v, want f1 and f2, oldest first", shas)
	}
	first, _ := r.json["commits"].([]any)[0].(map[string]any)
	if _, hasStats := first["stats"]; first["message"] != "f1" || hasStats {
		t.Errorf("the first commit %v, want f1 without stats", first)
	}
	f, paths := r.files()
	if _, hasPatch := f["a.txt"]["patch"]; !slices.Equal(paths, []string{"a.txt", "b.txt"}) || hasPatch || f["a.txt"]["status"] != "added" {
		t.Errorf("files %v, want a.txt and b.txt added, without patches", r.json["files"])
	}
	if conflict, has := r.json["conflict"]; r.json["mergeable"] != "clean" || !has || conflict != nil {
		t.Errorf("mergeable %v, conflict %v; want clean and null", r.json["mergeable"], conflict)
	}

	s := w.get(t, scmPath("cmp", "/compare?base=main&head="+c.id(t, "main~1")), w.ownerKey)
	if s.status != http.StatusOK || s.json["ahead"] != float64(0) || len(s.commitShas()) != 0 || s.json["mergeable"] != "clean" ||
		!strings.Contains(string(s.body), `"commits":[]`) || !strings.Contains(string(s.body), `"files":[]`) {
		t.Errorf("head contained in base: %d %s, want ahead 0, commits [], files [] and clean", s.status, s.body)
	}

	l := w.get(t, scmPath("cmp", "/compare?base=main&head=long"), w.ownerKey)
	if shas := l.commitShas(); l.json["ahead"] != float64(gitrepotest.LongCommits) || len(shas) != 200 || shas[0] != c.id(t, "long~249") {
		t.Errorf("long: ahead %v, %d commits; want %d and the first 200, oldest first", l.json["ahead"], len(shas), gitrepotest.LongCommits)
	}

	if n := w.get(t, scmPath("cmp", "/compare?base=nope&head=main"), w.ownerKey); n.status != http.StatusNotFound || n.errorType() != "ref_not_found" {
		t.Errorf("base=nope: %d %s, want 404 ref_not_found", n.status, n.body)
	}
	if after := c.refs(t); after != before {
		t.Errorf("the refs changed from\n%s\nto\n%s", before, after)
	}
}

func TestCompare_ReportsTheConflictOfTheFirstCommitThatDoesNotApply(t *testing.T) {
	w := newWorld(t)
	c := compareRepo{w.addCompare(t)}
	for _, tc := range []struct{ base, head, kind string }{
		{"main", "both-edit", "content"},
		{"main", "both-add", "add_add"},
		{"main", "head-deletes", "modify_delete"},
		{"trimmed", "base-deletes", "delete_modify"},
	} {
		before := c.refs(t)
		r := w.get(t, scmPath("cmp", "/compare?base="+tc.base+"&head="+tc.head), w.ownerKey)
		if r.status != http.StatusOK || r.json["mergeable"] != "conflict" {
			t.Errorf("%s onto %s: %d %s, want 200 with a conflict", tc.head, tc.base, r.status, r.body)
			continue
		}
		want := map[string]any{
			"target_sha": r.json["base_sha"], "head_sha": r.json["head_sha"], "base_sha": r.json["merge_base_sha"],
			"commit_sha": c.id(t, tc.head),
			"files": []any{map[string]any{"path": "f.txt", "kind": tc.kind, "base": c.side(t, tc.head+"~1", "f.txt"),
				"ours": c.side(t, tc.base, "f.txt"), "theirs": c.side(t, tc.head, "f.txt"), "landed_by": []any{}}},
			"landed_changes": []any{}, "change": nil,
		}
		if !reflect.DeepEqual(r.json["conflict"], want) {
			t.Errorf("%s onto %s: conflict %v, want %v", tc.head, tc.base, r.json["conflict"], want)
		}
		if after := c.refs(t); after != before {
			t.Errorf("%s onto %s: the refs changed from\n%s\nto\n%s", tc.head, tc.base, before, after)
		}
	}
}

func TestCompare_RefusesAMergeCommitAmongTheCommitsToReplay(t *testing.T) {
	w := newWorld(t)
	c := compareRepo{w.addCompare(t)}
	before := c.refs(t)
	if r := w.get(t, scmPath("cmp", "/compare?base=main&head=merged"), w.ownerKey); r.status != http.StatusBadRequest || r.errorType() != "merge_commits_unsupported" {
		t.Errorf("merged onto main: %d %s, want 400 merge_commits_unsupported", r.status, r.body)
	}
	if r := w.get(t, scmPath("cmp", "/diff?base=main&head=merged"), w.ownerKey); r.status != http.StatusOK {
		t.Errorf("the diff of main and merged: %d %s, want 200", r.status, r.body)
	}
	if after := c.refs(t); after != before {
		t.Errorf("the refs changed from\n%s\nto\n%s", before, after)
	}
}
