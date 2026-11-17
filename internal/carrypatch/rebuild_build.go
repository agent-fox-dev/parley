package carrypatch

import (
	"context"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// runPatch is a patch as the run listed it, with the tip of its branch the
// run read then: source is nil when the branch did not exist. The run uses
// this tip alone, whatever the branch becomes meanwhile.
type runPatch struct {
	Patch
	source *string
}

// listPatches opens the workspace's repository, lists the patches that are
// not deleted in position order and reads the tip of each patch branch
// once.
func (r *rebuildRun) listPatches(ctx context.Context) *attemptOutcome {
	repo, err := r.c.d.Git.Open(ctx, r.c.d.RepoPath(r.ws.Slug))
	if err != nil {
		return r.failed(ctx, fmt.Errorf("rebuild of %s: %w", r.ws.Slug, err))
	}
	r.repo = repo
	live, err := ListLivePatches(ctx, r.c.d.Store.Main(), r.ws.Slug)
	if err != nil {
		return r.failed(ctx, fmt.Errorf("rebuild of %s: list the patches: %w", r.ws.Slug, err))
	}
	r.patches = make([]runPatch, 0, len(live))
	for _, p := range live {
		var tip string
		var found bool
		err := r.git(ctx, func(ctx context.Context) (err error) {
			tip, found, err = repo.ResolveBranch(ctx, p.BranchName)
			return err
		})
		if err != nil {
			return r.failed(ctx, fmt.Errorf("rebuild of %s: read the branch %s: %w", r.ws.Slug, p.BranchName, err))
		}
		rp := runPatch{Patch: p}
		if found {
			rp.source = &tip
		}
		r.patches = append(r.patches, rp)
	}
	return nil
}

// build makes the head under construction, from the upstream base, by
// applying each patch in order in a scratch working tree of the
// workspace's repository, which no ref points to. It first discards what
// an earlier attempt that ended without its cleanup left there, which no
// other work uses: the queue runs one rebuild of a workspace at a time.
// The scratch working tree is removed when the build ends, however it
// ends. It records what it did with each patch; a patch that conflicts is
// undone, and the run stops there unless its fail mode is continue.
func (r *rebuildRun) build(ctx context.Context) *attemptOutcome {
	var scratch *gitrepo.Scratch
	err := r.git(ctx, func(ctx context.Context) (err error) {
		if err := r.repo.DiscardScratches(ctx); err != nil {
			return err
		}
		scratch, err = r.repo.AddScratch(ctx, r.job.ID, r.base)
		return err
	})
	defer func() {
		// The removal runs when the attempt's context may have ended.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		remove := r.repo.DiscardScratches
		if scratch != nil {
			remove = scratch.Remove
		}
		if err := remove(ctx); err != nil {
			r.c.logger().Warn("scratch working tree of a rebuild not removed",
				"workspace", r.ws.Slug, "job_id", r.job.ID, "error", err)
		}
	}()
	if err != nil {
		return r.failed(ctx, fmt.Errorf("rebuild of %s: make the scratch working tree: %w", r.ws.Slug, err))
	}
	r.head = r.base
	for i, p := range r.patches {
		if hook := r.c.d.BeforeRebuildPatch; hook != nil {
			hook(r.job.ID, i)
		}
		// An archive, a reclone or a delete of the workspace does not wait
		// for a rebuild: the run ends at its next step instead.
		if out := r.readWorkspace(ctx); out != nil {
			return out
		}
		res := resultOf(p)
		if reason := skipReason(p); reason != "" {
			res.Status, res.SkippedReason = resultSkipped, &reason
		} else {
			before := r.head
			applied, conflict, out := r.apply(ctx, scratch, p)
			if out != nil {
				return out
			}
			switch {
			case conflict != nil:
				if out := r.conflict(ctx, scratch, p, before, *conflict, &res); out != nil {
					return out
				}
			case applied:
				head := r.head
				res.Status, res.NewHeadSHA = resultSuccess, &head
			default:
				reason := skippedNothingToApply
				res.Status, res.SkippedReason = resultSkipped, &reason
			}
		}
		// A caller waiting for the job sees each patch as it is finished.
		r.results = append(r.results, res)
		if out := r.store(ctx); out != nil {
			return out
		}
		if res.Status == resultConflict && r.payload.FailMode != failModeContinue {
			return r.stopAtConflict(res)
		}
	}
	return nil
}

// apply applies the patch p onto the head under construction with the
// run's strategy, and reports whether it changed the head: a patch that
// leaves nothing to apply does not. A patch that does not apply returns
// where it conflicted, and leaves the scratch working tree for the caller
// to undo.
func (r *rebuildRun) apply(ctx context.Context, scratch *gitrepo.Scratch, p runPatch) (bool, *conflictAt, *attemptOutcome) {
	if r.payload.Strategy == strategyMerge {
		return r.merge(ctx, scratch, p)
	}
	return r.rebase(ctx, scratch, p)
}

// rebase applies the patch p by replaying, oldest first, onto the head
// under construction every commit its source reaches and the upstream base
// does not, leaving out merge commits; a commit whose change the head
// already holds is dropped. It reports whether any commit was replayed, or
// the first commit that does not apply.
func (r *rebuildRun) rebase(ctx context.Context, scratch *gitrepo.Scratch, p runPatch) (bool, *conflictAt, *attemptOutcome) {
	var commits []gitrepo.Commit
	err := r.git(ctx, func(ctx context.Context) error {
		ids, err := r.repo.CommitsBetween(ctx, r.base, *p.source, 0)
		if err != nil {
			return err
		}
		commits, err = r.repo.ReadCommits(ctx, ids)
		return err
	})
	if err != nil {
		return false, nil, r.failed(ctx, fmt.Errorf("rebuild of %s: read the commits of %s: %w", r.ws.Slug, p.BranchName, err))
	}
	committer := r.c.committer()
	applied := false
	for _, c := range commits {
		if len(c.Parents) > 1 {
			continue
		}
		var picked gitrepo.Picked
		err := r.git(ctx, func(ctx context.Context) (err error) {
			picked, err = scratch.Pick(ctx, c.ID, committer)
			return err
		})
		if err != nil {
			return false, nil, r.failed(ctx, fmt.Errorf("rebuild of %s: replay %s of %s: %w", r.ws.Slug, c.ID, p.BranchName, err))
		}
		switch picked.Outcome {
		case gitrepo.PickApplied:
			r.head, applied = picked.Commit, true
		case gitrepo.PickConflicted:
			return false, &conflictAt{commit: c.ID, paths: picked.Conflicts}, nil
		}
	}
	return applied, nil, nil
}

// merge applies the patch p by merging its source into the head under
// construction with a merge commit, even where the head could fast-forward
// to it. The hub makes that commit itself, so the landing identity is its
// author as well as its committer. A source the head already reaches
// leaves nothing to apply. It reports whether the merge commit was made,
// or that the source does not merge.
func (r *rebuildRun) merge(ctx context.Context, scratch *gitrepo.Scratch, p runPatch) (bool, *conflictAt, *attemptOutcome) {
	identity := r.c.committer()
	var merged gitrepo.Picked
	err := r.git(ctx, func(ctx context.Context) (err error) {
		merged, err = scratch.Merge(ctx, *p.source, "Merge branch '"+p.BranchName+"'", identity, identity)
		return err
	})
	if err != nil {
		return false, nil, r.failed(ctx, fmt.Errorf("rebuild of %s: merge %s: %w", r.ws.Slug, p.BranchName, err))
	}
	switch merged.Outcome {
	case gitrepo.PickApplied:
		r.head = merged.Commit
		return true, nil, nil
	case gitrepo.PickConflicted:
		return false, &conflictAt{commit: *p.source, paths: merged.Conflicts}, nil
	}
	return false, nil, nil
}

// git runs op, one git operation of the attempt, through RunGit, under the
// deadline of every git operation of carry-patch work or the shorter one a
// test gives.
func (r *rebuildRun) git(ctx context.Context, op func(ctx context.Context) error) error {
	return RunGit(ctx, r.c.d.RebuildGitDeadline, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error { return op(ctx) })
}

// committer is the landing identity at the clock's present time: the
// committer of every commit a rebuild makes, taken from the hub's
// configuration and never from the environment or a repository's.
func (c *Component) committer() gitrepo.Signature {
	s := gitrepo.Signature{Name: c.d.CommitterName, Email: c.d.CommitterEmail, Time: c.now()}
	if s.Name == "" {
		s.Name = DefaultCommitterName
	}
	if s.Email == "" {
		s.Email = DefaultCommitterEmail
	}
	return s
}
