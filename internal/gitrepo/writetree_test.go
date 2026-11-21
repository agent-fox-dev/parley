package gitrepo_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// A tree may hold names that collide on a file system that folds case, one
// of them a link out of the directory: neither may be written through.
func TestWriteTree_WritesNothingOutsideItsDirectory(t *testing.T) {
	t.Parallel()
	ctx, dir := context.Background(), filepath.Join(t.TempDir(), "repo.git")
	outside := t.TempDir()
	gitrepotest.InitBare(t, dir, "main")
	// The tree is made entry by entry: an import would fold the names that
	// collide where the repository itself lies on such a file system.
	run := func(stdin string, args ...string) string {
		t.Helper()
		out, err := gitrepotest.Run(dir, stdin, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	blob := func(content string) string { return run(content, "hash-object", "-w", "--stdin") }
	sub := run("100644 blob "+blob("planted\n")+"\tplanted.txt\n", "mktree")
	top := run("120000 blob "+blob(outside)+"\tEscape\n"+
		"120000 blob "+blob(filepath.Join(outside, "target.txt"))+"\tTarget\n"+
		"040000 tree "+sub+"\tescape\n"+
		"100644 blob "+blob("kept\n")+"\tkept.txt\n"+
		"100644 blob "+blob("through the link\n")+"\ttarget\n", "mktree")
	head := run("c\n", "-c", "user.name=T", "-c", "user.email=t@example.invalid", "commit-tree", top)
	repo, err := gitrepo.Git{}.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	tree := filepath.Join(t.TempDir(), "tree")
	if _, err := repo.WriteTree(ctx, head, tree); err != nil {
		t.Fatalf("WriteTree: %v", err)
	}
	if left, err := os.ReadDir(outside); err != nil || len(left) != 0 {
		t.Errorf("the directory the tree's links point at holds %v (%v), want nothing written into it", left, err)
	}
	if got, err := os.ReadFile(filepath.Join(tree, "kept.txt")); err != nil || string(got) != "kept\n" {
		t.Errorf("kept.txt = %q (%v), want the blob's bytes", got, err)
	}
	if target, err := os.Readlink(filepath.Join(tree, "Escape")); err != nil || target != outside {
		t.Errorf("Escape -> %q (%v), want the link as committed, to %s", target, err, outside)
	}
	// Where the names do not collide, both are written, each as itself.
	if runtime.GOOS == "linux" {
		if got, err := os.ReadFile(filepath.Join(tree, "escape", "planted.txt")); err != nil || !strings.HasPrefix(string(got), "planted") {
			t.Errorf("escape/planted.txt = %q (%v), want the file inside the tree", got, err)
		}
	}
}
