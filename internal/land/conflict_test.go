package land_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/land"
)

func TestAttribute_ReadsALandedChangeFromTheChangeComponentsLandedCommits(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.changes.RegisterSessionLinker(versionSessions{})
	ctx := context.Background()
	session := "11111111-1111-4111-8111-111111111111"
	commit(t, w.api, "one", w.base, "one", map[string]string{"a.txt": "l1\nl2\none\n"})
	opened := w.opened(t, change.OpenInput{Workspace: "api", SourceBranch: "one", Intent: new("why"),
		Kind: change.KindRef, Caller: w.caller(), SessionID: new(session)})
	landed := w.landNow(t, opened, w.caller(), nil)
	work := commit(t, w.api, "change", w.base, "change", map[string]string{"a.txt": "l1\nl2\nchange\n"})
	report := conflictOf(t, w.api, w.base, work, head(t, w.api, "main"))

	if err := w.land.Attribute(ctx, "api", report); err != nil {
		t.Fatalf("Attribute: %v", err)
	}

	want := []any{map[string]any{"number": float64(opened.Number), "title": opened.Title,
		"landed_sha": *landed.LandedSHA, "session_id": session}}
	if got := asJSON(t, report); !reflect.DeepEqual(got["landed_changes"], any(want)) {
		t.Errorf("landed_changes is %v, want %v", got["landed_changes"], want)
	}
}

func TestAttribute_ListsTheLandedChangesByNumberAndEachFilesOnesAscending(t *testing.T) {
	t.Parallel()
	session, titles := "s-1", map[int64]string{7: "seven", 8: "eight", 9: "nine"}
	for _, c := range []struct {
		name string
		// target are the commits main gets after the base, oldest first,
		// each with the files it sets and the landed change it belongs to
		// (0 for none).
		target []targetCommit
		// landedChanges are the numbers landed_changes lists, landedBy the
		// numbers each conflicted file lists.
		landedChanges []int64
		landedBy      map[string][]int64
	}{
		{
			name: "a landed change on a conflicted path, one on another path and a direct push",
			target: []targetCommit{
				{change: 7, files: map[string]string{"a.txt": "seven\n"}},
				{change: 8, files: map[string]string{"other.txt": "eight\n"}},
				{files: map[string]string{"b.txt": "direct\n"}},
			},
			landedChanges: []int64{7},
			landedBy:      map[string][]int64{"a.txt": {7}, "b.txt": {}},
		},
		{
			name: "landed changes that landed out of number order, one of them twice",
			target: []targetCommit{
				{change: 9, files: map[string]string{"a.txt": "nine\n"}},
				{change: 9, files: map[string]string{"b.txt": "nine\n"}},
				{change: 7, files: map[string]string{"a.txt": "seven\n", "b.txt": "seven\n"}},
			},
			landedChanges: []int64{7, 9},
			landedBy:      map[string][]int64{"a.txt": {7, 9}, "b.txt": {7, 9}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := bareRepo(t)
			base := commit(t, dir, "main", "", "base", map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
			// A change's landed_sha is the head its landing left: its last
			// commit.
			onto, of, landedSHA := base, map[string]int64{}, map[int64]string{}
			for i, tc := range c.target {
				onto = commit(t, dir, "main", onto, fmt.Sprintf("target %d", i), tc.files)
				if tc.change != 0 {
					of[onto], landedSHA[tc.change] = tc.change, onto
				}
			}
			lookup := landedTable{changes: map[string]land.CollidedChange{}}
			for sha, n := range of {
				lookup.changes[sha] = land.CollidedChange{Number: n, Title: titles[n], LandedSHA: landedSHA[n], SessionID: &session}
			}
			work := commit(t, dir, "work", base, "work", map[string]string{"a.txt": "work\n", "b.txt": "work\n"})
			report := conflictOf(t, dir, base, work, onto)

			if err := attributor(dir, lookup).Attribute(ctx, "api", report); err != nil {
				t.Fatalf("Attribute: %v", err)
			}

			got := asJSON(t, report)
			want := []any{}
			for _, n := range c.landedChanges {
				want = append(want, map[string]any{"number": float64(n), "title": titles[n], "landed_sha": landedSHA[n], "session_id": session})
			}
			if !reflect.DeepEqual(got["landed_changes"], any(want)) {
				t.Errorf("landed_changes is %v, want %v", got["landed_changes"], want)
			}
			files, _ := got["files"].([]any)
			if len(files) != len(c.landedBy) {
				t.Fatalf("the report's files are %v; want %v", files, slices.Sorted(maps.Keys(c.landedBy)))
			}
			for _, f := range files {
				file, _ := f.(map[string]any)
				path, _ := file["path"].(string)
				wantBy := []any{}
				for _, n := range c.landedBy[path] {
					wantBy = append(wantBy, float64(n))
				}
				if !reflect.DeepEqual(file["landed_by"], any(wantBy)) {
					t.Errorf("%s has landed_by %v, want %v", path, file["landed_by"], wantBy)
				}
			}
		})
	}
}

func TestConflict_KeepsTheReportWithEmptyLandedListsWhenNoLandedChangeCanBeNamed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// unrelatedBase bases the work on a history main does not hold;
		// lookupErr is the error of every lookup.
		unrelatedBase bool
		lookupErr     error
	}{
		{name: "a base on a branch unrelated to the target", unrelatedBase: true},
		{name: "a lookup that fails", lookupErr: errors.New("the mapping cannot be read")},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := bareRepo(t)
			files := map[string]string{"a.txt": "a\n", "b.txt": "b\n"}
			base := commit(t, dir, "main", "", "base", files)
			onto := commit(t, dir, "main", base, "seven", map[string]string{"a.txt": "seven\n", "b.txt": "seven\n"})
			lookup := landedTable{changes: map[string]land.CollidedChange{onto: {Number: 7, Title: "seven", LandedSHA: onto}}, err: c.lookupErr}
			if c.unrelatedBase {
				base = commit(t, dir, "elsewhere", "", "unrelated base", files)
			}
			work := commit(t, dir, "work", base, "work", map[string]string{"a.txt": "work\n", "b.txt": "work\n"})

			report := land.CompleteConflict(ctx, attributor(dir, lookup), "api", onto, work, base, conflictOf(t, dir, base, work, onto))

			got := asJSON(t, report)
			if !reflect.DeepEqual(got["landed_changes"], any([]any{})) {
				t.Errorf("landed_changes is %v, want []", got["landed_changes"])
			}
			entries, _ := got["files"].([]any)
			if len(entries) == 0 {
				t.Fatalf("the report has no files: %v", got)
			}
			for _, f := range entries {
				file, _ := f.(map[string]any)
				if !reflect.DeepEqual(file["landed_by"], any([]any{})) {
					t.Errorf("%s has landed_by %v, want []", file["path"], file["landed_by"])
				}
			}
		})
	}
}

// targetCommit is a commit of a target branch: the files it sets and the
// number of the landed change it belongs to, 0 for none.
type targetCommit struct {
	change int64
	files  map[string]string
}

// landedTable is a landed-change lookup backed by a table of commit id to
// landed change; err, when set, is the answer of every lookup.
type landedTable struct {
	changes map[string]land.CollidedChange
	err     error
}

func (l landedTable) LandedChange(_ context.Context, _, sha string) (land.CollidedChange, bool, error) {
	if l.err != nil {
		return land.CollidedChange{}, false, l.err
	}
	ch, found := l.changes[sha]
	return ch, found, nil
}

// bareRepo is a new bare repository whose initial branch is main.
func bareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	return dir
}

// attributor is a land component of the repository dir alone, whose
// attribution reads landed changes through lookup.
func attributor(dir string, lookup land.LandedLookup) *land.Component {
	c := new(land.Component)
	c.Bind(land.Deps{
		Repos:  func(ctx context.Context, _ string) (*gitrepo.Repo, error) { return gitrepo.Git{}.Open(ctx, dir) },
		Landed: lookup,
	})
	return c
}

// conflictOf is the conflict report of the replay of the commits of dir
// after base up to head onto onto, which must conflict.
func conflictOf(t *testing.T, dir, base, head, onto string) *gitrepo.ConflictReport {
	t.Helper()
	ctx := context.Background()
	repo, err := gitrepo.Git{}.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	report, err := repo.TrialRebaseFrom(ctx, base, head, onto)
	if err != nil || report == nil {
		t.Fatalf("replay = %+v, %v; want a conflict", report, err)
	}
	return report
}

func TestConflictReports_NameTheLandedChangesWhicheverOperationFoundThem(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	base := commit(t, w.api, "main", w.base, "f", map[string]string{"f.txt": "l1\nl2\nl3\n"})
	commit(t, w.api, "one", base, "one", map[string]string{"f.txt": "l1\nl2\none\n"})
	commit(t, w.api, "two", base, "two", map[string]string{"f.txt": "l1\nl2\ntwo\n"})
	commit(t, w.api, "three", base, "three", map[string]string{"f.txt": "l1\nl2\nthree\n"})
	one, two := w.openBranch(t, "one", w.caller()), w.openBranch(t, "two", w.caller())
	landed := w.landNow(t, one, w.caller(), nil)
	w.mergeable(t, two.Number, change.MergeableConflict)
	wantLanded := []any{map[string]any{"number": float64(one.Number), "title": one.Title,
		"landed_sha": *landed.LandedSHA, "session_id": nil}}

	// In this order: a restack's conflict replaces the one the check stored.
	for _, c := range []struct {
		name   string
		report func(t *testing.T) map[string]any
	}{
		{"the mergeability check", func(t *testing.T) map[string]any {
			var report map[string]any
			if err := json.Unmarshal(w.readChange(t, two.Number).Conflict, &report); err != nil {
				t.Fatalf("change %d stores the conflict %s: %v", two.Number, w.readChange(t, two.Number).Conflict, err)
			}
			return report
		}},
		{"the comparison", func(t *testing.T) map[string]any {
			status, answer := w.get(t, "/workspaces/api/scm/compare?base=main&head=three", w.ownerKey)
			if status != http.StatusOK {
				t.Fatalf("the comparison answered %d %v", status, answer)
			}
			report, _ := answer["conflict"].(map[string]any)
			return report
		}},
		{"the server-side rebase", func(t *testing.T) map[string]any {
			return conflictDetails(t, w, "/api/v1/workspaces/api/scm/rebase", map[string]any{"branch": "three", "onto": "main"})
		}},
		{"the restack", func(t *testing.T) map[string]any {
			return conflictDetails(t, w, fmt.Sprintf("/api/v1/workspaces/api/changes/%d/restack", two.Number), map[string]any{})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			report := c.report(t)
			if !reflect.DeepEqual(report["landed_changes"], any(wantLanded)) {
				t.Errorf("landed_changes is %v, want %v", report["landed_changes"], wantLanded)
			}
			files, _ := report["files"].([]any)
			file, _ := files[0].(map[string]any)
			if len(files) != 1 || file["path"] != "f.txt" || !reflect.DeepEqual(file["landed_by"], []any{float64(one.Number)}) {
				t.Errorf("the report's files are %v, want f.txt landed by %d", files, one.Number)
			}
		})
	}
}

// conflictDetails posts body to path with the owner's key and returns the
// conflict report of its 409 rebase_conflict answer.
func conflictDetails(t *testing.T, w *world, path string, body any) map[string]any {
	t.Helper()
	status, raw := w.Call(t, http.MethodPost, path, w.ownerKey, body)
	var answer map[string]any
	if err := json.Unmarshal(raw, &answer); err != nil || status != http.StatusConflict || errorType(answer) != "rebase_conflict" {
		t.Fatalf("POST %s answered %d %s, want 409 rebase_conflict", path, status, raw)
	}
	report, _ := answer["error"].(map[string]any)["details"].(map[string]any)
	return report
}
