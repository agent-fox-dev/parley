package sandbox

import (
	"context"
	"net/http"
	"time"
)

// useEntryOf is the entry of the route that uses a sandbox named name
// (exec, upload, download, logs or attach).
func useEntryOf(s *Service, name string) *useEntry {
	for _, e := range s.use {
		if e.name == name {
			return e
		}
	}
	panic("no route that uses a sandbox is named " + name)
}

// SetUseOperation makes op the operation of the route that uses a sandbox
// named name, so that a test drives the checks before it without the
// operation behind it. It is called while the hub composes.
func SetUseOperation(s *Service, name string, op func(http.ResponseWriter, *http.Request) error) {
	useEntryOf(s, name).operate = func(w http.ResponseWriter, r *http.Request, _ useRequest) error { return op(w, r) }
}

// SetUseValidation makes v the request validation of the route that uses a
// sandbox named name. It is called while the hub composes.
func SetUseValidation(s *Service, name string, v func(*http.Request) error) {
	useEntryOf(s, name).validate = func(r *http.Request, _ *useRequest) error { return v(r) }
}

// InsertLogLines stores lines as the log lines of the sandbox sandboxID in
// tx, as the collector does, so that a test sets up lines of any time.
var InsertLogLines = insertLogLines

// Touch records at as the last activity of the sandbox id, as an operation
// on it does.
func Touch(s *Service, ctx context.Context, id string, at time.Time) { s.touch(ctx, id, at) }

// SetCheckpointFileLimit makes n bytes the size above which a checkpoint
// leaves an untracked file out; 0 restores CheckpointFileLimit.
func SetCheckpointFileLimit(s *Service, n int64) { s.checkpointLimit.Store(n) }

// CheckpointTurns is how many checkpoint attempts of the sandbox id hold
// or wait for its turn.
func CheckpointTurns(s *Service, id string) int {
	l := &s.checkpointing
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.held[id]; e != nil {
		return e.users
	}
	return 0
}

// PolicyFollowers returns the ids of the sandboxes whose policy log the
// collector c follows: those of its followers that have not stopped.
func PolicyFollowers(c *PolicyCollector) []string { return c.following() }
