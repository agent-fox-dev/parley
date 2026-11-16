package gitrepo

import (
	"context"
	"fmt"
	"strings"
)

// Revert undoes the commits, full ids given in the order they are to be
// undone (newest first, to undo a line of commits), one after the other on
// top of the commit onto, and returns the last commit it made, the new
// head. Each undo is a three-way merge of the commit's parent into what
// the undos before it left, with the commit as the merge base, made a
// commit whose parent is that result, with git's usual revert message,
// Revert "<subject>" and This reverts commit <id>., and as as author and
// committer. An undo that changes nothing is left out, so that a head
// with nothing left to undo is onto. A commit with more than one parent is
// ErrMergeCommit, before anything is written.
//
// The first undo that does not apply ends the revert: it returns no head
// and a report whose target is onto, whose head is the first commit given,
// whose base and commit are the commit that could not be undone, and whose
// files are the conflicted files, the merge base being that commit, ours
// what the undos before left and theirs its parent.
//
// Like Rebase, the revert happens in the object store alone: no work tree,
// no index, no lock and no ref is used or changed, so nothing is left
// behind when it stops.
func (r *Repo) Revert(ctx context.Context, onto string, commits []string, as Signature) (string, *ConflictReport, error) {
	if len(commits) == 0 {
		return onto, nil, nil
	}
	read, err := r.ReadCommits(ctx, append([]string{onto}, commits...))
	if err != nil {
		return "", nil, err
	}
	for _, c := range read[1:] {
		if len(c.Parents) > 1 {
			return "", nil, fmt.Errorf("revert %s: %w", c.ID, ErrMergeCommit)
		}
	}
	ours, oursTree := read[0].ID, read[0].Tree
	for _, c := range read[1:] {
		parent, err := r.parentOf(ctx, c)
		if err != nil {
			return "", nil, err
		}
		tree, files, err := r.mergeTree(ctx, c.ID, ours, parent)
		if err != nil {
			return "", nil, err
		}
		if files != nil {
			return "", &ConflictReport{TargetSHA: read[0].ID, HeadSHA: read[1].ID, BaseSHA: c.ID, CommitSHA: c.ID, Files: files}, nil
		}
		if tree == oursTree {
			continue
		}
		subject, _, _ := strings.Cut(strings.TrimSpace(c.Message), "\n")
		ours, err = r.CreateCommit(ctx, CommitSpec{Tree: tree, Parents: []string{ours},
			Message: "Revert \"" + strings.TrimSpace(subject) + "\"\n\nThis reverts commit " + c.ID + ".\n", Author: as, Committer: as})
		if err != nil {
			return "", nil, err
		}
		oursTree = tree
	}
	return ours, nil, nil
}

// parentOf is the parent of the commit c, the side an undo of c merges in:
// its first parent, or for a root commit an unreferenced commit of the
// empty tree, since git merge-tree takes commits.
func (r *Repo) parentOf(ctx context.Context, c Commit) (string, error) {
	if len(c.Parents) > 0 {
		return c.Parents[0], nil
	}
	empty, err := r.emptyTree(ctx)
	if err != nil {
		return "", err
	}
	return r.trialCommit(ctx, empty)
}
