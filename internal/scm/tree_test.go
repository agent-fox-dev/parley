package scm_test

import (
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// item is the item of a list whose path is path, nil when there is none.
func (r reply) item(path string) map[string]any {
	items, _ := r.json["items"].([]any)
	for _, it := range items {
		if o, _ := it.(map[string]any); o["path"] == path {
			return o
		}
	}
	return nil
}

func TestTree_ListsADirectoryAtARef(t *testing.T) {
	w := newWorld(t)
	dir := w.addTree(t)
	id := func(rev string) string { return strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", rev)) }

	r := w.get(t, scmPath("tree", "/tree?ref=main"), w.ownerKey)
	if want := []string{"README.md", "docs", "link", "run.sh", "vendor"}; r.status != http.StatusOK || !slices.Equal(r.members("path"), want) {
		t.Fatalf("%d %s, want the paths %v", r.status, r.body, want)
	}
	if r.json["tree_sha"] != id("main^{tree}") || r.json["resolved_sha"] != id("main") {
		t.Errorf("tree_sha %v, resolved_sha %v, want main's tree and head", r.json["tree_sha"], r.json["resolved_sha"])
	}
	readme := map[string]any{"path": "README.md", "name": "README.md", "type": "blob", "mode": "100644",
		"sha": id("main:README.md"), "size": float64(len(treeReadme))}
	if got := r.item("README.md"); !reflect.DeepEqual(got, readme) {
		t.Errorf("README.md = %v, want %v", got, readme)
	}
	for _, c := range []struct {
		path, typ, mode string
		hasSize         bool
	}{
		{"run.sh", "blob", "100755", true},
		{"docs", "tree", "040000", false},
		{"link", "symlink", "120000", false},
	} {
		e := r.item(c.path)
		if _, hasSize := e["size"]; e["type"] != c.typ || e["mode"] != c.mode || hasSize != c.hasSize {
			t.Errorf("%s = %v, want type %s, mode %s, a size: %v", c.path, e, c.typ, c.mode, c.hasSize)
		}
	}
	if e := w.get(t, scmPath("tree", "/tree?path=vendor"), w.ownerKey).item("vendor/sub"); e["type"] != "commit" || e["mode"] != "160000" || e["size"] != nil {
		t.Errorf("vendor/sub = %v, want a submodule entry of type commit without a size", e)
	}
	d := w.get(t, scmPath("tree", "/tree?ref=main&path=docs"), w.ownerKey)
	if first := d.item("docs/a.md"); !slices.Equal(d.members("path"), []string{"docs/a.md", "docs/api"}) || first["name"] != "a.md" || d.json["tree_sha"] != id("main:docs") {
		t.Errorf("docs: %s, want docs/a.md (named a.md) and docs/api, and the tree id of docs", d.body)
	}
}

func TestTree_ListsEveryEntryBelowAPathInPagesFromOneCommit(t *testing.T) {
	w := newWorld(t)
	dir := w.addTree(t)
	r := w.get(t, scmPath("tree", "/tree?ref=main&recursive=true"), w.ownerKey)
	paths := r.members("path")
	if i := slices.Index(paths, "docs"); i < 0 || i+4 > len(paths) || !slices.Equal(paths[i:i+4], []string{"docs", "docs/a.md", "docs/api", "docs/api/b.md"}) {
		t.Errorf("recursive listing %v, want docs, docs/a.md, docs/api and docs/api/b.md in a row", paths)
	}

	path := scmPath("tree", "/tree?ref=big")
	p1 := w.get(t, path, w.ownerKey)
	next, _ := p1.json["next_cursor"].(string)
	if len(p1.members("path")) != 1000 || next == "" {
		t.Fatalf("first page: %d items, next_cursor %v; want 1,000 and a cursor", len(p1.members("path")), p1.json["next_cursor"])
	}
	// A page after the first reads the commit the first one resolved, though
	// the ref moved and the commit it moved to holds one more file.
	head := strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", "big"))
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/big", commit(t, dir, head, "f9999.txt", "later"))
	p2 := w.get(t, path+"&cursor="+url.QueryEscape(next), w.ownerKey)
	all := append(p1.members("path"), p2.members("path")...)
	if len(p2.members("path")) != 500 || p2.json["next_cursor"] != nil || p2.json["resolved_sha"] != head || len(slices.Compact(slices.Clone(all))) != 1500 {
		t.Errorf("second page: %d items, next_cursor %v, resolved_sha %v; want the other 500 of %s, no cursor", len(p2.members("path")), p2.json["next_cursor"], p2.json["resolved_sha"], head)
	}
	if r := w.get(t, scmPath("tree", "/tree?ref="+head+"&limit=10000"), w.ownerKey); r.status != http.StatusOK || len(r.members("path")) != 1500 {
		t.Errorf("limit=10000: %d with %d items, want 200 with 1,500", r.status, len(r.members("path")))
	}
}

func TestTree_RefusesAPathOrLimitItCannotList(t *testing.T) {
	w := newWorld(t)
	w.addTree(t)
	for _, c := range []struct {
		query  string
		status int
		want   string
	}{
		{"path=nope", http.StatusNotFound, "path_not_found"},
		{"path=docs/nope", http.StatusNotFound, "path_not_found"},
		{"path=README.md", http.StatusBadRequest, "invalid_request"},
		{"path=vendor/sub", http.StatusBadRequest, "invalid_request"},
		{"path=/docs", http.StatusBadRequest, "invalid_request"},
		{"path=docs//api", http.StatusBadRequest, "invalid_request"},
		{"path=docs/../x", http.StatusBadRequest, "invalid_request"},
		{"path=./docs", http.StatusBadRequest, "invalid_request"},
		{"path=.git/config", http.StatusBadRequest, "invalid_request"},
		{"limit=0", http.StatusBadRequest, "invalid_request"},
		{"limit=10001", http.StatusBadRequest, "invalid_request"},
		{"limit=x", http.StatusBadRequest, "invalid_request"},
		{"recursive=yes", http.StatusBadRequest, "invalid_request"},
	} {
		t.Run(c.query, func(t *testing.T) {
			if r := w.get(t, scmPath("tree", "/tree?ref=main&"+c.query), w.ownerKey); r.status != c.status || r.errorType() != c.want {
				t.Errorf("%d %s, want %d %s", r.status, r.body, c.status, c.want)
			}
		})
	}
}

// The tree route reads its ref through the resolver: a name, the default
// branch without one, and a form the resolver refuses.
func TestTree_ReadsTheCommitItsRefNames(t *testing.T) {
	w := newWorld(t)
	for _, c := range []struct{ ref, want string }{
		{"v2", w.id["c2"]}, {"", w.id["c2"]}, {"main~1", ""},
	} {
		t.Run(c.ref, func(t *testing.T) {
			r := w.get(t, scmPath("demo", "/tree?ref="+url.QueryEscape(c.ref)), w.ownerKey)
			switch {
			case c.want == "" && (r.status != http.StatusNotFound || r.errorType() != "ref_not_found"):
				t.Errorf("%d %s, want 404 ref_not_found", r.status, r.body)
			case c.want != "" && (r.status != http.StatusOK || r.json["resolved_sha"] != c.want):
				t.Errorf("%d %s, want 200 with resolved_sha %s", r.status, r.body, c.want)
			}
		})
	}
}
