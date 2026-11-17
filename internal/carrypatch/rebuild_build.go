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
	pr := &patchReplay{run: r, scratch: scratch, p: p}
	if r.payload.Strategy == strategyMerge {
		return pr.merge(ctx)
	}
	return pr.rebase(ctx)
}

// maxLearnMerges is how many merge commits of a patch, the newest, a run
// redoes to learn the resolutions of a conflict from.
const maxLearnMerges = 50

// patchReplay is the application of one patch in a run: the run, its
// scratch working tree, the patch, and whether the run has learned from
// the patch's merge commits yet, which it does at most once.
type patchReplay struct {
	run     *rebuildRun
	scratch *gitrepo.Scratch
	p       runPatch
	learned bool
}

// resolve runs op, one replay or merge of the patch onto the head under
// construction, which git does with its rerere on, so that the resolutions
// the workspace's repository holds are replayed; what names it in an
// error. Each conflict git recorded gets its entry. When paths are left in
// conflict and the patch has not been learned from in this attempt, it
// learns the resolutions of the patch's merge commits and, when one was
// learned, runs op once more.
func (pr *patchReplay) resolve(ctx context.Context, what string, op func(ctx context.Context) (gitrepo.Picked, error)) (gitrepo.Picked, *attemptOutcome) {
	picked, out := pr.once(ctx, what, op)
	if out != nil || picked.Outcome != gitrepo.PickConflicted || pr.learned {
		return picked, out
	}
	pr.learned = true
	learned, out := pr.learn(ctx)
	if out != nil || !learned {
		return picked, out
	}
	return pr.once(ctx, what, op)
}

// once runs op and records the conflicts git recorded.
func (pr *patchReplay) once(ctx context.Context, what string, op func(ctx context.Context) (gitrepo.Picked, error)) (gitrepo.Picked, *attemptOutcome) {
	r := pr.run
	var picked gitrepo.Picked
	err := r.git(ctx, func(ctx context.Context) (err error) {
		picked, err = op(ctx)
		return err
	})
	if err != nil {
		return picked, r.failed(ctx, fmt.Errorf("rebuild of %s: %s: %w", r.ws.Slug, what, err))
	}
	if err := r.recordConflicts(ctx, picked.Recorded, false); err != nil {
		return picked, r.failed(ctx, fmt.Errorf("rebuild of %s: record the conflicts of %s: %w", r.ws.Slug, pr.p.BranchName, err))
	}
	return picked, nil
}

// learn redoes, in the scratch working tree, the newest merge commits of
// two parents the patch's source reaches and the upstream base does not,
// so that git records the resolutions the developer gave there, and
// returns the tree to the head under construction. It reports whether a
// resolution was learned; each learned one's entry takes the clock's time.
func (pr *patchReplay) learn(ctx context.Context) (bool, *attemptOutcome) {
	r := pr.run
	var merges []string
	err := r.git(ctx, func(ctx context.Context) error {
		ids, err := r.repo.CommitsBetween(ctx, r.base, *pr.p.source, 0)
		if err != nil {
			return err
		}
		commits, err := r.repo.ReadCommits(ctx, ids)
		if err != nil {
			return err
		}
		for i := len(commits) - 1; i >= 0 && len(merges) < maxLearnMerges; i-- {
			if len(commits[i].Parents) == 2 {
				merges = append(merges, commits[i].ID)
			}
		}
		return nil
	})
	if err != nil {
		return false, r.failed(ctx, fmt.Errorf("rebuild of %s: read the merges of %s: %w", r.ws.Slug, pr.p.BranchName, err))
	}
	if len(merges) == 0 {
		return false, nil
	}
	var learned []gitrepo.RerereConflict
	steps := []func(ctx context.Context) error{func(ctx context.Context) error { return pr.scratch.Reset(ctx, r.head) }}
	for _, m := range merges {
		steps = append(steps, func(ctx context.Context) error {
			got, err := pr.scratch.Learn(ctx, m)
			learned = append(learned, got...)
			return err
		})
	}
	steps = append(steps, func(ctx context.Context) error { return pr.scratch.Reset(ctx, r.head) })
	for _, step := range steps {
		if err := r.git(ctx, step); err != nil {
			return false, r.failed(ctx, fmt.Errorf("rebuild of %s: learn from the merges of %s: %w", r.ws.Slug, pr.p.BranchName, err))
		}
	}
	if err := r.recordConflicts(ctx, learned, true); err != nil {
		return false, r.failed(ctx, fmt.Errorf("rebuild of %s: record the resolutions learned from %s: %w", r.ws.Slug, pr.p.BranchName, err))
	}
	return len(learned) > 0, nil
}

// rebase applies the patch by replaying, oldest first, onto the head
// under construction every commit its source reaches and the upstream base
// does not, leaving out merge commits; a commit whose change the head
// already holds is dropped, and one that conflicts is resolved as far as
// resolve can. It reports whether any commit was replayed, or the first
// commit that does not apply.
func (pr *patchReplay) rebase(ctx context.Context) (bool, *conflictAt, *attemptOutcome) {
	r, p := pr.run, pr.p
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
		picked, out := pr.resolve(ctx, "replay "+c.ID+" of "+p.BranchName, func(ctx context.Context) (gitrepo.Picked, error) {
			return pr.scratch.Pick(ctx, c.ID, committer)
		})
		if out != nil {
			return false, nil, out
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

// merge applies the patch by merging its source into the head under
// construction with a merge commit, even where the head could fast-forward
// to it, resolving a conflict as far as resolve can. The hub makes that
// commit itself, so the landing identity is its author as well as its
// committer. A source the head already reaches leaves nothing to apply. It
// reports whether the merge commit was made, or that the source does not
// merge.
func (pr *patchReplay) merge(ctx context.Context) (bool, *conflictAt, *attemptOutcome) {
	r, p := pr.run, pr.p
	identity := r.c.committer()
	merged, out := pr.resolve(ctx, "merge "+p.BranchName, func(ctx context.Context) (gitrepo.Picked, error) {
		return pr.scratch.Merge(ctx, *p.source, "Merge branch '"+p.BranchName+"'", identity, identity)
	})
	if out != nil {
		return false, nil, out
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
// test gives: each operation has the whole deadline to itself, whatever the
// earlier ones took.
func (r *rebuildRun) git(ctx context.Context, op func(ctx context.Context) error) error {
	return RunGit(ctx, r.c.d.RebuildGitDeadline, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		if hook := r.c.d.BeforeRebuildGit; hook != nil {
			hook(ctx)
		}
		return op(ctx)
	})
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
