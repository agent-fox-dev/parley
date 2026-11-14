package gitserver

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"strconv"
	"sync"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// ForkView says which refs a task fork shows of its parent's. The git
// server copies them from the parent's repository into the fork's before
// git serves the fork, so that stock git advertises them; the copies are a
// cache for git alone, and the fork feature's own ref view stays the one
// answer to which refs a fork has.
type ForkView interface {
	// ForkRefs returns the refs the fork slug shows of its parent's, or an
	// error when they cannot be read.
	ForkRefs(ctx context.Context, slug string) (ForkRefs, error)
}

// ForkRefs are the refs a fork shows of its parent's.
type ForkRefs struct {
	// Tracking are the full names of the fork's tracking refs, such as
	// refs/heads/main and refs/tags/v1: the branches and tags the parent had
	// when the fork was created, each shown at the parent's value of the
	// moment and not at all once the parent no longer has it.
	Tracking []string
	// Changes are the numbers of the changes, in any state, whose source is
	// the fork. Their versions, refs/parley/changes/<number>/<version>, are
	// written in the change's target workspace, the fork's parent, and the
	// fork shows them under the same names.
	Changes []int64
}

// Copier copies refs from one repository on disk into another:
// gitrepo.Git is one.
type Copier interface {
	CopyRefs(ctx context.Context, src, dst string, c gitrepo.RefCopy) (gitrepo.Copied, error)
}

// changesNamespace is where the versions of every change live.
const changesNamespace = "refs/parley/changes/"

var (
	errNoCopier = errors.New("no ref copier is installed")
	errNoParent = errors.New("the fork names no parent")
)

// forkLocks keep two requests from copying into the repository of one fork
// at once, where both would try to lock the same refs and one would fail.
// A fork's slug picks one of them, so that the locks take a fixed amount of
// memory however many forks the hub has served; two forks that share one
// only wait for each other's copy, which writes nothing once the refs are
// up to date.
type forkLocks [64]sync.Mutex

func (l *forkLocks) of(slug string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(slug))
	return &l[h.Sum32()%uint32(len(l))]
}

// forkRefs brings a task fork's repository up to date with its parent's
// before git runs on it, on every route, so that git advertises, and the
// version 2 want check counts as reachable, what the fork shows: each
// tracking ref at the parent's value at the time of the request, deleted
// once the parent no longer has it; and every version ref of the changes
// whose source is the fork, under the parent's names, while every other ref
// under refs/parley/changes/ is deleted. Nothing else of the parent is
// copied and no other ref of the fork is touched, its own branches above
// all. Without a ForkView the fork is served with the refs its repository
// holds. When the view or the copy fails, the request is answered 500
// internal error before git runs, since the fork's refs may be out of
// date.
func (c *Component) forkRefs(w http.ResponseWriter, r *http.Request, x *Exchange) bool {
	if x.Workspace.Kind != access.KindFork || c.deps.ForkView == nil {
		return true
	}
	if err := c.copyForkRefs(r, x); err != nil {
		c.fail(w, r, fmt.Errorf("bring the refs of the fork %q up to date: %w", x.Workspace.Slug, err))
		return false
	}
	return true
}

func (c *Component) copyForkRefs(r *http.Request, x *Exchange) error {
	ctx, cancel := context.WithTimeout(r.Context(), c.timeout())
	defer cancel()
	view, err := c.deps.ForkView.ForkRefs(ctx, x.Workspace.Slug)
	if err != nil {
		return fmt.Errorf("read the fork view: %w", err)
	}
	switch {
	case c.deps.Copier == nil:
		return errNoCopier
	case x.Workspace.ParentSlug == nil:
		return errNoParent
	}
	parent, err := c.deps.Repositories.Open(ctx, *x.Workspace.ParentSlug)
	if err != nil {
		return fmt.Errorf("open the parent's repository: %w", err)
	}
	spec := gitrepo.RefCopy{Names: view.Tracking, Prune: changesNamespace}
	// A fork's repository is made without a HEAD of its own; it names the
	// fork's base branch, as a workspace's HEAD names its default branch,
	// so that a clone checks it out and a single-branch clone takes it.
	if b := x.Workspace.DefaultBranch; b != nil && *b != "" {
		spec.Head = "refs/heads/" + *b
	}
	for _, n := range view.Changes {
		spec.Prefixes = append(spec.Prefixes, changesNamespace+strconv.FormatInt(n, 10)+"/")
	}
	lock := c.locks.of(x.Workspace.Slug)
	lock.Lock()
	defer lock.Unlock()
	done, err := c.deps.Copier.CopyRefs(ctx, parent, x.Repo, spec)
	if err != nil {
		return err
	}
	if len(done.Set)+len(done.Deleted) > 0 {
		c.deps.Logger.LogAttrs(ctx, slog.LevelDebug, "fork refs copied",
			slog.String("request_id", server.RequestID(ctx)),
			slog.String("fork", x.Workspace.Slug),
			slog.Any("set", done.Set), slog.Any("deleted", done.Deleted),
		)
	}
	return nil
}
