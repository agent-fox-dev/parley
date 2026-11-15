package scm_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/scm"
)

// gitArgs records the arguments of every git process a repository runs.
type gitArgs struct {
	mu   sync.Mutex
	args [][]string
}

func (g *gitArgs) trace(args []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.args = append(g.args, args)
}

// reset forgets what was recorded and returns it.
func (g *gitArgs) reset() [][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	seen := g.args
	g.args = nil
	return seen
}

// revisionRepo is a workspace repository whose refs name commits in every
// way a revision can: c1, c2 and c3 on main, f.txt different in each;
// feature/x with x on top of c2; trunk at c1; the lightweight tag v1 at c1
// and the annotated tag v2 at c3; refs/parley/changes/12/3 at x; a branch
// and a tag both named dup; a branch named refs/tags/v1 at c2; the commit
// gone, holding only-on-gone.txt, that no ref reaches; and two commits whose
// ids share the four characters prefix. id maps each of these names, blob
// (f.txt at c1) and v2tag (the tag object) to its object id.
type revisionRepo struct {
	repo   *gitrepo.Repo
	git    *gitArgs
	id     map[string]string
	prefix string
}

func newRevisionRepo(t *testing.T) *revisionRepo {
	t.Helper()
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	const who = "committer T <t@example.invalid> 1700000000 +0000\n"
	commit := func(ref, msg, from, file string) string {
		s := "commit " + ref + "\nmark :" + msg[1:] + "\n" + who + "data 2\n" + msg + "\n"
		if from != "" {
			s += "from " + from + "\n"
		}
		return s + "M 100644 inline " + file + "\ndata 3\n" + msg + "\n\n"
	}
	stream := commit("refs/heads/main", "c1", "", "f.txt") +
		commit("refs/heads/main", "c2", "", "f.txt") +
		commit("refs/heads/main", "c3", "", "f.txt") +
		commit("refs/heads/feature/x", "c4", ":2", "x.txt") +
		commit("refs/heads/gone", "c5", ":1", "only-on-gone.txt") +
		"reset refs/heads/trunk\nfrom :1\n\nreset refs/tags/v1\nfrom :1\n\n" +
		"reset refs/parley/changes/12/3\nfrom :4\n\nreset refs/heads/dup\nfrom :1\n\nreset refs/tags/dup\nfrom :2\n\n" +
		"reset refs/heads/refs/tags/v1\nfrom :2\n\n" +
		"tag v2\nfrom :3\ntagger T <t@example.invalid> 1700000000 +0000\ndata 2\nv2\n"
	if _, err := gitrepotest.Run(dir, stream, "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	w := &revisionRepo{git: &gitArgs{}, id: map[string]string{}, prefix: gitrepotest.SharedPrefix(t, dir)}
	names := []string{"c1", "c2", "c3", "x", "gone", "blob", "v2tag"}
	ids := strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main~2", "main~1", "main", "feature/x", "gone", "main~2:f.txt", "refs/tags/v2"))
	for i, name := range names {
		w.id[name] = ids[i]
	}
	gitrepotest.Git(t, dir, "update-ref", "-d", "refs/heads/gone")
	repo, err := gitrepo.Git{Trace: w.git.trace}.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	w.repo = repo
	w.git.reset()
	return w
}

// refList is a RefView that answers a fixed list of refs.
type refList []scm.Ref

func (l refList) Refs(context.Context) ([]scm.Ref, error) { return l, nil }
