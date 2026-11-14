package gitrepo

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// forkTempMarker follows the base name of a fork's place in the name of the
// directory CreateFork builds the fork's repository in: ".<base>.tmp-<random>".
const forkTempMarker = ".tmp-"

// IsForkTemp reports whether name, a directory entry's name, is that of the
// directory a CreateFork in progress builds a fork's repository in. A
// creation the process did not survive leaves the directory behind; the
// owner of the directory the forks live in removes such entries while no
// creation runs.
func IsForkTemp(name string) bool {
	return strings.HasPrefix(name, ".") && strings.Contains(name, forkTempMarker)
}

// CreateFork makes the repository of a fork of the repository at parentDir
// at forkDir: an empty bare repository whose alternates file names the
// parent's object directory by its absolute path. No object is copied and
// the parent is not changed; every object of the parent, also one it
// receives later, is readable through the fork, and what is written to the
// fork is stored in the fork alone.
//
// The repository is built in a new directory beside forkDir and renamed into
// place once complete, so that a creation that fails or is interrupted
// leaves nothing at forkDir that opens as a repository; on an error the
// directory is removed, and one a creation the process did not survive left
// is told by its name (IsForkTemp). Something at forkDir already is refused with an
// error matching fs.ErrExist and left as it is, as is a parentDir that is
// not a repository. Remove removes a fork's repository.
func (g Git) CreateFork(ctx context.Context, parentDir, forkDir string) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("create the fork %s of %s: %w", forkDir, parentDir, err)
		}
	}()
	dst, err := filepath.Abs(forkDir)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return fs.ErrExist
	} else if !os.IsNotExist(err) {
		return err
	}
	parent, err := g.Open(ctx, parentDir)
	if err != nil {
		return err
	}
	objects := filepath.Join(parent.Dir(), "objects")
	if info, err := os.Stat(objects); err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", objects)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dst), "."+filepath.Base(dst)+forkTempMarker)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = Remove(tmp)
		}
	}()
	if _, err := g.run(ctx, access{}, "init", "--quiet", "--bare", "--", tmp); err != nil {
		return err
	}
	info := filepath.Join(tmp, "objects", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(objects+"\n"), 0o644); err != nil {
		return err
	}
	// Rename refuses a directory that appeared at dst in the meantime.
	return os.Rename(tmp, dst)
}
