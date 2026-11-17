package gitrepotest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SetRef points the ref named in full, such as refs/heads/main, at the
// object id in the repository whose git directory is dir, as git update-ref
// <ref> <id> does, and fails the test when it cannot. It writes the ref's
// file itself where it can rather than start git, since a test process pays
// for every process it forks; unlike git it does not check that id names a
// commit, so a test that points a branch elsewhere runs git itself.
func SetRef(t testing.TB, dir, ref, id string) {
	t.Helper()
	if !writeLooseRef(dir, ref, id) {
		Git(t, dir, "update-ref", ref, id)
	}
}

// writeLooseRef writes ref as a loose ref holding id, through a lock file
// renamed into place as git does, and reports whether it did. It leaves to
// git, by answering false, whatever it does not handle as git would: an id
// that is not a full object id, a repository whose refs readRefs does not
// read, a symbolic ref, whose target git would move, and a ref whose name
// collides with an existing one's directory.
func writeLooseRef(dir, ref, id string) bool {
	if !strings.HasPrefix(ref, "refs/") || !isObjectID(id) {
		return false
	}
	refs, ok := readRefs(dir, "")
	if !ok {
		return false
	}
	for name := range refs {
		if strings.HasPrefix(name, ref+"/") || strings.HasPrefix(ref, name+"/") {
			return false
		}
	}
	path := filepath.Join(dir, filepath.FromSlash(ref))
	if raw, err := os.ReadFile(path); err == nil && strings.HasPrefix(string(raw), "ref: ") {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	lock, err := os.OpenFile(path+".lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false
	}
	_, err = lock.WriteString(id + "\n")
	if cerr := lock.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(lock.Name(), path)
	}
	if err != nil {
		_ = os.Remove(lock.Name())
		return false
	}
	return true
}
