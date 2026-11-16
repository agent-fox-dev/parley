package land_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

func TestAttribute_NamesTheLandedChangesThatTouchedEachConflictedFile(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// landed lands a change setting a.txt, other one adding other.txt,
		// direct writes a commit setting a.txt to main itself;
		// unrelatedBase puts a base that main does not hold in the report.
		landed, other, direct, unrelatedBase bool
	}{
		{name: "a path nobody landed", direct: true},
		{name: "a path a landed change touched", landed: true},
		{name: "a landed change that touched another path", other: true, direct: true},
		{name: "a base the target does not hold", landed: true, unrelatedBase: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			ctx := context.Background()
			repo, err := (gitrepo.Git{}).Open(ctx, w.api)
			if err != nil {
				t.Fatal(err)
			}
			if c.other {
				w.landNow(t, w.open(t, "other"), w.caller(), nil)
			}
			landedBy, landedChanges := []any{}, []any{}
			if c.landed {
				commit(t, w.api, "one", w.base, "one", map[string]string{"a.txt": "l1\nl2\none\n", "one.txt": "one\n"})
				one := w.landNow(t, w.openBranch(t, "one", w.caller()), w.caller(), nil)
				if !c.unrelatedBase {
					ch := w.readChange(t, one.Covered[0].Number)
					landedBy = []any{float64(ch.Number)}
					landedChanges = []any{map[string]any{"number": float64(ch.Number), "title": ch.Title,
						"landed_sha": *one.LandedSHA, "session_id": nil}}
				}
			}
			if c.direct {
				commit(t, w.api, "main", head(t, w.api, "main"), "direct", map[string]string{"a.txt": "l1\nl2\ndirect\n"})
			}
			change := commit(t, w.api, "change", w.base, "change", map[string]string{"a.txt": "l1\nl2\nchange\n"})
			_, _, report, err := repo.RebaseFromAs(ctx, w.base, change, head(t, w.api, "main"), gitrepo.HubSignature(time.Now()))
			if err != nil || report == nil {
				t.Fatalf("replay = %+v, %v; want a conflict", report, err)
			}
			if c.unrelatedBase {
				report.BaseSHA = commit(t, w.api, "elsewhere", "", "root", map[string]string{"z.txt": "z\n"})
			}

			if err := w.land.Attribute(ctx, "api", report); err != nil {
				t.Fatalf("Attribute: %v", err)
			}

			got := asJSON(t, report)
			files, _ := got["files"].([]any)
			if len(files) != 1 {
				t.Fatalf("the report's files are %v; want a.txt alone", got["files"])
			}
			file, _ := files[0].(map[string]any)
			if file["path"] != "a.txt" || !reflect.DeepEqual(file["landed_by"], any(landedBy)) {
				t.Errorf("%s has landed_by %v, want %v", file["path"], file["landed_by"], landedBy)
			}
			if !reflect.DeepEqual(got["landed_changes"], any(landedChanges)) {
				t.Errorf("landed_changes is %v, want %v", got["landed_changes"], landedChanges)
			}
		})
	}
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
