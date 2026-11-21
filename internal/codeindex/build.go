package codeindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/metrics"
)

// JobType is the type of a build in the hub's job queue.
const JobType = "code_index"

// The outcomes a build that ended is counted under.
const (
	outcomeOK      = "ok"
	outcomePartial = "partial"
	outcomeFailed  = "failed"
)

// The directories of one build of a workspace, below the directory named
// after the commit it built: the commit's files, the index stored from
// them, and where the index works.
const (
	treeDir    = "tree"
	storeDir   = "index"
	scratchDir = "run"
)

// Workspace is what the component needs to know of a workspace.
type Workspace struct {
	Slug string
	// Repository is false for a fork, which has no index of its own.
	Repository bool
	// DefaultBranch is the branch whose head is indexed, "" while the
	// workspace has none.
	DefaultBranch string
	// Indexed is the workspace's setting code_index.
	Indexed bool
}

// Repository is what a build reads of a workspace's repository.
type Repository interface {
	ResolveBranch(ctx context.Context, branch string) (id string, found bool, err error)
	WriteTree(ctx context.Context, commit, dir string) (written int, err error)
}

// attempt is what one attempt of a build job did, kept from its run to its
// settlement.
type attempt struct {
	slug, sha string
	partial   bool
	files     int
	took      time.Duration
	// err is why the build failed, nil when it built.
	err error
}

// workspaceDir is the directory that holds everything of the workspace
// slug, and buildDir the directory of its build of the commit sha.
func (c *Component) workspaceDir(slug string) string {
	return filepath.Join(c.deps.DataDir, DirName, slug)
}

func (c *Component) buildDir(slug, sha string) string {
	return filepath.Join(c.workspaceDir(slug), sha)
}

// TreeDir is the directory that holds the files of the commit sha of the
// workspace slug once its build has ended.
func (c *Component) TreeDir(slug, sha string) string {
	return filepath.Join(c.buildDir(slug, sha), treeDir)
}

// indexes reports whether the hub keeps an index of ws.
func (c *Component) indexes(ws Workspace) bool {
	return c.Enabled() && ws.Repository && ws.Indexed
}

// Queue queues a build of the workspace slug when the hub keeps an index of
// it: a repository workspace whose setting is on, on a hub with the
// section. A build already pending is success. A build that failed and
// waits for its next attempt is made due at once, since what it is asked to
// build may have changed; beside a running build one more is queued, which
// takes the head as it is when it starts.
func (c *Component) Queue(ctx context.Context, slug string) error {
	if !c.Enabled() {
		return nil
	}
	ws, found, err := c.deps.Workspace(ctx, slug)
	if err != nil || !found || !c.indexes(ws) {
		return err
	}
	return c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.queueIn(ctx, tx, slug) })
}

// queueIn is Queue in tx, for a workspace the hub keeps an index of.
func (c *Component) queueIn(ctx context.Context, tx *sql.Tx, slug string) error {
	payload := json.RawMessage(`{}`)
	_, err := c.deps.Jobs.Enqueue(ctx, tx, JobType, slug, payload)
	dup, pending := errors.AsType[*jobs.DuplicateError](err)
	if !pending {
		return err
	}
	if hastened, err := c.deps.Jobs.Hasten(ctx, tx, JobType, slug); err != nil || hastened {
		return err
	}
	// A queued build will take the head as it is then; a running one took
	// it before, so one more follows it.
	_, err = c.deps.Jobs.EnqueueFollowUp(ctx, tx, JobType, slug, dup.JobID, payload)
	if _, pending := errors.AsType[*jobs.DuplicateError](err); pending {
		return nil
	}
	return err
}

// Handler is the handler of the job type JobType.
func (c *Component) Handler() jobs.Handler { return buildHandler{c} }

type buildHandler struct{ c *Component }

// Run brings the tree and the index of the job's workspace to the head of
// its default branch as it is now. A workspace the hub keeps no index of, or
// one without a head, or whose index is at the head already, is nothing to
// do. A bound that stops the build leaves a partial index and is no
// failure.
func (h buildHandler) Run(ctx context.Context, j jobs.Job) error {
	c := h.c
	ws, found, err := c.deps.Workspace(ctx, j.Workspace)
	if err != nil {
		return err
	}
	if !found || !c.indexes(ws) || ws.DefaultBranch == "" {
		return nil
	}
	repo, err := c.deps.Repos(ctx, ws.Slug)
	if err != nil {
		return fmt.Errorf("open the repository of %s: %w", ws.Slug, err)
	}
	head, found, err := repo.ResolveBranch(ctx, ws.DefaultBranch)
	if err != nil {
		return fmt.Errorf("resolve %s of %s: %w", ws.DefaultBranch, ws.Slug, err)
	}
	if !found {
		return nil
	}
	rec, _, err := c.Record(ctx, ws.Slug)
	if err != nil {
		return err
	}
	if rec.IndexedSHA == head && c.stored(ws.Slug, head) {
		return nil
	}

	a := &attempt{slug: ws.Slug, sha: head}
	start := c.deps.Now()
	a.files, a.partial, a.err = c.build(ctx, repo, ws.Slug, head)
	a.took = c.deps.Now().Sub(start)
	if a.err != nil {
		// What a failed build wrote is of no use; the index there was is
		// in another directory and stays.
		_ = os.RemoveAll(c.buildDir(ws.Slug, head))
	}
	c.keep(j.ID, a)
	return a.err
}

// build writes the tree of the commit sha of the workspace slug and builds
// and stores its index, in the directory of that commit alone.
func (c *Component) build(ctx context.Context, repo Repository, slug, sha string) (files int, partial bool, err error) {
	dir := c.buildDir(slug, sha)
	// What is left of an attempt that did not end.
	if err := os.RemoveAll(dir); err != nil {
		return 0, false, fmt.Errorf("clear the build directory of %s: %w", slug, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, false, fmt.Errorf("make the build directory of %s: %w", slug, err)
	}
	if _, err := repo.WriteTree(ctx, sha, filepath.Join(dir, treeDir)); err != nil {
		return 0, false, fmt.Errorf("write the tree of %s at %s: %w", slug, sha, err)
	}
	idx, err := c.deps.Open(filepath.Join(dir, treeDir), filepath.Join(dir, storeDir), filepath.Join(dir, scratchDir), c.bounds())
	if err != nil {
		return 0, false, fmt.Errorf("make the index of %s: %w", slug, err)
	}
	// The stored index stays; the next use of it loads it.
	defer func() {
		if closeErr := idx.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close the index of %s: %w", slug, closeErr)
		}
	}()
	if err := idx.Build(ctx); err != nil {
		return 0, false, fmt.Errorf("build the index of %s at %s: %w", slug, sha, err)
	}
	files, partial, err = idx.State(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("read the index of %s at %s: %w", slug, sha, err)
	}
	return files, partial, nil
}

// stored reports whether the directory of the build of sha is there.
func (c *Component) stored(slug, sha string) bool {
	fi, err := os.Stat(filepath.Join(c.buildDir(slug, sha), storeDir))
	return err == nil && fi.IsDir()
}

// keep keeps a for the settlement of the job id, and take takes it back,
// nil when the attempt built nothing.
func (c *Component) keep(id string, a *attempt) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attempts == nil {
		c.attempts = map[string]*attempt{}
	}
	c.attempts[id] = a
}

func (c *Component) take(id string) *attempt {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.attempts[id]
	delete(c.attempts, id)
	return a
}

// Settled records what the attempt did, in the transaction that settles its
// job, and counts the build under its outcome. The queue does not call it
// for a job that is gone, with its workspace, or that a stop gave up, so
// such a build has no outcome and leaves no record.
func (h buildHandler) Settled(ctx context.Context, tx *sql.Tx, j jobs.Job, _ bool) error {
	c := h.c
	a := c.take(j.ID)
	if a == nil {
		return nil
	}
	now := c.deps.Now().UTC()
	outcome := outcomeOK
	switch {
	case a.err != nil:
		outcome = outcomeFailed
		if err := recordFailed(ctx, tx, a.slug, a.sha, c.deps.Scrub(a.err.Error()), now); err != nil {
			return err
		}
	default:
		if a.partial {
			outcome = outcomePartial
		}
		if err := recordBuilt(ctx, tx, a.slug, a.sha, a.partial, a.files, now); err != nil {
			return err
		}
	}
	_ = c.deps.Metrics.Add(metrics.CodeIndexBuildsTotal, map[string]string{"outcome": outcome}, 1)
	_ = c.deps.Metrics.Observe(metrics.CodeIndexBuildDurationSeconds, nil, a.took.Seconds())
	return nil
}
