package carrypatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// UpstreamRemote is the name the copy of a carry-patch workspace keeps its
// upstream's branches under, as refs/remotes/upstream/<branch>.
const UpstreamRemote = "upstream"

// upstreamDefaultKey is the key of the copy's git configuration that holds
// the upstream's default branch as of the last upstream fetch. It is kept
// in the configuration rather than in a ref, so that no extra ref is
// advertised to the clients of the copy.
const upstreamDefaultKey = "parley.upstreamDefaultBranch"

// ErrNoUpstreamBase is the error of UpstreamBase for a copy that holds
// neither the upstream's default branch nor the upstream branch of the
// workspace's default branch's name.
var ErrNoUpstreamBase = errors.New("no upstream base: the copy holds no upstream branch to start from")

// FetchUpstream fetches every branch of the upstream of the carry-patch
// workspace w into its copy, as refs/remotes/upstream/<branch>, pruning
// those the upstream no longer has and fetching no tag, and records the
// upstream's default branch in the copy's configuration (removing it when
// the upstream names none). The upstream credential is read from the
// workspace's secrets first (ResolveUpstreamCredential); when it cannot be,
// its error is returned and no git runs, so that no anonymous request is
// made in its place.
//
// Each git operation runs through RunGit, with DefaultGitDeadline: when it
// passes, the error matches ErrGitTimeout. The error text has no
// credential. A fetch that fails leaves the upstream refs and the recorded
// default branch as they were: the default branch is asked for first, and
// the refs and the record change only after both git operations with the
// upstream succeeded.
func (c *Component) FetchUpstream(ctx context.Context, w workspace.Workspace) error {
	if w.UpstreamURL == nil || *w.UpstreamURL == "" {
		return fmt.Errorf("fetch the upstream of %s: the workspace has no upstream_url", w.Slug)
	}
	url := *w.UpstreamURL
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(w.Slug))
	if err != nil {
		return fmt.Errorf("fetch the upstream of %s: %w", w.Slug, err)
	}
	cred, err := ResolveUpstreamCredential(ctx, c.d.Secrets, w.Slug)
	if err != nil {
		return fmt.Errorf("fetch the upstream of %s: %w", w.Slug, err)
	}
	var remote gitrepo.Remote
	err = RunGit(ctx, c.d.UpstreamFetchDeadline, cred, func(ctx context.Context, _ time.Duration) error {
		var listErr error
		remote, listErr = c.d.Git.ListRemote(ctx, url, cred)
		return listErr
	})
	if err != nil {
		return fmt.Errorf("ask the upstream of %s for its branches: %w", w.Slug, err)
	}
	err = RunGit(ctx, c.d.UpstreamFetchDeadline, cred, func(ctx context.Context, deadline time.Duration) error {
		_, err := c.d.Git.FetchRemote(ctx, repo.Dir(), url, UpstreamRemote, cred, deadline)
		return err
	})
	if err != nil {
		return fmt.Errorf("fetch the upstream of %s: %w", w.Slug, err)
	}
	err = RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		if remote.DefaultBranch == "" {
			return repo.UnsetConfig(ctx, upstreamDefaultKey)
		}
		return repo.SetConfig(ctx, upstreamDefaultKey, remote.DefaultBranch)
	})
	if err != nil {
		return fmt.Errorf("record the upstream default branch of %s: %w", w.Slug, err)
	}
	return nil
}

// UpstreamBase is the commit every rebuild of the carry-patch workspace w
// starts from: the head of refs/remotes/upstream/<b> for the upstream's
// default branch b as recorded by the last FetchUpstream, else for the
// workspace's default_branch, else ErrNoUpstreamBase. It reads the copy
// alone and never fetches.
func (c *Component) UpstreamBase(ctx context.Context, w workspace.Workspace) (string, error) {
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(w.Slug))
	if err != nil {
		return "", fmt.Errorf("upstream base of %s: %w", w.Slug, err)
	}
	var base string
	err = RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		recorded, _, err := repo.Config(ctx, upstreamDefaultKey)
		if err != nil {
			return err
		}
		candidates := []string{recorded}
		if w.DefaultBranch != nil {
			candidates = append(candidates, *w.DefaultBranch)
		}
		for _, b := range candidates {
			if b == "" {
				continue
			}
			ref, found, err := repo.LookupRef(ctx, "refs/remotes/"+UpstreamRemote+"/"+b)
			if err != nil {
				return err
			}
			if found && ref.Commit != "" {
				base = ref.Commit
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("upstream base of %s: %w", w.Slug, err)
	}
	if base == "" {
		return "", fmt.Errorf("%s: %w", w.Slug, ErrNoUpstreamBase)
	}
	return base, nil
}
