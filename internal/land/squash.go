package land

import (
	"context"
	"fmt"
	"strings"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// squash is the one commit that lands the covered change cov of the
// attempt a under the squash strategy on top of onto, the head its own
// commits were replayed onto, given replayed, the head that replay ended
// at, and became, what each of its own commits became: a commit on onto
// whose tree is replayed's, whose author is the author of the version's
// head commit, with the time and zone of its authorship, and whose
// committer is the landing identity committer, with the message
// squashMessage writes. It is "" when the replay left the tree
// of onto as it was, since the target already holds the change.
func (c *Component) squash(ctx context.Context, repo *gitrepo.Repo, a Attempt, cov CoveredChange, onto, replayed string,
	became []gitrepo.ReplayedCommit, committer gitrepo.Signature) (string, error) {
	ref := change.Ref(a.Workspace, cov.Number)
	heads, err := repo.ReadCommits(ctx, []string{onto, replayed})
	if err != nil {
		return "", fmt.Errorf("squash %s: %w", ref, err)
	}
	if heads[0].Tree == heads[1].Tree {
		return "", nil
	}
	own := make([]string, len(became))
	for i, r := range became {
		own[i] = r.From
	}
	commits, err := repo.ReadCommits(ctx, own)
	if err != nil {
		return "", fmt.Errorf("squash %s: %w", ref, err)
	}
	if len(commits) == 0 || commits[len(commits)-1].ID != cov.Head {
		return "", fmt.Errorf("squash %s: the version's head %s is not its last own commit", ref, cov.Head)
	}
	ch, err := c.deps.Changes.ReadChange(ctx, nil, a.Workspace, cov.Number)
	if err != nil {
		return "", err
	}
	// The author's time is read in UTC: the commit keeps the zone the
	// author wrote in, as a replayed commit does.
	author := commits[len(commits)-1].Author
	zone, err := repo.AuthorZone(ctx, cov.Head)
	if err != nil {
		return "", fmt.Errorf("squash %s: %w", ref, err)
	}
	author.Time = author.Time.In(zone)
	id, err := repo.CreateCommit(ctx, gitrepo.CommitSpec{
		Tree: heads[1].Tree, Parents: []string{onto}, Message: squashMessage(ch, commits),
		Author: author, Committer: committer,
	})
	if err != nil {
		return "", fmt.Errorf("squash %s: %w", ref, err)
	}
	return id, nil
}

// squashMessage is the message of the commit that squashes the change ch,
// whose own commits are own, oldest first, the version's head last: the
// title, a blank line, the intent, a blank line and the trailers, which
// name the change, its session when it has one, and each other author of
// its own commits, in the order they first wrote one, as a co-author, so
// that the commit keeps who wrote the change.
func squashMessage(ch change.Change, own []gitrepo.Commit) string {
	var b strings.Builder
	b.WriteString(ch.Title + "\n\n")
	if ch.Intent != "" {
		b.WriteString(ch.Intent + "\n\n")
	}
	b.WriteString("Parley-Change: " + ch.Ref + "\n")
	if ch.SessionID != nil {
		b.WriteString("Parley-Session: " + *ch.SessionID + "\n")
	}
	author := func(s gitrepo.Signature) string { return s.Name + " <" + s.Email + ">" }
	seen := map[string]bool{author(own[len(own)-1].Author): true}
	for _, cm := range own {
		if a := author(cm.Author); !seen[a] {
			seen[a] = true
			b.WriteString("Co-authored-by: " + a + "\n")
		}
	}
	return b.String()
}
