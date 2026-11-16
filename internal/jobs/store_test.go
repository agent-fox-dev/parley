package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestStore_KeepsCommittedJobsAcrossAReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st := openStore(t, dir)
	clock := newFakeClock(clockStart)
	q := New(Deps{Store: st, Clock: clock, NoWake: true})

	var enqueued []Job
	for i, slug := range []string{"a", "b", "c"} {
		clock.Advance(1500 * time.Millisecond) // created_at keeps its nanoseconds
		err := st.Tx(ctx, func(tx *sql.Tx) error {
			j, err := q.Enqueue(ctx, tx, "probe", slug, json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)))
			enqueued = append(enqueued, j)
			return err
		})
		if err != nil {
			t.Fatalf("enqueue for %s: %v", slug, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	q = New(Deps{Store: openStore(t, dir), Clock: newFakeClock(clockStart), NoWake: true})
	for _, want := range enqueued {
		got, err := q.Get(ctx, want.ID)
		if err != nil {
			t.Fatalf("Get %s after reopen: %v", want.ID, err)
		}
		if got.ID != want.ID || got.Type != want.Type || got.Workspace != want.Workspace || got.Status != want.Status ||
			string(got.Payload) != string(want.Payload) || !got.CreatedAt.Equal(want.CreatedAt) {
			t.Errorf("after reopen = %+v, want %+v", got, want)
		}
	}
}
