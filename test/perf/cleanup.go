package perf

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"
)

// cleanupWait bounds each clean-up request, which runs after the
// scenario's own deadline.
const cleanupWait = 30 * time.Second

// created records what a scenario created on the hub: the API paths whose
// DELETE removes a fork or a sandbox, and the connections it holds open.
type created struct {
	mu      sync.Mutex
	deletes []string
	closers []func()
}

// remove records that path, below the REST API's mount, is deleted when
// the scenario ends.
func (c *created) remove(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes = append(c.deletes, path)
}

// close records that fn, which closes a connection the scenario opened, is
// called when the scenario ends.
func (c *created) close(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closers = append(c.closers, fn)
}

// cleanUp closes the connections, then deletes what was created, the last
// first, each request bounded by cleanupWait and outliving ctx's
// cancellation. It returns a line for each deletion that failed, naming
// its method, path and status.
func (c *created) cleanUp(ctx context.Context, cl *client) []string {
	c.mu.Lock()
	deletes, closers := c.deletes, c.closers
	c.deletes, c.closers = nil, nil
	c.mu.Unlock()
	for _, fn := range slices.Backward(closers) {
		fn()
	}
	var failed []string
	for _, path := range slices.Backward(deletes) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
		_, err := cl.do(rctx, http.MethodDelete, path, nil)
		cancel()
		if err != nil {
			failed = append(failed, cl.detail(err.Error()))
		}
	}
	return failed
}
