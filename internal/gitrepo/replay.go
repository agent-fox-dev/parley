package gitrepo

import "context"

// ReplayedCommit is one commit of a rebase, From, and the commit it
// became, To: From itself when it was kept as it is, a new commit when it
// was replayed, and "" when it was left out because it changes nothing on
// top of what it was replayed onto.
type ReplayedCommit struct {
	From, To string
}

// RebaseFromAs is RebaseFrom with committer as the committer of every
// commit it makes, its name, e-mail address and time written as given and
// never taken from the environment, and which also returns, for each
// commit of base..head, oldest first, the commit it became. Whoever lands
// the result can thus say which new commit holds which commit of the
// head. A conflict or an error returns no head and no commits.
//
// The git processes run under ctx and are killed when it ends; a failure
// of git that is not a conflict is an error.
func (r *Repo) RebaseFromAs(ctx context.Context, base, head, onto string, committer Signature) (string, []ReplayedCommit, *ConflictReport, error) {
	newHead, became, report, err := r.rebase(ctx, base, head, onto, committer, nil)
	if report != nil {
		report.BaseSHA = base
	}
	return newHead, became, report, err
}
