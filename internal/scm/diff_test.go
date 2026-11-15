package scm_test

import (
	"bytes"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// files are the file changes of a diff answer, by path, and their paths in
// the answer's order.
func (r reply) files() (map[string]map[string]any, []string) {
	list, _ := r.json["files"].([]any)
	byPath, paths := map[string]map[string]any{}, []string{}
	for _, it := range list {
		o, _ := it.(map[string]any)
		p, _ := o["path"].(string)
		byPath[p], paths = o, append(paths, p)
	}
	return byPath, paths
}

// stat is the member key of the stats of a diff answer.
func (r reply) stat(key string) int {
	s, _ := r.json["stats"].(map[string]any)
	n, _ := s[key].(float64)
	return int(n)
}

// featurePaths are the files feature changes in the diff workspace since
// mb, in path order.
var featurePaths = []string{"docs/api/x.txt", "docs/guide.md", "edit.txt", "gone.txt", "img.bin", "kind", "moved/b.txt", "new.txt", "notes.md", "src/readme.md"}

func TestDiff_ComparesHeadWithTheMergeBaseOrWithBase(t *testing.T) {
	w := newWorld(t)
	id := w.addDiff(t)

	r := w.get(t, scmPath("diff", "/diff?base=main&head=feature"), w.ownerKey)
	if r.status != http.StatusOK || r.json["base_sha"] != id["main"] || r.json["head_sha"] != id["feature"] || r.json["merge_base_sha"] != id["mb"] {
		t.Fatalf("%d %s, want base_sha main, head_sha feature and merge_base_sha mb (%s)", r.status, r.body, id["mb"])
	}
	if _, paths := r.files(); !slices.Equal(paths, featurePaths) || r.json["truncated"] != false || r.stat("files_changed") != len(paths) {
		t.Errorf("files %v, truncated %v, files_changed %d; want %v, false and their number", paths, r.json["truncated"], r.stat("files_changed"), featurePaths)
	}

	d := w.get(t, scmPath("diff", "/diff?base=main&head=feature&mode=direct"), w.ownerKey)
	if f, paths := d.files(); f["only-main.txt"]["status"] != "deleted" || len(paths) != len(featurePaths)+1 || d.json["merge_base_sha"] != id["mb"] {
		t.Errorf("direct mode: %s, want feature's files and only-main.txt deleted, merge_base_sha mb", d.body)
	}

	o := w.get(t, scmPath("diff", "/diff?base=main&head=orphan"), w.ownerKey)
	f, paths := o.files()
	if v, has := o.json["merge_base_sha"]; o.status != http.StatusOK || !has || v != nil || !slices.Equal(paths, []string{"o1.txt", "o2.txt"}) {
		t.Fatalf("no common ancestor: %d %s, want merge_base_sha null and orphan's two files", o.status, o.body)
	}
	for _, p := range paths {
		if f[p]["status"] != "added" {
			t.Errorf("%s = %v, want it added", p, f[p])
		}
	}
}

func TestDiff_DescribesEachFileChange(t *testing.T) {
	w := newWorld(t)
	id := w.addDiff(t)
	blob := func(rev string) string {
		return strings.TrimSpace(gitrepotest.Git(t, w.workspaces.RepoPath("diff"), "rev-parse", rev))
	}
	r := w.get(t, scmPath("diff", "/diff?base="+id["mb"]+"&head="+id["feature"]), w.ownerKey)
	f, paths := r.files()
	if r.status != http.StatusOK || !slices.IsSorted(paths) {
		t.Fatalf("%d %s, want 200 with the files in path order", r.status, r.body)
	}

	if n := f["new.txt"]; n["status"] != "added" || n["old_sha"] != nil || n["new_sha"] != blob("feature:new.txt") || n["insertions"] != float64(3) {
		t.Errorf("new.txt = %v, want added with 3 insertions and no old_sha", n)
	}
	if g := f["gone.txt"]; g["status"] != "deleted" || g["new_sha"] != nil || g["old_sha"] != blob(id["mb"]+":gone.txt") || g["deletions"] != float64(1) {
		t.Errorf("gone.txt = %v, want deleted with no new_sha", g)
	}
	edit := maps.Clone(f["edit.txt"])
	patch, _ := edit["patch"].(string)
	delete(edit, "patch")
	want := map[string]any{"path": "edit.txt", "old_path": nil, "status": "modified", "insertions": float64(2), "deletions": float64(1),
		"binary": false, "old_sha": blob(id["mb"] + ":edit.txt"), "new_sha": blob("feature:edit.txt")}
	if !reflect.DeepEqual(edit, want) || !strings.Contains(patch, "@@") {
		t.Errorf("edit.txt = %v with the patch %q, want %v and a patch with a hunk", edit, patch, want)
	}
	if m := f["moved/b.txt"]; m["status"] != "renamed" || m["old_path"] != "a.txt" || f["a.txt"] != nil {
		t.Errorf("moved/b.txt = %v, and a.txt = %v; want one entry renamed from a.txt", m, f["a.txt"])
	}
	if b := f["img.bin"]; b["binary"] != true || b["insertions"] != float64(0) || b["deletions"] != float64(0) || b["patch"] != nil {
		t.Errorf("img.bin = %v, want binary with no lines and no patch", b)
	}
	if k := f["kind"]; k["status"] != "type_changed" {
		t.Errorf("kind = %v, want type_changed", k)
	}
}

func TestDiff_FiltersAndShapesTheDiff(t *testing.T) {
	w := newWorld(t)
	id := w.addDiff(t)
	diff := scmPath("diff", "/diff?base=main&head=feature")

	r := w.get(t, diff+"&path=docs/**&path=*.md", w.ownerKey)
	if _, paths := r.files(); !slices.Equal(paths, []string{"docs/api/x.txt", "docs/guide.md", "notes.md"}) || r.stat("files_changed") != 3 || r.stat("insertions") != 3 {
		t.Errorf("filtered by docs/** and *.md: %s, want docs/api/x.txt, docs/guide.md and notes.md, counted alone", r.body)
	}
	f, _ := w.get(t, diff+"&patches=false", w.ownerKey).files()
	for p, o := range f {
		if _, has := o["patch"]; has {
			t.Errorf("with patches=false, %s has a patch", p)
		}
	}
	f, _ = w.get(t, diff+"&context=0", w.ownerKey).files()
	p0, _ := f["edit.txt"]["patch"].(string)
	for line := range strings.Lines(p0) {
		if strings.HasPrefix(line, " ") {
			t.Errorf("with context=0, the patch of edit.txt has the context line %q", line)
		}
	}
	p20 := w.get(t, diff+"&context=20", w.ownerKey)
	f, _ = p20.files()
	if p, _ := f["edit.txt"]["patch"].(string); p20.status != http.StatusOK || !strings.Contains(p, "\n one\n two\n-three\n+3a\n+3b\n four\n five\n") {
		t.Errorf("with context=20: %d, the patch of edit.txt %q; want every line around the change", p20.status, p)
	}

	text := w.get(t, diff+"&format=patch", w.ownerKey)
	if text.status != http.StatusOK || !strings.HasPrefix(text.header.Get("Content-Type"), "text/x-diff") {
		t.Fatalf("format=patch: %d, Content-Type %q, want 200 text/x-diff", text.status, text.header.Get("Content-Type"))
	}
	checkApplies(t, w.workspaces.RepoPath("diff"), id["mb"], text.body)
}

// checkApplies checks that git apply --check accepts patch in a work tree
// that holds the tree of the commit of the repository repo.
func checkApplies(t *testing.T, repo, commit string, patch []byte) {
	t.Helper()
	work := t.TempDir()
	for _, args := range [][]string{{"clone", "--quiet", "--no-checkout", repo, "."}, {"checkout", "--quiet", "--detach", commit}, {"apply", "--check"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Stdin = work, bytes.NewReader(patch)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s\nthe patch:\n%s", args[0], err, out, patch)
		}
	}
}

func TestDiff_CutsADiffOverTheLimits(t *testing.T) {
	w := newWorld(t)
	// One stream: the empty root commit empty on main; many, adding 5,001
	// one-line files; wide, adding three text files of 3 MiB each.
	var s strings.Builder
	s.WriteString("commit refs/heads/main\nmark :1\n" + testCommitter + "data 5\nempty\n\nblob\nmark :2\ndata 2\nx\n\n" +
		"commit refs/heads/many\n" + testCommitter + "data 4\nmany\nfrom :1\n")
	for i := range 5001 {
		fmt.Fprintf(&s, "M 100644 :2 f%04d.txt\n", i)
	}
	s.WriteString("\ncommit refs/heads/wide\n" + testCommitter + "data 4\nwide\nfrom :1\n")
	big := strings.Repeat(strings.Repeat("w", 63)+"\n", 3<<20/64)
	for _, name := range []string{"w1.txt", "w2.txt", "w3.txt"} {
		s.WriteString("M 100644 inline " + name + "\ndata " + strconv.Itoa(len(big)) + "\n" + big + "\n")
	}
	empty := strings.TrimSpace(gitrepotest.Git(t, w.addWorkspace(t, "cut", w.org, s.String()+"\n"), "rev-parse", "main"))

	r := w.get(t, scmPath("cut", "/diff?base="+empty+"&head=many"), w.ownerKey)
	if _, paths := r.files(); len(paths) != 5000 || paths[4999] != "f4999.txt" || r.json["truncated"] != true || r.stat("files_changed") != 5001 {
		t.Errorf("5,001 files: %d files, truncated %v, files_changed %d; want the first 5,000, true and 5,001", len(paths), r.json["truncated"], r.stat("files_changed"))
	}

	wide := scmPath("cut", "/diff?base="+empty+"&head=wide")
	rw := w.get(t, wide, w.ownerKey)
	f, paths := rw.files()
	total := 0
	for _, p := range paths {
		patch, _ := f[p]["patch"].(string)
		total += len(patch)
	}
	if _, last := f["w3.txt"]["patch"]; len(paths) != 3 || f["w1.txt"]["patch"] == nil || f["w2.txt"]["patch"] == nil || last || total > 8<<20 {
		t.Errorf("three files of 3 MiB: %v, %d bytes of patch; want patches for the first two alone, at most 8 MiB", paths, total)
	}
	if rw.json["truncated"] != true || rw.stat("insertions") != 3*(3<<20/64) {
		t.Errorf("three files of 3 MiB: truncated %v, insertions %d; want true and every line counted", rw.json["truncated"], rw.stat("insertions"))
	}
	text := w.get(t, wide+"&format=patch", w.ownerKey)
	if text.header.Get("X-Parley-Truncated") != "true" || len(text.body) > 8<<20 || strings.Count(string(text.body), "diff --git ") != 2 {
		t.Errorf("format=patch: X-Parley-Truncated %q, %d bytes, %d files; want true, at most 8 MiB, the first two files whole",
			text.header.Get("X-Parley-Truncated"), len(text.body), strings.Count(string(text.body), "diff --git "))
	}
}

func TestDiff_RefusesAMalformedParameter(t *testing.T) {
	w := newWorld(t)
	for _, path := range []string{
		"/diff?head=main", "/diff?base=main", "/diff?base=main&head=feature/x&mode=three_dot",
		"/diff?base=main&head=feature/x&format=html", "/diff?base=main&head=feature/x&context=21",
		"/diff?base=main&head=feature/x&context=-1", "/diff?base=main&head=feature/x&context=x",
		"/diff?base=main&head=feature/x&patches=no", "/compare?base=main", "/compare?head=main",
	} {
		if r := w.get(t, scmPath("demo", path), w.ownerKey); r.status != http.StatusBadRequest || r.errorType() != "invalid_request" {
			t.Errorf("GET %s = %d %s, want 400 invalid_request", path, r.status, r.body)
		}
	}
}

func TestDiff_ResolvesBothRevisionsThroughTheResolver(t *testing.T) {
	w := newWorld(t)
	r := w.get(t, scmPath("demo", "/diff?base=v1&head=feature/x"), w.ownerKey)
	if r.status != http.StatusOK || r.json["base_sha"] != w.id["c1"] || r.json["head_sha"] != w.id["c3"] {
		t.Errorf("base=v1&head=feature/x: %d %s, want base_sha c1 and head_sha c3", r.status, r.body)
	}
	for _, path := range []string{"/diff?base=nope&head=main", "/diff?base=main&head=main~1"} {
		if r := w.get(t, scmPath("demo", path), w.ownerKey); r.status != http.StatusNotFound || r.errorType() != "ref_not_found" {
			t.Errorf("GET %s = %d %s, want 404 ref_not_found", path, r.status, r.body)
		}
	}
}
