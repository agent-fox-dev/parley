package gitrepo

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The readers of this file read refs from git's files ref backend in
// process, in the order git reads them: the loose ref files before
// packed-refs, since git pack-refs writes a ref into packed-refs before it
// removes its loose file, so that a ref a concurrent pack-refs moves is
// found in one or the other. git writes each file through a lock file
// renamed into place, so a file is read whole or not at all. A reader's ok
// is false for what it does not read the way git does, and its caller asks
// git then.

// filesBackend reports whether dir is a git directory whose refs are kept
// in git's files ref backend in dir itself: not one that is no longer a
// git directory, a linked worktree's git directory (commondir) or a
// repository of the reftable backend.
func filesBackend(dir string) bool {
	if isGitDir(dir) != nil {
		return false
	}
	for _, other := range []string{"commondir", "reftable"} {
		if _, err := os.Lstat(filepath.Join(dir, other)); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return true
}

// readBranch is the commit id the branch ref names in the git directory
// dir, and whether it exists. git writes nothing but a commit to a branch,
// so the id needs no peeling. ok is false for an invalid name, a directory
// filesBackend refuses, a symbolic ref and anything else it does not
// understand.
func readBranch(dir, ref string) (id string, found, ok bool) {
	if !ValidRefName(ref) || !filesBackend(dir) {
		return "", false, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ref)))
	switch {
	case err == nil:
		v := strings.TrimSpace(string(raw))
		return v, true, isHexID(v, idLen)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.EISDIR):
		// No loose ref: a directory of refs below the name is none either.
	default:
		return "", false, false
	}
	packed, ok := readPackedRefs(dir, ref)
	if !ok {
		return "", false, false
	}
	id, found = packed[ref]
	return id, found, true
}

// readRefs is what git for-each-ref lists below prefix, a ref name prefix
// ending in "/", in the git directory dir: each ref's object id, unpeeled,
// by its name without prefix, HEAD left out. ok is false for a directory
// filesBackend refuses, a symbolic ref, a name git would refuse and
// anything else it does not understand.
func readRefs(dir, prefix string) (refs map[string]string, ok bool) {
	if !strings.HasSuffix(prefix, "/") || !ValidRefName(prefix+"x") || !filesBackend(dir) {
		return nil, false
	}
	loose := map[string]string{}
	err := filepath.WalkDir(filepath.Join(dir, filepath.FromSlash(prefix)), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // no ref below prefix, or one removed while the walk ran
			}
			return err
		}
		if d.IsDir() || strings.HasSuffix(d.Name(), ".lock") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		loose[filepath.ToSlash(rel)] = strings.TrimSpace(string(raw))
		return nil
	})
	if err != nil {
		return nil, false
	}
	packed, ok := readPackedRefs(dir, prefix)
	if !ok {
		return nil, false
	}
	refs = map[string]string{}
	for _, names := range []map[string]string{packed, loose} {
		for ref, id := range names {
			if !ValidRefName(ref) || !isHexID(id, idLen) {
				return nil, false
			}
			if name := strings.TrimPrefix(ref, prefix); name != "HEAD" {
				refs[name] = id
			}
		}
	}
	return refs, true
}

// readPackedRefs maps the refs of dir's packed-refs file, which may be
// missing, whose names begin with prefix to their object ids, unpeeled.
// ok is false for a file it cannot read or a line it does not understand.
func readPackedRefs(dir, prefix string) (refs map[string]string, ok bool) {
	refs = map[string]string{}
	raw, err := os.ReadFile(filepath.Join(dir, "packed-refs"))
	if errors.Is(err, fs.ErrNotExist) {
		return refs, true
	}
	if err != nil {
		return nil, false
	}
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimRight(line, "\n")
		if line == "" || line[0] == '#' || line[0] == '^' { // header, peeled tag
			continue
		}
		id, name, cut := strings.Cut(line, " ")
		if !cut || !isHexID(id, idLen) {
			return nil, false
		}
		if strings.HasPrefix(name, prefix) {
			refs[name] = id
		}
	}
	return refs, true
}
