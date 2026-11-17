package gitrepotest

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// Tree writes into the repository whose git directory is dir the tree of
// the commit parent ("" for none) with the files, at the top level, holding
// the content given, and returns its id.
func Tree(t testing.TB, dir, parent string, files map[string]string) string {
	t.Helper()
	entries := map[string]string{}
	if parent != "" {
		for line := range strings.Lines(Git(t, dir, "ls-tree", parent)) {
			meta, name, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "\t")
			entries[name] = meta
		}
	}
	for name, content := range files {
		blob, err := Run(dir, content, "hash-object", "-w", "--stdin")
		if err != nil {
			t.Fatal(err)
		}
		entries[name] = "100644 blob " + blob
	}
	var tree strings.Builder
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		fmt.Fprintf(&tree, "%s\t%s\n", entries[name], name)
	}
	id, err := Run(dir, tree.String(), "mktree")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Commit writes into the repository whose git directory is dir a commit
// with the message msg and the parents given, the first parent first (none
// for a root commit), whose tree is the first parent's with the files, at
// the top level, holding the content given, and returns its id. It writes
// no ref.
func Commit(t testing.TB, dir, msg string, files map[string]string, parents ...string) string {
	t.Helper()
	first := ""
	if len(parents) > 0 {
		first = parents[0]
	}
	args := []string{"commit-tree", Tree(t, dir, first, files), "-m", msg}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	return Git(t, dir, args...)
}
