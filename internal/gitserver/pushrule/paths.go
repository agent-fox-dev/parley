package pushrule

import (
	"context"
	"fmt"
	"slices"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
)

const msgPathNotAllowed = "path %s is outside this token's allowed paths"

// VersionFiles tells which files the version an update makes of a change
// holds against its base, for an update that adds no commit of its own:
// one that moves the source branch of a draft or open change to commits the
// repository already had, or that opens a change with such commits. Branch
// is the ref name without refs/heads/ and head the update's after value;
// found is false, with no error, when the update makes no version. An error
// refuses the update.
type VersionFiles interface {
	Files(ctx context.Context, slug, branch, head string, opensChange bool) (files []string, found bool, err error)
}

// pathScoped holds a path-scoped token to its paths over the whole request,
// once every update of it passed the other rules: no commit the request
// adds to the repository, no file of a version an update without new
// commits makes, and no file of a tree an update points a ref at, may touch
// a path that matches none of the token's globs.
// It refuses the first update in command order that reaches the first such
// path, commits taken oldest first and paths in byte order, so that the
// same push is always refused with the same path. Any other caller is held
// to no paths, and the repository is asked nothing for it.
func (ev *evaluation) pathScoped(ctx context.Context, ds []Decision) {
	globs := ev.req.Caller.Paths
	if ev.req.Caller.CredentialType != CredentialPAT || globs == nil {
		return
	}
	// pending are the updates that bring commits: every one but a delete.
	var pending []int
	for i := range ds {
		if !ds[i].Deleted {
			pending = append(pending, i)
		}
	}
	if len(pending) == 0 {
		return
	}
	refuseAll := func() {
		for _, i := range pending {
			ds[i] = internal.decision(ds[i].Update)
		}
	}
	repo := ev.req.Repo
	if repo == nil {
		refuseAll()
		return
	}
	// reachedFirst is, for every commit the request adds, the first update
	// in command order that brings it, which a refusal for it goes to;
	// adds is how many commits each update brings; ordered is all the
	// commits, oldest first.
	reachedFirst := map[string]int{}
	adds := make(map[int]int, len(pending))
	var tips, ordered []string
	for _, i := range pending {
		commits, err := repo.NewCommits(ctx, []string{ds[i].After}, ev.req.ParentRepo)
		if err != nil {
			ds[i] = internal.decision(ds[i].Update)
			return
		}
		for _, c := range commits {
			if _, seen := reachedFirst[c]; !seen {
				reachedFirst[c] = i
			}
		}
		adds[i] = len(commits)
		tips = append(tips, ds[i].After)
		ordered = commits // all of them when the request has one such update
	}
	if len(pending) > 1 {
		var err error
		if ordered, err = repo.NewCommits(ctx, tips, ev.req.ParentRepo); err != nil {
			refuseAll()
			return
		}
	}
	for _, c := range ordered {
		at := reachedFirst[c]
		paths, err := repo.ChangedPaths(ctx, c)
		if err != nil {
			ds[at] = internal.decision(ds[at].Update)
			return
		}
		if no := outside(globs, paths); no != nil {
			ds[at] = no.decision(ds[at].Update)
			return
		}
	}
	for _, i := range pending {
		if adds[i] == 0 {
			if no := ev.withoutNewCommits(ctx, ds[i].Update, globs); no != nil {
				ds[i] = no.decision(ds[i].Update)
				return
			}
		}
	}
}

// withoutNewCommits holds u, an update that adds no commit, to globs by
// what its after value reaches through any tags: a commit by the files of
// the version the update makes, a tree by every path in it, as the tree of
// a root commit is held. A blob has no path that globs could allow, so it
// is refused, naming the ref it would be written to; so is anything else,
// since what cannot be read cannot be held to the paths.
func (ev *evaluation) withoutNewCommits(ctx context.Context, u Update, globs []string) *refusal {
	kind, paths, err := ev.req.Repo.ObjectPaths(ctx, u.After)
	switch {
	case err != nil:
		return internal
	case kind == gitrepo.ObjectCommit:
		return ev.versionFiles(ctx, u, globs)
	case kind == gitrepo.ObjectTree:
		return outside(globs, paths)
	}
	return &refusal{message: fmt.Sprintf(msgPathNotAllowed, u.Ref), errorType: ErrorTypePathNotAllowed, path: u.Ref}
}

// versionFiles holds u, an update that adds no commit, to globs through the
// files of the version it makes of a change. Only a branch is the source of
// a change, so an update of any other ref makes no version and is allowed.
func (ev *evaluation) versionFiles(ctx context.Context, u Update, globs []string) *refusal {
	branch, ok := branchOf(u.Ref)
	if !ok || ev.deps.VersionFiles == nil {
		return nil
	}
	files, found, err := ev.deps.VersionFiles.Files(ctx, ev.req.Workspace.Slug, branch, u.After, u.OpensChange)
	switch {
	case err != nil:
		return internal
	case !found:
		return nil
	}
	return outside(globs, slices.Sorted(slices.Values(files)))
}

// outside refuses the first of paths that matches none of globs, by the
// matcher that validated the token's paths; nil when every path matches.
func outside(globs, paths []string) *refusal {
	for _, p := range paths {
		if !identity.MatchAny(globs, p) {
			return &refusal{message: fmt.Sprintf(msgPathNotAllowed, p), errorType: ErrorTypePathNotAllowed, path: p}
		}
	}
	return nil
}
