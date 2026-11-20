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

// created records what a scenario created on the hub: the requests that
// undo it (the DELETE of a fork or a sandbox, the abandon of a change),
// and the connections it holds open.
type created struct {
	mu      sync.Mutex
	undos   []undo
	closers []func()
}

// undo is a request, without a body, to a path below the REST API's mount.
type undo struct{ method, path string }

// remove records that path, below the REST API's mount, is deleted when
// the scenario ends.
func (c *created) remove(path string) { c.add(undo{http.MethodDelete, path}) }

// post records that path, below the REST API's mount, is sent a POST
// without a body when the scenario ends.
func (c *created) post(path string) { c.add(undo{http.MethodPost, path}) }

func (c *created) add(u undo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.undos = append(c.undos, u)
}

// close records that fn, which closes a connection the scenario opened, is
// called when the scenario ends.
func (c *created) close(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closers = append(c.closers, fn)
}

// cleanUp closes the connections, then undoes what was created, the last
// first, each request bounded by cleanupWait and outliving ctx's
// cancellation. It returns a line for each request that failed, naming
// its method, path and status.
func (c *created) cleanUp(ctx context.Context, cl *client) []string {
	c.mu.Lock()
	undos, closers := c.undos, c.closers
	c.undos, c.closers = nil, nil
	c.mu.Unlock()
	for _, fn := range slices.Backward(closers) {
		fn()
	}
	var failed []string
	for _, u := range slices.Backward(undos) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
		_, err := cl.do(rctx, u.method, u.path, nil)
		cancel()
		if err != nil {
			failed = append(failed, cl.detail(err.Error()))
		}
	}
	return failed
}
