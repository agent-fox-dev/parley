package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// Workers run the clone work of the workspaces in the background: at most
// as many clones at the same time as there are workers, the waiting work
// started in the order it was recorded. The work itself is durable, since a
// workspace in pending or cloning is its own record of it; the queue only
// holds what this hub has been told about since its start, and Start fills
// it from the store.
type Workers struct {
	svc  *Service
	work func(ctx context.Context, slug string) error
	size int

	mu   sync.Mutex
	wake *sync.Cond
	// queue is the work waiting, first recorded first; held is every slug
	// waiting or running, so that a workspace is never cloned twice at once.
	queue   []string
	held    map[string]bool
	stopped bool
	// started is set by Start.
	started bool
	// cancel ends the context of the running clones; done is closed once
	// every worker has returned. Both are set by Launch.
	cancel context.CancelFunc
	done   chan struct{}
}

// NewWorkers returns size workers that run work for each workspace they are
// given, the clone work of svc (Service.Clone) when work is nil. Fewer than
// one worker could never clone anything, so a size below 1 is refused with
// an error that names the configuration key it comes from. No worker runs
// before Launch.
func NewWorkers(size int, svc *Service, work func(ctx context.Context, slug string) error) (*Workers, error) {
	if size < 1 {
		return nil, fmt.Errorf("[workspace].workers must be at least 1, got %d", size)
	}
	if work == nil {
		work = svc.Clone
	}
	wk := &Workers{svc: svc, work: work, size: size, held: map[string]bool{}}
	wk.wake = sync.NewCond(&wk.mu)
	return wk, nil
}

// Start removes what clones interrupted by an earlier stop left in the
// repository root and queues the clone work of every workspace in pending
// or cloning in the order it was recorded (CloneWork). It runs no clone:
// the hub calls it before it listens, so that the work of before the start
// is ahead of any new one, and launches the workers only once it serves, so
// that a start that fails after it leaves nothing running. Start runs once;
// a second call is an error.
func (wk *Workers) Start(ctx context.Context) error {
	wk.mu.Lock()
	started := wk.started
	wk.started = true
	wk.mu.Unlock()
	if started {
		return errors.New("the clone workers are started already")
	}
	if err := wk.svc.removeCloneDirs(); err != nil {
		return err
	}
	slugs, err := CloneWork(ctx, wk.svc.deps.Store.Main())
	if err != nil {
		return err
	}
	for _, slug := range slugs {
		wk.Enqueue(slug)
	}
	return nil
}

// Launch starts the workers on the work queued so far and on what Enqueue
// adds later. The clones run until Stop, or until ctx ends, which stops the
// workers as Stop does without waiting for them. Launch runs once; a second
// call is an error.
func (wk *Workers) Launch(ctx context.Context) error {
	wk.mu.Lock()
	if wk.done != nil {
		wk.mu.Unlock()
		return errors.New("the clone workers are launched already")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	wk.cancel, wk.done = cancel, done
	wk.mu.Unlock()
	context.AfterFunc(runCtx, wk.halt)
	var wg sync.WaitGroup
	for range wk.size {
		wg.Go(func() { wk.run(runCtx) })
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	return nil
}

// Enqueue records that the workspace slug has clone work: it waits behind
// the work recorded before it until a worker is free. The hub calls it once
// the transaction that recorded the work has committed, as does a feature
// that puts a workspace back to pending, through Service.Enqueue. A slug already waiting or running
// is not queued again, and nothing is queued after Stop.
func (wk *Workers) Enqueue(slug string) {
	wk.mu.Lock()
	defer wk.mu.Unlock()
	if wk.stopped || wk.held[slug] {
		return
	}
	wk.held[slug] = true
	wk.queue = append(wk.queue, slug)
	wk.wake.Signal()
}

// Stop stops the workers: they take no more work, the running clones are
// cancelled, which kills their git processes, and the clones in progress
// are removed. The status of an interrupted workspace is left as it is, so
// that the next Start takes its work up again. Once every worker has
// returned, Stop removes the clones in progress and returns the error of
// that removal, nil when it succeeds; it returns ctx's error when ctx ends
// before the workers return. Workers never launched have nothing to stop.
func (wk *Workers) Stop(ctx context.Context) error {
	wk.halt()
	wk.mu.Lock()
	cancel, done := wk.cancel, wk.done
	wk.mu.Unlock()
	if done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return wk.svc.removeCloneDirs()
}

// halt makes the workers take no more work and wakes the idle ones, so that
// they return.
func (wk *Workers) halt() {
	wk.mu.Lock()
	defer wk.mu.Unlock()
	wk.stopped = true
	wk.queue = nil
	wk.wake.Broadcast()
}

// run is one worker: it runs the work of the slugs it takes from the queue,
// one at a time, until the workers are stopped. An error that is not the
// end of ctx means the outcome could not be recorded; the workspace keeps
// its status, so its work is taken up again at the next start.
func (wk *Workers) run(ctx context.Context) {
	for {
		slug, ok := wk.next()
		if !ok {
			return
		}
		err := wk.work(ctx, slug)
		wk.mu.Lock()
		delete(wk.held, slug)
		wk.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			wk.svc.deps.Logger.Error("workspace clone work failed", "slug", slug, "error", err)
		}
	}
}

// next waits for the first slug of the queue and takes it; ok is false once
// the workers are stopped.
func (wk *Workers) next() (slug string, ok bool) {
	wk.mu.Lock()
	defer wk.mu.Unlock()
	for len(wk.queue) == 0 && !wk.stopped {
		wk.wake.Wait()
	}
	if wk.stopped {
		return "", false
	}
	slug, wk.queue = wk.queue[0], wk.queue[1:]
	return slug, true
}

// removeCloneDirs removes every clone in progress from the repository root.
// It runs only while no clone does: at the start of the workers and after
// they have stopped.
func (s *Service) removeCloneDirs() error {
	entries, err := os.ReadDir(s.deps.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the repository root: %w", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), cloneDirPrefix) {
			if err := gitrepo.Remove(filepath.Join(s.deps.Root, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// Enqueue hands the workspace slug to the clone workers (Deps.Notify). It is
// for the features that put a workspace back to pending, which call it once
// the transaction that did so has committed; the workers are the hub's, out
// of their reach. Without a Notify it tells no one, and the work waits for
// the next start.
func (s *Service) Enqueue(slug string) {
	if s.deps.Notify != nil {
		s.deps.Notify(slug)
	}
}
