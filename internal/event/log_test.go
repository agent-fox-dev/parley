package event

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestBounds_EmptyAndFilled(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if got := f.bounds(); got != (Bounds{}) {
		t.Errorf("Bounds on a new store = %+v, want {0, 0}", got)
	}
	for range 5 {
		f.emit(valid("parley.change.open"))
	}
	if got, want := f.bounds(), (Bounds{OldestSeq: 1, LastSeq: 5}); got != want {
		t.Errorf("Bounds after 5 events = %+v, want %+v", got, want)
	}
}

func TestExpire_MovesOldestSeq(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	start := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	for _, at := range []struct {
		time time.Time
		n    int
	}{{start, 3}, {start.Add(2 * time.Hour), 3}, {start.Add(4 * time.Hour), 1}} {
		f.clock.Set(at.time)
		for range at.n {
			f.emit(valid("parley.change.open"))
		}
	}

	// The steps run in order on one log: each starts from the bound the
	// previous one left.
	for _, step := range []struct {
		name   string
		cutoff time.Time
		want   int64
	}{
		{"a cutoff before every event moves nothing", start.Add(-time.Hour), 1},
		{"the events older than the cutoff leave", start.Add(time.Hour), 4},
		{"the same cutoff again changes nothing", start.Add(time.Hour), 4},
		{"an earlier cutoff never moves the bound back", start, 4},
		{"a cutoff after every event keeps the newest", start.Add(10 * time.Hour), 7},
	} {
		got, err := f.events.Expire(context.Background(), step.cutoff)
		if err != nil {
			t.Fatalf("%s: Expire: %v", step.name, err)
		}
		if got != step.want {
			t.Errorf("%s: Expire = %d, want %d", step.name, got, step.want)
		}
		if b := f.bounds(); b.OldestSeq != step.want || b.LastSeq != 7 {
			t.Errorf("%s: Bounds = %+v, want {%d, 7}", step.name, b, step.want)
		}
		if recs := f.readAll(step.want - 1); len(recs) == 0 || recs[0].Seq != step.want {
			t.Errorf("%s: ReadAfter(%d) starts at %v, want seq %d", step.name, step.want-1, seqs(recs), step.want)
		}
		if below := step.want - 2; below >= 0 {
			_, _, err := f.events.ReadAfter(context.Background(), below, 0)
			var expired *PositionExpiredError
			if !errors.Is(err, ErrPositionExpired) || !errors.As(err, &expired) || expired.OldestSeq != step.want {
				t.Errorf("%s: ReadAfter(%d) error = %v, want ErrPositionExpired with oldest_seq %d", step.name, below, err, step.want)
			}
		}
		var rows int
		if err := f.store.Main().QueryRow(`SELECT count(*) FROM hub_events`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 7 {
			t.Errorf("%s: %d event rows stored, want all 7", step.name, rows)
		}
	}
}

func TestReadAfter_PagesInSeqOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for range 5 {
		f.emit(valid("parley.change.open"))
	}
	for _, tc := range []struct {
		after        int64
		limit        int
		want         []int64
		wantExamined int64
	}{
		{0, 2, []int64{1, 2}, 2},
		{2, 10, []int64{3, 4, 5}, 5},
		{5, 10, nil, 5},
	} {
		recs, examined, err := f.events.ReadAfter(context.Background(), tc.after, tc.limit)
		if err != nil {
			t.Fatalf("ReadAfter(%d, %d): %v", tc.after, tc.limit, err)
		}
		if got := seqs(recs); !slices.Equal(got, tc.want) || examined != tc.wantExamined {
			t.Errorf("ReadAfter(%d, %d) = (%v; examined %d), want (%v; examined %d)",
				tc.after, tc.limit, got, examined, tc.want, tc.wantExamined)
		}
	}
}
