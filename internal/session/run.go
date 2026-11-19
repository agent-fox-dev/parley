package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// RunOf is the run id of the session id, for the record of what is done in
// a sandbox of the session's run. found is false when id names no stored
// session, an id that is not a UUID included, and runID is "" for a session
// opened without a run id: neither is an error. Ids are compared in lower
// case.
func (c *Component) RunOf(ctx context.Context, id string) (runID string, found bool, err error) {
	if !isUUID(id) {
		return "", false, nil
	}
	id = strings.ToLower(id)
	var run sql.NullString
	err = c.deps.Store.Main().QueryRowContext(ctx, `SELECT run_id FROM sessions WHERE id = ?`, id).Scan(&run)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the run of session %s: %w", id, err)
	}
	return run.String, true, nil
}
