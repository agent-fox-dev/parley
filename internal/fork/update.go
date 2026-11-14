package fork

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

// PurposeField is the handler of purpose in an update of a fork, a
// workspace.ForkFieldHandler: a string of at most 1024 characters (code
// points) is the fork's new purpose, null clears it, and any other value is
// refused.
func PurposeField(_ workspace.Workspace, raw json.RawMessage) (workspace.ForkFieldWrite, error) {
	var purpose *string
	if !isNull(raw) {
		err := readString(raw, &purpose, func(s string) error {
			if utf8.RuneCountInString(s) > purposeMax {
				return fmt.Errorf("must be at most %d characters long", purposeMax)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return writeColumn("purpose", purpose), nil
}

// AutoChangeField is the handler of auto_change in an update of a fork, a
// workspace.ForkFieldHandler: true or false, never null, since a fork either
// opens a change when an own branch is pushed or does not.
func AutoChangeField(_ workspace.Workspace, raw json.RawMessage) (workspace.ForkFieldWrite, error) {
	var on bool
	if isNull(raw) || json.Unmarshal(raw, &on) != nil {
		return nil, errors.New("must be true or false")
	}
	return writeColumn("auto_change", on), nil
}

// isNull reports whether raw is the JSON null.
func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// writeColumn is the write that stores value in the column col of the
// fork's record. col is one of the handlers' own, never a caller's.
func writeColumn(col string, value any) workspace.ForkFieldWrite {
	return func(ctx context.Context, tx *sql.Tx, f workspace.Workspace) error {
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET `+col+` = ? WHERE slug = ?`, value, f.Slug); err != nil {
			return fmt.Errorf("store the %s of fork %q: %w", col, f.Slug, err)
		}
		return nil
	}
}
