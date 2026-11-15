package change_test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// The diffs share one world: main gains b.txt and a longer a.txt (main1);
// change 1 from feat has version 1 on main1 (a7 becomes A7 in a.txt, b.txt
// gains b4), version 2 rebased onto main2, where main gained a line at the
// top of a.txt, with the same content, and version 3, where b.txt gains
// b5 and c.txt is added. The last subtest replaces demo's repository.
func TestDiff_ServesAVersionsDiffAndTheInterdiffOfTwoVersions(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	a := "a1\na2\na3\na4\na5\na6\na7\na8\n"
	featA := strings.Replace(a, "a7", "A7", 1)
	main1 := commit(t, w.demo, "main", w.base, "Main 1\n", map[string]string{"a.txt": a, "b.txt": "b1\nb2\nb3\n"})
	head1 := commit(t, w.demo, "feat", main1, "Feat\n\nWhy.\n", map[string]string{"a.txt": featA, "b.txt": "b1\nb2\nb3\nb4\n"})
	w.openBranch(t, "feat", nil)
	main2 := commit(t, w.demo, "main", main1, "Main 2\n", map[string]string{"a.txt": "main gained\n" + a})
	head2 := commit(t, w.demo, "feat", main2, "Feat\n\nWhy.\n", map[string]string{"a.txt": "main gained\n" + featA, "b.txt": "b1\nb2\nb3\nb4\n"})
	w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: head2, Kind: change.KindRebase, Caller: w.ownerCaller()})
	head3 := commit(t, w.demo, "feat", head2, "More\n", map[string]string{"b.txt": "b1\nb2\nb3\nb4\nb5 of version 3\n", "c.txt": "c\n"})
	w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "feat", Head: head3, Kind: change.KindPush, Caller: w.ownerCaller()})
	versions := w.versions(t, "demo", 1)
	if len(versions) != 3 || versions[1]["patch_equivalent_to"] != 1.0 || versions[2]["base_sha"] != main2 {
		t.Fatalf("versions = %v, want 3, version 2 patch-equivalent to 1, version 3 based on main2", versions)
	}
	diff := func(t *testing.T, query string) (int, map[string]any) {
		t.Helper()
		status, out, _ := w.get(t, "/workspaces/demo/changes/1/diff"+query, w.ownerKey)
		return status, out
	}

	t.Run("the diff of a version", func(t *testing.T) {
		status, got := diff(t, "")
		if status != http.StatusOK || got["version"] != 3.0 || got["against"] != nil || !hasMember(got, "against") {
			t.Fatalf("GET diff = %d %v, want 200 of version 3 against null", status, got)
		}
		if got["base_sha"] != main2 || got["merge_base_sha"] != main2 || got["head_sha"] != head3 || got["truncated"] != false {
			t.Errorf("GET diff = %v, want base and merge base %s, head %s, not truncated", got, main2, head3)
		}
		if paths := filePaths(got); !reflect.DeepEqual(paths, []string{"a.txt", "b.txt", "c.txt"}) {
			t.Errorf("files = %v, want a.txt, b.txt, c.txt", paths)
		}
		for _, f := range files(got) {
			if p, _ := f["patch"].(string); f["status"] == nil || f["insertions"] == nil || f["deletions"] == nil || p == "" || hasMember(f, "changed") {
				t.Errorf("file %v, want a status, numbers and a patch", f)
			}
		}
		status, got = diff(t, "?version=1")
		if status != http.StatusOK || got["version"] != 1.0 || got["head_sha"] != head1 || got["base_sha"] != main1 ||
			!reflect.DeepEqual(filePaths(got), []string{"a.txt", "b.txt"}) {
			t.Errorf("GET diff?version=1 = %d %v, want version 1 from %s to %s with a.txt, b.txt", status, got, main1, head1)
		}
		if status, latest := diff(t, "?version=latest"); status != http.StatusOK || latest["version"] != 3.0 {
			t.Errorf("GET diff?version=latest = %d %v, want version 3", status, latest)
		}
	})

	t.Run("the interdiff of two versions", func(t *testing.T) {
		status, got := diff(t, "?version=3&against=1")
		if status != http.StatusOK || got["version"] != 3.0 || got["against"] != 1.0 || got["merge_base_sha"] != nil ||
			!hasMember(got, "merge_base_sha") || got["base_sha"] != head1 || got["head_sha"] != head3 {
			t.Fatalf("GET diff?version=3&against=1 = %d %v, want 200 from %s to %s, merge base null", status, got, head1, head3)
		}
		byPath := filesByPath(got)
		if len(byPath) != 3 {
			t.Errorf("files = %v, want a.txt, b.txt and c.txt, and not main's own changes", filePaths(got))
		}
		if f := byPath["a.txt"]; f["changed"] != false || hasMember(f, "patch") {
			t.Errorf("a.txt = %v, want unchanged without a patch", f)
		}
		b := byPath["b.txt"]
		if p, _ := b["patch"].(string); b["changed"] != true || !strings.Contains(p, "+b5 of version 3") || strings.Contains(p, "main gained") {
			t.Errorf("b.txt = %v, want changed with the line of version 3 and none of main's", b)
		}
		if c := byPath["c.txt"]; c["changed"] != true || c["status"] != "added" {
			t.Errorf("c.txt = %v, want changed and added", c)
		}
		if stats, _ := got["stats"].(map[string]any); stats["files_changed"] != 2.0 || stats["insertions"] != 2.0 || stats["deletions"] != 0.0 {
			t.Errorf("stats = %v, want 2 files, 2 insertions, no deletion", got["stats"])
		}
		_, back := diff(t, "?version=1&against=3")
		if c := filesByPath(back)["c.txt"]; c["changed"] != true || c["status"] != "deleted" {
			t.Errorf("version 1 against 3: c.txt = %v, want changed and deleted", c)
		}
	})

	t.Run("patch-equivalent versions and a version with itself", func(t *testing.T) {
		for _, query := range []string{"?version=2&against=1", "?version=1&against=2", "?version=3&against=3"} {
			status, got := diff(t, query)
			if status != http.StatusOK || len(files(got)) == 0 {
				t.Errorf("GET diff%s = %d %v, want 200 listing the files", query, status, got)
				continue
			}
			for _, f := range files(got) {
				if f["changed"] != false || hasMember(f, "patch") {
					t.Errorf("GET diff%s: %v, want unchanged without a patch", query, f)
				}
			}
			if want := map[string]any{"files_changed": 0.0, "insertions": 0.0, "deletions": 0.0}; !reflect.DeepEqual(got["stats"], want) {
				t.Errorf("GET diff%s: stats = %v, want %v", query, got["stats"], want)
			}
		}
	})

	t.Run("paths, patches and the patch format", func(t *testing.T) {
		for _, c := range []struct {
			query string
			paths []string
		}{
			{"?path=b.*", []string{"b.txt"}},
			{"?path=a.txt&path=c.txt", []string{"a.txt", "c.txt"}},
			{"?path=zzz/**", []string{}},
			{"?against=1&path=b.*", []string{"b.txt"}},
		} {
			status, got := diff(t, c.query)
			stats, _ := got["stats"].(map[string]any)
			if paths := filePaths(got); status != http.StatusOK || !reflect.DeepEqual(paths, c.paths) || stats["files_changed"] != float64(len(c.paths)) {
				t.Errorf("GET diff%s = %d %v, want files %v and stats over them", c.query, status, got, c.paths)
			}
		}
		for _, query := range []string{"?patches=false", "?patches=false&against=1"} {
			status, got := diff(t, query)
			for _, f := range files(got) {
				if hasMember(f, "patch") {
					t.Errorf("GET diff%s: %v, want no patch", query, f)
				}
			}
			if status != http.StatusOK || len(files(got)) != 3 {
				t.Errorf("GET diff%s = %d %v, want 200 with 3 files", query, status, got)
			}
		}
		for _, c := range []struct {
			query, onto string
			has, hasNot []string
		}{
			{"?format=patch", main2, []string{"a.txt", "b.txt", "c.txt"}, nil},
			{"?format=patch&against=1", head1, []string{"b.txt", "c.txt"}, []string{"a.txt"}},
		} {
			status, header, body := w.getText(t, "/workspaces/demo/changes/1/diff"+c.query)
			if status != http.StatusOK || !strings.HasPrefix(header.Get("Content-Type"), "text/x-diff") {
				t.Errorf("GET diff%s = %d %s %q, want 200 text/x-diff", c.query, status, header.Get("Content-Type"), body)
				continue
			}
			for _, name := range c.has {
				if !strings.Contains(string(body), "b/"+name) {
					t.Errorf("GET diff%s does not patch %s: %s", c.query, name, body)
				}
			}
			for _, name := range c.hasNot {
				if strings.Contains(string(body), name) {
					t.Errorf("GET diff%s patches %s: %s", c.query, name, body)
				}
			}
			applies(t, w.demo, c.onto, body)
		}
	})

	t.Run("a malformed or unknown version", func(t *testing.T) {
		for _, c := range []struct {
			query     string
			status    int
			errorType string
		}{
			{"?version=x", http.StatusBadRequest, "invalid_request"},
			{"?version=0", http.StatusBadRequest, "invalid_request"},
			{"?against=latest", http.StatusBadRequest, "invalid_request"},
			{"?against=-2", http.StatusBadRequest, "invalid_request"},
			{"?format=html", http.StatusBadRequest, "invalid_request"},
			{"?patches=maybe", http.StatusBadRequest, "invalid_request"},
			{"?version=9", http.StatusNotFound, "not_found"},
			{"?against=9", http.StatusNotFound, "not_found"},
		} {
			if status, got := diff(t, c.query); status != c.status || errorType(got) != c.errorType {
				t.Errorf("GET diff%s = %d %v, want %d %s", c.query, status, got, c.status, c.errorType)
			}
		}
	})

	// Change 2 adds d.txt and e.txt; main then gains the same d.txt, so
	// version 2, rebased, touches only e.txt.
	t.Run("a changed file the same at both heads", func(t *testing.T) {
		commit(t, w.demo, "dup", main2, "Dup\n\nWhy.\n", map[string]string{"d.txt": "d\n", "e.txt": "e\n"})
		w.openBranch(t, "dup", nil)
		main3 := commit(t, w.demo, "main", main2, "Main 3\n", map[string]string{"d.txt": "d\n"})
		dup2 := commit(t, w.demo, "dup", main3, "Dup\n\nWhy.\n", map[string]string{"e.txt": "e\n"})
		w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "dup", Head: dup2, Kind: change.KindRebase, Caller: w.ownerCaller()})
		status, got, _ := w.get(t, "/workspaces/demo/changes/2/diff?against=1", w.ownerKey)
		byPath := filesByPath(got)
		if d := byPath["d.txt"]; status != http.StatusOK || d["changed"] != true || d["status"] != "modified" || d["insertions"] != 0.0 || d["patch"] != "" {
			t.Errorf("GET changes/2/diff?against=1 = %d %v, want d.txt changed, modified, with an empty patch", status, got)
		}
		if e := byPath["e.txt"]; e["changed"] != false {
			t.Errorf("e.txt = %v, want unchanged", e)
		}
		if want := map[string]any{"files_changed": 1.0, "insertions": 0.0, "deletions": 0.0}; !reflect.DeepEqual(got["stats"], want) {
			t.Errorf("stats = %v, want %v", got["stats"], want)
		}
	})

	// Change 3 edits x.txt; its version 2, on the same base, renames x.txt
	// to y.txt with the same edit.
	t.Run("a file renamed between versions", func(t *testing.T) {
		x := "x1\nx2\nx3\nx4\nx5\n"
		withX := commit(t, w.demo, "main", "main", "Main x\n", map[string]string{"x.txt": x})
		commit(t, w.demo, "ren", withX, "Ren\n\nWhy.\n", map[string]string{"x.txt": x + "x6\n"})
		w.openBranch(t, "ren", nil)
		ren2 := commit(t, w.demo, "ren", withX, "Ren\n\nWhy.\n", map[string]string{"x.txt": gone, "y.txt": x + "x6\n"})
		w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "ren", Head: ren2, Kind: change.KindPush, Caller: w.ownerCaller()})
		for _, c := range []struct{ query, path, oldPath string }{
			{"?version=2&against=1", "y.txt", "x.txt"},
			{"?version=1&against=2", "x.txt", "y.txt"},
		} {
			status, got, _ := w.get(t, "/workspaces/demo/changes/3/diff"+c.query, w.ownerKey)
			fs := files(got)
			if status != http.StatusOK || len(fs) != 1 {
				t.Errorf("GET changes/3/diff%s = %d %v, want 200 with the one entry of the rename", c.query, status, got)
				continue
			}
			if f := fs[0]; f["path"] != c.path || f["old_path"] != c.oldPath || f["status"] != "renamed" || f["changed"] != true {
				t.Errorf("GET changes/3/diff%s: %v, want %s renamed from %s, changed", c.query, f, c.path, c.oldPath)
			}
			if want := map[string]any{"files_changed": 1.0, "insertions": 0.0, "deletions": 0.0}; !reflect.DeepEqual(got["stats"], want) {
				t.Errorf("GET changes/3/diff%s: stats = %v, want %v", c.query, got["stats"], want)
			}
		}
	})

	// A reclone: demo's repository again, holding only main.
	t.Run("commits missing from the repository", func(t *testing.T) {
		if err := os.RemoveAll(w.demo); err != nil {
			t.Fatal(err)
		}
		gitrepotest.Git(t, w.demo, "init", "--quiet", "--bare", "--initial-branch=main", w.demo)
		if _, err := gitrepotest.Run(w.demo, "commit refs/heads/main\n"+committer+data("Recloned")+"M 100644 inline a.txt\n"+data(a)+"\n",
			"fast-import", "--quiet"); err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{"", "?version=3&against=1"} {
			if status, got := diff(t, query); status != http.StatusNotFound || errorType(got) != "ref_not_found" {
				t.Errorf("GET diff%s = %d %v, want 404 ref_not_found", query, status, got)
			}
		}
		if status, got, _ := w.get(t, "/workspaces/demo/changes/1", w.ownerKey); status != http.StatusOK {
			t.Errorf("GET changes/1 = %d %v, want 200", status, got)
		}
		if status, got, _ := w.get(t, "/workspaces/demo/changes/1/versions", w.ownerKey); status != http.StatusOK || len(versionNumbers(got["items"])) != 3 {
			t.Errorf("GET versions = %d %v, want 200 with 3 versions", status, got)
		}
	})
}

// files are the file objects of a decoded diff answer.
func files(diff map[string]any) []map[string]any {
	list, _ := diff["files"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		f, _ := item.(map[string]any)
		out = append(out, f)
	}
	return out
}

// filePaths are the paths of the files of a decoded diff answer, in order.
func filePaths(diff map[string]any) []string {
	out := []string{}
	for _, f := range files(diff) {
		p, _ := f["path"].(string)
		out = append(out, p)
	}
	return out
}

// filesByPath are the files of a decoded diff answer by their paths.
func filesByPath(diff map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, f := range files(diff) {
		p, _ := f["path"].(string)
		out[p] = f
	}
	return out
}

// hasMember says whether the decoded object has the member name, null or
// not.
func hasMember(obj map[string]any, name string) bool {
	_, ok := obj[name]
	return ok
}

// getText reads path, relative to /api/v1, with the owner's key, and
// returns the status, the headers and the body of the answer.
func (w *world) getText(t *testing.T, path string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.URL("/api/v1"+path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.ownerKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, res.Header, body
}

// applies fails the test unless git apply --check accepts patch on the
// tree of commit in the repository at dir.
func applies(t *testing.T, dir, commit string, patch []byte) {
	t.Helper()
	tmp := t.TempDir()
	file := filepath.Join(tmp, "patch")
	if err := os.WriteFile(file, patch, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"read-tree", commit}, {"apply", "--check", "--cached", file}} {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_DIR="+dir, "GIT_INDEX_FILE="+filepath.Join(tmp, "index"),
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("git %v on %s: %v: %s\npatch:\n%s", args, commit, err, out, patch)
			return
		}
	}
}
