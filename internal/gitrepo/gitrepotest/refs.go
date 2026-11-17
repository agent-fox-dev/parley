package gitrepotest

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// readRefs is what git for-each-ref lists below prefix ("" or a prefix
// ending in "/") in the repository whose git directory is dir, by name
// without prefix, read from the files of git's files ref backend. ok is
// false for a repository it does not read that way, which a linked
// worktree's git directory or the reftable backend is, and for anything it
// does not understand; the caller asks git then.
//
// It reads the loose refs before packed-refs, as git does: git pack-refs
// writes a ref into packed-refs before it removes its loose file, so a ref
// a concurrent pack-refs moves is found in one or the other.
func readRefs(dir, prefix string) (refs map[string]string, ok bool) {
	for _, other := range []string{"commondir", "reftable"} {
		if _, err := os.Lstat(filepath.Join(dir, other)); !errors.Is(err, fs.ErrNotExist) {
			return nil, false
		}
	}
	loose := map[string]string{} // name to its file's content, trimmed
	err := filepath.WalkDir(filepath.Join(dir, "refs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed by git while the walk ran
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") || strings.HasSuffix(d.Name(), ".lock") {
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
	packed, ok := readPackedRefs(dir)
	if !ok {
		return nil, false
	}
	// resolve is the object id of the ref name, following symbolic refs.
	var resolve func(name string, depth int) (string, bool)
	resolve = func(name string, depth int) (string, bool) {
		if depth > 5 {
			return "", false
		}
		if v, found := loose[name]; found {
			if target, symbolic := strings.CutPrefix(v, "ref: "); symbolic {
				return resolve(target, depth+1)
			}
			return v, isObjectID(v)
		}
		id, found := packed[name]
		return id, found
	}
	refs = map[string]string{}
	for _, names := range []map[string]string{packed, loose} {
		for name := range names {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			id, found := resolve(name, 0)
			if !found {
				// A dangling symbolic ref, which git leaves out too, or a
				// file this reader does not understand.
				if v := loose[name]; !strings.HasPrefix(v, "ref: ") {
					return nil, false
				}
				continue
			}
			refs[strings.TrimPrefix(name, prefix)] = id
		}
	}
	return refs, true
}

// readPackedRefs maps the refs of dir's packed-refs file, which may be
// missing, to their object ids.
func readPackedRefs(dir string) (map[string]string, bool) {
	refs := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(dir, "packed-refs"))
	if errors.Is(err, fs.ErrNotExist) {
		return refs, true
	}
	if err != nil {
		return nil, false
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' || line[0] == '^' { // header, peeled tag
			continue
		}
		id, name, found := strings.Cut(line, " ")
		if !found || !isObjectID(id) {
			return nil, false
		}
		refs[name] = id
	}
	return refs, sc.Err() == nil
}

// isObjectID reports whether s is a full SHA-1 or SHA-256 object id in
// lower-case hex.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
