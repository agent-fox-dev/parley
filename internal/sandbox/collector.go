package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

const (
	// PolicyCollectInterval is the default time between two passes of the
	// policy collector on the hub's clock: a sandbox that became ready is
	// followed from within it, and one that left ready no later than it.
	PolicyCollectInterval = 5 * time.Second
	// PolicyRetryDelay is the default time the policy collector waits,
	// after the runtime could not be reached or a sandbox's policy log
	// broke, before it opens that log again.
	PolicyRetryDelay = 5 * time.Second
	// EventDeny is written for the first denial of each (program, host,
	// port) of a sandbox.
	EventDeny = "parley.sandbox.deny"
	// DenialsMetric counts every denial the runtime reports for a sandbox
	// the hub holds, first or repeated.
	DenialsMetric = "parley_sandbox_denials_total"
)

// actionDeny is the action of a policy log entry the runtime blocked.
const actionDeny = "deny"

// PolicyCollectorOptions configure a policy collector; a zero value takes
// the defaults.
type PolicyCollectorOptions struct {
	// Interval is the time between two passes of Run on the hub's clock;
	// 0 for PolicyCollectInterval.
	Interval time.Duration
	// RetryDelay is the time after a runtime failure before a sandbox's
	// policy log is opened again; 0 for PolicyRetryDelay.
	RetryDelay time.Duration
	// Now is the clock RetryDelay is counted on; nil for the hub's clock.
	Now func() time.Time
	// Hold keeps CollectPolicy from running a collector, for a test that
	// drives the passes of a collector of its own.
	Hold bool
}

// PolicyCollector takes what the runtime reports of the network policy of
// the sandboxes the hub holds. It follows the policy log of every ready
// sandbox and reports each denial: every one is counted in DenialsMetric,
// and the first of each (program, host, port) of a sandbox is stored with
// its parley.sandbox.deny event. The destinations stored are what makes a
// denial a first one, so a collector built on the same store, as after a
// restart, reports none of them again.
//
// A pass (Pass) is driven by Run on the hub's clock, or by a test. Its
// followers read their streams on goroutines of their own, until the
// context of the pass that started them ends, their sandbox leaves ready
// or Stop is called.
type PolicyCollector struct {
	s        *Service
	interval time.Duration
	retry    time.Duration
	now      func() time.Time

	// pass gives the passes and Stop their turns; it guards followers, the
	// follower of each followed sandbox by id.
	pass      sync.Mutex
	followers map[string]*policyFollower

	// mu guards what the followers write: how far each sandbox's log has
	// been taken (resume), when a log that failed may be opened again
	// (retryAt), and the sandboxes whose runtime answered that it keeps no
	// policy log, which is logged once per sandbox (unsupported).
	mu          sync.Mutex
	resume      map[string]logPoint
	retryAt     map[string]time.Time
	unsupported map[string]bool
}

// policyFollower is the follower of one sandbox's policy log: cancel stops
// it, and done is closed once it has stopped.
type policyFollower struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// logPoint is how far a policy log has been taken: every entry before at,
// and the first n entries at at. A followed log that is opened again
// replays the entries from at, which are left out up to that point, so
// that no denial is counted twice by one collector.
type logPoint struct {
	at time.Time
	n  int
}

// NewPolicyCollector returns a policy collector of the service with the
// options o.
func (s *Service) NewPolicyCollector(o PolicyCollectorOptions) *PolicyCollector {
	c := &PolicyCollector{
		s: s, interval: o.Interval, retry: o.RetryDelay, now: o.Now,
		followers: map[string]*policyFollower{}, resume: map[string]logPoint{},
		retryAt: map[string]time.Time{}, unsupported: map[string]bool{},
	}
	if c.interval <= 0 {
		c.interval = PolicyCollectInterval
	}
	if c.retry <= 0 {
		c.retry = PolicyRetryDelay
	}
	if c.now == nil {
		c.now = func() time.Time {
			if s.deps.Now == nil {
				return time.Now()
			}
			return s.deps.Now()
		}
	}
	return c
}

// CollectPolicy runs a policy collector with the options o until ctx ends
// and returns once its followers have stopped. With sandboxes off, or with
// o.Hold, it returns at once.
func (s *Service) CollectPolicy(ctx context.Context, o PolicyCollectorOptions) {
	if o.Hold || s.client == nil || s.deps.Store == nil {
		return
	}
	s.NewPolicyCollector(o).Run(ctx)
}

// Run runs a pass right away and then one every interval on the hub's
// clock until ctx ends, and returns once every follower has stopped. A
// runtime failure never ends it.
func (c *PolicyCollector) Run(ctx context.Context) {
	defer c.Stop()
	for {
		// The timer of the next pass is asked for before this one runs, so
		// that a pass that takes long does not push the next one back.
		t := c.s.newTimer(c.interval)
		c.Pass(ctx)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
		t.Stop()
	}
}

// Pass is one pass of the collector. It reads the sandboxes in state ready,
// stops the followers of the others and starts one for each ready sandbox
// that has none, under ctx; a sandbox whose log failed is not followed
// again before the retry delay has passed on the collector's clock. A
// failure is logged and never returned. With sandboxes off it does nothing.
func (c *PolicyCollector) Pass(ctx context.Context) {
	c.pass.Lock()
	defer c.pass.Unlock()
	if ctx.Err() != nil || c.s.client == nil || c.s.deps.Store == nil {
		return
	}
	c.followDenials(ctx)
}

// Stop stops every follower and returns once they have stopped.
func (c *PolicyCollector) Stop() {
	c.pass.Lock()
	defer c.pass.Unlock()
	for _, f := range c.followers {
		f.cancel()
	}
	for id, f := range c.followers {
		<-f.done
		delete(c.followers, id)
	}
}

// following returns the ids of the sandboxes whose follower has not
// stopped, sorted.
func (c *PolicyCollector) following() []string {
	c.pass.Lock()
	defer c.pass.Unlock()
	var out []string
	for id, f := range c.followers {
		select {
		case <-f.done:
		default:
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// followDenials has the followers of the pass match the ready sandboxes.
// The caller holds c.pass.
func (c *PolicyCollector) followDenials(ctx context.Context) {
	recs, err := ListByState(ctx, c.s.deps.Store.Main(), StateReady)
	if err != nil {
		if ctx.Err() == nil {
			c.s.logger().Warn("the policy collector cannot read the ready sandboxes", "error", c.s.scrub(err.Error()))
		}
		return
	}
	ready := make(map[string]bool, len(recs))
	for _, rec := range recs {
		ready[rec.ID] = true
	}
	for id, f := range c.followers {
		select {
		case <-f.done:
			delete(c.followers, id)
			continue
		default:
		}
		if !ready[id] {
			// Waited for, so that a sandbox that is ready again by the next
			// pass never has two followers.
			f.cancel()
			<-f.done
			delete(c.followers, id)
		}
	}
	// What is kept of a sandbox that left ready is dropped with its
	// follower, so that the collector keeps nothing of the sandboxes gone.
	now := c.now()
	waiting := map[string]bool{}
	c.mu.Lock()
	for id := range c.resume {
		if !ready[id] {
			delete(c.resume, id)
		}
	}
	for id := range c.unsupported {
		if !ready[id] {
			delete(c.unsupported, id)
		}
	}
	for id, at := range c.retryAt {
		if ready[id] && now.Before(at) {
			waiting[id] = true
		} else {
			delete(c.retryAt, id)
		}
	}
	c.mu.Unlock()
	for _, rec := range recs {
		if c.followers[rec.ID] != nil || waiting[rec.ID] {
			continue
		}
		fctx, cancel := context.WithCancel(ctx)
		f := &policyFollower{cancel: cancel, done: make(chan struct{})}
		c.followers[rec.ID] = f
		go func() {
			defer close(f.done)
			defer cancel()
			c.ended(fctx, rec.ID, c.follow(fctx, rec))
		}()
	}
}

// follow reports the denials of the policy log of the sandbox rec until the
// stream ends: it opens the log from where this collector took it up to
// and leaves out the entries it took before.
func (c *PolicyCollector) follow(ctx context.Context, rec Record) error {
	c.mu.Lock()
	from := c.resume[rec.ID]
	c.mu.Unlock()
	stream, err := c.s.client.Logs(ctx, rec.ID, runtime.LogQuery{Source: runtime.SourcePolicy, Since: from.at, Follow: true})
	if err != nil {
		return err
	}
	defer stream.Close()
	skip := from.n
	for {
		e, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if e.Time.IsZero() {
			e.Time = c.now()
		}
		if e.Time.Before(from.at) {
			continue
		}
		if e.Time.Equal(from.at) && skip > 0 {
			skip--
			continue
		}
		if e.Action == actionDeny {
			// Counted before it is stored: an entry whose storing fails is
			// taken again after the retry delay, and an event is never
			// lost, at the price of counting that entry twice.
			_ = c.s.deps.Recorder.Add(DenialsMetric, nil, 1)
			if err := c.s.reportDenial(ctx, rec, e); err != nil {
				return err
			}
		}
		c.taken(rec.ID, e.Time)
	}
}

// taken records that the entry at at of the sandbox id's log was taken.
func (c *PolicyCollector) taken(id string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.resume[id]
	switch {
	case at.After(p.at):
		c.resume[id] = logPoint{at: at, n: 1}
	case at.Equal(p.at):
		p.n++
		c.resume[id] = p
	}
}

// ended handles the end of the follower of the sandbox id with err: a
// failure while the follower was meant to run is logged, and the log is not
// opened again before the retry delay.
func (c *PolicyCollector) ended(ctx context.Context, id string, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	c.retryAt[id] = c.now().Add(c.retry)
	logged := false
	if errors.Is(err, runtime.ErrUnsupported) {
		logged = c.unsupported[id]
		c.unsupported[id] = true
	}
	c.mu.Unlock()
	if logged {
		return
	}
	text := c.s.scrub(c.s.ScrubSandbox(context.WithoutCancel(ctx), id, err.Error()))
	c.s.logger().Warn("stopped reading a sandbox's policy log", "sandbox_id", id, "retry_in", c.retry.String(), "error", text)
}

// reportDenial stores the destination of the denial e of the sandbox rec
// and, when the sandbox had not been reported denied it, writes its
// parley.sandbox.deny event in the same transaction. Its texts pass
// through the sandbox's scrubbing, the stored destination's too, so that
// no exposed value is stored; a method and a path the entry does not have,
// as for a blocked connection, are null.
func (s *Service) reportDenial(ctx context.Context, rec Record, e runtime.LogEntry) error {
	clean := func(text string) string { return s.ScrubSandbox(ctx, rec.ID, text) }
	program, host := clean(e.Program), clean(e.Host)
	meta := map[string]any{"program": program, "host": host, "port": e.Port, "method": nil, "path": nil}
	if e.Method != "" {
		meta["method"] = clean(e.Method)
	}
	if e.Path != "" {
		meta["path"] = clean(e.Path)
	}
	return s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO sandbox_denials (sandbox_id, program, host, port) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			rec.ID, program, host, e.Port)
		if err != nil {
			return fmt.Errorf("store a denial of sandbox %s: %w", rec.ID, err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		_, err = s.deps.Events.Emit(ctx, tx, event.Input{
			Type: EventDeny, ActorType: event.ActorSystem,
			ResourceType: "sandbox", ResourceID: rec.ID, Workspace: &rec.Workspace, SessionID: rec.SessionID,
			Metadata: meta,
		})
		if err != nil {
			return fmt.Errorf("event of sandbox %s: %w", rec.ID, err)
		}
		return nil
	})
}
