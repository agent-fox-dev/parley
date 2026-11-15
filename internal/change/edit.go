package change

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// editRoute edits the change :number of the workspace :slug with the
// members the PATCH body names, through modify; members the body does not
// name, and members it does not know, are left as they are.
func (c *Component) editRoute(w http.ResponseWriter, r *http.Request) error {
	return c.modify(w, r, c.parseEdit)
}

// edit is a PATCH body as parseEdit reads it: labels is nil when the body
// does not name it.
type edit struct {
	labels []string
}

// parseEdit decodes the PATCH body, keeping apart a member that is absent
// and one that is null, and checks each member it names.
func (c *Component) parseEdit(r *http.Request) (operation, error) {
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return nil, err
	}
	var e edit
	if raw, ok := body["labels"]; ok {
		if err := json.Unmarshal(raw, &e.labels); err != nil || e.labels == nil {
			return nil, server.InvalidRequest("labels: must be an array of strings")
		}
	}
	return func(ctx context.Context, tx *sql.Tx, ch Change, _ Caller) error {
		return c.applyEdit(ctx, tx, ch, e)
	}, nil
}

// applyEdit stores the members e names on the change ch.
func (c *Component) applyEdit(ctx context.Context, tx *sql.Tx, ch Change, e edit) error {
	if e.labels == nil {
		return nil
	}
	b, err := json.Marshal(e.labels)
	if err != nil {
		return err
	}
	return update(ctx, tx, ch.WorkspaceSlug, ch.Number, c.now(), `labels = ?`, string(b))
}
