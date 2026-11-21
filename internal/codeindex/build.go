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
	// Parent is the slug of a fork's parent, "" for a repository workspace.
	Parent string
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
	// moved says that the head was another commit when the build ended.
	moved bool
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
	return c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.queueIn(ctx, tx, slug, true) })
}

// QueueIn is Queue in tx, for a caller that learns of a new head inside a
// transaction of its own: the build is queued when tx commits.
func (c *Component) QueueIn(ctx context.Context, tx *sql.Tx, slug string) error {
	if !c.Enabled() {
		return nil
	}
	ws, found, err := c.deps.Workspace(ctx, slug)
	if err != nil || !found || !c.indexes(ws) {
		return err
	}
	return c.queueIn(ctx, tx, slug, true)
}

// BranchMoved queues a build of the workspace slug when branch, a branch
// that has just moved in it, is its default branch.
func (c *Component) BranchMoved(ctx context.Context, slug, branch string) error {
	if !c.Enabled() {
		return nil
	}
	ws, found, err := c.deps.Workspace(ctx, slug)
	if err != nil || !found || !c.indexes(ws) || ws.DefaultBranch != branch {
		return err
	}
	return c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.queueIn(ctx, tx, slug, true) })
}

// queueIn queues, in tx, a build of the workspace slug, which the hub keeps
// an index of. With hasten, a build that failed and waits is made due at
// once, as for a head that moved; without, it keeps its delay, as for a
// look at a head that did not.
func (c *Component) queueIn(ctx context.Context, tx *sql.Tx, slug string, hasten bool) error {
	payload := json.RawMessage(`{}`)
	_, err := c.deps.Jobs.Enqueue(ctx, tx, JobType, slug, payload)
	dup, pending := errors.AsType[*jobs.DuplicateError](err)
	if !pending {
		return err
	}
	if !hasten {
		return nil
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
	// From here on the directory of this build is no leftover, though no
	// record names it until its settlement has committed, which nothing
	// reports; so it stays the workspace's last build until the next one.
	c.mu.Lock()
	if c.building == nil {
		c.building = map[string]string{}
	}
	c.building[ws.Slug] = head
	c.mu.Unlock()
	start := c.deps.Now()
	a.files, a.partial, a.err = c.build(ctx, repo, ws.Slug, head)
	a.took = c.deps.Now().Sub(start)
	// A head that moved while the build ran, whose notice was lost or came
	// before the build's job was running, is built next.
	if now, found, err := repo.ResolveBranch(ctx, ws.DefaultBranch); err == nil && found && now != head {
		a.moved = true
	}
	if a.err != nil {
		// What a failed build wrote is of no use; the index there was is
		// in another directory and stays.
		_ = os.RemoveAll(c.buildDir(ws.Slug, head))
	}
	if a.err != nil && ctx.Err() != nil {
		// Ended from outside: by the hub's stop, the workspace's delete, or
		// its setting turned off. It is no outcome of a build, and where
		// the hub keeps no index of the workspace any more, no failure to
		// try again.
		if now, found, err := c.deps.Workspace(context.WithoutCancel(ctx), ws.Slug); err == nil && (!found || !c.indexes(now)) {
			return nil
		}
		return a.err
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
		if err := recordFailed(ctx, tx, a.slug, a.sha, c.failureText(a.err), now); err != nil {
			return err
		}
	default:
		if a.partial {
			outcome = outcomePartial
		}
		if err := recordBuilt(ctx, tx, a.slug, a.sha, a.partial, a.files, now); err != nil {
			return err
		}
		go c.settled(a.slug)
		if a.moved {
			// The job is settled, so this is the workspace's one pending
			// build, or the one a notice queued already is.
			if err := c.queueIn(ctx, tx, a.slug, false); err != nil {
				return err
			}
		}
	}
	_ = c.deps.Metrics.Add(metrics.CodeIndexBuildsTotal, map[string]string{"outcome": outcome}, 1)
	_ = c.deps.Metrics.Observe(metrics.CodeIndexBuildDurationSeconds, nil, a.took.Seconds())
	return nil
}

// Reconcile queues a build for every workspace the hub keeps an index of
// whose index is not at the head of its default branch and that has no
// build pending. It is what makes the index follow a head whose move no
// one reported, and what a hub does at its start; a build that failed and
// waits for its next attempt keeps waiting.
func (c *Component) Reconcile(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}
	all, err := c.deps.Workspaces(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, ws := range all {
		if !c.indexes(ws) || ws.DefaultBranch == "" {
			continue
		}
		behind, err := c.behind(ctx, ws)
		if err == nil && behind {
			err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.queueIn(ctx, tx, ws.Slug, false) })
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("code index of %s: %w", ws.Slug, err))
		}
	}
	return errors.Join(failures...)
}

// behind reports whether the index of ws is not at the head of its default
// branch. A workspace without a repository or without that branch has
// nothing to index yet and is not behind.
func (c *Component) behind(ctx context.Context, ws Workspace) (bool, error) {
	repo, err := c.deps.Repos(ctx, ws.Slug)
	if err != nil {
		// No repository yet, or none any more: its clone reports itself.
		return false, nil
	}
	head, found, err := repo.ResolveBranch(ctx, ws.DefaultBranch)
	if err != nil || !found {
		return false, err
	}
	rec, _, err := c.Record(ctx, ws.Slug)
	if err != nil {
		return false, err
	}
	return rec.IndexedSHA != head || !c.stored(ws.Slug, head), nil
}

// Sweep removes from the code index directory what no index needs: the
// directory of a workspace that does not exist any more or that the hub
// keeps no index of, with its record, and, of a workspace it keeps one of,
// every build but the one its record names and the one being built.
func (c *Component) Sweep(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(c.deps.DataDir, DirName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the code index directory: %w", err)
	}
	var failures []error
	for _, e := range entries {
		if err := c.sweepWorkspace(ctx, e.Name()); err != nil {
			failures = append(failures, fmt.Errorf("code index of %s: %w", e.Name(), err))
		}
	}
	c.measure()
	return errors.Join(failures...)
}

// sweepWorkspace is Sweep for the directory named slug.
func (c *Component) sweepWorkspace(ctx context.Context, slug string) error {
	ws, found, err := c.deps.Workspace(ctx, slug)
	if err != nil {
		return err
	}
	dir := c.workspaceDir(slug)
	if !found || !c.indexes(ws) {
		// A build of it may still run, for a workspace whose setting was
		// just turned off; what it writes goes with the next pass.
		err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := c.deps.Jobs.CancelPending(ctx, tx, JobType, slug); err != nil {
				return err
			}
			return RemoveWorkspace(ctx, tx, slug)
		})
		if err != nil {
			return err
		}
		c.deps.Jobs.Interrupt(JobType, slug)
		c.mu.Lock()
		delete(c.building, slug)
		c.mu.Unlock()
		c.release(slug, "")
		return os.RemoveAll(dir)
	}
	rec, _, err := c.Record(ctx, slug)
	if err != nil {
		return err
	}
	builds, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	c.release(slug, rec.IndexedSHA)
	var failures []error
	for _, b := range builds {
		// The build under way is asked for last: one that began after the
		// record was read is kept, not taken for a leftover.
		if b.Name() == rec.IndexedSHA || b.Name() == c.buildingOf(slug) {
			continue
		}
		failures = append(failures, os.RemoveAll(filepath.Join(dir, b.Name())))
	}
	return errors.Join(failures...)
}

// RemoveWorkspace deletes, in tx, the record of the workspace slug.
func RemoveWorkspace(ctx context.Context, tx *sql.Tx, slug string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM code_index_records WHERE workspace_slug = ?`, slug); err != nil {
		return fmt.Errorf("delete the code index record of %s: %w", slug, err)
	}
	return nil
}

// buildingOf is the commit of the last build of the workspace slug this
// process began, "" for none.
func (c *Component) buildingOf(slug string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.building[slug]
}

// Files are the paths of the files of the workspace slug whose content
// matches query, a query of the index, from the index its record names. It
// builds nothing: the index a build stored is loaded on its first use.
func (c *Component) Files(ctx context.Context, slug, query string) ([]string, error) {
	idx, err := c.loaded(ctx, slug)
	if err != nil {
		return nil, err
	}
	return idx.Files(ctx, query)
}

// ErrNoIndex is the error of a use of the index of a workspace that has
// none.
var ErrNoIndex = errors.New("the workspace has no code index")

// loaded is the index the record of the workspace slug names, loaded from
// what its build stored on its first use and kept until the record names
// another. A stored index that cannot be loaded is treated as none: its
// directory is removed and its build queued.
func (c *Component) loaded(ctx context.Context, slug string) (Index, error) {
	rec, found, err := c.Record(ctx, slug)
	if err != nil {
		return nil, err
	}
	if !found || rec.IndexedSHA == "" {
		return nil, ErrNoIndex
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if o := c.open[slug]; o != nil && o.sha == rec.IndexedSHA {
		return o.idx, nil
	}
	dir := c.buildDir(slug, rec.IndexedSHA)
	idx, err := c.deps.Open(filepath.Join(dir, treeDir), filepath.Join(dir, storeDir), filepath.Join(dir, scratchDir), c.bounds())
	if err == nil {
		// Over the directories a build stored into, this loads; only a
		// partial index, which is not stored whole, is built again.
		if err = idx.Build(ctx); err != nil {
			_ = idx.Close()
		}
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		go func() { _ = c.Queue(context.WithoutCancel(ctx), slug) }()
		return nil, fmt.Errorf("%w that can be loaded: %w", ErrNoIndex, err)
	}
	if o := c.open[slug]; o != nil {
		_ = o.idx.Close()
	}
	if c.open == nil {
		c.open = map[string]*openIndex{}
	}
	c.open[slug] = &openIndex{sha: rec.IndexedSHA, idx: idx}
	return idx, nil
}

// openIndex is a loaded index and the commit it holds.
type openIndex struct {
	sha string
	idx Index
}

// release closes the loaded index of the workspace slug unless it holds
// the commit keep.
func (c *Component) release(slug, keep string) {
	c.mu.Lock()
	o := c.open[slug]
	if o != nil && o.sha != keep {
		delete(c.open, slug)
	} else {
		o = nil
	}
	c.mu.Unlock()
	if o != nil {
		_ = o.idx.Close()
	}
}

// Run keeps the component until ctx ends, the hub's stop, and then closes
// every index it has loaded; what their builds stored stays.
func (c *Component) Run(ctx context.Context) {
	<-ctx.Done()
	c.mu.Lock()
	open := c.open
	c.open = nil
	c.mu.Unlock()
	for _, o := range open {
		_ = o.idx.Close()
	}
}

// Stop ends the builds of the workspace slug: the ones that wait are
// cancelled and the one that runs is interrupted. It is what turning the
// workspace's setting off does at once; the maintenance pass removes the
// record, the tree and the index.
func (c *Component) Stop(ctx context.Context, slug string) error {
	if !c.Enabled() {
		return nil
	}
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := c.deps.Jobs.CancelPending(ctx, tx, JobType, slug)
		return err
	})
	c.deps.Jobs.Interrupt(JobType, slug)
	return err
}

// settled runs once the settlement of a build of the workspace slug has
// committed: the builds its record no longer names go, the index loaded
// from one of them is closed, and the bytes on disk are measured.
func (c *Component) settled(slug string) {
	ctx := context.Background()
	// The store runs its write transactions one at a time, in the order
	// they are asked for, so the settlement's has ended when this one has.
	if err := c.deps.Store.Tx(ctx, func(*sql.Tx) error { return nil }); err != nil {
		return
	}
	_ = c.sweepWorkspace(ctx, slug)
	c.measure()
}

// measure reports the bytes of the code index directory.
func (c *Component) measure() {
	var total int64
	_ = filepath.WalkDir(filepath.Join(c.deps.DataDir, DirName), func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			// A build or a removal changes the directory meanwhile.
			return nil
		}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			total += info.Size()
		}
		return nil
	})
	_ = c.deps.Metrics.Set(metrics.CodeIndexBytes, nil, float64(total))
}

// Measure reports the bytes of the code index directory, as the hub does at
// its start.
func (c *Component) Measure() {
	if c.Enabled() {
		c.measure()
	}
}
