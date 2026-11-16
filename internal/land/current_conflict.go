package land

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// trialFunc trial-rebases head onto onto in repo, the commits after base
// for a base, onto..head for none: nil for clean, the conflict report
// otherwise.
type trialFunc func(ctx context.Context, repo *gitrepo.Repo, base, head, onto string) (*gitrepo.ConflictReport, error)

// setTrial makes trial the trial rebase the conflict reads run.
func (c *Component) setTrial(trial trialFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.trialSeam = trial
}

// trial is the trial rebase the conflict reads run: the mergeability
// check's, unless a test set another.
func (c *Component) trial() trialFunc {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.trialSeam != nil {
		return c.trialSeam
	}
	return change.TrialRebase
}

// currentConflict is the conflict report of the change ch as it is now,
// nil when it is clean, which the reads of a change's conflict answer. A
// landed or abandoned change, which nothing rebases any more, has the
// report it holds, or none. A draft or open change has the report its
// mergeability check stored when its mergeable is conflict and the report
// is of the change's present head onto its present onto commit; otherwise
// its head is replayed again as the check replays it (change.ReplayPair):
// onto the head of the draft or open change it depends on, else onto the
// head of its target branch, and only its own commits when it is in a
// stack. A replayed conflict names the landed changes it collided with,
// as every report does. Nothing is stored and no event is written: the
// check stays the one writer of a change's mergeable.
//
// A draft or open change whose target branch does not exist is 409
// conflict, since its mergeable is then unknown, and one whose commits to
// replay hold a merge commit 400 merge_commits_unsupported, as the
// comparison answers it.
func (c *Component) currentConflict(ctx context.Context, ch change.Change) (*gitrepo.ConflictReport, error) {
	stored, err := storedConflict(ch)
	if err != nil || (ch.State != change.StateDraft && ch.State != change.StateOpen) {
		return stored, err
	}
	repo, err := c.deps.Repos(ctx, ch.WorkspaceSlug)
	if err != nil {
		return nil, err
	}
	if _, found, err := repo.ResolveBranch(ctx, ch.TargetBranch); err != nil || !found {
		if err != nil {
			return nil, err
		}
		return nil, errTargetGone(ch)
	}
	onto, base, err := c.deps.Changes.ReplayPair(ctx, c.deps.Store.Main(), ch)
	switch {
	case err != nil:
		return nil, err
	case onto == "":
		return nil, errTargetGone(ch)
	case ch.Mergeable == change.MergeableConflict && stored != nil && stored.TargetSHA == onto && stored.HeadSHA == ch.HeadSHA:
		return stored, nil
	}
	report, err := c.trial()(ctx, repo, base, ch.HeadSHA, onto)
	switch {
	case errors.Is(err, gitrepo.ErrMergeCommit):
		return nil, server.NewError("merge_commits_unsupported", http.StatusBadRequest,
			fmt.Sprintf("%s has a merge commit among the commits to replay onto %s", ch.Ref, onto))
	case err != nil:
		return nil, err
	}
	if err := c.Attribute(ctx, ch.WorkspaceSlug, report); err != nil {
		c.deps.Logger.Warn("conflict read: conflict not attributed", "workspace", ch.WorkspaceSlug, "number", ch.Number, "error", err)
	}
	return report, nil
}

// storedConflict is the conflict report the change ch holds, nil for
// none.
func storedConflict(ch change.Change) (*gitrepo.ConflictReport, error) {
	if len(ch.Conflict) == 0 || string(ch.Conflict) == "null" {
		return nil, nil
	}
	var report gitrepo.ConflictReport
	if err := json.Unmarshal(ch.Conflict, &report); err != nil {
		return nil, fmt.Errorf("read the conflict of %s: %w", ch.Ref, err)
	}
	return &report, nil
}

// errTargetGone is the refusal to read the conflict of the draft or open
// change ch, whose target branch does not exist.
func errTargetGone(ch change.Change) error {
	return server.NewError("conflict", http.StatusConflict,
		fmt.Sprintf("%s has no conflict report: its target branch %s does not exist", ch.Ref, ch.TargetBranch))
}
