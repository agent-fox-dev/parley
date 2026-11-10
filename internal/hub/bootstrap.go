package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// AdminTokenFile is the file in the config dir that hands the operator a new
// admin token.
const AdminTokenFile = "admin_token"

// bootstrap is the stage after the stores are open and before the start
// hooks. The hub is bootstrapped once an admin token hash is stored.
//
//   - --reset-admin-token, in any state: write a new token, replacing the
//     stored hash (and storing --admin-email when none was stored), and end
//     the run.
//   - not bootstrapped, --admin-email given: store it and a new token, and
//     end the run.
//   - not bootstrapped, no address: go on with the start.
//   - bootstrapped, the token file exists: refuse to start.
//   - bootstrapped, no token file: go on, ignoring --admin-email.
func (r *hubRun) bootstrap(ctx context.Context) (done bool, code int, err error) {
	path := filepath.Join(r.cfg.Dir, AdminTokenFile)
	settings, err := identity.AdminSettings(ctx, r.st)
	if err != nil {
		return false, 0, err
	}
	email := r.flags.adminEmail
	switch {
	case r.flags.resetToken:
		if settings.Stored {
			email = ""
		}
	case !settings.Stored && email != "":
	case !settings.Stored:
		return false, 0, nil
	default:
		if _, err := os.Lstat(path); err == nil {
			return false, 0, fmt.Errorf("admin token file %s exists: save the token, delete the file and start the hub again", path)
		}
		return false, 0, nil
	}
	if err := r.writeAdminToken(ctx, path, email); err != nil {
		return false, 0, err
	}
	r.logger.Warn("admin token written: save the token and delete the file", "path", path)
	return true, 0, nil
}

// writeAdminToken generates a token, stores its hash (and the e-mail) and
// writes the file, all in one transaction, so that a file that cannot be
// written stores nothing. The token itself is never logged.
func (r *hubRun) writeAdminToken(ctx context.Context, path, email string) error {
	token, err := identity.GenerateAdminToken()
	if err != nil {
		return err
	}
	return r.st.Tx(ctx, func(tx *sql.Tx) error {
		if err := identity.SetAdmin(ctx, tx, email, identity.HashToken(token)); err != nil {
			return err
		}
		if err := writeFileAtomic(path, []byte(token)); err != nil {
			return fmt.Errorf("cannot write admin token file %s: %w", path, err)
		}
		return nil
	})
}

// writeFileAtomic writes data to a new file with mode 0600 beside path and
// renames it over path, so that an existing file is replaced with the new
// mode and a reader never sees a part of it.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}
