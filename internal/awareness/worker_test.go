package awareness_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/awareness"
)

func TestWorker_IndexesAnOpenedChangeOnlyAfterAPass(t *testing.T) {
	t.Parallel()
	h := newChangeHub(t)
	number := h.open(t, "feat", map[string]string{"api/a.go": "feat\n"})
	ctx := context.Background()
	if got, err := h.comp.InFlight(ctx, nil, "demo", "main"); err != nil || len(got) != 0 {
		t.Fatalf("in flight before a pass = %v, %v; want none", got, err)
	}

	if err := h.comp.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := h.comp.InFlight(ctx, nil, "demo", "main"); err != nil || !slices.Equal(got, []int64{number}) {
		t.Errorf("in flight after a pass = %v, %v; want [%d]", got, err, number)
	}
	if got, err := h.comp.Files(ctx, nil, "demo", number); err != nil || !slices.Equal(got, []string{"api/a.go"}) {
		t.Errorf("files = %v, %v; want [api/a.go]", got, err)
	}
}

func TestWorker_AppliesEachEventOnceAcrossARestartAndAFailedCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	base := commit(t, f.repo, "main", "", map[string]string{"a.go": "a\n", "b.go": "b\n"})
	var versions []awareness.Change
	for i, files := range []map[string]string{
		{"a.go": "1\n"}, {"b.go": "1\n"}, {"a.go": "2\n", "c.go": "c\n"}, {"b.go": "2\n", "d.go": "d\n"}, {"e.go": "e\n"},
	} {
		head := commit(t, f.repo, "v", base, files)
		versions = append(versions, awareness.Change{Number: int64(i%2 + 1), Version: int64(i/2 + 1), BaseSHA: &base, HeadSHA: head})
	}

	whole := newFixtureOn(t, f.repo)
	for _, v := range versions {
		whole.version(v)
	}
	whole.pass()

	f.version(versions[0])
	f.version(versions[1])
	f.pass()
	f.restart()
	var fifth int64
	for _, v := range versions[2:] {
		fifth = f.version(v)
	}
	f.pass()
	f.pass()

	if got := f.position(); got != fifth {
		t.Errorf("position = %d, want %d, the fifth event's", got, fifth)
	}
	if got, want := f.dump(), whole.dump(); !slices.Equal(got, want) {
		t.Errorf("index after a restart = %v, want that of a run without it, %v", got, want)
	}
	if got := f.changes.readCount(); got != 5 {
		t.Errorf("the change reader was asked %d times, want 5, once per event", got)
	}

	before := f.dump()
	head := commit(t, f.repo, "v", base, map[string]string{"f.go": "f\n"})
	sixth := f.version(awareness.Change{Number: 1, Version: 4, BaseSHA: &base, HeadSHA: head})
	awareness.FailNextCommit(f.comp, errors.New("disk I/O error"))
	if err := f.comp.Pass(context.Background()); err == nil {
		t.Error("a pass whose transaction failed returned no error")
	}
	if got := f.position(); got != fifth {
		t.Errorf("position after a failed transaction = %d, want %d", got, fifth)
	}
	if got := f.dump(); !slices.Equal(got, before) {
		t.Errorf("index after a failed transaction = %v, want it unchanged, %v", got, before)
	}

	f.pass()
	if got := f.position(); got != sixth {
		t.Errorf("position after the next pass = %d, want %d", got, sixth)
	}
	if got := f.files(1); !slices.Equal(got, []string{"f.go"}) {
		t.Errorf("files of 1 after the next pass = %v, want [f.go]", got)
	}
}
