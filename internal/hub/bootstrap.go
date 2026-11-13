package hub

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// adminTokenFile is the name of the admin token file in the config dir.
const adminTokenFile = "admin_token"

// composeBootstrap adds the admin settings to the transactional store.
func composeBootstrap(c *Composition) {
	c.AddSchemaStep(identity.AdminSettingsStep)
}

// bootstrap is the start stage between opening the stores and the start
// hooks. It hands the operator of a new hub its first admin token, and
// reports done when the run ends after it, without serving.
//
// With --reset-admin-token it always writes a new token; without it, a hub
// that is not bootstrapped (no token hash stored) writes one when
// --admin-email has a value and otherwise starts. A bootstrapped hub refuses
// to start while the token file is still there, so that the token is not
// left in clear text next to a running hub, and ignores --admin-email.
func bootstrap(ctx context.Context, rt *Runtime, cmd command) (done bool, err error) {
	path, err := filepath.Abs(filepath.Join(rt.Config.Dir, adminTokenFile))
	if err != nil {
		return false, fmt.Errorf("admin token file: %w", err)
	}
	admin, err := identity.AdminSettings(ctx, rt.Store)
	if err != nil {
		return false, err
	}
	switch {
	case cmd.resetAdminToken || (!admin.Stored() && cmd.adminEmail != ""):
		if err := issueAdminToken(ctx, rt, path, cmd.adminEmail); err != nil {
			return false, err
		}
		rt.Logger.Warn("admin token written: save the token and delete the file", "path", path)
		return true, nil
	case !admin.Stored():
		return false, nil
	}
	switch _, err := os.Lstat(path); {
	case err == nil:
		return false, fmt.Errorf("admin token file %s exists: save the token, delete the file and start the hub again", path)
	case !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("admin token file %s: %w", path, err)
	}
	return false, nil
}

// issueAdminToken stores a new admin token's hash, and email when no hash
// was stored before, and writes the token to the file at path in the same
// transaction: a file that cannot be written stores nothing. A file written
// for a transaction that then fails to commit is removed, since its token
// would not be the stored one, and the file it replaced is put back, since
// its token still is.
func issueAdminToken(ctx context.Context, rt *Runtime, path, email string) error {
	var kept string
	written := false
	err := identity.IssueAdminToken(ctx, rt.Store, email, func(token string) error {
		var err error
		if kept, err = keepAside(path); err != nil {
			return err
		}
		if err := writeTokenFile(path, token); err != nil {
			return err
		}
		written = true
		return nil
	})
	switch {
	case err != nil && kept != "":
		_ = os.Rename(kept, path)
	case err != nil && written:
		_ = os.Remove(path)
	case kept != "":
		_ = os.Remove(kept)
	}
	return err
}

// keepAside gives the regular file at path a second name beside it, so that
// it outlives its replacement, and returns that name; "" when there is no
// such file.
func keepAside(path string) (string, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.Mode().IsRegular()) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("admin token file %s: %w", path, err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+adminTokenFile+"-old-*")
	if err != nil {
		return "", fmt.Errorf("keep admin token file %s: %w", path, err)
	}
	kept := f.Name()
	_ = f.Close()
	if err := os.Remove(kept); err != nil {
		return "", fmt.Errorf("keep admin token file %s: %w", path, err)
	}
	if err := os.Link(path, kept); err != nil {
		return "", fmt.Errorf("keep admin token file %s: %w", path, err)
	}
	return kept, nil
}

// writeTokenFile writes token, without a trailing newline, to the file at
// path with mode 0600, replacing a file that is there. It writes a new file
// beside it and renames it over path, so that the mode is 0600 whatever the
// old file's was, and a reader never sees half a token.
func writeTokenFile(path, token string) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+adminTokenFile+"-*")
	if err != nil {
		return fmt.Errorf("write admin token file %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			err = fmt.Errorf("write admin token file %s: %w", path, err)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.WriteString(token); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
