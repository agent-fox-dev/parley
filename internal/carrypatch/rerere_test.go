package carrypatch_test

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
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

func TestRerereList_ShowsTheEntryARebuildRecordsAndThenLearns(t *testing.T) {
	t.Parallel()
	const file = "src/net/dial.go"
	f := startRebuildFixtureFrom(t, map[string]string{file: "1\n2\n3\n"}, "", nil)
	c := gitrepotest.Commit(t, f.copy, "p1 dials", map[string]string{file: "1\n2\n3 p1\n"}, f.base)
	f.putPatch(t, "p1", c)
	up := f.moveUpstream(t, "U2", map[string]string{file: "1\n2\n3 upstream\n"})
	list := "/workspaces/" + f.slug + "/rerere"

	if out, _ := f.attemptWith(t, carrypatch.RebuildOptions{FailMode: new("fail_fast")}, nil); out.Kind != attemptPermanent || out.Reason != "conflict" {
		t.Fatalf("the first rebuild = %+v, want permanent with reason conflict", out)
	}
	l1 := f.get(t, list)
	if l1.status != http.StatusOK || len(l1.Items) != 1 || l1.Items[0]["path"] != file || l1.Items[0]["resolved"] != false {
		t.Fatalf("GET rerere after the conflict = %d %s, want one entry for %s, unresolved", l1.status, l1.raw, file)
	}
	first := l1.Items[0]

	// The developer merges the upstream into the patch branch and resolves.
	m := gitrepotest.Commit(t, f.copy, "Merge upstream", map[string]string{file: "1\n2\n3 resolved\n"}, c, up)
	gitrepotest.SetRef(t, f.copy, "refs/heads/p1", m)
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
	if e["path"] != file || e["resolved"] != true || err != nil || after.Before(before) {
		t.Errorf("the entry after the learning = %v, want %v resolved, at %s or later", e, first["id"], first["recorded_at"])
	}
}
