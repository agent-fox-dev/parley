package carrypatch

import (
	"context"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// The statuses of a patch's result in a rebuild.
const resultSuccess = "success"

// patchResult is what a run did with one patch, as the job's patch_results
// lists it: the patch as the run listed it, the tip of its branch the run
// read then (nil when the branch did not exist), and the head under
// construction after it.
type patchResult struct {
	PatchID    string  `json:"patch_id"`
	BranchName string  `json:"branch_name"`
	Position   int     `json:"position"`
	Status     string  `json:"status"`
	SourceSHA  *string `json:"source_sha"`
	NewHeadSHA *string `json:"new_head_sha"`
}

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
		return attemptFailed(attemptRetryable, reasonGitError, fmt.Errorf("rebuild of %s: %w", r.ws.Slug, err))
	}
	r.repo = repo
	live, err := ListLivePatches(ctx, r.c.d.Store.Main(), r.ws.Slug)
	if err != nil {
		return attemptFailed(attemptRetryable, reasonGitError, fmt.Errorf("rebuild of %s: list the patches: %w", r.ws.Slug, err))
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
			return attemptFailed(attemptRetryable, reasonGitError, fmt.Errorf("rebuild of %s: read the branch %s: %w", r.ws.Slug, p.BranchName, err))
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
// workspace's repository, which no ref points to and which is removed
// when the build ends. It records what it did with each patch.
func (r *rebuildRun) build(ctx context.Context) *attemptOutcome {
	var scratch *gitrepo.Scratch
	err := r.git(ctx, func(ctx context.Context) (err error) {
		scratch, err = r.repo.AddScratch(ctx, r.job.ID, r.base)
		return err
	})
	if err != nil {
		return attemptFailed(attemptRetryable, reasonGitError, fmt.Errorf("rebuild of %s: make the scratch working tree: %w", r.ws.Slug, err))
	}
	defer func() {
		// The removal runs when the attempt's context may have ended.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := scratch.Remove(ctx); err != nil {
			r.c.logger().Warn("scratch working tree of a rebuild not removed",
				"workspace", r.ws.Slug, "job_id", r.job.ID, "error", err)
		}
	}()
	r.head = r.base
	for i, p := range r.patches {
		if hook := r.c.d.BeforeRebuildPatch; hook != nil {
			hook(r.job.ID, i)
		}
		res := patchResult{PatchID: p.ID, BranchName: p.BranchName, Position: *p.Position, SourceSHA: p.source}
		if p.source == nil {
			r.results = append(r.results, res)
			continue
		}
		if out := r.rebase(ctx, scratch, p); out != nil {
			return out
		}
		head := r.head
		res.Status, res.NewHeadSHA = resultSuccess, &head
		r.results = append(r.results, res)
	}
	return nil
}

// rebase applies the patch p by replaying, oldest first, onto the head
// under construction every commit its source reaches and the upstream base
// does not, leaving out merge commits; a commit whose change the head
// already holds is dropped.
func (r *rebuildRun) rebase(ctx context.Context, scratch *gitrepo.Scratch, p runPatch) *attemptOutcome {
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
		return attemptFailed(attemptRetryable, reasonGitError, fmt.Errorf("rebuild of %s: read the commits of %s: %w", r.ws.Slug, p.BranchName, err))
	}
	committer := r.c.committer()
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
			return attemptFailed(attemptRetryable, reasonGitError, fmt.Errorf("rebuild of %s: replay %s of %s: %w", r.ws.Slug, c.ID, p.BranchName, err))
		}
		switch picked.Outcome {
		case gitrepo.PickApplied:
			r.head = picked.Commit
		case gitrepo.PickConflicted:
			return attemptFailed(attemptRetryable, reasonGitError, fmt.Errorf("rebuild of %s: %s of %s does not apply: conflict in %d files", r.ws.Slug, c.ID, p.BranchName, len(picked.Conflicts)))
		}
	}
	return nil
}

// git runs op, one git operation of the attempt, through RunGit.
func (r *rebuildRun) git(ctx context.Context, op func(ctx context.Context) error) error {
	return RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error { return op(ctx) })
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
