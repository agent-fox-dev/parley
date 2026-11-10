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

	"github.com/agent-fox-dev/parley/internal/store"
)

// backup is parleyd backup <directory>: it reads the configuration as the
// server does and writes a consistent snapshot of the data into an empty or
// missing directory: the transactional store under the database file's own
// name, the analytical store as analytics.db and the workspace root under
// its own name. It binds no listener, runs no bootstrap, start hook,
// maintenance task or schema step, and writes nothing to the data. On a
// failure it removes what it wrote.
func (r *hubRun) backup(ctx context.Context) int {
	fail := func(err error) int {
		r.logger.Error("parleyd backup failed", "error", err.Error())
		return 1
	}
	if err := r.setUp(); err != nil {
		return fail(err)
	}
	target := r.flags.backup
	if !filepath.IsAbs(target) {
		target = filepath.Join(r.opts.WorkDir, target)
	}
	if err := checkTarget(target); err != nil {
		return fail(err)
	}
	if _, err := os.Stat(r.cfg.Database.Path); err != nil {
		return fail(fmt.Errorf("no hub data at %s: %w", r.cfg.Database.Path, err))
	}
	created := false
	if _, err := os.Stat(target); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(target, 0o700); err != nil {
			return fail(fmt.Errorf("cannot create backup directory %s: %w", target, err))
		}
		created = true
	}
	if err := r.writeSnapshot(ctx, target); err != nil {
		if cerr := removeWritten(target, created); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return fail(err)
	}
	r.logger.Info("backup written", "target", target)
	return 0
}

// checkTarget refuses a target that exists and is not an empty directory.
func checkTarget(target string) error {
	fi, err := os.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot use backup directory %s: %w", target, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("backup target %s is not a directory", target)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return fmt.Errorf("cannot read backup directory %s: %w", target, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("backup directory %s is not empty", target)
	}
	return nil
}

// removeWritten removes what a failed backup wrote: the directory it
// created, or the entries it wrote into the empty directory it was given.
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

// writeSnapshot copies the stores first, each as one consistent read, then
// the repositories.
func (r *hubRun) writeSnapshot(ctx context.Context, target string) error {
	db := r.cfg.Database.Path
	if err := store.Snapshot(ctx, db, filepath.Join(target, filepath.Base(db))); err != nil {
		return err
	}
	analytics := r.cfg.AnalyticsPath()
	if err := store.Snapshot(ctx, analytics, filepath.Join(target, store.AnalyticsFile)); err != nil {
		return err
	}
	ws := r.cfg.Workspace.Path
	return copyWorkspaces(ws, filepath.Join(target, filepath.Base(ws)))
}

// underObjects reports whether rel lies inside a repository's objects
// directory.
func underObjects(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "objects" {
			return true
		}
	}
	return false
}

// copyWorkspaces copies every file and directory under src to dst with the
// same relative paths and content. Everything outside the objects
// directories — refs, packed-refs, HEAD — is copied before the objects, so
// that every copied ref points at a copied object even while the hub
// writes.
func copyWorkspaces(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return fmt.Errorf("cannot create %s: %w", dst, err)
	}
	if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	for _, objects := range []bool{false, true} {
		err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("cannot read %s: %w", p, err)
			}
			rel, _ := filepath.Rel(src, p)
			if rel == "." {
				return nil
			}
			if underObjects(rel) != objects && !(d.IsDir() && !objects) {
				return nil
			}
			return copyEntry(p, filepath.Join(dst, rel), d)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// copyEntry copies one directory, symbolic link or regular file.
func copyEntry(src, dst string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", src, err)
	}
	switch {
	case d.IsDir():
		if err := os.MkdirAll(dst, info.Mode().Perm()|0o700); err != nil {
			return fmt.Errorf("cannot create %s: %w", dst, err)
		}
		return nil
	case info.Mode()&fs.ModeSymlink != 0:
		link, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", src, err)
		}
		return os.Symlink(link, dst)
	case !info.Mode().IsRegular():
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("cannot copy %s: %w", src, err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("cannot create %s: %w", filepath.Dir(dst), err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("cannot write %s: %w", dst, err)
	}
	_, cerr := io.Copy(out, in)
	if err := errors.Join(cerr, out.Close()); err != nil {
		return fmt.Errorf("cannot copy %s: %w", src, err)
	}
	return nil
}
