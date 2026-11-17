package gitrepotest

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
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
// no ref. A test process pays for every process it forks, so it writes the
// commit's objects itself, as loose objects, where it can (see
// writeCommit), and otherwise in one run of git fast-import rather than one
// git process per object.
func Commit(t testing.TB, dir, msg string, files map[string]string, parents ...string) string {
	t.Helper()
	if id, ok := writeCommit(dir, msg, files, parents, time.Now()); ok {
		return id
	}
	// fast-import commits on a branch; the reset at the end leaves the ref
	// unwritten, so the name only has to be one no test reads.
	const scratch = "refs/gitrepotest/commit"
	var stream strings.Builder
	fmt.Fprintf(&stream, "commit %s\nmark :1\ncommitter Origin <origin@example.invalid> now\n%s", scratch, fastData(msg+"\n"))
	for i, p := range parents {
		if i == 0 {
			fmt.Fprintf(&stream, "from %s\n", p)
		} else {
			fmt.Fprintf(&stream, "merge %s\n", p)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		fmt.Fprintf(&stream, "M 100644 inline %s\n%s", name, fastData(files[name]))
	}
	fmt.Fprintf(&stream, "\nget-mark :1\nreset %s\n\n", scratch)
	id, err := Run(dir, stream.String(), "fast-import", "--quiet", "--date-format=now")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// fastData is git fast-import's data command for content, given whole.
func fastData(content string) string {
	return fmt.Sprintf("data %d\n%s\n", len(content), content)
}
