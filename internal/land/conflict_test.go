package land_test

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

func TestAttribute_NamesTheLandedChangesThatTouchedEachConflictedFile(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	repo, err := (gitrepo.Git{}).Open(ctx, w.api)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range []struct {
		name string
		// target and change are what main and the change's branch, each
		// made on base, set a.txt to.
		target, change string
		landedBy       []any
	}{
		{"a path nobody landed", "l1\nl2\ntarget\n", "l1\nl2\nchange\n", []any{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			onto := commit(t, w.api, "target-"+strconv.Itoa(i), w.base, "target", map[string]string{"a.txt": c.target})
			head := commit(t, w.api, "change-"+strconv.Itoa(i), w.base, "change", map[string]string{"a.txt": c.change})
			_, _, report, err := repo.RebaseFromAs(ctx, w.base, head, onto, gitrepo.HubSignature(time.Now()))
			if err != nil || report == nil {
				t.Fatalf("replay = %+v, %v; want a conflict", report, err)
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
			if by, _ := file["landed_by"].([]any); file["path"] != "a.txt" || by == nil || !slices.Equal(by, c.landedBy) {
				t.Errorf("%s has landed_by %v, want %v", file["path"], file["landed_by"], c.landedBy)
			}
			if landed, _ := got["landed_changes"].([]any); landed == nil || len(landed) != 0 {
				t.Errorf("landed_changes is %v, want []", got["landed_changes"])
			}
		})
	}
}
