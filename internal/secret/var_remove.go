package secret

import (
	"context"
	"database/sql"
	"fmt"
)

// RemoveVarScope deletes every variable of one scope inside tx, as the
// deletion of its owner does, and nothing else. It writes no event: the
// owner's deletion is the change the log records. Its error is the store's,
// so that the deletion's transaction is rolled back and every variable
// stays. It needs no component, since a variable needs no key.
func RemoveVarScope(ctx context.Context, tx *sql.Tx, scope Scope, scopeID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM variables WHERE scope = ? AND scope_id = ?`, scope, scopeID); err != nil {
		return fmt.Errorf("remove the variables of %s %s: %w", scope, scopeID, err)
	}
	return nil
}

// RemoveWorkspaceVars deletes every variable of the workspace slug inside
// tx. It is the remover of the workspace's owned data that runs when a
// workspace is deleted or a task fork removed, so that a workspace made
// again under the slug starts with none.
func RemoveWorkspaceVars(ctx context.Context, tx *sql.Tx, slug string) error {
	return RemoveVarScope(ctx, tx, ScopeWorkspace, slug)
}
