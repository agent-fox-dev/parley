package land

import (
	"context"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// built is what the replay of an attempt's changes made of them: the
// target head it was built on, onto, the commit the target is to move to,
// head, which is onto when nothing was left to land, and one entry per
// landed commit, bottom first.
type built struct {
	onto, head string
	commits    []Commit
}

// noop reports whether nothing was left to land.
func (b built) noop() bool { return b.head == b.onto }

// build replays the own commits of each change a covers, those of its
// version that are not in its base, in order onto onto, each change onto
// the result of the one below it, in the repository of the target, which
// holds the head of every version. A commit whose parent already is what
// it is replayed onto is kept as it is, so a version on the target head
// lands as it is; a commit whose change the target already has is left
// out; every other commit is made anew with its author, author date and
// message and the landing identity as committer. A commit that does not
// apply ends the replay with its conflict report, completed (conflict).
func (c *Component) build(ctx context.Context, repo *gitrepo.Repo, a Attempt, onto string) (built, *gitrepo.ConflictReport, error) {
	b := built{onto: onto, head: onto, commits: []Commit{}}
	committer := gitrepo.Signature{Name: c.deps.Committer.Name, Email: c.deps.Committer.Email, Time: c.now()}
	for _, cov := range a.Covered {
		head, became, report, err := repo.RebaseFromAs(ctx, cov.Base, cov.Head, b.head, committer)
		if err != nil {
			return built{}, nil, fmt.Errorf("replay %s onto %s: %w", change.Ref(a.Workspace, cov.Number), b.head, err)
		}
		if report != nil {
			return built{}, c.conflict(ctx, a, cov, onto, report), nil
		}
		b.head = head
		for _, r := range became {
			if r.To != "" {
				b.commits = append(b.commits, Commit{Change: cov.Number, VersionSHA: r.From, LandedSHA: r.To})
			}
		}
	}
	return b, nil, nil
}
