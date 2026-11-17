package gitrepotest_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestCommit_WritesTheCommitGitWould(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "r.git")
	gitrepotest.InitBare(t, dir, "main")
	// The parent's tree holds the directory a, which git orders as "a/":
	// after a.c and before a0. It comes in loose, as a fetch of a few
	// objects leaves it.
	other := filepath.Join(t.TempDir(), "other.git")
	gitrepotest.InitBare(t, other, "main")
	who := "committer T <t@example.invalid> 1700000000 +0000\n"
	parent, err := gitrepotest.Run(other, "commit refs/heads/main\nmark :1\n"+who+"data 2\np\n"+
		"M 100644 inline a-b\ndata 2\nab\nM 100644 inline a.c\ndata 2\nac\nM 100644 inline a0\ndata 2\na0\n"+
		"M 100644 inline a/x.txt\ndata 2\nax\n\nget-mark :1\n", "fast-import", "--quiet")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitrepotest.Run(dir, gitrepotest.Pack(t, other, parent), "unpack-objects", "-q"); err != nil {
		t.Fatal(err)
	}

	child := gitrepotest.Commit(t, dir, "two", map[string]string{"a.c": "changed\n", "new": "n\n"}, parent)
	merge := gitrepotest.Commit(t, dir, "merge", nil, child, parent)

	// git fast-import's commit of the same files on the same parent has the
	// same tree.
	want, err := gitrepotest.Run(dir, "commit refs/gitrepotest/want\nmark :1\n"+who+"data 4\nwant\nfrom "+parent+"\n"+
		"M 100644 inline a.c\ndata 8\nchanged\nM 100644 inline new\ndata 2\nn\n\nget-mark :1\nreset refs/gitrepotest/want\n\n",
		"fast-import", "--quiet")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ commit, wantTree string }{
		{child, want + "^{tree}"},
		{merge, child + "^{tree}"},
	} {
		if got, want := gitrepotest.Git(t, dir, "rev-parse", c.commit+"^{tree}"), gitrepotest.Git(t, dir, "rev-parse", c.wantTree); got != want {
			t.Errorf("the tree of %s = %s, want %s", c.commit, got, want)
		}
	}
	if got := gitrepotest.Git(t, dir, "log", "--format=%P|%an <%ae>|%cn <%ce>|%B", "-1", merge); got != child+" "+parent+"|Origin <origin@example.invalid>|Origin <origin@example.invalid>|merge" {
		t.Errorf("the merge commit = %q, want parents %s %s by Origin with the message merge", got, child, parent)
	}
	gitrepotest.Git(t, dir, "fsck", "--strict", "--no-dangling", merge)
}

func TestOriginCommit_MovesTheBranchToACommitOfTheOneFile(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	first := o.Commit("main", "one")
	// A head whose tree holds other files: the next commit drops them.
	wide := gitrepotest.Commit(t, o.Dir(), "wide", map[string]string{"a.txt": "a\n", "README": "r\n"}, first)
	gitrepotest.SetRef(t, o.Dir(), "refs/heads/main", wide)
	second := o.Commit("main", "two")

	if got := o.Head("main"); got != second {
		t.Errorf("main = %s, want the new commit %s", got, second)
	}
	for _, c := range []struct{ commit, parents, msg string }{
		{first, "", "one"},
		{second, wide, "two"},
	} {
		if got := gitrepotest.Git(t, o.Dir(), "log", "--format=%P|%an <%ae>|%B", "-1", c.commit); got != c.parents+"|Origin <origin@example.invalid>|"+c.msg {
			t.Errorf("the commit %s = %q, want parents %q by Origin with the message %s", c.commit, got, c.parents, c.msg)
		}
		if got := gitrepotest.Git(t, o.Dir(), "ls-tree", "--name-only", c.commit); got != "README" {
			t.Errorf("the files of %s = %q, want README alone", c.commit, got)
		}
	}
	if a, b := gitrepotest.Blob(t, o.Dir(), first+":README"), gitrepotest.Blob(t, o.Dir(), second+":README"); string(a) == string(b) || !strings.HasPrefix(string(b), "two\n") {
		t.Errorf("README = %q then %q, want a new content of each commit, the second's starting with its message", a, b)
	}
	gitrepotest.Git(t, o.Dir(), "fsck", "--strict", "--no-dangling", second)
}
