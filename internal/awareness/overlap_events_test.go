package awareness_test

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/event"
)

func TestOverlapEvents_AnnounceANewPairOnEachChange(t *testing.T) {
	f := newFixture(t)
	f.version(f.touching(1, 1, session1, "api/a.go", "api/b.go"))
	f.pass()
	f.version(f.touching(2, 1, session2, "api/a.go", "x.go"))
	f.pass()

	ev := f.overlapEvents()
	if len(ev) != 2 {
		t.Fatalf("%d overlap events, want 2: %v", len(ev), ev)
	}
	byResource := map[string]event.Record{}
	for _, r := range ev {
		byResource[r.ResourceID] = r
	}
	e1, e2 := byResource["1"], byResource["2"]
	for _, c := range []struct {
		r       event.Record
		session string
	}{{e1, session1}, {e2, session2}} {
		if c.r.ActorType != event.ActorSystem || c.r.ActorID != nil {
			t.Errorf("event on %s: actor %s %v, want system", c.r.ResourceID, c.r.ActorType, c.r.ActorID)
		}
		if c.r.ResourceType != "change" {
			t.Errorf("event on %s: resource_type %q, want change", c.r.ResourceID, c.r.ResourceType)
		}
		if c.r.Workspace == nil || *c.r.Workspace != "demo" {
			t.Errorf("event on %s: workspace %v, want demo", c.r.ResourceID, c.r.Workspace)
		}
		if c.r.SessionID == nil || *c.r.SessionID != c.session {
			t.Errorf("event on %s: session_id %v, want %s", c.r.ResourceID, c.r.SessionID, c.session)
		}
	}
	var want1 overlapMeta
	want1.Number, want1.With.Number, want1.With.Ref = 1, 2, "demo#2"
	want1.WithSessionID, want1.Paths, want1.Count = new(session2), []string{"api/a.go"}, 1
	if got := metaOf(t, e1); !reflect.DeepEqual(got, want1) {
		t.Errorf("metadata of the event on 1 = %+v, want %+v", got, want1)
	}
	if !jsonHasKeys(t, e1.Metadata, "number", "with", "with_session_id", "paths", "count") {
		t.Errorf("metadata of the event on 1 = %s, want exactly number, with, with_session_id, paths and count", e1.Metadata)
	}
	m2 := metaOf(t, e2)
	if m2.Number != 2 || m2.With.Number != 1 || m2.With.Ref != "demo#1" || m2.WithSessionID == nil || *m2.WithSessionID != session1 {
		t.Errorf("metadata of the event on 2 = %+v, want number 2 naming 1 (demo#1) and with_session_id %s", m2, session1)
	}

	many := make([]string, 150)
	for i := range many {
		many[i] = fmt.Sprintf("gen/f%03d.go", 149-i)
	}
	f.version(f.touching(3, 1, "", many...))
	f.version(f.touching(4, 1, "", many...))
	f.pass()
	for _, r := range f.overlapEvents() {
		m := metaOf(t, r)
		if min(m.Number, m.With.Number) != 3 || max(m.Number, m.With.Number) != 4 {
			continue
		}
		if m.Count != 150 || len(m.Paths) != 100 || !m.Truncated || !slices.IsSorted(m.Paths) || m.Paths[0] != "gen/f000.go" {
			t.Errorf("event on %s for 150 common files: count %d, %d paths from %v, truncated %v; want 150, the first 100 ascending, truncated",
				r.ResourceID, m.Count, len(m.Paths), m.Paths[:min(1, len(m.Paths))], m.Truncated)
		}
		if r.SessionID != nil || m.WithSessionID != nil {
			t.Errorf("event on %s of changes without a session: session_id %v, with_session_id %v; want null", r.ResourceID, r.SessionID, m.WithSessionID)
		}
	}
	if got := f.announced(); !slices.Equal(got, []string{"1-2", "1-2", "3-4", "3-4"}) {
		t.Errorf("announced pairs = %v, want two events each for 1-2 and 3-4", got)
	}
}

func TestOverlapEvents_AnnounceAPairOnceWhileItOverlaps(t *testing.T) {
	f := newFixture(t)
	f.version(f.touching(1, 1, session1, "api/a.go"))
	f.version(f.touching(2, 1, session2, "api/a.go"))
	f.pass()
	if got := len(f.overlapEvents()); got != 2 {
		t.Fatalf("%d overlap events after the announcement, want 2", got)
	}

	for v := int64(2); v <= 4; v++ {
		f.version(f.touching(2, v, session2, "api/a.go", fmt.Sprintf("other%d.go", v)))
	}
	f.pass()
	if got := len(f.overlapEvents()); got != 2 {
		t.Errorf("%d overlap events after versions 2 to 4 of change 2, want 2", got)
	}

	f.restart()
	f.version(f.touching(2, 5, session2, "api/a.go"))
	f.pass()
	if got := len(f.overlapEvents()); got != 2 {
		t.Errorf("%d overlap events after a restart and version 5 of change 2, want 2", got)
	}
	if got := f.pairs(); !slices.Equal(got, []string{"1-2"}) {
		t.Errorf("recorded pairs = %v, want [1-2]", got)
	}
}

func TestOverlapEvents_AnnounceAPairAgainAfterItStoppedOverlapping(t *testing.T) {
	cases := []struct {
		name         string
		stop, resume func(f *fixture, two awareness.Change)
	}{
		{
			name: "a version without the common file, then one with it",
			stop: func(f *fixture, two awareness.Change) {
				f.version(f.touching(2, 2, session2, "x.go"))
			},
			resume: func(f *fixture, two awareness.Change) {
				f.version(f.touching(2, 3, session2, "api/a.go"))
			},
		},
		{
			name: "a retarget to another branch, then back",
			stop: func(f *fixture, two awareness.Change) {
				two.Version, two.TargetBranch = 2, "release"
				f.version(two)
			},
			resume: func(f *fixture, two awareness.Change) {
				two.Version, two.TargetBranch = 3, "main"
				f.version(two)
			},
		},
		{
			name: "abandon, then restore",
			stop: func(f *fixture, two awareness.Change) {
				two.State = awareness.StateAbandoned
				f.emit(eventAbandon, two)
			},
			resume: func(f *fixture, two awareness.Change) {
				two.State = awareness.StateOpen
				f.emit(eventRestore, two)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.version(f.touching(1, 1, session1, "api/a.go"))
			two := f.touching(2, 1, session2, "api/a.go")
			f.version(two)
			f.pass()
			if got := len(f.overlapEvents()); got != 2 {
				t.Fatalf("%d overlap events after the announcement, want 2", got)
			}

			tc.stop(f, two)
			f.pass()
			if got := len(f.overlapEvents()); got != 2 {
				t.Errorf("%d overlap events after the pair stopped overlapping, want 2", got)
			}
			if got := f.pairs(); len(got) != 0 {
				t.Errorf("recorded pairs after the pair stopped overlapping = %v, want none", got)
			}

			tc.resume(f, two)
			f.pass()
			if got := f.announced(); !slices.Equal(got, []string{"1-2", "1-2", "1-2", "1-2"}) {
				t.Errorf("announced after the pair overlaps again = %v, want four events for 1-2", got)
			}
			if got := f.pairs(); !slices.Equal(got, []string{"1-2"}) {
				t.Errorf("recorded pairs after the pair overlaps again = %v, want [1-2]", got)
			}
		})
	}
}

func TestOverlapEvents_AnnounceEachPairOneUpdateMakes(t *testing.T) {
	f := newFixture(t)
	f.version(f.touching(1, 1, "", "a.go"))
	f.version(f.touching(2, 1, "", "b.go"))
	f.version(f.touching(3, 1, "", "c.go"))
	f.pass()
	if got := len(f.overlapEvents()); got != 0 {
		t.Fatalf("%d overlap events among changes without a common file, want 0", got)
	}

	f.version(f.touching(4, 1, "", "a.go", "b.go", "c.go"))
	f.pass()
	ev := f.overlapEvents()
	if len(ev) != 6 {
		t.Fatalf("%d overlap events, want 6", len(ev))
	}
	var onFour []int64
	namingFour := map[string]int{}
	for _, r := range ev {
		m := metaOf(t, r)
		if r.ResourceID == "4" {
			onFour = append(onFour, m.With.Number)
			continue
		}
		if m.With.Number == 4 && r.ResourceID == strconv.FormatInt(m.Number, 10) {
			namingFour[r.ResourceID]++
		}
	}
	slices.Sort(onFour)
	if !slices.Equal(onFour, []int64{1, 2, 3}) {
		t.Errorf("the events on 4 name %v, want [1 2 3]", onFour)
	}
	if want := map[string]int{"1": 1, "2": 1, "3": 1}; !reflect.DeepEqual(namingFour, want) {
		t.Errorf("events naming 4 per change = %v, want one on each of 1, 2 and 3", namingFour)
	}
}
