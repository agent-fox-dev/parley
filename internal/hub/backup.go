package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/store"
)

// backup is parleyd backup <directory>. It composes the hub and loads the
// configuration as serve does, so that the keys the hub's packages register
// apply, and then only reads the data: it opens no store for writing,
// applies no schema step, runs no bootstrap, start hook or maintenance task,
// binds nothing and writes nothing outside the target directory.
func (r *run) backup(ctx context.Context) int {
	_, cfg, err := r.configure()
	if err != nil {
		return r.backupFailed(err)
	}
	target := r.cmd.backupDir
	if !filepath.IsAbs(target) {
		target = filepath.Join(r.opts.WorkDir, target)
	}
	target = filepath.Clean(target)
	if err := checkSources(cfg); err != nil {
		return r.backupFailed(err)
	}
	created, err := prepareTarget(target, cfg.Workspace.Path)
	if err != nil {
		return r.backupFailed(err)
	}
	if err := writeSnapshot(ctx, cfg, target); err != nil {
		if rmErr := removeWritten(target, created); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
		return r.backupFailed(err)
	}
	r.logger.Info("backup written", "dir", target)
	return 0
}

// backupFailed writes the one error line of a failed backup and returns its
// status.
func (r *run) backupFailed(err error) int {
	r.logger.Error("backup failed", "error", err.Error())
	return 1
}

// checkSources fails, before anything is written, when the data a backup
// copies is not there: a hub that never started on this configuration has
// no database file, and the backup must not create one.
func checkSources(cfg *config.Config) error {
	for _, path := range []string{cfg.Database.Path, cfg.AnalyticsPath()} {
		fi, err := os.Stat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("database file %s does not exist", path)
		case err != nil:
			return fmt.Errorf("database file %s: %w", path, err)
		case fi.IsDir():
			return fmt.Errorf("database file %s is a directory", path)
		}
	}
	fi, err := os.Stat(cfg.Workspace.Path)
	switch {
	case err != nil:
		return fmt.Errorf("workspace root %s: %w", cfg.Workspace.Path, err)
	case !fi.IsDir():
		return fmt.Errorf("workspace root %s is not a directory", cfg.Workspace.Path)
	}
	return nil
}

// prepareTarget makes sure target is an empty directory, creating it with
// mode 0700 when it is missing, and reports whether it created it. It
// refuses, writing nothing, a target that is a non-empty directory, that is
// not a directory, or that lies inside the workspace root, which the backup
// would copy into itself.
func prepareTarget(target, workspaceRoot string) (created bool, err error) {
	if inside(resolve(target), resolve(workspaceRoot)) || inside(target, workspaceRoot) {
		return false, fmt.Errorf("backup directory %s is inside the workspace root %s", target, workspaceRoot)
	}
	fi, err := os.Stat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The parent is not created: the backup removes what it wrote on a
		// failure, and parents it made would be left behind.
		if err := os.Mkdir(target, 0o700); err != nil {
			return false, fmt.Errorf("create backup directory %s: %w", target, err)
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("backup directory %s: %w", target, err)
	case !fi.IsDir():
		return false, fmt.Errorf("backup directory %s is not a directory", target)
	}
	d, err := os.Open(target)
	if err != nil {
		return false, fmt.Errorf("backup directory %s: %w", target, err)
	}
	defer d.Close()
	if _, err := d.Readdirnames(1); err == nil {
		return false, fmt.Errorf("backup directory %s is not empty", target)
	} else if !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("backup directory %s: %w", target, err)
	}
	return false, nil
}

// resolve returns path with its symbolic links resolved, as far as it
// exists.
func resolve(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	if dir := filepath.Dir(path); dir != path {
		return filepath.Join(resolve(dir), filepath.Base(path))
	}
	return path
}

// inside reports whether path is dir or lies under it.
func inside(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// writeSnapshot writes the snapshot into the empty directory target: both
// stores first, each as one consistent read, then the workspace root.
func writeSnapshot(ctx context.Context, cfg *config.Config, target string) error {
	if err := store.Snapshot(ctx, cfg.Database.Path, filepath.Join(target, filepath.Base(cfg.Database.Path))); err != nil {
		return err
	}
	if err := store.Snapshot(ctx, cfg.AnalyticsPath(), filepath.Join(target, store.AnalyticsFile)); err != nil {
		return err
	}
	return copyWorkspaceRoot(cfg.Workspace.Path, filepath.Join(target, filepath.Base(cfg.Workspace.Path)))
}

// copyWorkspaceRoot copies every file, directory and symbolic link under
// root to dst, keeping their relative paths. The objects of each git
// repository are copied after the rest of it, that is, after its refs,
// packed-refs and HEAD: objects are only added while a hub runs, so every
// ref copied points at an object that is copied after it.
func copyWorkspaceRoot(root, dst string) error {
	var objects []string
	err := copyTree(root, dst, func(path string) bool {
		if filepath.Base(path) != "objects" {
			return false
		}
		if fi, err := os.Stat(filepath.Join(filepath.Dir(path), "HEAD")); err != nil || fi.IsDir() {
			return false
		}
		objects = append(objects, path)
		return true
	})
	if err != nil {
		return err
	}
	for _, dir := range objects {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		if err := copyTree(dir, filepath.Join(dst, rel), func(string) bool { return false }); err != nil {
			return err
		}
	}
	return nil
}

// copyTree copies the directory src to dst, which must not exist, leaving
// out the directories below src that deferred reports true for. The error names the path that
// failed.
func copyTree(src, dst string, deferred func(path string) bool) error {
	return filepath.WalkDir(src, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("copy %s: %w", path, err)
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		switch {
		case e.IsDir():
			if path != src && deferred(path) {
				return filepath.SkipDir
			}
			if err := os.Mkdir(to, 0o700); err != nil {
				return fmt.Errorf("copy %s: %w", path, err)
			}
			return nil
		case e.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err == nil {
				err = os.Symlink(link, to)
			}
			if err != nil {
				return fmt.Errorf("copy %s: %w", path, err)
			}
			return nil
		case e.Type().IsRegular():
			if err := copyFile(path, to); err != nil {
				return fmt.Errorf("copy %s: %w", path, err)
			}
			return nil
		default:
			return fmt.Errorf("copy %s: not a regular file, directory or symbolic link", path)
		}
	})
}

// copyFile copies the regular file src to the new file dst with the same
// permission bits.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

// removeWritten removes what a failed backup wrote: the target directory
// when the backup created it, and otherwise what it holds, since it was
// empty before.
func removeWritten(target string, created bool) error {
	if created {
		return os.RemoveAll(target)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		errs = append(errs, os.RemoveAll(filepath.Join(target, e.Name())))
	}
	return errors.Join(errs...)
}
