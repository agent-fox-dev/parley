package gitrepotest_test

import (
	"path/filepath"
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
