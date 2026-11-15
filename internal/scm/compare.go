package scm

import (
	"errors"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// maxCompareCommits is the number of commits a compare answer lists at most.
const maxCompareCommits = 200

// The values of a compare answer's mergeable.
const (
	mergeableClean    = "clean"
	mergeableConflict = "conflict"
)

// compareAnswer is the answer of the compare route.
type compareAnswer struct {
	BaseSHA      string                  `json:"base_sha"`
	HeadSHA      string                  `json:"head_sha"`
	MergeBaseSHA *string                 `json:"merge_base_sha"`
	Ahead        int                     `json:"ahead"`
	Behind       int                     `json:"behind"`
	Commits      []commitObject          `json:"commits"`
	Files        []fileChange            `json:"files"`
	Mergeable    string                  `json:"mergeable"`
	Conflict     *gitrepo.ConflictReport `json:"conflict"`
}

// getCompare answers GET …/scm/compare: how head relates to base (their
// merge base, the commits only each one has, the first maxCompareCommits
// commits of head not in base, oldest first, and the first maxDiffFiles
// file changes from the merge base to head) and whether rebasing head onto
// base would succeed, from a trial rebase that changes no ref. A merge
// commit among the commits to replay is 400 merge_commits_unsupported,
// since mergeable has no value for a rebase that cannot be tried.
func (c *Component) getCompare(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	baseRev, headRev := q.Get("base"), q.Get("head")
	if baseRev == "" || headRev == "" {
		return server.InvalidRequest("base and head are required")
	}
	rd, err := c.open(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	base, err := rd.resolver.Resolve(ctx, baseRev)
	if err != nil {
		return err
	}
	head, err := rd.resolver.Resolve(ctx, headRev)
	if err != nil {
		return err
	}
	conflict, err := rd.repo.TrialRebase(ctx, base.SHA, head.SHA)
	if errors.Is(err, gitrepo.ErrMergeCommit) {
		return server.NewError("merge_commits_unsupported", http.StatusBadRequest,
			"the commits of head that are not in base include a merge commit, which a rebase cannot replay")
	}
	if err != nil {
		return err
	}
	answer := compareAnswer{BaseSHA: base.SHA, HeadSHA: head.SHA, Mergeable: mergeableClean, Conflict: conflict}
	if conflict != nil {
		answer.Mergeable = mergeableConflict
	}
	mergeBase, err := rd.repo.MergeBase(ctx, base.SHA, head.SHA)
	if err != nil {
		return err
	}
	if mergeBase != "" {
		answer.MergeBaseSHA = &mergeBase
	}
	if answer.Ahead, answer.Behind, err = rd.repo.AheadBehind(ctx, base.SHA, head.SHA); err != nil {
		return err
	}
	ids, err := rd.repo.CommitsBetween(ctx, base.SHA, head.SHA, maxCompareCommits)
	if err != nil {
		return err
	}
	commits, err := rd.repo.ReadCommits(ctx, ids)
	if err != nil {
		return err
	}
	if answer.Commits, err = c.commitObjects(ctx, rd, commits, nil); err != nil {
		return err
	}
	// No common ancestor ("") compares from the empty tree.
	changes, err := rd.repo.DiffChanges(ctx, mergeBase, head.SHA)
	if err != nil {
		return err
	}
	answer.Files = fileChanges(changes[:min(len(changes), maxDiffFiles)])
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}
