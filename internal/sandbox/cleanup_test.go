package sandbox_test

import (
	"context"
	"database/sql"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/sandbox"
)

func TestRecordCleanup_RemovesTheRecordsDeletedMoreThanADayAgo(t *testing.T) {
	t.Parallel()
	purged := &idLog{}
	var passes atomic.Int64
	h := newExecHub(t, "", func(cfg *hubtest.Config) {
		cfg.MaintenanceInterval = 20 * time.Millisecond
		compose := cfg.Compose
		cfg.Compose = func(c *hub.Composition) {
			compose(c)
			c.Sandbox().RegisterPurgeHook(func(_ context.Context, _ *sql.Tx, id string) error {
				purged.add(id)
				return nil
			})
			// A task under a name the maintenance order does not know runs
			// after those it knows, the record cleanup among them.
			c.AddMaintenanceTask("cleanup_test_pass", func(context.Context, *hub.Runtime) error {
				passes.Add(1)
				return nil
			})
		}
	})
	now := h.clock.Now()
	rows := []struct {
		name    string
		state   string
		ended   time.Duration
		removed bool
	}{
		{"deleted a day and a second ago", sandbox.StateDeleted, 24*time.Hour + time.Second, true},
		{"deleted exactly a day ago", sandbox.StateDeleted, 24 * time.Hour, false},
		{"deleted an hour ago", sandbox.StateDeleted, time.Hour, false},
		{"stopped thirty days ago", sandbox.StateStopped, 30 * 24 * time.Hour, false},
		{"failed thirty days ago", sandbox.StateFailed, 30 * 24 * time.Hour, false},
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = h.sandbox(t, func(r *sandbox.Record) {
			ended := now.Add(-row.ended)
			r.State, r.EndedAt, r.CreatedAt, r.UpdatedAt = row.state, &ended, ended, ended
		})
	}
	from := passes.Load()
	eventually(t, "a maintenance pass", func() bool { return passes.Load() >= from+2 })
	for i, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			a := h.call(t, http.MethodGet, sandboxPath("ws", ids[i]), h.key, nil)
			n := purged.snapshot()[ids[i]]
			switch {
			case row.removed && (a.status != http.StatusNotFound || a.err.ErrorType != "sandbox_not_found"):
				t.Errorf("GET = %d %s; want 404 sandbox_not_found", a.status, a.body)
			case row.removed && n != 1:
				t.Errorf("the purge hooks ran %d times for the record; want once", n)
			case !row.removed && (a.status != http.StatusOK || n != 0):
				t.Errorf("GET = %d %s, purged %d times; want 200 and the record kept", a.status, a.body, n)
			}
		})
	}
}
