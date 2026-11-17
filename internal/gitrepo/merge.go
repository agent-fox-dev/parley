package gitrepo

import (
	"context"
	"fmt"
)

// TrialMergeResult is what a trial merge of two commits found.
type TrialMergeResult struct {
	// Base is the merge base of the two commits the merge started from, ""
	// when they share no ancestor and the merge started from the empty
	// tree.
	Base string
	// Tree is the merged tree. With conflicts it holds what git leaves of
	// each conflicted file, such as its text with conflict markers.
	Tree string
	// Conflicts are the conflicted files, in byte order of path, each with
	// the stage entries of its sides, base the file in Base, ours in the
	// first commit and theirs in the second; nil when the merge is clean.
	Conflicts []ConflictFile
}

// TrialMerge merges the commit of the full id theirs into the commit of the
// full id ours, three-way from their merge base, and reports the merged
// tree, or the conflicted files when the merge does not apply cleanly. A
// conflict is a result, not an error: an error is a failure of another
// kind, such as an id that names no commit.
//
// The merge happens in the object store alone (git merge-tree): no work
// tree, no index and no ref is used or changed, and no remembered
// resolution is read or recorded. It may leave objects no ref reaches,
// which git's garbage collection removes.
func (r *Repo) TrialMerge(ctx context.Context, ours, theirs string) (TrialMergeResult, error) {
	ours, theirs, err := rangeEnds(ours, theirs)
	if err != nil {
		return TrialMergeResult{}, err
	}
	base, err := r.MergeBase(ctx, ours, theirs)
	if err != nil {
		return TrialMergeResult{}, err
	}
	from := base
	if from == "" {
		// Commits that share no ancestor merge from the empty tree, so that
		// everything each side holds is its addition.
		empty, err := r.emptyTree(ctx)
		if err != nil {
			return TrialMergeResult{}, err
		}
		if from, err = r.trialCommit(ctx, empty); err != nil {
			return TrialMergeResult{}, err
		}
	}
	tree, conflicts, err := r.mergeTree(ctx, from, ours, theirs)
	if err != nil {
		return TrialMergeResult{}, err
	}
	if conflicts != nil && len(conflicts) == 0 {
		// merge-tree said the merge has conflicts and named no file: what
		// it wrote cannot be read, and the tree is not a clean merge.
		return TrialMergeResult{}, fmt.Errorf("merge of %s into %s: merge-tree reported conflicts and no conflicted file", theirs, ours)
	}
	return TrialMergeResult{Base: base, Tree: tree, Conflicts: conflicts}, nil
}
