package gitrepo_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// readFixture is a repository to read from: main with c1 and c2, f.txt
// different in each; feature/x with x on top of c1, adding dir/x.txt and the
// empty dir.md; the annotated tag v2 at c2, the tag tree at c1's tree, the
// commit gone that no ref reaches, and commits of which two share the
// abbreviated id prefix. calls counts the git processes run on repo.
type readFixture struct {
	repo                          *gitrepo.Repo
	calls                         *atomic.Int32
	c1, c2, x, v2, blob, gone, tr string
	prefix                        string
}

func newReadFixture(t *testing.T) readFixture {
	t.Helper()
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	const who = "committer T <t@example.invalid> 1700000000 +0000\n"
	stream := "commit refs/heads/main\nmark :1\n" + who + "data 2\nc1\nM 100644 inline f.txt\ndata 4\none\n\n" +
		"commit refs/heads/main\n" + who + "data 2\nc2\nM 100644 inline f.txt\ndata 4\ntwo\n\n" +
		"commit refs/heads/feature/x\n" + who + "data 1\nx\nfrom :1\nM 100644 inline dir/x.txt\ndata 2\nx\nM 100644 inline dir.md\ndata 0\n\n" +
		"commit refs/heads/gone\n" + who + "data 4\ngone\nfrom :1\n\n" +
		"tag v2\nfrom refs/heads/main\ntagger T <t@example.invalid> 1700000000 +0000\ndata 2\nv2\n"
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	f := readFixture{calls: new(atomic.Int32), prefix: gitrepotest.SharedPrefix(t, dir)}
	ids := strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main~1", "main", "feature/x", "refs/tags/v2", "main~1:f.txt", "gone", "main~1^{tree}"))
	f.c1, f.c2, f.x, f.v2, f.blob, f.gone, f.tr = ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6]
	gitrepotest.Git(t, dir, "update-ref", "refs/tags/tree", f.tr)
	gitrepotest.Git(t, dir, "update-ref", "-d", "refs/heads/gone")
	repo, err := gitrepo.Git{Trace: func([]string) { f.calls.Add(1) }}.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	f.repo = repo
	return f
}

// check compares a read's outcome with the wanted id or error, and, when
// noGit is set, asserts that the input reached no git process.
func (f readFixture) check(t *testing.T, read func() (string, error), want string, wantErr error, noGit bool) {
	t.Helper()
	f.calls.Store(0)
	got, err := read()
	if !errors.Is(err, wantErr) || got != want {
		t.Errorf("got %q, %v; want %q, %v", got, err, want, wantErr)
	}
	if n := f.calls.Load(); noGit && n != 0 {
		t.Errorf("%d git processes ran for an input refused before git", n)
	}
}

func TestListRefs_ListsTheRefsBelowAPrefixWithTheCommitTheyReach(t *testing.T) {
	t.Parallel()
	f := newReadFixture(t)
	for _, c := range []struct {
		prefixes []string
		want     []gitrepo.Ref
	}{
		{[]string{"refs/tags/"}, []gitrepo.Ref{{"refs/tags/tree", f.tr, ""}, {"refs/tags/v2", f.v2, f.c2}}},
		{[]string{"refs/heads/feature/", "refs/keep/none/"}, []gitrepo.Ref{{"refs/heads/feature/x", f.x, f.x}}},
		{[]string{"refs/heads/"}, []gitrepo.Ref{{"refs/heads/feature/x", f.x, f.x}, {"refs/heads/main", f.c2, f.c2}}},
	} {
		got, err := f.repo.ListRefs(context.Background(), c.prefixes...)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("ListRefs(%v) = %v, %v, want %v", c.prefixes, got, err, c.want)
		}
	}
	for _, p := range []string{"refs/tags/v", "refs/heads/*/", "--all"} {
		if got, err := f.repo.ListRefs(context.Background(), p); err == nil {
			t.Errorf("ListRefs(%q) = %v, want an error for a prefix that is not one", p, got)
		}
	}
}

func TestResolveRef_PeelsAFullRefNameToItsCommit(t *testing.T) {
	t.Parallel()
	f := newReadFixture(t)
	for _, c := range []struct {
		name    string
		want    string
		wantErr error
		noGit   bool
	}{
		{"refs/heads/main", f.c2, nil, false},
		{"refs/tags/v2", f.c2, nil, false},
		{"refs/heads/feature", "", gitrepo.ErrNotFound, false},
		{"refs/heads/nope", "", gitrepo.ErrNotFound, false},
		{"refs/tags/tree", "", gitrepo.ErrNotCommit, false},
		{"main", "", gitrepo.ErrNotFound, true},
		{"refs/heads/main~1", "", gitrepo.ErrNotFound, true},
		{"refs/heads/../main", "", gitrepo.ErrNotFound, true},
		{"refs/heads/a b", "", gitrepo.ErrNotFound, true},
		{"refs/heads/x.lock", "", gitrepo.ErrNotFound, true},
		{"refs/heads/*", "", gitrepo.ErrNotFound, true},
		{"--all", "", gitrepo.ErrNotFound, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f.check(t, func() (string, error) { return f.repo.ResolveRef(context.Background(), c.name) }, c.want, c.wantErr, c.noGit)
		})
	}
}

func TestResolveCommit_FindsTheOneCommitAnIdNames(t *testing.T) {
	t.Parallel()
	f := newReadFixture(t)
	for _, c := range []struct {
		name, id string
		want     string
		wantErr  error
		noGit    bool
	}{
		{"full id", f.c1, f.c1, nil, false},
		{"abbreviated id", f.c1[:7], f.c1, nil, false},
		{"upper case", strings.ToUpper(f.c1[:7]), f.c1, nil, false},
		{"commit no ref reaches", f.gone, f.gone, nil, false},
		{"prefix of two commits", f.prefix, "", gitrepo.ErrAmbiguous, false},
		{"no such object", "ffffffffff", "", gitrepo.ErrNotFound, false},
		{"blob", f.blob, "", gitrepo.ErrNotCommit, false},
		{"annotated tag", f.v2, "", gitrepo.ErrNotCommit, false},
		{"three characters", f.c1[:3], "", gitrepo.ErrNotFound, true},
		{"longer than an id", f.c1 + "0", "", gitrepo.ErrNotFound, true},
		{"not hexadecimal", "main", "", gitrepo.ErrNotFound, true},
		{"option", "--all", "", gitrepo.ErrNotFound, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f.check(t, func() (string, error) { return f.repo.ResolveCommit(context.Background(), c.id) }, c.want, c.wantErr, c.noGit)
		})
	}
}

func TestPeelCommit_ReachesTheCommitOfAnObject(t *testing.T) {
	t.Parallel()
	f := newReadFixture(t)
	for _, c := range []struct {
		name, id string
		want     string
		wantErr  error
		noGit    bool
	}{
		{"annotated tag", f.v2, f.c2, nil, false},
		{"commit", f.c1, f.c1, nil, false},
		{"blob", f.blob, "", gitrepo.ErrNotCommit, false},
		{"no such object", strings.Repeat("f", 40), "", gitrepo.ErrNotFound, false},
		{"abbreviated id", f.v2[:7], "", gitrepo.ErrNotFound, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f.check(t, func() (string, error) { return f.repo.PeelCommit(context.Background(), c.id) }, c.want, c.wantErr, c.noGit)
		})
	}
}

func TestObjectInfo_ReportsTypeAndSize(t *testing.T) {
	t.Parallel()
	f := newReadFixture(t)
	for _, c := range []struct {
		name    string
		want    gitrepo.Object
		wantErr error
	}{
		{f.c1 + ":f.txt", gitrepo.Object{ID: f.blob, Type: gitrepo.ObjectBlob, Size: 4}, nil},
		{f.x + ":dir", gitrepo.Object{ID: gitrepotest.Git(t, f.repo.Dir(), "rev-parse", f.x+":dir"), Type: gitrepo.ObjectTree, Size: 33}, nil},
		{f.c1 + ":nope", gitrepo.Object{}, gitrepo.ErrNotFound},
		{"main:f.txt", gitrepo.Object{}, gitrepo.ErrNotFound},
		{f.c1 + ":f.txt\n" + f.c1, gitrepo.Object{}, gitrepo.ErrNotFound},
	} {
		got, err := f.repo.ObjectInfo(context.Background(), c.name)
		if !errors.Is(err, c.wantErr) || got != c.want {
			t.Errorf("ObjectInfo(%q) = %+v, %v, want %+v, %v", c.name, got, err, c.want, c.wantErr)
		}
	}
}

func TestReadTree_ListsADirectoryOfACommitInPathOrder(t *testing.T) {
	t.Parallel()
	f := newReadFixture(t)
	dir := f.repo.Dir()
	id := func(rev string) string { return strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", rev)) }
	entry := func(path, mode, typ string, size int64) gitrepo.TreeEntry {
		return gitrepo.TreeEntry{Path: path, Name: path[strings.LastIndex(path, "/")+1:], Mode: mode, ID: id(f.x + ":" + path), Type: typ, Size: size}
	}
	// git orders dir.md before dir, as if dir were dir/; a path's byte
	// order puts dir first.
	d := entry("dir", "040000", gitrepo.ObjectTree, 0)
	md := entry("dir.md", "100644", gitrepo.ObjectBlob, 0)
	x := entry("dir/x.txt", "100644", gitrepo.ObjectBlob, 2)
	ft := entry("f.txt", "100644", gitrepo.ObjectBlob, 4)
	for _, c := range []struct {
		path      string
		recursive bool
		want      gitrepo.Tree
	}{
		{"", false, gitrepo.Tree{ID: id(f.x + "^{tree}"), Entries: []gitrepo.TreeEntry{d, md, ft}}},
		{"", true, gitrepo.Tree{ID: id(f.x + "^{tree}"), Entries: []gitrepo.TreeEntry{d, md, x, ft}}},
		{"dir", false, gitrepo.Tree{ID: id(f.x + ":dir"), Entries: []gitrepo.TreeEntry{x}}},
	} {
		got, err := f.repo.ReadTree(context.Background(), f.x, c.path, c.recursive)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ReadTree(x, %q, %v) = %+v, %v, want %+v", c.path, c.recursive, got, err, c.want)
		}
	}
}

func TestReadTree_TellsAMissingPathFromOneThatIsNoDirectory(t *testing.T) {
	t.Parallel()
	f := newReadFixture(t)
	for _, c := range []struct {
		commit, path string
		want         error
	}{
		{f.x, "nope", gitrepo.ErrNotFound},
		{f.x, "dir/nope", gitrepo.ErrNotFound},
		{f.x, "nope/deeper", gitrepo.ErrNotFound},
		{f.x, "f.txt/deeper", gitrepo.ErrNotFound},
		{f.x, "f.txt", gitrepo.ErrNotDirectory},
		{f.x, "dir/x.txt", gitrepo.ErrNotDirectory},
		{strings.Repeat("f", 40), "", gitrepo.ErrNotFound},
		{f.x[:7], "", gitrepo.ErrNotFound},
	} {
		if got, err := f.repo.ReadTree(context.Background(), c.commit, c.path, false); !errors.Is(err, c.want) {
			t.Errorf("ReadTree(%s, %q) = %+v, %v, want %v", c.commit, c.path, got, err, c.want)
		}
	}
}

func TestLookupFile_FindsTheFileAPathNamesAtACommit(t *testing.T) {
	t.Parallel()
	f := newReadFixture(t)
	id := func(rev string) string { return strings.TrimSpace(gitrepotest.Git(t, f.repo.Dir(), "rev-parse", rev)) }
	file := func(path string, size int64) gitrepo.TreeEntry {
		return gitrepo.TreeEntry{Path: path, Name: path[strings.LastIndex(path, "/")+1:], Mode: "100644", ID: id(f.x + ":" + path), Type: gitrepo.ObjectBlob, Size: size}
	}
	for _, c := range []struct {
		commit, path string
		want         gitrepo.TreeEntry
		wantErr      error
	}{
		{f.x, "f.txt", file("f.txt", 4), nil},
		{f.x, "dir/x.txt", file("dir/x.txt", 2), nil},
		{f.x, "dir.md", file("dir.md", 0), nil},
		{f.x, "dir", gitrepo.TreeEntry{}, gitrepo.ErrNotFile},
		{f.x, "nope", gitrepo.TreeEntry{}, gitrepo.ErrNotFound},
		{f.x, "dir/nope", gitrepo.TreeEntry{}, gitrepo.ErrNotFound},
		{f.x, "nope/x.txt", gitrepo.TreeEntry{}, gitrepo.ErrNotFound},
		{f.x, "f.txt/deeper", gitrepo.TreeEntry{}, gitrepo.ErrNotFound},
		{f.x, "", gitrepo.TreeEntry{}, gitrepo.ErrNotFound},
		{strings.Repeat("f", 40), "f.txt", gitrepo.TreeEntry{}, gitrepo.ErrNotFound},
		{f.x[:7], "f.txt", gitrepo.TreeEntry{}, gitrepo.ErrNotFound},
	} {
		got, err := f.repo.LookupFile(context.Background(), c.commit, c.path)
		if !errors.Is(err, c.wantErr) || got != c.want {
			t.Errorf("LookupFile(%s, %q) = %+v, %v, want %+v, %v", c.commit, c.path, got, err, c.want, c.wantErr)
		}
	}
}

func TestWriteBlob_WritesTheBytesOfABlob(t *testing.T) {
	t.Parallel()
	f := newReadFixture(t)
	var b strings.Builder
	if err := f.repo.WriteBlob(context.Background(), f.blob, &b); err != nil || b.String() != "one\n" {
		t.Errorf("WriteBlob(f.txt at c1) wrote %q, %v; want %q", b.String(), err, "one\n")
	}
	for _, id := range []string{f.tr, strings.Repeat("f", 40)} {
		if err := f.repo.WriteBlob(context.Background(), id, io.Discard); err == nil {
			t.Errorf("WriteBlob(%s), no blob, = nil, want an error", id)
		}
	}
	f.calls.Store(0)
	if err := f.repo.WriteBlob(context.Background(), f.blob[:7], io.Discard); !errors.Is(err, gitrepo.ErrNotFound) || f.calls.Load() != 0 {
		t.Errorf("WriteBlob(abbreviated id) = %v after %d git processes, want ErrNotFound and none", err, f.calls.Load())
	}
}

// stallingWriter takes the first write, signals it on first, and then
// takes nothing more until ctx ends, so that git fills its pipe and waits.
type stallingWriter struct {
	ctx   context.Context
	first chan struct{}
	once  sync.Once
}

func (w *stallingWriter) Write(p []byte) (int, error) {
	took := false
	w.once.Do(func() { took = true; close(w.first) })
	if took {
		return len(p), nil
	}
	<-w.ctx.Done()
	return 0, w.ctx.Err()
}

func TestWriteBlob_EndsGitWhenItsContextEndsMidRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	big := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB, far more than a pipe holds
	stream := "commit refs/heads/main\ncommitter T <t@example.invalid> 1700000000 +0000\ndata 3\nbig\n" +
		"M 100644 inline big.dat\ndata " + strconv.Itoa(len(big)) + "\n" + big + "\n\n"
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	blob := strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", "main:big.dat"))
	repo, err := gitrepo.Git{}.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &stallingWriter{ctx: ctx, first: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- repo.WriteBlob(ctx, blob, w) }()
	select {
	case <-w.first:
	case err := <-done:
		t.Fatalf("WriteBlob returned %v before git wrote anything", err)
	case <-time.After(5 * time.Second):
		t.Fatal("git wrote nothing within 5s")
	}
	cancel()
	// WriteBlob returns once git has exited and been waited for.
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("WriteBlob = %v, want an error wrapping context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WriteBlob did not return within 1s of the end of its context")
	}
}
