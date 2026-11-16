package jobs

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestList_ReadsAWorkspacesJobsOfATypeNewestFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := newTestQueue(t)
	// first and second share the second their created_at shows, so the id
	// orders them; third is the newest.
	first := q.seed(t, "probe", "w", StatusCompleted)
	q.clock.Advance(400 * time.Millisecond)
	second := q.seed(t, "probe", "w", StatusCancelled)
	q.clock.Advance(2 * time.Second)
	third := q.seed(t, "probe", "w", StatusQueued)
	q.seed(t, "probe", "other", StatusQueued)
	q.seed(t, "other", "w", StatusQueued)
	sameSecond := []string{first.ID, second.ID}
	slices.Sort(sameSecond)
	slices.Reverse(sameSecond)
	all := append([]string{third.ID}, sameSecond...)
	afterThird := third.Position()

	cases := []struct {
		name   string
		status string
		after  *ListPosition
		limit  int
		want   []string
	}{
		{"all", "", nil, 10, all},
		{"one status", StatusCancelled, nil, 10, []string{second.ID}},
		{"limited", "", nil, 2, all[:2]},
		{"after the newest", "", &afterThird, 10, all[1:]},
		{"after one of the same second", "", &ListPosition{Seconds: first.CreatedAt.Unix(), ID: sameSecond[0]}, 10, all[2:]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobs, err := q.List(ctx, "probe", "w", tc.status, tc.after, tc.limit)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			var got []string
			for _, j := range jobs {
				got = append(got, j.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("List = %v, want %v", got, tc.want)
			}
		})
	}
}
