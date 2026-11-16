package carrypatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The resolutions of ResolveBranch: where it found the branch of a patch.
const (
	// ResolutionLocal is a branch the copy holds, taken as it is.
	ResolutionLocal = "local"
	// ResolutionOriginTracking is a branch created from the copy's origin
	// head of that name.
	ResolutionOriginTracking = "origin_tracking"
	// ResolutionOriginFetch is a branch fetched from origin and created at
	// origin's head of it.
	ResolutionOriginFetch = "origin_fetch"
	// ResolutionSkipped is a branch that was not looked up.
	ResolutionSkipped = "skipped"
)

// DefaultBranchFetchDeadline is the deadline of the single-branch fetch from
// origin of ResolveBranch, that of the fetch of a workspace's sync: a
// request that waits for a patch's branch must not wait for origin as long
// as a whole upstream fetch may take.
const DefaultBranchFetchDeadline = 2 * time.Minute

// patchBranchSourceKey is the workspace setting that tells whether
// ResolveBranch asks origin for a branch the copy does not hold: "origin"
// does, anything else, "hub" its default, does not.
const patchBranchSourceKey = "patch_branch_source"

// headsPrefix is where the copy keeps its branches.
const headsPrefix = "refs/heads/"

// ResolveBranch finds the branch of a patch of the carry-patch workspace
// w, so that the patch can be registered, and returns where it found it:
//
//   - skip: ResolutionSkipped at once, without reading a ref or asking a
//     remote.
//   - refs/heads/<branch> in the copy: ResolutionLocal, and nothing is
//     written.
//   - else refs/remotes/origin/<branch>: the branch is created at its
//     commit, ResolutionOriginTracking.
//   - else, only when the workspace's effective patch_branch_source is
//     origin, the one branch is fetched from origin (git_url) with the
//     workspace's origin credential under DefaultBranchFetchDeadline into
//     refs/remotes/origin/<branch>, and the branch is created at its head,
//     ResolutionOriginFetch.
//
// The branch is created only when absent: when another writer created it
// after the lookup, it is kept as that writer left it and the resolution
// stands. Errors of the REST API: 404 branch_not_found when no lookup that
// applies finds the branch (with patch_branch_source hub, origin is never
// asked); 502 origin_fetch_failed when the credential cannot be read or
// origin cannot be reached, refuses or does not answer in time, with git's
// text cleaned of the credential and of URL user information; 409 conflict
// when another branch's name is in the way of the branch's (feat beside
// feat/x), which git cannot hold together; 400 invalid_request for a name
// git would not take for a branch. Then no ref is created. Any other error
// is a fault of the copy.
func (c *Component) ResolveBranch(ctx context.Context, w workspace.Workspace, branch string, skip bool) (string, error) {
	if skip {
		return ResolutionSkipped, nil
	}
	head, tracking := headsPrefix+branch, "refs/remotes/origin/"+branch
	if !gitrepo.ValidRefName(head) {
		return "", server.InvalidRequest(fmt.Sprintf("%q is not a branch name", branch))
	}
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(w.Slug))
	if err != nil {
		return "", fmt.Errorf("resolve the branch %s of %s: %w", branch, w.Slug, err)
	}
	var foundLocal, foundTracking bool
	var trackingRef gitrepo.Ref
	err = RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		var err error
		if _, foundLocal, err = repo.LookupRef(ctx, head); err != nil || foundLocal {
			return err
		}
		trackingRef, foundTracking, err = repo.LookupRef(ctx, tracking)
		return err
	})
	switch {
	case err != nil:
		return "", fmt.Errorf("resolve the branch %s of %s: %w", branch, w.Slug, err)
	case foundLocal:
		return ResolutionLocal, nil
	case foundTracking:
		return c.createBranch(ctx, repo, branch, trackingRef.ID, ResolutionOriginTracking)
	case !c.fromOrigin(w):
		return "", branchNotFound(branch)
	}
	// Origin is not asked for a branch that could not be created.
	if blocker, blocked, err := blockerOf(ctx, repo, head); err != nil {
		return "", fmt.Errorf("resolve the branch %s of %s: %w", branch, w.Slug, err)
	} else if blocked {
		return "", nameInTheWay(branch, blocker)
	}
	tip, err := c.fetchBranch(ctx, repo, w, branch, branchNotFound)
	if err != nil {
		return "", err
	}
	return c.createBranch(ctx, repo, branch, tip, ResolutionOriginFetch)
}

// fromOrigin reports whether the effective patch_branch_source of w is
// origin; a missing key or registry reads as hub.
func (c *Component) fromOrigin(w workspace.Workspace) bool {
	if c.d.Settings == nil {
		return false
	}
	source, _ := c.d.Settings.EffectiveSettings(w.Settings, w.DefaultBranch)[patchBranchSourceKey].(string)
	return source == "origin"
}

// fetchBranch fetches branch from the origin of w into the copy repo and
// returns its head on origin; when origin has no such branch, the error is
// missing(branch), the answer of the caller's route.
func (c *Component) fetchBranch(ctx context.Context, repo *gitrepo.Repo, w workspace.Workspace, branch string, missing func(branch string) error) (string, error) {
	if w.GitURL == nil || *w.GitURL == "" {
		return "", originFetchFailed(branch, "the workspace has no git_url")
	}
	cred, err := workspace.ResolveOriginCredential(ctx, c.d.Secrets, w.Slug)
	if err != nil {
		// No anonymous request is made in place of the credential.
		return "", originFetchFailed(branch, err.Error())
	}
	limit := c.d.BranchFetchDeadline
	if limit <= 0 {
		limit = DefaultBranchFetchDeadline
	}
	var tip string
	err = RunGit(ctx, limit, cred, func(ctx context.Context, deadline time.Duration) error {
		var err error
		tip, err = c.d.Git.FetchOriginBranch(ctx, repo.Dir(), *w.GitURL, branch, cred, deadline)
		return err
	})
	switch {
	case err == nil:
		return tip, nil
	case errors.Is(err, gitrepo.ErrNoSuchBranch):
		return "", missing(branch)
	case errors.Is(err, gitrepo.ErrRefNameConflict):
		return "", nameConflict(ctx, repo, branch, "refs/remotes/origin/"+branch)
	case ctx.Err() != nil:
		return "", err
	}
	return "", originFetchFailed(branch, gitrepo.SanitizeGitText(err.Error(), cred.Username, cred.Password))
}

// createBranch creates refs/heads/<branch> at id in repo unless it exists,
// and returns resolution: a branch another writer created since the lookup
// is kept as it is, since the patch's branch is found either way.
func (c *Component) createBranch(ctx context.Context, repo *gitrepo.Repo, branch, id, resolution string) (string, error) {
	if c.d.BeforeBranchCreate != nil {
		c.d.BeforeBranchCreate(branch)
	}
	err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		return repo.CompareAndSwapRef(ctx, headsPrefix+branch, id, "")
	})
	switch {
	case err == nil, errors.Is(err, gitrepo.ErrRefMoved):
		return resolution, nil
	case errors.Is(err, gitrepo.ErrRefNameConflict):
		return "", nameConflict(ctx, repo, branch, headsPrefix+branch)
	}
	return "", fmt.Errorf("create the branch %s: %w", branch, err)
}

// blockerOf is the ref of repo whose name is in the way of ref's, found
// false when there is none.
func blockerOf(ctx context.Context, repo *gitrepo.Repo, ref string) (name string, found bool, err error) {
	err = RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		var err error
		name, found, err = repo.RefInTheWay(ctx, ref)
		return err
	})
	return name, found, err
}

// nameConflict is the error for branch, whose ref (refs/heads/<branch> or
// refs/remotes/origin/<branch>) could not be created because another ref's
// name is in the way of its name: 409 conflict, naming that ref when it can
// be read.
func nameConflict(ctx context.Context, repo *gitrepo.Repo, branch, ref string) error {
	blocker, blocked, err := blockerOf(ctx, repo, ref)
	if err != nil || !blocked {
		blocker = "another ref"
	}
	return nameInTheWay(branch, blocker)
}

// nameInTheWay is 409 conflict for branch, whose ref cannot be created
// beside blocker: a name and a directory of it cannot both be refs.
func nameInTheWay(branch, blocker string) error {
	return server.Conflict(fmt.Sprintf("the branch %q cannot be created in the hub's copy: %s is in the way of its name", branch, blocker))
}

// branchNotFound is 404 branch_not_found for branch.
func branchNotFound(branch string) error {
	return server.NewError("branch_not_found", http.StatusNotFound, fmt.Sprintf("branch %q not found", branch))
}

// originFetchFailed is 502 origin_fetch_failed for branch with the reason
// why, which holds no credential.
func originFetchFailed(branch, why string) error {
	return server.NewError("origin_fetch_failed", http.StatusBadGateway, fmt.Sprintf("fetch the branch %q from origin: %s", branch, why))
}
