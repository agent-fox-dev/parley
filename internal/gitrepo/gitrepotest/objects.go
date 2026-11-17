package gitrepotest

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// writeCommit writes the commit Commit makes as loose objects of the
// repository whose git directory is dir, as git fast-import would write it
// at the time when, and returns its id. ok is false, with nothing a caller
// could reach written, for what it leaves to git fast-import: a parent that
// is not a full id of a loose commit, and what writeCommitOf leaves.
func writeCommit(dir, msg string, files map[string]string, parents []string, when time.Time) (id string, ok bool) {
	entries := map[string]treeEntry{}
	for i, p := range parents {
		tree, ok := commitTree(dir, p)
		if !ok {
			return "", false
		}
		if i > 0 {
			continue
		}
		if entries, ok = readTree(dir, tree); !ok {
			return "", false
		}
	}
	return writeCommitOf(dir, msg, entries, files, parents, when)
}

// writeCommitOf writes, as loose objects of the repository whose git
// directory is dir, the commit by Origin at the time when with the message
// msg and the parents given, whose tree is entries with the files, at the
// top level, holding the content given, and returns its id. It does not
// check the parents. ok is false, with nothing a caller could reach
// written, for a repository whose objects are not git's SHA-1 ones, a file
// name that is not a plain top-level name, and anything it cannot write.
func writeCommitOf(dir, msg string, entries map[string]treeEntry, files map[string]string, parents []string, when time.Time) (id string, ok bool) {
	if !sha1Repository(dir) {
		return "", false
	}
	for name := range files {
		if name == "" || name == "." || name == ".." || name[0] == '"' || strings.ContainsAny(name, "/\x00\n") {
			return "", false
		}
	}
	tree := maps.Clone(entries)
	if tree == nil {
		tree = map[string]treeEntry{}
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		blob, ok := writeObject(dir, "blob", []byte(files[name]))
		if !ok {
			return "", false
		}
		tree[name] = treeEntry{mode: "100644", id: blob}
	}
	treeID, ok := writeObject(dir, "tree", treeContent(tree))
	if !ok {
		return "", false
	}
	var c bytes.Buffer
	fmt.Fprintf(&c, "tree %s\n", treeID)
	for _, p := range parents {
		fmt.Fprintf(&c, "parent %s\n", strings.ToLower(p))
	}
	who := fmt.Sprintf("Origin <origin@example.invalid> %d %s", when.Unix(), when.Format("-0700"))
	fmt.Fprintf(&c, "author %s\ncommitter %s\n\n%s\n", who, who, msg)
	return writeObject(dir, "commit", c.Bytes())
}

// treeEntry is an entry of a tree object: its mode as git writes it, such
// as 100644 or 40000, and its object's id.
type treeEntry struct {
	mode, id string
}

// sha1Repository reports whether dir is a git directory of SHA-1 objects
// whose object store this file writes: not a linked worktree's, and not one
// whose configuration names an object format.
func sha1Repository(dir string) bool {
	if info, err := os.Stat(filepath.Join(dir, "objects")); err != nil || !info.IsDir() {
		return false
	}
	if _, err := os.Lstat(filepath.Join(dir, "commondir")); err == nil {
		return false
	}
	config, err := os.ReadFile(filepath.Join(dir, "config"))
	return err == nil && !strings.Contains(strings.ToLower(string(config)), "objectformat")
}

// commitTree is the id of the tree of the loose commit id.
func commitTree(dir, id string) (string, bool) {
	content, ok := readObject(dir, "commit", id)
	if !ok {
		return "", false
	}
	tree, _, _ := strings.Cut(string(content), "\n")
	tree, found := strings.CutPrefix(tree, "tree ")
	return tree, found && isObjectID(tree)
}

// readTree is the entries of the loose tree id, by name.
func readTree(dir, id string) (map[string]treeEntry, bool) {
	content, ok := readObject(dir, "tree", id)
	if !ok {
		return nil, false
	}
	entries := map[string]treeEntry{}
	for len(content) > 0 {
		head, rest, found := bytes.Cut(content, []byte{0})
		mode, name, cut := strings.Cut(string(head), " ")
		if !found || !cut || len(rest) < sha1.Size {
			return nil, false
		}
		entries[name] = treeEntry{mode: mode, id: hex.EncodeToString(rest[:sha1.Size])}
		content = rest[sha1.Size:]
	}
	return entries, true
}

// treeContent is the content of the tree object of entries, in git's
// order: by name, a tree's name compared as if it ended in "/".
func treeContent(entries map[string]treeEntry) []byte {
	key := func(name string) string {
		if entries[name].mode == "40000" {
			return name + "/"
		}
		return name
	}
	names := slices.SortedFunc(maps.Keys(entries), func(a, b string) int { return strings.Compare(key(a), key(b)) })
	var out bytes.Buffer
	for _, name := range names {
		raw, _ := hex.DecodeString(entries[name].id)
		fmt.Fprintf(&out, "%s %s\x00", entries[name].mode, name)
		out.Write(raw)
	}
	return out.Bytes()
}

// readObject is the content of the loose object id of the type typ.
func readObject(dir, typ, id string) ([]byte, bool) {
	if !isObjectID(id) {
		return nil, false
	}
	id = strings.ToLower(id)
	f, err := os.Open(filepath.Join(dir, "objects", id[:2], id[2:]))
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	z, err := zlib.NewReader(f)
	if err != nil {
		return nil, false
	}
	raw, err := io.ReadAll(z)
	if err != nil {
		return nil, false
	}
	header, content, found := bytes.Cut(raw, []byte{0})
	if !found || string(header) != fmt.Sprintf("%s %d", typ, len(content)) {
		return nil, false
	}
	return content, true
}

// writeObject writes the object of the type typ and content as a loose
// object, unless the repository holds it loose already, and returns its id.
// It writes the file under a temporary name and renames it into place, as
// git does, so that a reader finds it whole or not at all.
func writeObject(dir, typ string, content []byte) (string, bool) {
	header := fmt.Sprintf("%s %d\x00", typ, len(content))
	sum := sha1.New()
	sum.Write([]byte(header))
	sum.Write(content)
	id := hex.EncodeToString(sum.Sum(nil))
	path := filepath.Join(dir, "objects", id[:2], id[2:])
	if _, err := os.Stat(path); err == nil {
		return id, true
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", false
	}
	return id, true
}
