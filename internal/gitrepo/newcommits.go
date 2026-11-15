package gitrepo

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// NewCommits lists the commits reachable from tips, the ids of commits or
// of tags that lead to one, that no ref of the repository reaches, nor,
// when parentDir is not "", any ref of the repository whose git directory
// that is, such as a fork's parent, whose objects the repository reads. They
// come oldest first, every commit after its parents. A Repo opened with a
// quarantine as an alternate object directory lists the commits it holds.
func (r *Repo) NewCommits(ctx context.Context, tips []string, parentDir string) ([]string, error) {
	if len(tips) == 0 {
		return nil, nil
	}
	// The ids go to git on its standard input, where a parent with many refs
	// cannot overflow the command line; a ^ in front of an id excludes what
	// it reaches.
	in := []byte(strings.Join(tips, "\n") + "\n")
	if parentDir != "" {
		refs, err := r.g.run(ctx, access{}, "--git-dir="+parentDir, "for-each-ref", "--format=^%(objectname)")
		if err != nil {
			return nil, err
		}
		in = append(in, refs...)
	}
	out, err := r.g.runInput(ctx, access{}, in, "--git-dir="+r.dir, "rev-list", "--reverse", "--topo-order", "--stdin", "--not", "--all")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// ChangedPaths lists, in byte order and each once, the paths commit adds,
// changes or removes against its first parent, or every path of its tree
// when it has no parent. A rename counts as both its old and its new path.
func (r *Repo) ChangedPaths(ctx context.Context, commit string) ([]string, error) {
	// Without rename detection a rename is a removal of its old path and an
	// addition of its new one, so every entry is a status and one path.
	out, err := r.git(ctx, "diff-tree", "-r", "-z", "--name-status", "--no-renames", "--root", "--diff-merges=first-parent", "--no-commit-id", commit)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	var paths []string
	for i := 1; i < len(fields); i += 2 {
		paths = append(paths, fields[i])
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

// The kinds of object ObjectPaths reports an id peels to.
const (
	ObjectCommit = "commit"
	ObjectTree   = "tree"
	ObjectBlob   = "blob"
)

// ObjectPaths peels id through any annotated tags to the object it reaches
// and reports its kind, ObjectCommit, ObjectTree or ObjectBlob, and for a tree
// every path of a file in it, at any depth, in byte order. An id that names
// no object is an error.
func (r *Repo) ObjectPaths(ctx context.Context, id string) (kind string, paths []string, err error) {
	// A revision ^{} peels every tag; cat-file says what it reaches, or
	// "missing".
	out, err := r.g.runInput(ctx, access{}, []byte(id+"^{}\n"), "--git-dir="+r.dir, "cat-file", "--batch-check=%(objecttype)")
	if err != nil {
		return "", nil, err
	}
	switch kind = strings.TrimSpace(string(out)); kind {
	case ObjectCommit, ObjectBlob:
		return kind, nil, nil
	case ObjectTree:
	default:
		return "", nil, fmt.Errorf("object %s: %s", id, kind)
	}
	out, err = r.git(ctx, "ls-tree", "-r", "-z", "--name-only", "--full-tree", id+"^{}")
	if err != nil {
		return "", nil, err
	}
	paths = strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(out) == 0 {
		paths = nil
	}
	slices.Sort(paths)
	return kind, paths, nil
}
