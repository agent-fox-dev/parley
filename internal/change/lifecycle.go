package change

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// EventRestore is the event of an abandoned change brought back to open.
const EventRestore = "parley.change.restore"

// abandonBody is the body of POST …/abandon, which may be absent.
type abandonBody struct {
	Reason *string `json:"reason"`
}

// abandonRoute abandons the change :number of the workspace :slug, through
// modify, for the reason the body gives, "" when it gives none.
func (c *Component) abandonRoute(w http.ResponseWriter, r *http.Request) error {
	return c.modify(w, r, c.parseAbandon)
}

// parseAbandon decodes the abandon's body: none, or one of white space
// only, is the same as an empty object.
func (c *Component) parseAbandon(r *http.Request) (operation, error) {
	var body abandonBody
	if err := decodeOptional(r, &body); err != nil {
		return nil, err
	}
	var reason string
	if body.Reason != nil {
		reason = *body.Reason
	}
	return func(ctx context.Context, tx *sql.Tx, ch Change, caller Caller) error {
		return c.Abandon(ctx, tx, ch.WorkspaceSlug, ch.Number, reason, caller)
	}, nil
}

// Abandon moves the change number of the workspace to abandoned for
// reason, as actor did, in tx, and writes parley.change.abandon. The move
// goes through SetAbandoned, so a fork source's activity recorder is told.
// Its source branch and its version refs stay as they are.
func (c *Component) Abandon(ctx context.Context, tx *sql.Tx, workspace string, number int64, reason string, actor Caller) error {
	if err := c.SetAbandoned(ctx, tx, workspace, number, reason); err != nil {
		return err
	}
	return c.emitAbandon(ctx, tx, actor, workspace, number, reason)
}

// restoreRoute brings the change :number of the workspace :slug back to
// open, through modify. The restore takes no body; one that is sent is
// ignored.
func (c *Component) restoreRoute(w http.ResponseWriter, r *http.Request) error {
	return c.modify(w, r, func(*http.Request) (operation, error) { return c.restore, nil })
}

// restore moves the change ch back to open at its last version, with
// mergeable unknown, and writes parley.change.restore by caller.
func (c *Component) restore(ctx context.Context, tx *sql.Tx, ch Change, caller Caller) error {
	if err := update(ctx, tx, ch.WorkspaceSlug, ch.Number, c.now(),
		`state = 'open', abandon_reason = NULL, mergeable = 'unknown'`); err != nil {
		return err
	}
	return c.emit(ctx, tx, caller, ch, EventRestore, map[string]any{"number": ch.Number, "version": ch.Version, "head_sha": ch.HeadSHA})
}

// decodeOptional is server.DecodeJSON for a body that may be absent: no
// body, or one of white space only, leaves v as it is.
func decodeOptional(r *http.Request, v any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return server.InvalidRequest("request body could not be read")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return server.DecodeJSON(r, v)
}
