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
	// IsAncestor reports whether the commit ancestor is reachable from the
	// commit descendant.
	IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error)
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
	// idx is the index of a partial build, left open: codesearch does not
	// store a partial index whole, so only its build can load it. The
	// settlement keeps it for the workspace's use.
	idx Index
}

// closeIndex closes the index a partial build left open, if any.
func (a *attempt) closeIndex() {
	if a.idx != nil {
		_ = a.idx.Close()
		a.idx = nil
	}
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

// queueLook is Queue for a look at a head that did not move: a build that
// failed and waits keeps its delay, and beside a running build none more is
// queued.
func (c *Component) queueLook(ctx context.Context, slug string) error {
	ws, found, err := c.deps.Workspace(ctx, slug)
	if err != nil || !found || !c.indexes(ws) {
		return err
	}
	return c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.queueIn(ctx, tx, slug, false) })
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
	if c.current(ws.Slug, head, rec) {
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
	var idx Index
	a.files, a.partial, idx, a.err = c.build(ctx, repo, ws.Slug, head)
	a.took = c.deps.Now().Sub(start)
	switch {
	case idx == nil:
	case a.partial:
		a.idx = idx
	default:
		// A complete index is stored whole; its next use loads it.
		if err := idx.Close(); err != nil {
			a.err = fmt.Errorf("close the index of %s: %w", ws.Slug, err)
		}
	}
	// A head that moved while the build ran, whose notice was lost or came
	// before the build's job was running, is built next.
	if now, found, err := repo.ResolveBranch(ctx, ws.DefaultBranch); err == nil && found && now != head {
		a.moved = true
	}
	if a.err != nil && ctx.Err() == nil {
		// What a failed build wrote is of no use; the index there was is
		// in another directory and stays. A build ended from outside
		// leaves its directory to the sweep: by now it may be another
		// build's, of a workspace made again under the slug.
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
// and stores its index, in the directory of that commit alone. The index it
// built is returned open; on a failure nothing is.
func (c *Component) build(ctx context.Context, repo Repository, slug, sha string) (files int, partial bool, idx Index, err error) {
	dir := c.buildDir(slug, sha)
	// What is left of an attempt that did not end.
	if err := os.RemoveAll(dir); err != nil {
		return 0, false, nil, fmt.Errorf("clear the build directory of %s: %w", slug, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, false, nil, fmt.Errorf("make the build directory of %s: %w", slug, err)
	}
	if _, err := repo.WriteTree(ctx, sha, filepath.Join(dir, treeDir)); err != nil {
		return 0, false, nil, fmt.Errorf("write the tree of %s at %s: %w", slug, sha, err)
	}
	idx, err = c.deps.Open(filepath.Join(dir, treeDir), filepath.Join(dir, storeDir), filepath.Join(dir, scratchDir), c.bounds())
	if err != nil {
		return 0, false, nil, fmt.Errorf("make the index of %s: %w", slug, err)
	}
	if err := idx.Build(ctx); err != nil {
		_ = idx.Close()
		return 0, false, nil, fmt.Errorf("build the index of %s at %s: %w", slug, sha, err)
	}
	files, partial, err = idx.State(ctx)
	if err != nil {
		_ = idx.Close()
		return 0, false, nil, fmt.Errorf("read the index of %s at %s: %w", slug, sha, err)
	}
	return files, partial, idx, nil
}

// stored reports whether the directory of the build of sha is there.
func (c *Component) stored(slug, sha string) bool {
	fi, err := os.Stat(filepath.Join(c.buildDir(slug, sha), storeDir))
	return err == nil && fi.IsDir()
}

// current reports whether the index rec names is of the commit sha and can
// be used as it is: its build stored it, and it is complete or loaded.
func (c *Component) current(slug, sha string, rec Record) bool {
	return rec.IndexedSHA == sha && c.stored(slug, sha) && c.usable(slug, rec)
}

// usable reports whether the index rec names can be used without a build:
// a complete one is loaded from what its build stored, but codesearch does
// not store a partial one whole, so only the build of this process that
// made it, which left it open, can. After a restart a partial index is
// behind, and its build is a job like any other.
func (c *Component) usable(slug string, rec Record) bool {
	if !rec.Partial {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	o, b := c.open[slug], c.built[slug]
	return (o != nil && o.sha == rec.IndexedSHA) || (b != nil && b.sha == rec.IndexedSHA)
}

// keep keeps a for the settlement of the job id, and take takes it back,
// nil when the attempt built nothing.
func (c *Component) keep(id string, a *attempt) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attempts == nil {
		c.attempts = map[string]*attempt{}
	}
	if old := c.attempts[id]; old != nil {
		old.closeIndex()
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
		go c.settled(a.slug, nil)
	default:
		if a.partial {
			outcome = outcomePartial
		}
		if err := recordBuilt(ctx, tx, a.slug, a.sha, a.partial, a.files, now); err != nil {
			a.closeIndex()
			return err
		}
		// Kept before the record commits, so that no one sees the record
		// of a partial index without the index.
		go c.settled(a.slug, c.hold(a))
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
	return !c.current(ws.Slug, head, rec), nil
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
		for id, a := range c.attempts {
			if a.slug == slug {
				a.closeIndex()
				delete(c.attempts, id)
			}
		}
		c.mu.Unlock()
		c.release(slug, "")
		c.wake()
		// A build a query still reads stays until that query has ended,
		// which sweeps again.
		builds, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		var failures []error
		kept := false
		for _, b := range builds {
			removed, ok := c.claimRemoval(slug, b.Name())
			if !ok {
				kept = true
				continue
			}
			failures = append(failures, os.RemoveAll(filepath.Join(dir, b.Name())))
			removed()
		}
		if !kept {
			failures = append(failures, os.Remove(dir))
		}
		return errors.Join(failures...)
	}
	builds, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var failures []error
	for _, b := range builds {
		// Each removal is decided on the record as it is then, and the
		// build under way asked for after it: a build that settled or began
		// while an older tree was being removed is kept.
		rec, _, err := c.Record(ctx, slug)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		c.release(slug, rec.IndexedSHA)
		if b.Name() == rec.IndexedSHA || b.Name() == c.buildingOf(slug) {
			continue
		}
		// A build whose index a query still loads or reads stays; the end
		// of that query sweeps again.
		removed, ok := c.claimRemoval(slug, b.Name())
		if !ok {
			continue
		}
		failures = append(failures, os.RemoveAll(filepath.Join(dir, b.Name())))
		removed()
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
	idx, _, done, err := c.acquire(ctx, slug)
	if err != nil {
		return nil, err
	}
	defer done()
	res, err := idx.Search(ctx, query, maxSearchFiles)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, f := range res.Files {
		paths = append(paths, f.Path)
	}
	return paths, nil
}

// maxSearchFiles is the most files the index answers a search with.
const maxSearchFiles = 25

// ErrNoIndex is the error of a use of the index of a workspace that has
// none.
var ErrNoIndex = errors.New("the workspace has no code index")

// acquire is the index the record of the workspace slug names and the
// commit it holds, loaded from what its build stored on its first use and
// kept until the record names another. The caller calls done when its use
// has ended: until then the index stays open and the directory of its
// build stays on disk, also when a newer build replaces it meanwhile, since
// the index reads the tree it was built from. A stored index that cannot
// be loaded is treated as none: its directory is removed and its build
// queued.
func (c *Component) acquire(ctx context.Context, slug string) (idx Index, sha string, done func(), err error) {
	for attempt := 0; ; attempt++ {
		rec, found, err := c.Record(ctx, slug)
		if err != nil {
			return nil, "", nil, err
		}
		if !found || rec.IndexedSHA == "" {
			return nil, "", nil, ErrNoIndex
		}
		sha := rec.IndexedSHA
		// The record may name another commit by now, and the sweep may be
		// taking this one's directory, or have taken it: then the record
		// is read again. Only a pinned build that is on disk is loaded; an
		// index made over a tree that is gone would hold no file.
		again := func() (retry bool, err error) {
			if attempt >= acquireTries {
				return false, fmt.Errorf("%w that stays long enough to be loaded", ErrNoIndex)
			}
			select {
			case <-ctx.Done():
				return false, context.Cause(ctx)
			case <-time.After(time.Millisecond):
				return true, nil
			}
		}
		if !c.pin(slug, sha) {
			if retry, err := again(); !retry {
				return nil, "", nil, err
			}
			continue
		}
		if !c.onDisk(slug, sha) {
			c.unpin(slug, sha)
			if now, _, recErr := c.Record(ctx, slug); recErr == nil && now.IndexedSHA == sha {
				// The record's own build is gone: no index; its build is
				// queued by whoever waits for it.
				return nil, "", nil, fmt.Errorf("%w on disk", ErrNoIndex)
			}
			if retry, err := again(); !retry {
				return nil, "", nil, err
			}
			continue
		}
		o, err := c.open1(ctx, slug, sha, rec.Partial)
		if err == nil {
			return o.idx, sha, func() { c.done(o); c.unpin(slug, sha) }, nil
		}
		c.unpin(slug, sha)
		if ctx.Err() != nil {
			// The caller gave up; the stored index is not at fault.
			return nil, "", nil, context.Cause(ctx)
		}
		if now, _, recErr := c.Record(ctx, slug); recErr == nil && now.IndexedSHA != sha && attempt < acquireTries {
			continue
		}
		if errors.Is(err, errNotStored) {
			// Its build makes it again, as a job; until then there is none.
			go func() { _ = c.queueLook(context.WithoutCancel(ctx), slug) }()
			return nil, "", nil, fmt.Errorf("%w loaded: %w", ErrNoIndex, err)
		}
		// An index that cannot be loaded is none: its directory goes,
		// unless another query still reads it, and its build is queued.
		if removed, ok := c.claimRemoval(slug, sha); ok {
			_ = os.RemoveAll(c.buildDir(slug, sha))
			removed()
		}
		go func() { _ = c.Queue(context.WithoutCancel(ctx), slug) }()
		return nil, "", nil, fmt.Errorf("%w that can be loaded: %w", ErrNoIndex, err)
	}
}

// onDisk reports whether the tree and the stored index of the build of sha
// are there.
func (c *Component) onDisk(slug, sha string) bool {
	tree, err := os.Stat(c.TreeDir(slug, sha))
	return err == nil && tree.IsDir() && c.stored(slug, sha)
}

// acquireTries is how often acquire reads the record again when the index
// it named was replaced before it could be loaded.
const acquireTries = 50

// open1 is the loaded index of the build of the commit sha of the
// workspace slug, with one more use counted; the caller has pinned the
// build's directory. A commit is loaded once however many callers ask for
// it at once, and the load goes on when the caller that began it gives up,
// so that a load longer than one request's time still ends for the
// requests after it. A partial index is never loaded, since that would
// build it: unless its build left it loaded, it is errNotStored.
func (c *Component) open1(ctx context.Context, slug, sha string, partial bool) (*openIndex, error) {
	key := slug + "/" + sha
	c.mu.Lock()
	if o := c.open[slug]; o != nil && o.sha == sha {
		o.uses++
		c.mu.Unlock()
		return o, nil
	}
	if b := c.built[slug]; b != nil && b.sha == sha {
		// The index of the partial build the record names, which settled
		// a moment ago.
		c.loadedLocked(b)
		b.uses++
		c.mu.Unlock()
		return b, nil
	}
	if partial {
		c.mu.Unlock()
		return nil, errNotStored
	}
	l := c.loads[key]
	if l == nil {
		l = &load{done: make(chan struct{})}
		if c.loads == nil {
			c.loads = map[string]*load{}
		}
		c.loads[key] = l
		// The load's own pin: its caller may leave before it ends.
		if c.pins == nil {
			c.pins = map[string]int{}
		}
		c.pins[key]++
		go c.load(slug, sha, l)
	}
	c.mu.Unlock()
	select {
	case <-l.done:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	if l.err != nil {
		return nil, l.err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if o := c.open[slug]; o != nil && o.sha == sha {
		o.uses++
		return o, nil
	}
	// Loaded, and replaced by a newer build before this caller came back.
	return nil, errReplaced
}

// errNotStored is the error of the use of a partial index that no build of
// this process left loaded.
var errNotStored = errors.New("a partial index is not stored whole, and is built again as a build")

// errReplaced is the error of a load whose index a newer build replaced
// before its caller could use it.
var errReplaced = errors.New("the index was replaced while it was loaded")

// load is one load of an index, which the callers that asked for it wait
// for.
type load struct {
	done chan struct{}
	err  error
}

// load loads the complete index of the build of sha for the callers that
// wait on l. It runs outside the lock, which every settlement of a build
// takes: a load may copy a large index.
func (c *Component) load(slug, sha string, l *load) {
	key := slug + "/" + sha
	// Bounded like a build: codesearch builds a stored index that does not
	// read back.
	ctx, cancel := context.WithTimeout(context.Background(), 2*c.deps.Settings.BuildTimeout+time.Minute)
	defer cancel()
	dir := c.buildDir(slug, sha)
	loaded, err := c.deps.Open(filepath.Join(dir, treeDir), filepath.Join(dir, storeDir), filepath.Join(dir, scratchDir), c.bounds())
	if err == nil {
		// Over the directories a build stored into, this loads.
		if err = loaded.Build(ctx); err != nil {
			_ = loaded.Close()
		}
	}
	c.mu.Lock()
	delete(c.loads, key)
	l.err = err
	if err == nil {
		c.loadedLocked(&openIndex{slug: slug, sha: sha, idx: loaded})
	}
	c.mu.Unlock()
	close(l.done)
	c.unpin(slug, sha)
}

// loadedLocked makes o the loaded index of its workspace, in place of the
// one there was, and out of the built ones. The caller holds c.mu.
func (c *Component) loadedLocked(o *openIndex) {
	if c.built[o.slug] == o {
		delete(c.built, o.slug)
	}
	if old := c.open[o.slug]; old != nil && old != o {
		c.retire(o.slug, old)
	}
	if c.open == nil {
		c.open = map[string]*openIndex{}
	}
	c.open[o.slug] = o
}

// pin keeps the directory of the build of the commit sha of the workspace
// slug from the sweep until unpin; it reports false when the sweep is
// taking the directory already.
func (c *Component) pin(slug, sha string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := slug + "/" + sha
	if c.removing[key] {
		return false
	}
	if c.pins == nil {
		c.pins = map[string]int{}
	}
	c.pins[key]++
	return true
}

// unpin ends one pin; the last one lets the sweep take the directory of a
// build the record no longer names.
func (c *Component) unpin(slug, sha string) {
	c.mu.Lock()
	key := slug + "/" + sha
	c.pins[key]--
	last := c.pins[key] <= 0
	if last {
		delete(c.pins, key)
	}
	// A build the workspace's loaded index is of is the record's, which
	// the sweep keeps anyway; any other was replaced and may go now.
	replaced := c.open[slug] == nil || c.open[slug].sha != sha
	c.mu.Unlock()
	if last && replaced {
		go func() { _ = c.sweepWorkspace(context.Background(), slug) }()
	}
}

// claimRemoval reports whether the sweep may remove the directory of the
// build of sha, which no one has pinned, and keeps anyone from pinning it
// until removed is called.
func (c *Component) claimRemoval(slug, sha string) (removed func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := slug + "/" + sha
	if c.pins[key] > 0 || c.removing[key] {
		return nil, false
	}
	if c.removing == nil {
		c.removing = map[string]bool{}
	}
	c.removing[key] = true
	return func() {
		c.mu.Lock()
		delete(c.removing, key)
		c.mu.Unlock()
	}, true
}

// openIndex is a loaded index of a workspace, the commit it holds, how many
// callers use it now, and whether it is to be closed once none does.
type openIndex struct {
	slug, sha string
	idx       Index
	uses      int
	retired   bool
}

// retire takes o, the loaded index of slug, out of use: it is closed now,
// or once its last user is done. The caller holds c.mu.
func (c *Component) retire(slug string, o *openIndex) {
	if c.open[slug] == o {
		delete(c.open, slug)
	}
	if o.uses == 0 {
		_ = o.idx.Close()
		return
	}
	// A query still reads it; its last use closes it.
	o.retired = true
}

// done ends one use of o, and closes it when it was the last use of an
// index a newer one replaced.
func (c *Component) done(o *openIndex) {
	c.mu.Lock()
	o.uses--
	closing := o.retired && o.uses == 0
	c.mu.Unlock()
	if closing {
		_ = o.idx.Close()
	}
}

// release takes the loaded index of the workspace slug out of use unless
// it holds the commit keep.
func (c *Component) release(slug, keep string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if o := c.open[slug]; o != nil && o.sha != keep {
		c.retire(slug, o)
	}
}

// Run keeps the component until ctx ends, the hub's stop, and then closes
// every index it has loaded; what their builds stored stays.
func (c *Component) Run(ctx context.Context) {
	<-ctx.Done()
	c.mu.Lock()
	open, built := c.open, c.built
	c.open, c.built = nil, nil
	c.stopped = true
	for id, a := range c.attempts {
		a.closeIndex()
		delete(c.attempts, id)
	}
	c.mu.Unlock()
	for _, o := range open {
		_ = o.idx.Close()
	}
	for _, b := range built {
		_ = b.idx.Close()
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
	// Whoever waits for a build of it is told that none will come.
	c.wake()
	return err
}

// settled runs once the settlement of a build of the workspace slug has
// committed: the index of a partial build, b, becomes the loaded one, the
// builds the record no longer names go, the index loaded from one of them
// is closed, and the bytes on disk are measured.
func (c *Component) settled(slug string, b *openIndex) {
	ctx := context.Background()
	// The store runs its write transactions one at a time, in the order
	// they are asked for, so the settlement's has ended when this one has.
	err := c.deps.Store.Tx(ctx, func(*sql.Tx) error { return nil })
	if b != nil {
		c.adopt(ctx, b, err == nil)
	}
	if err != nil {
		return
	}
	c.wake()
	_ = c.sweepWorkspace(ctx, slug)
	c.measure()
}

// hold keeps the index a partial build left open as the one built for its
// workspace, from the settlement that records it to adopt; nil when there
// is none.
func (c *Component) hold(a *attempt) *openIndex {
	if a.idx == nil {
		return nil
	}
	b := &openIndex{slug: a.slug, sha: a.sha, idx: a.idx}
	a.idx = nil
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		_ = b.idx.Close()
		return nil
	}
	// One that waits still is of a settlement this one follows; no one has
	// used it, or it would be loaded.
	if old := c.built[a.slug]; old != nil {
		_ = old.idx.Close()
	}
	if c.built == nil {
		c.built = map[string]*openIndex{}
	}
	c.built[a.slug] = b
	return b
}

// adopt makes b, the index a partial build left open, the loaded index of
// its workspace when the settlement committed and the record names the
// build's commit, and closes it otherwise. A use or a later settlement
// that took it first leaves nothing to do.
func (c *Component) adopt(ctx context.Context, b *openIndex, committed bool) {
	rec, _, err := c.Record(ctx, b.slug)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.built[b.slug] != b {
		return
	}
	if !committed || err != nil || rec.IndexedSHA != b.sha {
		delete(c.built, b.slug)
		_ = b.idx.Close()
		return
	}
	c.loadedLocked(b)
}

// settlements is what is closed when the next settlement of a build has
// committed. A caller takes it before it reads the record it then waits
// to change, so that no settlement between the two is missed.
func (c *Component) settlements() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.changed == nil {
		c.changed = make(chan struct{})
	}
	return c.changed
}

// wake tells everyone who waits on settlements.
func (c *Component) wake() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.changed != nil {
		close(c.changed)
		c.changed = nil
	}
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
// its start, beside it: the walk of every tree does not hold the start up.
func (c *Component) Measure() {
	if c.Enabled() {
		go c.measure()
	}
}
