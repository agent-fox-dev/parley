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
type SyncOptions struct{}

// SyncResult is what one sync did.
type SyncResult struct {
	// Workspace is the workspace's record as the sync read it.
	Workspace Workspace
	// CreatedTags maps every tag the sync created in the copy to the object
	// id it created it at, origin's value.
	CreatedTags map[string]string
}

// Sync brings the copy of the workspace slug up to date with its origin: it
// resolves the origin credential from the workspace's secrets at this
// moment, fetches every branch and tag of origin with it within
// syncFetchDeadline, which sets the copy's origin heads to origin's
// branches, and creates in the copy every tag origin has and the copy lacks,
// at origin's value. A tag the copy has keeps its value, whether origin has
// it at another value, has deleted it or never had it. The caller has
// checked that the workspace may be synced.
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
	return SyncResult{Workspace: w, CreatedTags: created}, nil
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
