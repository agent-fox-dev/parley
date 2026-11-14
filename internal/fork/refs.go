package fork

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// ErrNotFork is the error of a read that names a workspace that is not a
// task fork, or no workspace.
var ErrNotFork = errors.New("not a task fork")

// Branch is a branch of a fork's ref view. A tracking branch (Tracking
// true) is the parent's branch of the name, at the parent's head of it; an
// own branch is the fork's, at the fork's head of it.
type Branch struct {
	Name     string `json:"name"`
	SHA      string `json:"sha"`
	Tracking bool   `json:"tracking"`
}

// Tag is a tag of a fork's ref view: a tag the parent had when the fork was
// created, at the object the parent's tag of the name points at now (the tag
// object of an annotated tag).
type Tag struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

// RefView is the one answer to which refs a fork has, each list ordered by
// name and never nil.
type RefView struct {
	Branches []Branch
	Tags     []Tag
}

// Branch is the branch of v named name; ok is false when v has none.
func (v RefView) Branch(name string) (b Branch, ok bool) {
	i := slices.IndexFunc(v.Branches, func(b Branch) bool { return b.Name == name })
	if i < 0 {
		return Branch{}, false
	}
	return v.Branches[i], true
}

// Tag is the tag of v named name; ok is false when v has none.
func (v RefView) Tag(name string) (t Tag, ok bool) {
	i := slices.IndexFunc(v.Tags, func(t Tag) bool { return t.Name == name })
	if i < 0 {
		return Tag{}, false
	}
	return v.Tags[i], true
}

// Refs is the ref view of the fork slug, read now. Each branch name the
// fork recorded at its creation is a tracking branch at the parent's
// current head of that name, and absent while the parent has no branch of
// the name; the recorded tags are listed the same way; a branch or tag the
// parent gained later is never shown. The branches of the fork's own
// repository are its own branches, except those under a recorded tracking
// name: git serving a fork copies the parent's tracking refs into its
// repository, and such a copy is never an own branch. A slug that names no
// fork is an error matching ErrNotFork.
func (c *Component) Refs(ctx context.Context, slug string) (RefView, error) {
	f, err := c.fork(ctx, slug)
	if err != nil {
		return RefView{}, err
	}
	parent, err := c.parentRepo(ctx, f)
	if err != nil {
		return RefView{}, err
	}
	heads, err := parent.Branches(ctx)
	if err != nil {
		return RefView{}, fmt.Errorf("the refs of fork %q: %w", slug, err)
	}
	tags, err := parent.Tags(ctx)
	if err != nil {
		return RefView{}, fmt.Errorf("the refs of fork %q: %w", slug, err)
	}
	own, err := c.ownBranches(ctx, f)
	if err != nil {
		return RefView{}, err
	}
	v := RefView{Branches: []Branch{}, Tags: []Tag{}}
	for _, name := range f.TrackingBranches {
		if id, ok := heads[name]; ok {
			v.Branches = append(v.Branches, Branch{Name: name, SHA: id, Tracking: true})
		}
	}
	for name, id := range own {
		v.Branches = append(v.Branches, Branch{Name: name, SHA: id})
	}
	for _, name := range f.TrackingTags {
		if id, ok := tags[name]; ok {
			v.Tags = append(v.Tags, Tag{Name: name, SHA: id})
		}
	}
	slices.SortFunc(v.Branches, func(a, b Branch) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(v.Tags, func(a, b Tag) int { return cmp.Compare(a.Name, b.Name) })
	return v, nil
}

// ownBranches maps the own branches of the fork f to their heads in its
// repository: its branches without those under a recorded tracking name.
// Every listing of a fork's own branches goes through it.
func (c *Component) ownBranches(ctx context.Context, f Fork) (map[string]string, error) {
	repo, err := c.deps.Git.Open(ctx, c.deps.Workspaces.RepoPath(f.Slug))
	if err != nil {
		return nil, fmt.Errorf("the repository of fork %q: %w", f.Slug, err)
	}
	heads, err := repo.Branches(ctx)
	if err != nil {
		return nil, fmt.Errorf("the branches of fork %q: %w", f.Slug, err)
	}
	for _, name := range f.TrackingBranches {
		delete(heads, name)
	}
	return heads, nil
}

// HeadSHA is the head_sha of the fork slug: the parent's current head of
// the fork's base branch, nil when the parent has no such branch. The fork
// point recorded at creation is never read for it. A slug that names no
// fork is an error matching ErrNotFork.
func (c *Component) HeadSHA(ctx context.Context, slug string) (*string, error) {
	f, err := c.fork(ctx, slug)
	if err != nil {
		return nil, err
	}
	return c.headOf(ctx, f.Workspace)
}

// WorkspaceView completes the workspace object o of the fork w with what is
// computed for a fork: its head_sha (see HeadSHA). The hub registers it as
// the workspace service's fork view.
func (c *Component) WorkspaceView(ctx context.Context, w workspace.Workspace, o *workspace.Object) error {
	head, err := c.headOf(ctx, w)
	if err != nil {
		return err
	}
	o.HeadSHA = head
	return nil
}

// headOf is the parent's current head of the base branch of the fork w.
func (c *Component) headOf(ctx context.Context, w workspace.Workspace) (*string, error) {
	if w.DefaultBranch == nil {
		return nil, nil
	}
	parent, err := c.parentRepo(ctx, Fork{Workspace: w})
	if err != nil {
		return nil, err
	}
	id, found, err := parent.ResolveBranch(ctx, *w.DefaultBranch)
	if err != nil {
		return nil, fmt.Errorf("the head of fork %q: %w", w.Slug, err)
	}
	if !found {
		return nil, nil
	}
	return &id, nil
}

// TrackingBranchError is the refusal of a write to the tracking branch
// branch of a fork: 403 tracking_branch.
func TrackingBranchError(branch string) *server.Error {
	return server.NewError("tracking_branch", http.StatusForbidden,
		fmt.Sprintf("%s is a tracking branch of the fork's parent and cannot be created, moved or deleted in the fork", branch))
}

// CheckBranchWrite answers whether the branch named branch (without
// refs/heads/) of the workspace slug may be created, moved or deleted: for
// a fork and one of its recorded tracking branch names it is
// TrackingBranchError, also while the parent has no branch of that name,
// since the name stays reserved for the tracking branch; for every other
// name, and for any branch of a workspace that is not a fork, nil.
func (c *Component) CheckBranchWrite(ctx context.Context, slug, branch string) error {
	f, found, err := BySlug(ctx, c.deps.Store.Main(), slug)
	if err != nil {
		return err
	}
	if found && slices.Contains(f.TrackingBranches, branch) {
		return TrackingBranchError(branch)
	}
	return nil
}

// fork reads the fork slug, an error matching ErrNotFork when slug names
// none.
func (c *Component) fork(ctx context.Context, slug string) (Fork, error) {
	f, found, err := BySlug(ctx, c.deps.Store.Main(), slug)
	if err != nil {
		return Fork{}, err
	}
	if !found {
		return Fork{}, fmt.Errorf("%q: %w", slug, ErrNotFork)
	}
	return f, nil
}

// parentRepo opens the repository of the parent of the fork f.
func (c *Component) parentRepo(ctx context.Context, f Fork) (*gitrepo.Repo, error) {
	if f.ParentSlug == nil {
		return nil, fmt.Errorf("fork %q has no parent", f.Slug)
	}
	repo, err := c.deps.Git.Open(ctx, c.deps.Workspaces.RepoPath(*f.ParentSlug))
	if err != nil {
		return nil, fmt.Errorf("the parent of fork %q: %w", f.Slug, err)
	}
	return repo, nil
}
