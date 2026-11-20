package gitrepo_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// treeFixture is a repository whose branch main holds a.txt (100644, "a"),
// bin/run (100755), docs/guide.md and the submodule entry vendor/lib, with
// main's commit, its tree and the tree's files.
type treeFixture struct {
	repo   *gitrepo.Repo
	dir    string
	parent string
	tree   string
	base   map[string]entry
}

// entry is a file of a tree as git ls-tree -r lists it.
type entry struct{ mode, id string }

func newTreeFixture(t *testing.T) treeFixture {
	t.Helper()
	dir := t.TempDir()
	gitrepotest.InitBare(t, dir, "main")
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	stream := "commit refs/heads/main\ncommitter T <t@example.invalid> 1700000000 +0000\n" + data("c1") +
		"M 100644 inline a.txt\n" + data("a\n") +
		"M 100755 inline bin/run\n" + data("#!/bin/sh\n") +
		"M 100644 inline docs/guide.md\n" + data("guide\n") +
		"M 160000 " + strings.Repeat("5", 40) + " vendor/lib\n"
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	repo, err := (gitrepo.Git{}).Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	f := treeFixture{repo: repo, dir: dir}
	f.parent = gitrepotest.Git(t, dir, "rev-parse", "main")
	f.tree = gitrepotest.Git(t, dir, "rev-parse", "main^{tree}")
	f.base = f.entries(t, f.tree)
	return f
}

// entries are the files of tree by path.
func (f treeFixture) entries(t *testing.T, tree string) map[string]entry {
	t.Helper()
	files := map[string]entry{}
	for rec := range strings.SplitSeq(gitrepotest.Git(t, f.dir, "ls-tree", "-r", "-z", tree), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, _ := strings.Cut(rec, "\t")
		fields := strings.Fields(meta)
		files[path] = entry{mode: fields[0], id: fields[2]}
	}
	return files
}

// file is the entry of a file of mode with content.
func (f treeFixture) file(t *testing.T, mode, content string) *entry {
	t.Helper()
	id, err := gitrepotest.Run(f.dir, content, "hash-object", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	return &entry{mode: mode, id: id}
}

// blob is the content of the blob id, as git cat-file blob writes it.
func (f treeFixture) blob(t *testing.T, id string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+f.dir, "cat-file", "blob", id).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// apply applies ops to main and returns the tree.
func (f treeFixture) apply(t *testing.T, ops ...gitrepo.Operation) string {
	t.Helper()
	tree, err := f.repo.ApplyOperations(context.Background(), f.parent, ops)
	if err != nil {
		t.Fatalf("ApplyOperations: %v", err)
	}
	return tree
}

// assertTree fails t unless tree holds main's files with changes made, a
// nil change being a path the tree lacks, and lacks every path of absent.
func (f treeFixture) assertTree(t *testing.T, tree string, changes map[string]*entry, absent ...string) {
	t.Helper()
	want := maps.Clone(f.base)
	for path, e := range changes {
		if e == nil {
			delete(want, path)
		} else {
			want[path] = *e
		}
	}
	if got := f.entries(t, tree); !reflect.DeepEqual(got, want) {
		t.Errorf("tree %s holds\n%v\nwant\n%v", tree, got, want)
	}
	for _, path := range absent {
		if out := gitrepotest.Git(t, f.dir, "ls-tree", tree, "--", path); out != "" {
			t.Errorf("tree %s holds %s: %s", tree, path, out)
		}
	}
}

func put(path, content string) gitrepo.Operation {
	return gitrepo.Operation{Op: gitrepo.OpPut, Path: path, Content: []byte(content)}
}

func putMode(path, content, mode string) gitrepo.Operation {
	return gitrepo.Operation{Op: gitrepo.OpPut, Path: path, Content: []byte(content), Mode: mode}
}

func del(path string) gitrepo.Operation { return gitrepo.Operation{Op: gitrepo.OpDelete, Path: path} }

func move(from, to string) gitrepo.Operation {
	return gitrepo.Operation{Op: gitrepo.OpMove, From: from, To: to}
}

func chmod(path, mode string) gitrepo.Operation {
	return gitrepo.Operation{Op: gitrepo.OpChmod, Path: path, Mode: mode}
}

func TestApplyOperations_PutsAFile(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t)
	for _, c := range []struct {
		name          string
		op            gitrepo.Operation
		mode, content string
	}{
		{"creates the parent directories", put("new/deep/f.txt", "x"), "100644", "x"},
		{"replaces a file", put("a.txt", "new"), "100644", "new"},
		{"stores the bytes as given", put("img.bin", "\x00\xff\x10"), "100644", "\x00\xff\x10"},
		{"sets the executable mode", putMode("tool", "t", "100755"), "100755", "t"},
		{"stores a symlink's target", putMode("link", "a.txt", "120000"), "120000", "a.txt"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tree := f.apply(t, c.op)
			f.assertTree(t, tree, map[string]*entry{c.op.Path: f.file(t, c.mode, c.content)})
			if got := f.blob(t, f.entries(t, tree)[c.op.Path].id); got != c.content {
				t.Errorf("%s holds %q, want %q", c.op.Path, got, c.content)
			}
		})
	}
	t.Run("leaves the tree as it was for a file's own content", func(t *testing.T) {
		t.Parallel()
		if tree := f.apply(t, put("a.txt", "a\n")); tree != f.tree {
			t.Errorf("tree %s, want the parent's %s", tree, f.tree)
		}
	})
}

func TestApplyOperations_DeletesMovesAndChangesModes(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t)
	run := f.base["bin/run"]
	for _, c := range []struct {
		name    string
		op      gitrepo.Operation
		changes map[string]*entry
		absent  []string
	}{
		{"deletes a file", del("a.txt"), map[string]*entry{"a.txt": nil}, nil},
		{"drops the directory it empties", del("docs/guide.md"), map[string]*entry{"docs/guide.md": nil}, []string{"docs"}},
		{"moves a file with its blob and mode", move("bin/run", "tools/run"),
			map[string]*entry{"bin/run": nil, "tools/run": &run}, []string{"bin"}},
		{"changes the mode alone", chmod("a.txt", "100755"),
			map[string]*entry{"a.txt": {mode: "100755", id: f.base["a.txt"].id}}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f.assertTree(t, f.apply(t, c.op), c.changes, c.absent...)
		})
	}
}

func TestApplyOperations_AppliesEachToTheTreeTheLastOneLeft(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t)
	for _, c := range []struct {
		name    string
		ops     []gitrepo.Operation
		changes map[string]*entry
	}{
		{"a file put and deleted is gone", []gitrepo.Operation{put("a", "x"), del("a")}, nil},
		{"a deleted path can be put again", []gitrepo.Operation{del("a.txt"), put("a.txt", "z")},
			map[string]*entry{"a.txt": f.file(t, "100644", "z")}},
		{"a put file can be moved and its mode changed",
			[]gitrepo.Operation{put("n", "x"), move("n", "m"), chmod("m", "100755")},
			map[string]*entry{"m": f.file(t, "100755", "x")}},
		{"a moved file's old path can be put", []gitrepo.Operation{move("a.txt", "b.txt"), put("a.txt", "again")},
			map[string]*entry{"b.txt": new(f.base["a.txt"]), "a.txt": f.file(t, "100644", "again")}},
		{"a directory emptied can become a file",
			[]gitrepo.Operation{put("d/f", "x"), del("d/f"), put("d", "file")},
			map[string]*entry{"d": f.file(t, "100644", "file")}},
		{"a file deleted can become a directory",
			[]gitrepo.Operation{del("a.txt"), put("a.txt/b", "x")},
			map[string]*entry{"a.txt": nil, "a.txt/b": f.file(t, "100644", "x")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f.assertTree(t, f.apply(t, c.ops...), c.changes)
		})
	}
}

func TestApplyOperations_RefusesTheFirstOperationThatDoesNotApply(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t)
	ok := put("ok.txt", "ok")
	for _, c := range []struct {
		name string
		ops  []gitrepo.Operation
		err  error
	}{
		{"delete of a missing path", []gitrepo.Operation{ok, del("missing.txt")}, gitrepo.ErrNoFile},
		{"delete of a directory", []gitrepo.Operation{ok, del("docs")}, gitrepo.ErrNoFile},
		{"delete of a path deleted before", []gitrepo.Operation{del("a.txt"), del("a.txt")}, gitrepo.ErrNoFile},
		{"move of a missing path", []gitrepo.Operation{ok, move("missing.txt", "x")}, gitrepo.ErrNoFile},
		{"move onto a file", []gitrepo.Operation{ok, move("a.txt", "bin/run")}, gitrepo.ErrPathExists},
		{"move onto a directory", []gitrepo.Operation{ok, move("a.txt", "docs")}, gitrepo.ErrPathExists},
		{"move onto itself", []gitrepo.Operation{ok, move("a.txt", "a.txt")}, gitrepo.ErrPathExists},
		{"move under a file", []gitrepo.Operation{ok, move("a.txt", "bin/run/x")}, gitrepo.ErrUnderFile},
		{"chmod of a missing path", []gitrepo.Operation{ok, chmod("missing.txt", "100755")}, gitrepo.ErrNoFile},
		{"chmod of a directory", []gitrepo.Operation{ok, chmod("docs", "100755")}, gitrepo.ErrNoFile},
		{"put under a file", []gitrepo.Operation{ok, put("a.txt/child", "x")}, gitrepo.ErrUnderFile},
		{"put deep under a file", []gitrepo.Operation{ok, put("bin/run/x/y", "x")}, gitrepo.ErrUnderFile},
		{"put under a file put before", []gitrepo.Operation{put("d", "x"), put("d/f", "y")}, gitrepo.ErrUnderFile},
		{"put onto a directory", []gitrepo.Operation{ok, put("docs", "x")}, gitrepo.ErrIsDirectory},
		{"put onto a directory made before", []gitrepo.Operation{put("d/f", "x"), put("d", "y")}, gitrepo.ErrIsDirectory},
		{"put onto a submodule", []gitrepo.Operation{ok, put("vendor/lib", "x")}, gitrepo.ErrSubmodule},
		{"put under a submodule", []gitrepo.Operation{ok, put("vendor/lib/x", "x")}, gitrepo.ErrSubmodule},
		{"delete of a submodule", []gitrepo.Operation{ok, del("vendor/lib")}, gitrepo.ErrSubmodule},
		{"move of a submodule", []gitrepo.Operation{ok, move("vendor/lib", "lib")}, gitrepo.ErrSubmodule},
		{"move into a submodule", []gitrepo.Operation{ok, move("a.txt", "vendor/lib/a.txt")}, gitrepo.ErrSubmodule},
		{"chmod of a submodule", []gitrepo.Operation{ok, chmod("vendor/lib", "100755")}, gitrepo.ErrSubmodule},
		{"the first of several", []gitrepo.Operation{ok, del("missing.txt"), put("docs", "x")}, gitrepo.ErrNoFile},
		{"an unknown kind", []gitrepo.Operation{ok, {Op: "copy", Path: "a.txt"}}, gitrepo.ErrInvalidOperation},
		{"an empty path", []gitrepo.Operation{ok, put("", "x")}, gitrepo.ErrInvalidOperation},
		{"an absolute path", []gitrepo.Operation{ok, put("/abs", "x")}, gitrepo.ErrInvalidOperation},
		{"a path ending in a slash", []gitrepo.Operation{ok, put("dir/", "x")}, gitrepo.ErrInvalidOperation},
		{"an empty segment", []gitrepo.Operation{ok, put("a//b", "x")}, gitrepo.ErrInvalidOperation},
		{"a dot segment", []gitrepo.Operation{ok, put("a/./b", "x")}, gitrepo.ErrInvalidOperation},
		{"a dot-dot segment", []gitrepo.Operation{ok, del("a/../a.txt")}, gitrepo.ErrInvalidOperation},
		{"a .git segment", []gitrepo.Operation{ok, put("x/.git/hooks/h", "x")}, gitrepo.ErrInvalidOperation},
		{"a .git segment of another case", []gitrepo.Operation{ok, move("a.txt", ".GIT/config")}, gitrepo.ErrInvalidOperation},
		{"a put mode of no file", []gitrepo.Operation{ok, putMode("x", "x", "100600")}, gitrepo.ErrInvalidOperation},
		{"a put of a submodule", []gitrepo.Operation{ok, putMode("x", "x", "160000")}, gitrepo.ErrInvalidOperation},
		{"a chmod to a symlink", []gitrepo.Operation{ok, chmod("a.txt", "120000")}, gitrepo.ErrInvalidOperation},
		{"a chmod without a mode", []gitrepo.Operation{ok, chmod("a.txt", "")}, gitrepo.ErrInvalidOperation},
		{"a move without a target", []gitrepo.Operation{ok, move("a.txt", "")}, gitrepo.ErrInvalidOperation},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tree, err := f.repo.ApplyOperations(context.Background(), f.parent, c.ops)
			var opErr *gitrepo.OperationError
			if !errors.As(err, &opErr) || opErr.Index != 1 || !errors.Is(err, c.err) {
				t.Fatalf("ApplyOperations = %q, %v; want an OperationError of index 1 and %v", tree, err, c.err)
			}
			if tree != "" {
				t.Errorf("tree %q, want none", tree)
			}
		})
	}
	t.Run("a parent that is no commit", func(t *testing.T) {
		t.Parallel()
		_, err := f.repo.ApplyOperations(context.Background(), f.tree, []gitrepo.Operation{ok})
		var opErr *gitrepo.OperationError
		if err == nil || errors.As(err, &opErr) {
			t.Errorf("ApplyOperations on a tree = %v, want an error naming no operation", err)
		}
	})
}

// The temporary index lives in the system's temporary directory, which the
// test points elsewhere, so it cannot run in parallel.
func TestApplyOperations_LeavesNoIndexBehind(t *testing.T) {
	f := newTreeFixture(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	if _, err := f.repo.ApplyOperations(context.Background(), f.parent,
		[]gitrepo.Operation{put("new.txt", "x"), move("a.txt", "b.txt")}); err != nil {
		t.Fatalf("ApplyOperations: %v", err)
	}
	if _, err := f.repo.ApplyOperations(context.Background(), f.parent,
		[]gitrepo.Operation{put("new.txt", "x"), del("missing.txt")}); err == nil {
		t.Fatal("ApplyOperations of a missing path's delete: no error")
	}
	if left, err := os.ReadDir(tmp); err != nil || len(left) > 0 {
		t.Errorf("temporary directory holds %v (%v), want nothing", left, err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "index")); !os.IsNotExist(err) {
		t.Errorf("the repository has an index: %v", err)
	}
}

func TestCreateCommit_WritesTheCommitAsGiven(t *testing.T) {
	t.Parallel()
	f := newTreeFixture(t)
	author := gitrepo.Signature{Name: "Ada Lovelace", Email: "ada@example.invalid",
		Time: time.Unix(1700000000, 0).In(time.FixedZone("", 2*3600))}
	committer := gitrepo.HubSignature(time.Unix(1700000100, 0).In(time.FixedZone("", -(5*3600 + 30*60))))
	for _, c := range []struct {
		name string
		spec gitrepo.CommitSpec
		want string
	}{
		{"a commit with a parent", gitrepo.CommitSpec{Tree: f.tree, Parents: []string{f.parent}, Message: "change\n\nbody\n",
			Author: author, Committer: committer},
			"tree " + f.tree + "\nparent " + f.parent + "\nauthor Ada Lovelace <ada@example.invalid> 1700000000 +0200\n" +
				"committer parley <parley@localhost> 1700000100 -0530\n\nchange\n\nbody\n"},
		{"a root commit, its message ended by a newline", gitrepo.CommitSpec{Tree: f.tree, Message: "root",
			Author: author, Committer: author},
			"tree " + f.tree + "\nauthor Ada Lovelace <ada@example.invalid> 1700000000 +0200\n" +
				"committer Ada Lovelace <ada@example.invalid> 1700000000 +0200\n\nroot\n"},
		{"names without the characters that end them", gitrepo.CommitSpec{Tree: f.tree, Parents: []string{f.parent}, Message: "m\n",
			Author: gitrepo.Signature{Name: "Eve <x>\n", Email: "<eve@example.invalid>", Time: time.Unix(1700000000, 0).UTC()}, Committer: committer},
			"tree " + f.tree + "\nparent " + f.parent + "\nauthor Eve x <eve@example.invalid> 1700000000 +0000\n" +
				"committer parley <parley@localhost> 1700000100 -0530\n\nm\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			repo, err := gitrepo.Git{Trace: func([]string) { calls.Add(1) }}.Open(context.Background(), f.dir)
			if err != nil {
				t.Fatal(err)
			}
			id, err := repo.CreateCommit(context.Background(), c.spec)
			if err != nil {
				t.Fatalf("CreateCommit: %v", err)
			}
			if got := gitrepotest.Git(t, f.dir, "cat-file", "commit", id) + "\n"; got != c.want {
				t.Errorf("commit %s is\n%s\nwant\n%s", id, got, c.want)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("CreateCommit ran %d git processes, want none: it reads the types of its tree and parents and writes the commit itself", n)
			}
			gitrepotest.Git(t, f.dir, "fsck", "--strict", "--no-dangling")
		})
	}
	t.Run("into the object store a Git's environment names", func(t *testing.T) {
		t.Parallel()
		// As a quarantine has git write the new objects of a push.
		objects := t.TempDir()
		repo, err := gitrepo.Git{Env: []string{"GIT_OBJECT_DIRECTORY=" + objects,
			"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + filepath.Join(f.dir, "objects")}}.Open(context.Background(), f.dir)
		if err != nil {
			t.Fatal(err)
		}
		id, err := repo.CreateCommit(context.Background(), gitrepo.CommitSpec{Tree: f.tree, Parents: []string{f.parent},
			Message: "quarantined\n", Author: author, Committer: committer})
		if err != nil {
			t.Fatalf("CreateCommit: %v", err)
		}
		if _, err := os.Stat(filepath.Join(objects, id[:2], id[2:])); err != nil {
			t.Errorf("the commit is not in the object store the environment names: %v", err)
		}
		if _, err := os.Stat(filepath.Join(f.dir, "objects", id[:2], id[2:])); err == nil {
			t.Errorf("the commit %s is in the repository's own object store, want it only in the one the environment names", id)
		}
	})
	for _, c := range []struct {
		name string
		spec gitrepo.CommitSpec
	}{
		{"a tree that is no tree", gitrepo.CommitSpec{Tree: f.parent, Author: author, Committer: committer}},
		{"a missing parent", gitrepo.CommitSpec{Tree: f.tree, Parents: []string{strings.Repeat("1", 40)}, Author: author, Committer: committer}},
		{"a parent that is no commit", gitrepo.CommitSpec{Tree: f.tree, Parents: []string{f.tree}, Author: author, Committer: committer}},
		{"an author without a name", gitrepo.CommitSpec{Tree: f.tree, Author: gitrepo.Signature{Name: "<>", Email: "e@example.invalid", Time: author.Time}, Committer: committer}},
		{"a committer without a time", gitrepo.CommitSpec{Tree: f.tree, Author: author, Committer: gitrepo.HubSignature(time.Time{})}},
	} {
		t.Run("refuses "+c.name, func(t *testing.T) {
			t.Parallel()
			if id, err := f.repo.CreateCommit(context.Background(), c.spec); err == nil {
				t.Errorf("CreateCommit = %s, want an error", id)
			}
		})
	}
}
