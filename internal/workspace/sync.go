package workspace

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// syncFetchDeadline is how long the fetch of a sync may take, the deadline
// of a push to origin: a fetch without one could hold its request for ever,
// and the CLI's request timeout of three minutes leaves room for it.
const syncFetchDeadline = 2 * time.Minute

// Fetcher fetches a workspace's origin into its copy, as gitrepo's
// Git.FetchOrigin describes it. gitrepo.Git satisfies it.
type Fetcher interface {
	FetchOrigin(ctx context.Context, dir, url string, cred Credential, deadline time.Duration) (gitrepo.Fetched, error)
}

// SyncOptions are the choices a caller makes for one sync.
type SyncOptions struct {
	// ResetToOrigin moves every diverged branch to origin's head; without
	// it a diverged branch is left as it is.
	ResetToOrigin bool
}

// BranchOutcome is what a sync did with one branch of origin.
type BranchOutcome string

// The outcomes of a branch: created when the hub had no such branch,
// up_to_date when both heads are equal, fast_forward when the hub's head was
// an ancestor of origin's, ahead when origin's head is an ancestor of the
// hub's, and, when each side has commits the other lacks, diverged (left as
// it is) or, with ResetToOrigin, reset (moved to origin's head).
const (
	BranchCreated     BranchOutcome = "created"
	BranchUpToDate    BranchOutcome = "up_to_date"
	BranchFastForward BranchOutcome = "fast_forward"
	BranchAhead       BranchOutcome = "ahead"
	BranchDiverged    BranchOutcome = "diverged"
	BranchReset       BranchOutcome = "reset"
)

// BranchResult is one entry of a sync's branch list, as the sync answer
// marshals it.
type BranchResult struct {
	// Name is the branch's name, without refs/heads/.
	Name   string        `json:"name"`
	Result BranchOutcome `json:"result"`
	// LocalSHA is the hub's head of the branch after the sync handled it.
	LocalSHA string `json:"local_sha"`
	// OriginSHA is origin's head of the branch as the fetch found it.
	OriginSHA string `json:"origin_sha"`
}

// SyncResult is what one sync did.
type SyncResult struct {
	// Workspace is the workspace's record as the sync read it.
	Workspace Workspace
	// CreatedTags maps every tag the sync created in the copy to the object
	// id it created it at, origin's value.
	CreatedTags map[string]string
	// Branches has one entry per branch of origin, sorted by name in byte
	// order; it is empty, never nil, when origin has no branch, so that it
	// marshals as [].
	Branches []BranchResult
}

// Sync brings the copy of the workspace slug up to date with its origin: it
// resolves the origin credential from the workspace's secrets at this
// moment, fetches every branch and tag of origin with it within
// syncFetchDeadline, which sets the copy's origin heads to origin's
// branches, and creates in the copy every tag origin has and the copy lacks,
// at origin's value. A tag the copy has keeps its value, whether origin has
// it at another value, has deleted it or never had it. It then evaluates
// every branch of origin in name order (see syncBranch) and reports each in
// Branches; a branch only the hub has, never on origin or deleted there, is
// neither looked at nor reported. The caller has checked that the workspace
// may be synced.
func (s *Service) Sync(ctx context.Context, slug string, opts SyncOptions) (SyncResult, error) {
	w, found, err := BySlug(ctx, s.deps.Store.Main(), slug)
	if err != nil {
		return SyncResult{}, err
	}
	if !found {
		return SyncResult{}, fmt.Errorf("sync workspace %q: %w", slug, ErrNotFound)
	}
	fetched, err := s.fetchOrigin(ctx, w)
	if err != nil {
		return SyncResult{}, err
	}
	repo, err := s.deps.Git.Open(ctx, s.RepoPath(slug))
	if err != nil {
		return SyncResult{}, err
	}
	created, err := createTags(ctx, repo, fetched.Tags)
	if err != nil {
		return SyncResult{}, err
	}
	branches := make([]BranchResult, 0, len(fetched.Branches))
	for _, name := range slices.Sorted(maps.Keys(fetched.Branches)) {
		b, err := syncBranch(ctx, repo, name, fetched.Branches[name], opts.ResetToOrigin)
		if err != nil {
			return SyncResult{}, err
		}
		branches = append(branches, b)
	}
	return SyncResult{Workspace: w, CreatedTags: created, Branches: branches}, nil
}

// syncBranch evaluates the branch name of repo once against originHead,
// origin's head of it: it classifies the hub's head and, for created,
// fast_forward and reset, moves the branch to originHead only while it is
// still at the head it classified, so that a move by another writer
// meanwhile is never lost; that case is an error matching
// gitrepo.ErrRefMoved. A diverged branch moves only when reset is true.
// Whether the branch is protected does not matter: sync is one of the ways a
// protected branch may move.
func syncBranch(ctx context.Context, repo *gitrepo.Repo, name, originHead string, reset bool) (BranchResult, error) {
	local, found, err := repo.ResolveBranch(ctx, name)
	if err != nil {
		return BranchResult{}, err
	}
	outcome, err := classifyBranch(ctx, repo, local, found, originHead, reset)
	if err != nil {
		return BranchResult{}, fmt.Errorf("sync branch %s: %w", name, err)
	}
	switch outcome {
	case BranchCreated, BranchFastForward, BranchReset:
		if err := repo.CompareAndSwapRef(ctx, "refs/heads/"+name, originHead, local); err != nil {
			return BranchResult{}, fmt.Errorf("sync branch %s: %w", name, err)
		}
		local = originHead
	}
	return BranchResult{Name: name, Result: outcome, LocalSHA: local, OriginSHA: originHead}, nil
}

// classifyBranch is the outcome for a branch whose head in the hub is local
// (found false when the hub has no such branch) and on origin originHead.
func classifyBranch(ctx context.Context, repo *gitrepo.Repo, local string, found bool, originHead string, reset bool) (BranchOutcome, error) {
	if !found {
		return BranchCreated, nil
	}
	if local == originHead {
		return BranchUpToDate, nil
	}
	if ff, err := repo.IsAncestor(ctx, local, originHead); err != nil || ff {
		return BranchFastForward, err
	}
	if ahead, err := repo.IsAncestor(ctx, originHead, local); err != nil || ahead {
		return BranchAhead, err
	}
	if reset {
		return BranchReset, nil
	}
	return BranchDiverged, nil
}

// fetchOrigin resolves the origin credential of w now, so that a changed
// secret counts at the next sync, and fetches w's origin into its copy with
// it.
func (s *Service) fetchOrigin(ctx context.Context, w Workspace) (gitrepo.Fetched, error) {
	cred, err := ResolveOriginCredential(ctx, s.deps.Secrets, w.Slug)
	if err != nil {
		return gitrepo.Fetched{}, err
	}
	return s.deps.Fetcher.FetchOrigin(ctx, s.RepoPath(w.Slug), deref(w.GitURL), cred, syncFetchDeadline)
}

// createTags creates in repo, in name order, every tag of tags that it
// lacks, at the value tags gives, and returns those it created. Each is
// created only while it does not exist, so that a tag another writer creates
// meanwhile keeps that writer's value.
func createTags(ctx context.Context, repo *gitrepo.Repo, tags map[string]string) (map[string]string, error) {
	have, err := repo.Tags(ctx)
	if err != nil {
		return nil, err
	}
	created := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(tags)) {
		if _, exists := have[name]; exists {
			continue
		}
		err := repo.CompareAndSwapRef(ctx, "refs/tags/"+name, tags[name], "")
		if errors.Is(err, gitrepo.ErrRefMoved) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("create tag %s: %w", name, err)
		}
		created[name] = tags[name]
	}
	return created, nil
}
