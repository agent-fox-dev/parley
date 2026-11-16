package land

import (
	"context"
	"fmt"
	"slices"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// CheckPaths holds a version to the path limits globs of a credential: it
// returns the first path in byte order that differs between base ("" for
// the empty tree) and head in repo and matches none of globs, "" when every
// one matches one. A rename counts with both its paths, so that a token can
// neither move a file out of its paths nor bring one in from outside them.
//
// The paths are read from the repository on every call, never from the
// list a change stores, which stops at change.MaxPaths. globs nil is a
// credential without path limits, and so are the versions the hub makes on
// its own, which carry none: every version is allowed then and the
// repository is not read. The matcher is the one the token's paths were
// validated with (identity.MatchAny, in which ? is literal), never the
// policy's. A git failure is an error, never an answer, so that the caller
// answers an internal error rather than allowing the version or naming a
// path.
func CheckPaths(ctx context.Context, repo *gitrepo.Repo, globs []string, base, head string) (string, error) {
	if globs == nil {
		return "", nil
	}
	files, err := filesBetween(ctx, repo, base, head)
	if err != nil {
		return "", err
	}
	for _, p := range files {
		if !identity.MatchAny(globs, p) {
			return p, nil
		}
	}
	return "", nil
}

// filesBetween are the paths that differ between base ("" for the empty
// tree) and head in repo, both paths of a rename or a copy, in byte order
// and each once.
func filesBetween(ctx context.Context, repo *gitrepo.Repo, base, head string) ([]string, error) {
	changes, err := repo.DiffChanges(ctx, base, head)
	if err != nil {
		return nil, fmt.Errorf("read the files between %s and %s: %w", base, head, err)
	}
	files := make([]string, 0, len(changes))
	for _, f := range changes {
		files = append(files, f.Path)
		if f.OldPath != "" {
			files = append(files, f.OldPath)
		}
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

// VersionFiles is the push rule engine's version-files lookup, which holds
// a path-scoped token to its paths on an update that adds no commit of its
// own: a git push or an API ref move onto commits the repository already
// has, which a check of the commits it adds cannot see. The hub binds it
// to the engine (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md).
// It answers once the component is bound.
func (c *Component) VersionFiles() pushrule.VersionFiles { return versionFiles{c} }

// versionFiles answers the lookup from the component's change records,
// workspace records and repositories.
type versionFiles struct{ c *Component }

// Files are the files of the version the update of branch of the workspace
// slug to head makes: none, with found false, when the branch is the
// source of no draft or open change and the update opens none, since no
// version results. Otherwise they are the files between head and its merge
// base with the head of the target branch, the change's, or for an update
// that opens a change the branch the open defaults to: the workspace's
// default branch, or for a task fork its own base branch in its parent.
// They are read in the repository of slug, which holds head and, for a
// fork, reads its parent's objects. A change whose target branch is
// missing is measured from its version's base, as its versions are; an
// update that would open a change onto a missing branch opens none.
func (v versionFiles) Files(ctx context.Context, slug, branch, head string, opensChange bool) ([]string, bool, error) {
	d := v.c.deps
	var target, targetBranch, fallback string
	ch, found, err := d.Changes.OpenChangeOfBranch(ctx, nil, slug, branch)
	switch {
	case err != nil:
		return nil, false, err
	case found:
		full, err := d.Changes.ReadChange(ctx, nil, ch.WorkspaceSlug, ch.Number)
		if err != nil {
			return nil, false, err
		}
		target, targetBranch = full.WorkspaceSlug, full.TargetBranch
		if full.BaseSHA != nil {
			fallback = *full.BaseSHA
		}
	case !opensChange:
		return nil, false, nil
	default:
		ws, found, err := d.Workspaces.Workspace(ctx, d.Store.Main(), slug)
		if err != nil {
			return nil, false, err
		}
		if !found {
			return nil, false, fmt.Errorf("version files: workspace %q not found", slug)
		}
		target, targetBranch = ws.Slug, ws.DefaultBranch
		if ws.Kind == access.KindFork {
			target = ws.Parent
		}
	}
	targetRepo, err := d.Repos(ctx, target)
	if err != nil {
		return nil, false, err
	}
	targetHead, onTarget, err := targetRepo.ResolveBranch(ctx, targetBranch)
	switch {
	case err != nil:
		return nil, false, err
	case !onTarget && !found:
		return nil, false, nil
	}
	repo := targetRepo
	if slug != target {
		if repo, err = d.Repos(ctx, slug); err != nil {
			return nil, false, err
		}
	}
	base := fallback
	if onTarget {
		// A head that shares no history with the target is measured from
		// the empty tree, as its version is.
		if base, err = repo.MergeBase(ctx, head, targetHead); err != nil {
			return nil, false, err
		}
	}
	files, err := filesBetween(ctx, repo, base, head)
	if err != nil {
		return nil, false, err
	}
	return files, true, nil
}
