package gitrepo

import (
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The writer of this file writes an object the hub makes, such as a
// commit, as a loose object of the repository in process, as
// git hash-object -w writes it, rather than start a git process for each.
// Like the readers of objectfile.go and commitfile.go, its ok is false for
// what it does not write the way git would, and its caller asks git then:
// a repository whose objects are not SHA-1 ones, one that shares its files
// with a group (core.sharedRepository), a linked worktree's git directory,
// a git environment that moves the object store, such as a quarantine's,
// and any file it cannot write.

// writeLooseObject writes the object of the type typ ("commit", "tree",
// "blob") holding content as a loose object of the git directory dir, for
// a git process with the environment entries env, and returns its id. An
// object already there is freshened, its file's time set to now, as git
// does, so that a prune does not take it for an old one.
func writeLooseObject(dir string, env []string, typ string, content []byte) (string, bool) {
	if !writesOwnLooseObjects(dir, env) {
		return "", false
	}
	header := fmt.Sprintf("%s %d\x00", typ, len(content))
	sum := sha1.New()
	sum.Write([]byte(header))
	sum.Write(content)
	id := hex.EncodeToString(sum.Sum(nil))
	path := filepath.Join(dir, "objects", id[:2], id[2:])
	if _, err := os.Stat(path); err == nil {
		now := time.Now()
		return id, os.Chtimes(path, now, now) == nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return "", false
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "tmp_obj_")
	if err != nil {
		return "", false
	}
	z := zlib.NewWriter(tmp)
	_, err = z.Write([]byte(header))
	if err == nil {
		_, err = z.Write(content)
	}
	if cerr := z.Close(); err == nil {
		err = cerr
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o444)
	}
	if err == nil {
		// The same id is the same content: a writer that got there first
		// wrote what this one would.
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", false
	}
	return id, true
}

// writesOwnLooseObjects reports whether a git process with the environment
// entries env would write a new object of the git directory dir as a loose
// SHA-1 object below dir/objects, with the modes of the hub's own account.
func writesOwnLooseObjects(dir string, env []string) bool {
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_QUARANTINE_PATH",
			"GIT_DIR", "GIT_COMMON_DIR", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT":
			return false
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "commondir")); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if info, err := os.Stat(filepath.Join(dir, "objects")); err != nil || !info.IsDir() {
		return false
	}
	config, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(config))
	return !strings.Contains(lower, "objectformat") && !strings.Contains(lower, "sharedrepository") &&
		!strings.Contains(lower, "[include")
}
