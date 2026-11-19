package telemetry_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestWriteTimeout runs every ingestion route against a store whose writes
// block until their context ends, with a write limit of 100ms, and then
// against the same store recovered.
func TestWriteTimeout(t *testing.T) {
	t.Parallel()
	w := newWorldTOML(t, "[audit]\nwrite_timeout = \"100ms\"\n")
	w.blockWrites.Store(true)

	t.Run("a write that outlasts the limit is refused as unavailable and stores nothing", func(t *testing.T) {
		for _, route := range ingestionRoutes {
			t.Run(route.path, func(t *testing.T) {
				start := time.Now()
				status, header, body := w.Exchange(t, http.MethodPost, "/api/v1/workspaces/"+demo+"/runs/"+w.freshRun()+route.path,
					w.aliceKey, route.valid(), nil)
				elapsed := time.Since(start)
				if status != http.StatusServiceUnavailable {
					t.Fatalf("status = %d %s, want 503", status, body)
				}
				if typ, _ := errorOf(t, body); typ != "unavailable" {
					t.Errorf("error_type = %q, want unavailable", typ)
				}
				if got := header.Get("Retry-After"); got != "5" {
					t.Errorf("Retry-After = %q, want 5", got)
				}
				if elapsed >= 2*time.Second {
					t.Errorf("answered after %s, want within 2s", elapsed)
				}
				if n := w.countAllRecords(t); n != 0 {
					t.Errorf("%d records stored, want none", n)
				}
			})
		}
	})

	t.Run("a request refused by the limit is stored as new once the store recovers", func(t *testing.T) {
		run := w.freshRun()
		single := map[string]any{"id": w.freshID(), "event_type": "e"}
		items := []any{
			map[string]any{"id": w.freshID(), "event_type": "e"},
			map[string]any{"id": w.freshID(), "event_type": "e"},
			map[string]any{"id": w.freshID(), "event_type": "e"},
		}
		if status, body := w.post(t, w.aliceKey, demo, run, "/events", single); status != http.StatusServiceUnavailable {
			t.Fatalf("single while blocked: status = %d %s, want 503", status, body)
		}
		if status, body := w.post(t, w.aliceKey, demo, run, "/events/batch", items); status != http.StatusServiceUnavailable {
			t.Fatalf("batch while blocked: status = %d %s, want 503", status, body)
		}

		w.blockWrites.Store(false)
		if status, body := w.post(t, w.aliceKey, demo, run, "/events", single); status != http.StatusCreated {
			t.Fatalf("single after recovery: status = %d %s, want 201", status, body)
		}
		status, body := w.post(t, w.aliceKey, demo, run, "/events/batch", items)
		if status != http.StatusOK {
			t.Fatalf("batch after recovery: status = %d %s, want 200", status, body)
		}
		var ans batchAnswer
		if err := json.Unmarshal(body, &ans); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if ans.Accepted != 3 || ans.Duplicates != 0 || ans.Errors == nil || len(ans.Errors) != 0 {
			t.Errorf("batch after recovery = %s, want {accepted: 3, duplicates: 0, errors: []}", body)
		}
	})

	t.Run("a write that waits past the limit for another writer's lock is refused as unavailable and stores nothing", func(t *testing.T) {
		w.blockWrites.Store(false)
		ctx := context.Background()
		db := w.Store.Analytics()
		holder, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		release := func() {
			_, _ = holder.ExecContext(ctx, "ROLLBACK")
			_ = holder.Close()
		}
		defer release()
		if _, err := holder.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			t.Fatalf("take the write lock: %v", err)
		}

		run := w.freshRun()
		start := time.Now()
		status, header, body := w.Exchange(t, http.MethodPost, "/api/v1/workspaces/"+demo+"/runs/"+run+"/events",
			w.aliceKey, map[string]any{"event_type": "e"}, nil)
		elapsed := time.Since(start)
		release()

		if status != http.StatusServiceUnavailable {
			t.Errorf("status = %d %s, want 503", status, body)
		} else if typ, _ := errorOf(t, body); typ != "unavailable" {
			t.Errorf("error_type = %q, want unavailable", typ)
		}
		if got := header.Get("Retry-After"); got != "5" {
			t.Errorf("Retry-After = %q, want 5", got)
		}
		if elapsed >= 2*time.Second {
			t.Errorf("answered after %s, want within 2s", elapsed)
		}
		if n := w.count(t, `SELECT COUNT(*) FROM run_events WHERE run_id = ?`, run); n != 0 {
			t.Errorf("%d run events of the run stored, want none", n)
		}

		// Every connection of the store's pool still waits as long as it
		// did for a lock.
		conns := make([]*sql.Conn, db.Stats().OpenConnections+1)
		for i := range conns {
			if conns[i], err = db.Conn(ctx); err != nil {
				t.Fatal(err)
			}
			defer conns[i].Close()
		}
		for i, c := range conns {
			var ms int
			if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&ms); err != nil {
				t.Fatal(err)
			}
			if ms != 10000 {
				t.Errorf("connection %d of the pool has a busy timeout of %d ms, want 10000", i, ms)
			}
		}
	})
}
