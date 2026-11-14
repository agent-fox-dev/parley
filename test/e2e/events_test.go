package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// rowsStep creates the table of the test route's rows.
var rowsStep = store.Step{
	Name:   "e2e_rows",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE e2e_rows (name TEXT NOT NULL PRIMARY KEY, workspace TEXT NOT NULL)`)
		return err
	},
}

// rowsPath is the path of the test route.
const rowsPath = "/api/v1/test/rows"

// startEventsHub starts a hub with cfg, as hubtest.StartWith does, with one
// more route, which stands in for the routes of the packages that emit:
// POST /api/v1/test/rows with {"name": …, "workspace": …} writes that row to
// a table of its own and emits parley.change.open on the workspace, with
// the name as its resource_id, in the same store transaction, and answers
// 201 with the event's record. A name already written fails the
// transaction: the answer is 500 internal, and neither the row nor the
// event is kept. The route needs a credential and no scope.
func startEventsHub(t testing.TB, cfg hubtest.Config) *hubtest.Hub {
	t.Helper()
	compose := cfg.Compose
	cfg.Compose = func(c *hub.Composition) {
		var st *store.Store
		c.AddSchemaStep(rowsStep)
		c.AddStartHook("e2e-rows", func(_ context.Context, rt *hub.Runtime) error {
			st = rt.Store
			return nil
		})
		events := c.Events()
		c.AddRoute(server.Route{Method: http.MethodPost, Pattern: "/test/rows", Handler: func(w http.ResponseWriter, r *http.Request) error {
			var in struct {
				Name      string `json:"name"`
				Workspace string `json:"workspace"`
			}
			if err := server.DecodeJSON(r, &in); err != nil {
				return err
			}
			ctx := r.Context()
			var rec event.Record
			err := st.Tx(ctx, func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, `INSERT INTO e2e_rows (name, workspace) VALUES (?, ?)`, in.Name, in.Workspace); err != nil {
					return err
				}
				var err error
				rec, err = events.Emit(ctx, tx, event.Input{Type: "parley.change.open", Workspace: &in.Workspace,
					ActorType: event.ActorAdminToken, ResourceType: "change", ResourceID: in.Name})
				return err
			})
			if err != nil {
				return err
			}
			server.WriteJSON(w, http.StatusCreated, rec)
			return nil
		}})
		if compose != nil {
			compose(c)
		}
	}
	return hubtest.StartWith(t, cfg)
}

// hubEvent is the part of a hub event record the end-to-end flows read.
type hubEvent struct {
	Seq        int64   `json:"seq"`
	Type       string  `json:"type"`
	Workspace  *string `json:"workspace"`
	ResourceID string  `json:"resource_id"`
}

// logItems returns the items of GET /api/v1/events/log?after=0 as the admin
// token, and fails the test unless it is answered 200.
func logItems(t testing.TB, h *hubtest.Hub) []hubEvent {
	t.Helper()
	status, body := h.Call(t, http.MethodGet, "/api/v1/events/log?after=0", h.Token, nil)
	var page struct {
		Items []hubEvent `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
		t.Fatalf("GET /api/v1/events/log?after=0 = %d %s, want 200 with a page", status, body)
	}
	return page.Items
}

func TestEventsHub_TestRouteCommitsRowAndEventTogether(t *testing.T) {
	t.Parallel()
	h := startEventsHub(t, hubtest.Config{})
	row := map[string]string{"name": "r1", "workspace": "api"}

	status, body := h.Call(t, http.MethodPost, rowsPath, h.Token, row)
	var emitted hubEvent
	if status != http.StatusCreated || json.Unmarshal(body, &emitted) != nil || emitted.Seq == 0 {
		t.Fatalf("POST %s = %d %s, want 201 with the event", rowsPath, status, body)
	}
	var rows int
	if err := h.Store.Main().QueryRow(`SELECT count(*) FROM e2e_rows WHERE name = 'r1'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows named r1 = %d, %v, want 1", rows, err)
	}
	items := logItems(t, h)
	if len(items) != 1 || items[0].Seq != emitted.Seq || items[0].Type != "parley.change.open" ||
		items[0].Workspace == nil || *items[0].Workspace != "api" || items[0].ResourceID != "r1" {
		t.Fatalf("the log = %+v, want the one event the route answered, %+v", items, emitted)
	}

	// The same name again fails the row, and the event is rolled back with it.
	if status, body := h.Call(t, http.MethodPost, rowsPath, h.Token, row); status != http.StatusInternalServerError {
		t.Errorf("POST %s with a name already written = %d %s, want 500", rowsPath, status, body)
	}
	if items := logItems(t, h); len(items) != 1 {
		t.Errorf("the log after the failed write holds %d events, want 1", len(items))
	}
}
