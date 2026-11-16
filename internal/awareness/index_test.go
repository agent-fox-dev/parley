package awareness_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/awareness"
)

// renamed is content long enough for git to find a rename of it.
const renamed = "package a\n\nfunc A() int {\n\treturn 1\n}\n\nfunc B() int {\n\treturn 2\n}\n"

func TestIndex_RecordsEveryFileOfTheCurrentVersion(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	base := commit(t, f.repo, "main", "", map[string]string{"a.go": renamed, "c.go": "c\n", "d.go": "d\n"})
	v1 := commit(t, f.repo, "v1", base, map[string]string{"a.go": gone, "b.go": renamed, "c.go": "c2\n"})
	v2 := commit(t, f.repo, "v2", base, map[string]string{"d.go": "d2\n"})

	f.version(awareness.Change{Number: 1, Version: 1, BaseSHA: &base, HeadSHA: v1})
	f.pass()
	if got, want := f.files(1), []string{"a.go", "b.go", "c.go"}; !slices.Equal(got, want) {
		t.Errorf("files after version 1 = %v, want %v (a rename counts under both paths)", got, want)
	}

	f.version(awareness.Change{Number: 1, Version: 2, BaseSHA: &base, HeadSHA: v2})
	f.pass()
	if got, want := f.files(1), []string{"d.go"}; !slices.Equal(got, want) {
		t.Errorf("files after version 2 = %v, want %v (replacing version 1's)", got, want)
	}

	f.version(awareness.Change{Number: 1, Version: 2, BaseSHA: &base, HeadSHA: v2, TargetBranch: "release"})
	f.pass()
	ch, found, err := f.comp.Indexed(context.Background(), nil, "demo", 1)
	if err != nil || !found {
		t.Fatalf("Indexed(demo, 1) = %v, %v", found, err)
	}
	if ch.TargetBranch != "release" || ch.Phase != awareness.PhaseInFlight {
		t.Errorf("after the retarget: target %q, phase %q; want release, in_flight", ch.TargetBranch, ch.Phase)
	}
	if got := f.inFlight("main"); len(got) != 0 {
		t.Errorf("in flight on main after the retarget = %v, want none", got)
	}

	many := map[string]string{}
	for i := range 2001 {
		many[fmt.Sprintf("gen/f%04d.txt", i)] = fmt.Sprintf("%d\n", i)
	}
	big := commit(t, f.repo, "big", base, many)
	f.version(awareness.Change{Number: 2, Version: 1, BaseSHA: &base, HeadSHA: big})
	f.pass()
	if got := f.files(2); len(got) != 2001 {
		t.Errorf("a version adding 2001 files is recorded with %d files, want 2001", len(got))
	}
}

func TestIndex_FollowsLandAbandonAndRestore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	base := commit(t, f.repo, "main", "", map[string]string{"api/a.go": "a\n"})
	h1 := commit(t, f.repo, "one", base, map[string]string{"api/a.go": "one\n"})
	h2 := commit(t, f.repo, "two", base, map[string]string{"api/a.go": "two\n"})
	one := awareness.Change{Number: 1, Version: 1, BaseSHA: &base, HeadSHA: h1}
	two := awareness.Change{Number: 2, Version: 1, BaseSHA: &base, HeadSHA: h2}
	f.version(one)
	f.version(two)
	f.pass()
	if got := f.inFlight("main"); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("in flight = %v, want [1 2]", got)
	}

	two.State = awareness.StateAbandoned
	f.emit(eventAbandon, two)
	f.pass()
	if got := f.inFlight("main"); !slices.Equal(got, []int64{1}) {
		t.Errorf("in flight after abandon(2) = %v, want [1]", got)
	}

	two.State = awareness.StateOpen
	f.emit(eventRestore, two)
	f.pass()
	if got := f.inFlight("main"); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("in flight after restore(2) = %v, want [1 2]", got)
	}
	if got := f.files(2); !slices.Equal(got, []string{"api/a.go"}) {
		t.Errorf("files of 2 after restore = %v, want [api/a.go]", got)
	}

	at := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	two.State, two.Landed = awareness.StateLanded, &awareness.Landed{SHA: strings.Repeat("5", 40), At: at}
	f.emit(eventLand, two)
	f.pass()
	if got := f.inFlight("main"); !slices.Equal(got, []int64{1}) {
		t.Errorf("in flight after land(2) = %v, want [1]", got)
	}
	landed, err := f.comp.Landed(context.Background(), nil, "demo", "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []awareness.LandedChange{{Number: 2, SHA: strings.Repeat("5", 40), At: at}}
	if len(landed) != 1 || landed[0].Number != want[0].Number || landed[0].SHA != want[0].SHA || !landed[0].At.Equal(at) {
		t.Errorf("landed on main = %+v, want %+v", landed, want)
	}
	if got := f.files(2); !slices.Equal(got, []string{"api/a.go"}) {
		t.Errorf("files of 2 after land = %v, want [api/a.go] (kept)", got)
	}
}

func TestIndex_RecordsAVersionWhoseFilesCannotBeListedWithNoFiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	base := commit(t, f.repo, "main", "", map[string]string{"a.go": "a\n"})
	head := commit(t, f.repo, "four", base, map[string]string{"a.go": "four\n"})
	f.version(awareness.Change{Number: 3, Version: 1, BaseSHA: &base, HeadSHA: strings.Repeat("0", 40)})
	f.version(awareness.Change{Number: 4, Version: 1, BaseSHA: &base, HeadSHA: head})
	last := f.version(awareness.Change{Number: 5, Version: 1, BaseSHA: &base, HeadSHA: head})

	if err := f.comp.Pass(context.Background()); err != nil {
		t.Fatalf("pass = %v, want no error", err)
	}
	if got := f.files(3); len(got) != 0 {
		t.Errorf("files of 3 = %v, want none", got)
	}
	if _, found, err := f.comp.Indexed(context.Background(), nil, "demo", 3); err != nil || !found {
		t.Errorf("change 3 is not in the index (found %v, %v)", found, err)
	}
	if got := f.files(4); len(got) == 0 {
		t.Error("files of 4 are empty, want the event after the failure processed")
	}
	if got := f.position(); got != last {
		t.Errorf("position = %d, want %d", got, last)
	}
	// Change 3, with no files, overlaps none of the changes that touch
	// one, and no overlap event names it.
	for number, want := range map[int64][]int64{3: {}, 4: {5}, 5: {4}} {
		if got := f.partners(number); !slices.Equal(got, want) {
			t.Errorf("partners of %d = %v, want %v", number, got, want)
		}
	}
	if got := f.announced(); !slices.Equal(got, []string{"4-5", "4-5"}) {
		t.Errorf("announced pairs = %v, want two events for 4-5 alone", got)
	}
	if log := f.log.String(); !strings.Contains(log, "workspace=demo") || !strings.Contains(log, "number=3") {
		t.Errorf("log = %q, want the workspace and the number of the change", log)
	}
}
