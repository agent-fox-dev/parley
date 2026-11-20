package perf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// The counts the capacity scenarios create up to, and how many creations
// of one are in flight at once: enough that 5,000 forks fit in the
// capacity deadline, few enough that the hub is not measured under a
// load no client sends.
const (
	activeForksCount      = 5000
	openChangesCount      = 2000
	eventStreamsCount     = 500
	sandboxesRunningCount = 200
	capacityInFlight      = 8
)

// fill calls create until want calls succeeded or the hub refused one,
// with at most capacityInFlight calls in flight; n is the call's index,
// from 0. It returns how many succeeded and the hub's first refusal (an
// answer of 400 or more), nil when want was reached. Calls in flight when
// the hub refuses one are counted when they succeed. Any other error of
// create ends fill with that error.
func fill(ctx context.Context, want int, create func(ctx context.Context, n int) error) (int, *requestError, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		next     int
		done     int
		refusal  *requestError
		failure  error
		workers  sync.WaitGroup
		stopping = func() bool { return next >= want || refusal != nil || failure != nil }
	)
	for range min(capacityInFlight, want) {
		workers.Go(func() {
			for {
				mu.Lock()
				if stopping() {
					mu.Unlock()
					return
				}
				n := next
				next++
				mu.Unlock()
				err := create(ctx, n)
				mu.Lock()
				re, ok := errors.AsType[*requestError](err)
				switch {
				case err == nil:
					done++
				case ok && re.status >= 400:
					if refusal == nil {
						refusal = re
					}
				case failure == nil:
					failure = err
					cancel()
				}
				mu.Unlock()
			}
		})
	}
	workers.Wait()
	if failure != nil {
		return done, nil, failure
	}
	return done, refusal, nil
}

// refused is what a detail says of the hub's refusal: its status and
// error_type, which come first so that a detail cut to its length keeps
// them, or its message when the answer had no error_type.
func refused(re *requestError) string {
	what := re.errorType
	if what == "" {
		what = re.message
	}
	return fmt.Sprintf("refused with %d %s", re.status, what)
}

// stopDetail says why a capacity scenario stopped after n creations: the
// refusal re, or nothing when it reached its count.
func stopDetail(n int, re *requestError) string {
	if re == nil {
		return ""
	}
	return fmt.Sprintf("%s after %d: %s %s: %s", refused(re), n, re.method, re.path, re.message)
}

// countOutcome is the outcome of a capacity scenario that had count at
// once, with the notes of its detail.
func countOutcome(count int, notes ...string) outcome {
	v := float64(count)
	var kept []string
	for _, n := range notes {
		if n != "" {
			kept = append(kept, n)
		}
	}
	return outcome{measured: &v, samples: count, detail: strings.Join(kept, "; ")}
}

// activeForks creates forks of each listed workspace in turn, moving on to
// the next when the hub refuses one, until activeForksCount exist or every
// workspace has refused. The default limit of forks per workspace is below
// the target, so the operator lists as many workspaces as the target
// needs, or raises the limit; the detail names each refusal.
func activeForks(ctx context.Context, e *env) (outcome, error) {
	total := 0
	var refusals []string
	for _, slug := range e.cfg.Workspaces {
		if total >= activeForksCount {
			break
		}
		n, re, err := fill(ctx, activeForksCount-total, func(ctx context.Context, _ int) error {
			_, _, err := e.forkOf(ctx, slug)
			return err
		})
		total += n
		if err != nil {
			return outcome{}, err
		}
		if re != nil {
			refusals = append(refusals, fmt.Sprintf("%s %s after %d", slug, refused(re), n))
		}
	}
	return countOutcome(total, strings.Join(refusals, "; ")), nil
}

// openChanges opens one-commit changes in the first workspace against the
// run's target branch until openChangesCount are open or the hub refuses
// one. Each change is abandoned when the scenario ends.
func openChanges(ctx context.Context, e *env) (outcome, error) {
	target, err := e.targetBranch(ctx)
	if err != nil {
		return outcome{}, err
	}
	slug := e.cfg.Workspaces[0]
	n, re, err := fill(ctx, openChangesCount, func(ctx context.Context, i int) error {
		number, err := e.openChange(ctx, target, "open_changes", i, 1)
		if err == nil {
			e.created.post(workspacePath(slug, "/changes/"+strconv.FormatInt(number, 10)+"/abandon"))
		}
		return err
	})
	if err != nil {
		return outcome{}, err
	}
	return countOutcome(n, stopDetail(n, re)), nil
}

// eventStreams opens GET /events on the first workspace until
// eventStreamsCount are open or the hub refuses one, and reports how many
// are still open when the last has opened. Each is read until the
// scenario ends, so that the hub never holds an event for a reader that
// does not read.
func eventStreams(ctx context.Context, e *env) (outcome, error) {
	path := "/events?workspace=" + url.QueryEscape(e.cfg.Workspaces[0])
	var ended atomic.Int64
	// A stream lives as long as the scenario, not as long as fill.
	n, re, err := fill(ctx, eventStreamsCount, func(context.Context, int) error {
		return e.holdStream(ctx, path, &ended)
	})
	if err != nil {
		return outcome{}, err
	}
	gone := int(ended.Load())
	var note string
	if gone > 0 {
		note = fmt.Sprintf("%d of the %d streams opened ended before the last opened", gone, n)
	}
	return countOutcome(n-gone, stopDetail(n, re), note), nil
}

// holdStream opens GET path below the REST API's mount and reads it,
// discarding what comes, until the scenario's clean-up closes it; ended
// counts a stream the hub ended before that.
func (e *env) holdStream(ctx context.Context, path string, ended *atomic.Int64) error {
	sctx, cancel := context.WithCancel(ctx)
	body, err := e.client.stream(sctx, path)
	if err != nil {
		cancel()
		return err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, body)
		if sctx.Err() == nil {
			ended.Add(1)
		}
	}()
	e.created.close(func() {
		cancel()
		_ = body.Close()
		<-done
	})
	return nil
}

// sandboxesRunning creates sandboxes of the configured profile in the
// first workspace until sandboxesRunningCount exist or the hub refuses
// one, then reads each and reports how many are provisioning or ready.
func sandboxesRunning(ctx context.Context, e *env) (outcome, error) {
	var (
		mu  sync.Mutex
		ids []string
	)
	n, re, err := fill(ctx, sandboxesRunningCount, func(ctx context.Context, _ int) error {
		id, err := e.sandbox(ctx)
		if err == nil {
			mu.Lock()
			ids = append(ids, id)
			mu.Unlock()
		}
		return err
	})
	if err != nil {
		return outcome{}, err
	}
	running := 0
	for _, id := range ids {
		state, _, err := e.sandboxState(ctx, id)
		if err != nil {
			return outcome{}, err
		}
		if state == "provisioning" || state == "ready" {
			running++
		}
	}
	var note string
	if running < n {
		note = fmt.Sprintf("%d of the %d sandboxes created were neither provisioning nor ready", n-running, n)
	}
	return countOutcome(running, stopDetail(n, re), note), nil
}
