package land_test

import (
	"context"
	"reflect"
	"testing"
	"time"

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
