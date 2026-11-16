package awareness

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
)

// kindRetarget is the kind of a version event that records a retarget,
// which measures the current version against another target branch and
// records no version.
const kindRetarget = "retarget"

// firstVersion reports whether the version event rec records the first
// version of its change: version 1 in its metadata, and not a retarget,
// which repeats the current version's number. The event, not the change
// record, tells: by the time the worker reads the record, a later version
// may have been recorded.
func firstVersion(rec event.Record) bool {
	var meta struct {
		Version int64  `json:"version"`
		Kind    string `json:"kind"`
	}
	if json.Unmarshal(rec.Metadata, &meta) != nil {
		return false
	}
	return meta.Version == 1 && meta.Kind != kindRetarget
}

// linkClaims links to the change ch, in tx, every claim of its session that
// is active at now, stored on its workspace, on its target branch and not
// linked to a change yet: the claims the session made for the work the
// change now carries. A claim stored as active whose expires_at has come is
// expired, and is not linked.
func linkClaims(ctx context.Context, tx *sql.Tx, ch Change, now time.Time) error {
	if ch.SessionID == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE awareness_claims SET change_number = ?
WHERE session_id = ? AND workspace_slug = ? AND target_branch = ? AND change_number IS NULL
	AND state = 'active' AND expires_at > ?`,
		ch.Number, *ch.SessionID, ch.Workspace, ch.TargetBranch, now.UnixNano())
	if err != nil {
		return fmt.Errorf("link the claims of the session %s to %s#%d: %w", *ch.SessionID, ch.Workspace, ch.Number, err)
	}
	return nil
}

// fulfilClaims stores, in tx, every claim linked to the change number of
// workspace that is active at now as fulfilled, ended at now; a fulfilment
// is announced by no event. A claim stored as active whose expires_at has
// come is expired, and is left to the expiry run.
func fulfilClaims(ctx context.Context, tx *sql.Tx, workspace string, number int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE awareness_claims SET state = 'fulfilled', ended_at = ?
WHERE workspace_slug = ? AND change_number = ? AND state = 'active' AND expires_at > ?`,
		now.UnixNano(), workspace, number, now.UnixNano())
	if err != nil {
		return fmt.Errorf("fulfil the claims of %s#%d: %w", workspace, number, err)
	}
	return nil
}
