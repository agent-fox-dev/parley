package carrypatch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// rerereEntry is an entry the test records: its conflict id, the path it
// was met at, when it was recorded, and whether the copy's rr-cache holds
// a resolution for it.
type rerereEntry struct {
	id, path string
	at       time.Time
	resolved bool
}

// recordAt records each of the entries of the workspace slug through the
// entry store, at its own time.
func (h *readHub) recordAt(t *testing.T, slug string, entries ...rerereEntry) {
	t.Helper()
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, e := range entries {
			c := []gitrepo.RerereConflict{{ID: e.id, Path: e.path}}
			if err := carrypatch.RecordRerereEntries(ctx, tx, slug, c, e.at); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record the entries of %s: %v", slug, err)
	}
}

// rrCacheOf writes, in the rr-cache of the repository dir, the conflict
// directory of each of the entries, with a postimage when it is resolved,
// as git's rerere leaves them.
func rrCacheOf(t *testing.T, dir string, entries ...rerereEntry) {
	t.Helper()
	for _, e := range entries {
		conflict := filepath.Join(dir, "rr-cache", e.id)
		if err := os.MkdirAll(conflict, 0o755); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{"preimage": "<<<<<<<\nours\n=======\ntheirs\n>>>>>>>\n"}
		if e.resolved {
			files["postimage"] = "resolved\n"
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(conflict, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// filesIn is the content of every file under root, by its path there.
func filesIn(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		rel, _ := filepath.Rel(root, path)
		files[filepath.ToSlash(rel)] = string(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// storedEntries are the entry rows of the workspace slug, as the store
// holds them.
func (h *readHub) storedEntries(t *testing.T, slug string) []string {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT id, path, recorded_at FROM rerere_entries WHERE workspace_slug = ? ORDER BY id`, slug)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, path string
		var at int64
		if err := rows.Scan(&id, &path, &at); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s %d", id, path, at))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// ids are the id members of the list's items, in order.
func (a answer) ids() []string {
	out := make([]string, 0, len(a.Items))
	for _, it := range a.Items {
		out = append(out, fmt.Sprint(it["id"]))
	}
	return out
}

// rerereID is a conflict id made of the hex digit d.
func rerereID(d string) string { return strings.Repeat(d, 40) }

func TestRerereList(t *testing.T) {
	t.Parallel()
	h := startReadHub(t)
	const list = "/workspaces/%s/rerere"

	t.Run("every entry newest first, resolved as git holds it, and nothing changed or contacted", func(t *testing.T) {
		t.Parallel()
		origin, upstream := gitrepotest.NewOrigin(t), gitrepotest.NewOrigin(t)
		h.add(t, "demo", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone, func(w *workspace.Workspace) {
			w.GitURL, w.UpstreamURL = new(origin.URL()), new(upstream.URL())
		})
		copyDir := h.repoPath("demo")
		gitrepotest.InitBare(t, copyDir, "")
		rootsAt(t, copyDir, map[string]string{"refs/heads/deploy": "deploy"})
		tie := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
		entries := map[string]rerereEntry{
			"A": {rerereID("a"), "src/a.go", tie, true},
			"B": {rerereID("b"), "src/b.go", tie, false},
			"C": {rerereID("c"), "src/c.go", tie.Add(time.Hour + 5*time.Second + 700*time.Millisecond), true},
			"D": {rerereID("d"), "src/d.go", tie.Add(-time.Hour), false},
		}
		all := slices.Collect(maps.Values(entries))
		h.recordAt(t, "demo", all...)
		rrCacheOf(t, copyDir, all...)
		rowsBefore, filesBefore := h.storedEntries(t, "demo"), filesIn(t, copyDir)

		a := h.get(t, fmt.Sprintf(list, "demo"))
		want := []string{entries["C"].id, entries["A"].id, entries["B"].id, entries["D"].id}
		if a.status != http.StatusOK || !slices.Equal(a.ids(), want) || a.Next != nil {
			t.Fatalf("GET rerere = %d %v next %v (%s), want 200 %v and no next_cursor", a.status, a.ids(), a.Next, a.raw, want)
		}
		wantItems := []map[string]any{
			{"id": entries["C"].id, "path": "src/c.go", "resolved": true, "recorded_at": "2026-11-16T11:00:05Z"},
			{"id": entries["A"].id, "path": "src/a.go", "resolved": true, "recorded_at": "2026-11-16T10:00:00Z"},
			{"id": entries["B"].id, "path": "src/b.go", "resolved": false, "recorded_at": "2026-11-16T10:00:00Z"},
			{"id": entries["D"].id, "path": "src/d.go", "resolved": false, "recorded_at": "2026-11-16T09:00:00Z"},
		}
		for i, it := range a.Items {
			if !maps.Equal(it, wantItems[i]) {
				t.Errorf("item %d = %v, want exactly %v", i, it, wantItems[i])
			}
		}
		if got := h.storedEntries(t, "demo"); !slices.Equal(got, rowsBefore) {
			t.Errorf("the entry rows after the list = %v, want them unchanged, %v", got, rowsBefore)
		}
		if got := filesIn(t, copyDir); !maps.Equal(got, filesBefore) {
			t.Errorf("the files of the copy changed in the list: %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(filesBefore)))
		}
		if o, u := origin.Requests(), upstream.Requests(); o != 0 || u != 0 {
			t.Errorf("the origin and the upstream received %d and %d requests, want none", o, u)
		}
	})

	t.Run("an entry git cannot answer for is listed unresolved", func(t *testing.T) {
		t.Parallel()
		at := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
		gone, broken := rerereEntry{rerereID("e"), "e.go", at, true}, rerereEntry{rerereID("f"), "f.go", at, true}
		h.add(t, "nocopy", workspace.ModeCarryPatch, workspace.StatusFailed, workspace.MarkerNone)
		h.recordAt(t, "nocopy", gone)
		h.add(t, "broken", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
		copyDir := h.repoPath("broken")
		gitrepotest.InitBare(t, copyDir, "")
		h.recordAt(t, "broken", gone, broken)
		rrCacheOf(t, copyDir, gone)
		// Git cannot read a conflict whose directory is a file.
		if err := os.WriteFile(filepath.Join(copyDir, "rr-cache", broken.id), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		for slug, want := range map[string]map[string]bool{
			"nocopy": {gone.id: false},
			"broken": {gone.id: true, broken.id: false},
		} {
			a := h.get(t, fmt.Sprintf(list, slug))
			if a.status != http.StatusOK || len(a.Items) != len(want) {
				t.Errorf("GET rerere of %s = %d %s, want 200 with %d items", slug, a.status, a.raw, len(want))
				continue
			}
			for _, it := range a.Items {
				if it["resolved"] != want[fmt.Sprint(it["id"])] {
					t.Errorf("%s lists %v resolved %v, want %v", slug, it["id"], it["resolved"], want[fmt.Sprint(it["id"])])
				}
			}
		}
	})

	t.Run("pages hold every entry once, in the order of the list", func(t *testing.T) {
		t.Parallel()
		h.add(t, "paged", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
		copyDir := h.repoPath("paged")
		gitrepotest.InitBare(t, copyDir, "")
		tie := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
		entries := []rerereEntry{
			{rerereID("1"), "1.go", tie, false},
			{rerereID("2"), "2.go", tie, true},
			{rerereID("3"), "3.go", tie, false},
			{rerereID("4"), "4.go", tie.Add(time.Minute), false},
			{rerereID("5"), "5.go", tie.Add(-time.Minute), true},
		}
		h.recordAt(t, "paged", entries...)
		rrCacheOf(t, copyDir, entries...)

		whole := h.get(t, fmt.Sprintf(list, "paged")+"?limit=200")
		if whole.status != http.StatusOK || len(whole.Items) != 5 || whole.Next != nil {
			t.Fatalf("GET rerere?limit=200 = %d %s, want 200 with 5 items and no next_cursor", whole.status, whole.raw)
		}
		var paged []string
		cursor, pages := "", 0
		for {
			q := "?limit=2"
			if cursor != "" {
				q += "&cursor=" + url.QueryEscape(cursor)
			}
			a := h.get(t, fmt.Sprintf(list, "paged")+q)
			if a.status != http.StatusOK {
				t.Fatalf("GET rerere%s = %d %s, want 200", q, a.status, a.raw)
			}
			pages++
			paged = append(paged, a.ids()...)
			if a.Next == nil {
				break
			}
			if len(a.Items) != 2 || pages > 5 {
				t.Fatalf("page %d holds %v with a next_cursor, want 2 items and at most 3 pages", pages, a.ids())
			}
			cursor = *a.Next
		}
		if pages != 3 || !slices.Equal(paged, whole.ids()) {
			t.Errorf("the pages of limit 2 hold %v over %d pages, want %v over 3", paged, pages, whole.ids())
		}
	})

	t.Run("no entry is an empty list", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			slug     string
			status   workspace.Status
			withCopy bool
		}{
			{"pending", workspace.StatusPending, false},
			{"failed", workspace.StatusFailed, false},
			{"empty", workspace.StatusReady, true},
		} {
			h.add(t, tc.slug, workspace.ModeCarryPatch, tc.status, workspace.MarkerNone)
			if tc.withCopy {
				gitrepotest.InitBare(t, h.repoPath(tc.slug), "")
			}
			a := h.get(t, fmt.Sprintf(list, tc.slug))
			if a.status != http.StatusOK || !strings.Contains(string(a.raw), `"items":[]`) || a.Next != nil {
				t.Errorf("GET rerere of a %s workspace = %d %s, want 200 with items [] and next_cursor null", tc.status, a.status, a.raw)
			}
		}
	})
}

// dialFile is the file of the conflict the rebuild tests of the entries
// meet.
const dialFile = "src/net/dial.go"

// dialConflict is the rebuild fixture with the patch p1, whose commit c
// changes line 3 of src/net/dial.go, and an upstream whose commit up then
// changes the same line: a rebuild of the workspace meets a conflict in
// src/net/dial.go.
type dialConflict struct {
	*rebuildFixture
	c, up string
}

func startDialConflict(t *testing.T) dialConflict {
	t.Helper()
	f := startRebuildFixtureFrom(t, map[string]string{dialFile: "1\n2\n3\n"}, "", nil)
	c := gitrepotest.Commit(t, f.copy, "p1 dials", map[string]string{dialFile: "1\n2\n3 p1\n"}, f.base)
	f.putPatch(t, "p1", c)
	up := f.moveUpstream(t, "U2", map[string]string{dialFile: "1\n2\n3 upstream\n"})
	return dialConflict{rebuildFixture: f, c: c, up: up}
}

// resolveOnPatchBranch moves p1 to a merge of the upstream into it that
// resolves src/net/dial.go, as the developer would: the next rebuild
// learns the resolution from it. The copy is given the upstream's commit
// first, which a rebuild has not fetched yet when none ran.
func (f dialConflict) resolveOnPatchBranch(t *testing.T) {
	t.Helper()
	gitrepotest.Git(t, f.copy, "fetch", "--quiet", f.upstream.Dir(), f.up)
	m := gitrepotest.Commit(t, f.copy, "Merge upstream", map[string]string{dialFile: "1\n2\n3 resolved\n"}, f.c, f.up)
	gitrepotest.SetRef(t, f.copy, "refs/heads/p1", m)
}

func TestRerereList_ShowsTheEntryARebuildRecordsAndThenLearns(t *testing.T) {
	t.Parallel()
	f := startDialConflict(t)
	list := "/workspaces/" + f.slug + "/rerere"

	if out, _ := f.attemptWith(t, carrypatch.RebuildOptions{FailMode: new("fail_fast")}, nil); out.Kind != attemptPermanent || out.Reason != "conflict" {
		t.Fatalf("the first rebuild = %+v, want permanent with reason conflict", out)
	}
	l1 := f.get(t, list)
	if l1.status != http.StatusOK || len(l1.Items) != 1 || l1.Items[0]["path"] != dialFile || l1.Items[0]["resolved"] != false {
		t.Fatalf("GET rerere after the conflict = %d %s, want one entry for %s, unresolved", l1.status, l1.raw, dialFile)
	}
	first := l1.Items[0]

	f.resolveOnPatchBranch(t)
	f.advance(time.Minute)
	if out, id := f.attemptWith(t, carrypatch.RebuildOptions{FailMode: new("fail_fast")}, nil); out.Kind != attemptCompleted {
		t.Fatalf("the second rebuild = %+v (%v), want completed", out, f.patchResults(t, id))
	}
	l2 := f.get(t, list)
	i := slices.IndexFunc(l2.Items, func(it map[string]any) bool { return it["id"] == first["id"] })
	if l2.status != http.StatusOK || i < 0 {
		t.Fatalf("GET rerere after the learning = %d %s, want the entry %v", l2.status, l2.raw, first["id"])
	}
	e := l2.Items[i]
	before, _ := time.Parse(time.RFC3339, fmt.Sprint(first["recorded_at"]))
	after, err := time.Parse(time.RFC3339, fmt.Sprint(e["recorded_at"]))
	if e["path"] != dialFile || e["resolved"] != true || err != nil || after.Before(before) {
		t.Errorf("the entry after the learning = %v, want %v resolved, at %s or later", e, first["id"], first["recorded_at"])
	}
}

// forget sends DELETE /api/v1/workspaces/<slug>/rerere/<pathspec> as the
// owner, the pathspec as it is given, escaped or not, and decodes the
// answer.
func (h *readHub) forget(t *testing.T, slug, pathspec string) answer {
	t.Helper()
	status, body := h.Call(t, http.MethodDelete, "/api/v1/workspaces/"+slug+"/rerere/"+pathspec, h.key, nil)
	a := answer{status: status, raw: body}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &a); err != nil {
			t.Fatalf("DELETE rerere/%s of %s = %d %s: %v", pathspec, slug, status, body, err)
		}
	}
	return a
}

// rerereCopy stores the ready carry-patch workspace slug with a copy made
// by git init and records the entries, with their rr-cache directories,
// and returns the copy's directory.
func (h *readHub) rerereCopy(t *testing.T, slug string, entries ...rerereEntry) string {
	t.Helper()
	h.add(t, slug, workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
	dir := h.repoPath(slug)
	gitrepotest.InitBare(t, dir, "")
	h.recordAt(t, slug, entries...)
	rrCacheOf(t, dir, entries...)
	return dir
}

// filesWithout is files without those of the rr-cache directories of the
// entries.
func filesWithout(files map[string]string, entries ...rerereEntry) map[string]string {
	out := maps.Clone(files)
	for _, e := range entries {
		maps.DeleteFunc(out, func(path, _ string) bool { return strings.HasPrefix(path, "rr-cache/"+e.id+"/") })
	}
	return out
}

// rowsWithout is the stored entry rows without those of the entries.
func rowsWithout(rows []string, entries ...rerereEntry) []string {
	return slices.DeleteFunc(slices.Clone(rows), func(row string) bool {
		return slices.ContainsFunc(entries, func(e rerereEntry) bool { return strings.HasPrefix(row, e.id+" ") })
	})
}

func TestRerereForget(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	const list = "/workspaces/%s/rerere"
	when := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)

	// Runs before the parallel subtests, so that the git commands of the
	// hub are this subtest's alone.
	t.Run("an empty or option-like pathspec is refused before anything is read", func(t *testing.T) {
		e := rerereEntry{rerereID("a"), "-rf", when, true}
		dir := h.rerereCopy(t, "flags", e)
		h.add(t, "flagsbusy", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerArchive)
		h.add(t, "flagsplain", workspace.ModeStandard, workspace.StatusReady, workspace.MarkerNone)
		rows, files, git := h.storedEntries(t, "flags"), filesIn(t, dir), h.gitCommands()
		for _, slug := range []string{"flags", "flagsbusy", "flagsplain"} {
			for _, pathspec := range []string{"", "-rf", "--all", "%2Dx"} {
				a := h.forget(t, slug, pathspec)
				if a.status != http.StatusBadRequest || a.Error.ErrorType != "invalid_request" {
					t.Errorf("DELETE rerere/%s of %s = %d %s, want 400 invalid_request", pathspec, slug, a.status, a.raw)
				}
			}
		}
		if got := h.storedEntries(t, "flags"); !slices.Equal(got, rows) {
			t.Errorf("the entry rows after the refusals = %v, want them unchanged, %v", got, rows)
		}
		if got := filesIn(t, dir); !maps.Equal(got, files) {
			t.Errorf("the files of the copy changed in the refusals: %v", slices.Sorted(maps.Keys(got)))
		}
		if got := h.gitCommands(); !slices.Equal(got, git) {
			t.Errorf("the hub ran git in the refusals: %q", got[len(git):])
		}
	})

	t.Run("an id removes its entry alone, also beside an entry whose path is that id", func(t *testing.T) {
		t.Parallel()
		x, y := rerereEntry{rerereID("1"), "src/x.go", when, true}, rerereEntry{rerereID("2"), "src/y.go", when, false}
		z := rerereEntry{rerereID("3"), x.id, when.Add(time.Minute), true}
		dir := h.rerereCopy(t, "byid", x, y, z)
		rows, files := h.storedEntries(t, "byid"), filesIn(t, dir)

		if a := h.forget(t, "byid", x.id); a.status != http.StatusNoContent || len(a.raw) != 0 {
			t.Fatalf("DELETE rerere/<id of X> = %d %q, want 204 with an empty body", a.status, a.raw)
		}
		if ids := h.get(t, fmt.Sprintf(list, "byid")).ids(); slices.Contains(ids, x.id) || !slices.Contains(ids, y.id) || !slices.Contains(ids, z.id) {
			t.Errorf("the list after the forget of X holds %v, want Y and Z and not X", ids)
		}
		if got, want := h.storedEntries(t, "byid"), rowsWithout(rows, x); !slices.Equal(got, want) {
			t.Errorf("the entry rows after the forget of X = %v, want %v", got, want)
		}
		if got, want := filesIn(t, dir), filesWithout(files, x); !maps.Equal(got, want) {
			t.Errorf("the files of the copy after the forget of X = %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
		}
	})

	t.Run("a path removes every entry of that path, compared byte for byte", func(t *testing.T) {
		t.Parallel()
		dial1, dial2 := rerereEntry{rerereID("4"), "src/net/dial.go", when, true}, rerereEntry{rerereID("5"), "src/net/dial.go", when, false}
		listen, readMe := rerereEntry{rerereID("6"), "src/net/listen.go", when, true}, rerereEntry{rerereID("7"), "docs/read me.md", when, true}
		for i, tc := range []struct {
			pathspec, path string
			removed        []rerereEntry
		}{
			{"src/net/dial.go", "src/net/dial.go", []rerereEntry{dial1, dial2}},
			{"docs/read%20me.md", "docs/read me.md", []rerereEntry{readMe}},
		} {
			slug := fmt.Sprintf("bypath%d", i)
			dir := h.rerereCopy(t, slug, dial1, dial2, listen, readMe)
			rows, files := h.storedEntries(t, slug), filesIn(t, dir)
			if a := h.forget(t, slug, tc.pathspec); a.status != http.StatusNoContent || len(a.raw) != 0 {
				t.Errorf("DELETE rerere/%s = %d %q, want 204 with an empty body", tc.pathspec, a.status, a.raw)
				continue
			}
			l := h.get(t, fmt.Sprintf(list, slug))
			paths := make([]any, 0, len(l.Items))
			for _, it := range l.Items {
				paths = append(paths, it["path"])
			}
			if slices.Contains(paths, any(tc.path)) || !slices.Contains(paths, any(listen.path)) {
				t.Errorf("the list after DELETE rerere/%s holds the paths %v, want none %q and %q still", tc.pathspec, paths, tc.path, listen.path)
			}
			if got, want := h.storedEntries(t, slug), rowsWithout(rows, tc.removed...); !slices.Equal(got, want) {
				t.Errorf("the entry rows after DELETE rerere/%s = %v, want %v", tc.pathspec, got, want)
			}
			if got, want := filesIn(t, dir), filesWithout(files, tc.removed...); !maps.Equal(got, want) {
				t.Errorf("the files of the copy after DELETE rerere/%s = %v, want %v", tc.pathspec, slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
			}
		}
	})

	t.Run("a pathspec that is no entry's id or path is not found and removes nothing", func(t *testing.T) {
		t.Parallel()
		x := rerereEntry{rerereID("8"), "src/net/dial.go", when, true}
		dirs := map[string]string{"nomatch": h.rerereCopy(t, "nomatch", x), "noentry": h.rerereCopy(t, "noentry")}
		for slug, pathspecs := range map[string][]string{
			// The dots are escaped, so that they reach the hub as they are.
			"nomatch": {rerereID("9"), "src/other.go", "src/net", "SRC/NET/DIAL.GO", "%2E%2E/rr-cache/" + x.id, "%2E%2E", "refs/heads/main"},
			"noentry": {x.id, x.path},
		} {
			rows, files := h.storedEntries(t, slug), filesIn(t, dirs[slug])
			for _, pathspec := range pathspecs {
				a := h.forget(t, slug, pathspec)
				if a.status != http.StatusNotFound || a.Error.ErrorType != "not_found" || a.Error.Message != "resolution not found" {
					t.Errorf("DELETE rerere/%s of %s = %d %s, want 404 not_found 'resolution not found'", pathspec, slug, a.status, a.raw)
				}
			}
			if got := h.storedEntries(t, slug); !slices.Equal(got, rows) {
				t.Errorf("the entry rows of %s after the refusals = %v, want them unchanged, %v", slug, got, rows)
			}
			if got := filesIn(t, dirs[slug]); !maps.Equal(got, files) {
				t.Errorf("the files of the copy of %s changed in the refusals: %v", slug, slices.Sorted(maps.Keys(got)))
			}
		}
	})

	t.Run("a forget touches no job, patch or ref and writes no event", func(t *testing.T) {
		t.Parallel()
		h.carryWith(t, "quiet", nil, carrypatch.StatusActive, carrypatch.StatusActive)
		dir := h.repoPath("quiet")
		gitrepotest.InitBare(t, dir, "")
		rootsAt(t, dir, map[string]string{"refs/heads/deploy": "deploy", "refs/heads/patch/1": "p1", "refs/heads/patch/2": "p2"})
		completed := h.queueRebuild(t, "quiet")
		h.drive(t, completed, jobs.StatusCompleted)
		h.queueRebuild(t, "quiet")
		e1, e2 := rerereEntry{rerereID("b"), "a.go", when, true}, rerereEntry{rerereID("c"), "b.go", when, false}
		h.recordAt(t, "quiet", e1, e2)
		rrCacheOf(t, dir, e1, e2)
		for _, tc := range []struct {
			name, pathspec string
			status         int
		}{
			{"by id", e1.id, http.StatusNoContent},
			{"by path", e2.path, http.StatusNoContent},
			{"answered 404", "c.go", http.StatusNotFound},
		} {
			before := h.previewState(t, "quiet")
			if a := h.forget(t, "quiet", tc.pathspec); a.status != tc.status {
				t.Errorf("the forget %s = %d %s, want %d", tc.name, a.status, a.raw, tc.status)
			}
			if after := h.previewState(t, "quiet"); !reflect.DeepEqual(after, before) {
				t.Errorf("the forget %s changed the refs, patches, jobs or the event log:\n got %+v\nwant %+v", tc.name, after, before)
			}
		}
	})

	t.Run("two forgets of one entry at once remove it once", func(t *testing.T) {
		t.Parallel()
		dir := h.rerereCopy(t, "race")
		for i := range 50 {
			e := rerereEntry{fmt.Sprintf("%040x", i+1), "race.go", when, true}
			h.recordAt(t, "race", e)
			rrCacheOf(t, dir, e)
			start := make(chan struct{})
			var wg sync.WaitGroup
			statuses := make([]int, 2)
			for j := range statuses {
				wg.Go(func() {
					<-start
					statuses[j] = h.forget(t, "race", e.id).status
				})
			}
			close(start)
			wg.Wait()
			slices.Sort(statuses)
			if !slices.Equal(statuses, []int{204, 204}) && !slices.Equal(statuses, []int{204, 404}) {
				t.Errorf("round %d: two forgets at once answered %v, want 204 and 204 or 404", i, statuses)
			}
			if ids := h.get(t, fmt.Sprintf(list, "race")).ids(); slices.Contains(ids, e.id) {
				t.Errorf("round %d: the list after the forgets holds %v, want the entry gone", i, ids)
			}
			if _, err := os.Stat(filepath.Join(dir, "rr-cache", e.id)); !os.IsNotExist(err) {
				t.Errorf("round %d: rr-cache/%s after the forgets: %v, want it gone", i, e.id, err)
			}
		}
	})
}

func TestRerereForget_ARebuildAfterTheForgetMeetsTheConflictAgain(t *testing.T) {
	t.Parallel()
	f := startDialConflict(t)
	f.resolveOnPatchBranch(t)
	if out, id := f.attemptWith(t, continueRebase, nil); out.Kind != attemptCompleted {
		t.Fatalf("the rebuild that learns = %+v (%v), want completed", out, f.patchResults(t, id))
	}
	// The patch branch goes back to the tip that conflicts: the resolution
	// replays.
	gitrepotest.SetRef(t, f.copy, "refs/heads/p1", f.c)
	out, id := f.attemptWith(t, continueRebase, nil)
	if results := f.patchResults(t, id); out.Kind != attemptCompleted || len(results) != 1 || results[0]["status"] != "success" {
		t.Fatalf("the rebuild with the resolution = %+v %v, want completed with p1 a success", out, results)
	}

	if a := f.forget(t, f.slug, dialFile); a.status != http.StatusNoContent {
		t.Fatalf("DELETE rerere/%s = %d %s, want 204", dialFile, a.status, a.raw)
	}
	out, id = f.attemptWith(t, continueRebase, nil)
	results := f.patchResults(t, id)
	if out.Kind != attemptCompleted || len(results) != 1 || results[0]["status"] != "conflict" ||
		!strings.Contains(fmt.Sprint(results[0]["conflict_files"]), dialFile) {
		t.Fatalf("the rebuild after the forget = %+v %v, want completed with p1 a conflict in %s", out, results, dialFile)
	}
	l := f.get(t, "/workspaces/"+f.slug+"/rerere")
	if l.status != http.StatusOK || len(l.Items) != 1 || l.Items[0]["path"] != dialFile || l.Items[0]["resolved"] != false {
		t.Errorf("GET rerere after the rebuild = %d %s, want one entry for %s, unresolved", l.status, l.raw, dialFile)
	}
}
