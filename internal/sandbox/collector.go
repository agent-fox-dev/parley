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

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

const (
	// PolicyCollectInterval is the default time between two passes of the
	// policy collector on the hub's clock: a sandbox that became ready is
	// followed from within it, and one that left ready no later than it.
	// It is also the poll interval of the runtime's pending proposals.
	PolicyCollectInterval = 5 * time.Second
	// PolicyRetryDelay is the default time the policy collector waits,
	// after the runtime could not be reached or a sandbox's policy log
	// broke, before it opens that log again.
	PolicyRetryDelay = 5 * time.Second
	// EventDeny is written for the first denial of each (program, host,
	// port) of a sandbox.
	EventDeny = "parley.sandbox.deny"
	// DenialsMetric counts every denial the runtime reports for a sandbox
	// the hub holds, first or repeated, once, though the runtime replays
	// the entries of a log opened again.
	DenialsMetric = "parley_sandbox_denials_total"
)

// actionDeny is the action of a policy log entry the runtime blocked.
const actionDeny = "deny"

// proposalsNotAllowed is the reason the runtime is given for a proposal of
// a sandbox whose profile did not allow proposals when it was created.
const proposalsNotAllowed = "the sandbox's profile does not allow proposals"

// PolicyCollectorOptions configure a policy collector; a zero value takes
// the defaults.
type PolicyCollectorOptions struct {
	// Interval is the time between two passes of Run on the hub's clock,
	// and the least time between two listings of the runtime's pending
	// proposals on the collector's clock; 0 for PolicyCollectInterval.
	Interval time.Duration
	// RetryDelay is the time after a runtime failure before a sandbox's
	// policy log is opened again; 0 for PolicyRetryDelay.
	RetryDelay time.Duration
	// Now is the clock RetryDelay and the poll interval of Pass are counted
	// on; nil for the hub's clock.
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
// denial a first one, and the position stored with each denial is how far
// the sandbox's log was taken, so a collector built on the same store, as
// after a restart, or one whose sandbox is ready again, reports and counts
// none of them again.
//
// It also lists the runtime's pending proposals, at most once per poll
// interval, and stores each new proposal of a sandbox whose profile
// allowed proposals when it was created as open, with its
// parley.sandbox.proposal event; a proposal of a sandbox whose profile did
// not is rejected at the runtime. The stored runtime ids are what makes a
// proposal new, so a collector built on the same store stores none twice.
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
	// polled is when the pending proposals were last listed, on the
	// collector's clock; guarded by pass.
	polled time.Time

	// mu guards what the followers write: how far each followed sandbox's
	// log has been taken, the entries of other actions than deny included,
	// which the store does not keep (resume), when a log that failed may be
	// opened again (retryAt), and the sandboxes whose runtime answered that
	// it keeps no policy log, which is logged once per sandbox
	// (unsupported).
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
// that no denial is counted twice.
type logPoint struct {
	at time.Time
	n  int
}

// after is the point once the entry at at that follows p was taken.
func (p logPoint) after(at time.Time) logPoint {
	switch {
	case at.After(p.at):
		return logPoint{at: at, n: 1}
	case at.Equal(p.at):
		p.n++
	}
	return p
}

// storedPoint reads, through q, how far the policy log of the sandbox id
// was taken when its last denial was reported; the zero point, the log's
// start, when none was.
func storedPoint(ctx context.Context, q access.Querier, id string) (logPoint, error) {
	var at, n int64
	err := q.QueryRowContext(ctx, `SELECT taken_at, taken FROM sandbox_policy_log WHERE sandbox_id = ?`, id).Scan(&at, &n)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return logPoint{}, nil
	case err != nil:
		return logPoint{}, fmt.Errorf("read how far the policy log of sandbox %s was taken: %w", id, err)
	}
	return logPoint{at: time.Unix(0, at), n: int(n)}, nil
}

// storePoint stores p in tx as how far the policy log of the sandbox id
// was taken.
func storePoint(ctx context.Context, tx *sql.Tx, id string, p logPoint) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO sandbox_policy_log (sandbox_id, taken_at, taken) VALUES (?, ?, ?)
ON CONFLICT (sandbox_id) DO UPDATE SET taken_at = excluded.taken_at, taken = excluded.taken`, id, p.at.UnixNano(), p.n)
	if err != nil {
		return fmt.Errorf("store how far the policy log of sandbox %s was taken: %w", id, err)
	}
	return nil
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
// runtime failure never ends it. Every pass but the first lists the
// pending proposals: the first listing comes one interval after the start,
// so that a hub's start calls the runtime for nothing besides its check.
func (c *PolicyCollector) Run(ctx context.Context) {
	defer c.Stop()
	for poll := false; ; poll = true {
		// The timer of the next pass is asked for before this one runs, so
		// that a pass that takes long does not push the next one back.
		t := c.s.newTimer(c.interval)
		c.pass.Lock()
		c.passLocked(ctx, poll)
		c.pass.Unlock()
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
// again before the retry delay has passed on the collector's clock. Then,
// unless they were listed less than the poll interval before on the
// collector's clock, it takes the runtime's pending proposals. A failure is
// logged and never returned. With sandboxes off it does nothing.
func (c *PolicyCollector) Pass(ctx context.Context) {
	c.pass.Lock()
	defer c.pass.Unlock()
	c.passLocked(ctx, c.polled.IsZero() || !c.now().Before(c.polled.Add(c.interval)))
}

// passLocked is a pass that lists the pending proposals when poll is true.
// The caller holds c.pass.
func (c *PolicyCollector) passLocked(ctx context.Context, poll bool) {
	if ctx.Err() != nil || c.s.client == nil || c.s.deps.Store == nil {
		return
	}
	c.followDenials(ctx)
	if poll {
		c.polled = c.now()
		c.takeProposals(ctx)
	}
}

// takeProposals lists the runtime's pending proposals and takes each one
// (takeProposal). A listing that fails is logged and ends the part of the
// pass, with no record changed: the next listing comes a poll interval
// later.
func (c *PolicyCollector) takeProposals(ctx context.Context) {
	list, err := c.s.client.ListProposals(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.s.logger().Warn("the policy collector cannot list the pending proposals", "retry_in", c.interval.String(), "error", c.s.scrub(err.Error()))
		}
		return
	}
	for _, p := range list {
		if ctx.Err() != nil {
			return
		}
		if err := c.s.takeProposal(ctx, p); err != nil && ctx.Err() == nil {
			text := c.s.scrub(c.s.ScrubSandbox(context.WithoutCancel(ctx), p.Sandbox, err.Error()))
			c.s.logger().Warn("the policy collector cannot take a proposal", "sandbox_id", p.Sandbox, "proposal", p.ID, "error", text)
		}
	}
}

// takeProposal takes the pending proposal p of the runtime. A proposal of a
// sandbox the hub holds no live record of is left alone, for the runtime to
// drop with its sandbox. One of a sandbox whose profile did not allow
// proposals when it was created is rejected at the runtime, and nothing is
// stored. Otherwise a proposal not stored yet for its sandbox is stored as
// open, and its parley.sandbox.proposal event written, in one transaction,
// which reads the record again so that a sandbox deleted meanwhile, whose
// open proposals the delete hook withdrew, gets no open one.
func (s *Service) takeProposal(ctx context.Context, p runtime.Proposal) error {
	rec, err := Get(ctx, s.deps.Store.Main(), p.Sandbox)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case rec.State == StateDeleted:
		return nil
	case !rec.Snapshot.AllowProposals:
		if err := s.client.RejectProposal(ctx, p.Sandbox, p.ID, proposalsNotAllowed); err != nil {
			return fmt.Errorf("reject the proposal: %w", err)
		}
		return nil
	}
	if stored, err := proposalStored(ctx, s.deps.Store.Main(), p.Sandbox, p.ID); err != nil || stored {
		return err
	}
	now := time.Now()
	if s.deps.Now != nil {
		now = s.deps.Now()
	}
	// The summary is the runtime's text about what the sandbox did, which
	// may quote one of its values: it is stored scrubbed, so that the
	// event, the proposal objects and a workspace rule promoted from it
	// show it scrubbed. The rule is stored as the runtime gave it, since
	// the hub hands it back to the runtime unchanged.
	stored := Proposal{
		ID: NewProposalID(), SandboxID: p.Sandbox, RuntimeID: p.ID, Rule: p.Rule.Body, Summary: s.ScrubSandbox(ctx, p.Sandbox, p.Summary),
		Findings: p.Findings, State: ProposalOpen, CreatedAt: now,
	}
	return s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		rec, err := Get(ctx, tx, p.Sandbox)
		if errors.Is(err, ErrNotFound) || (err == nil && rec.State == StateDeleted) {
			return nil
		}
		if err != nil {
			return err
		}
		if inserted, err := InsertProposal(ctx, tx, stored); err != nil || !inserted {
			return err
		}
		_, err = s.deps.Events.Emit(ctx, tx, event.Input{
			Type: EventProposal, ActorType: event.ActorSystem,
			ResourceType: "sandbox", ResourceID: rec.ID, Workspace: &rec.Workspace, SessionID: rec.SessionID,
			Metadata: map[string]any{"proposal_id": stored.ID, "summary": stored.Summary, "max_severity": MaxSeverity(stored.Findings)},
		})
		if err != nil {
			return fmt.Errorf("event of sandbox %s: %w", rec.ID, err)
		}
		return nil
	})
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
	// follower, so that the collector keeps nothing of the sandboxes gone;
	// the store keeps how far its log was taken.
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
// stream ends: it opens the log from where it was taken up to, as this
// collector followed it or else as the store says, and leaves out the
// entries taken before.
func (c *PolicyCollector) follow(ctx context.Context, rec Record) error {
	c.mu.Lock()
	from, ok := c.resume[rec.ID]
	c.mu.Unlock()
	if !ok {
		var err error
		if from, err = storedPoint(ctx, c.s.deps.Store.Main(), rec.ID); err != nil {
			return err
		}
	}
	stream, err := c.s.client.Logs(ctx, rec.ID, runtime.LogQuery{Source: runtime.SourcePolicy, Since: from.at, Follow: true})
	if err != nil {
		return err
	}
	defer stream.Close()
	skip, point := from.n, from
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
		point = point.after(e.Time)
		if e.Action == actionDeny {
			// A denial whose texts cannot be scrubbed is not taken: it is
			// taken again after the retry delay, once the values may be
			// known, rather than stored as a destination that would stand
			// for every other one.
			v, err := c.s.ValuesOf(ctx, rec.ID)
			if err != nil {
				return fmt.Errorf("the values the denials of the sandbox are scrubbed of: %w", err)
			}
			// Counted before it is stored: an entry whose storing fails is
			// taken again after the retry delay, and an event is never
			// lost, at the price of counting that entry twice.
			_ = c.s.deps.Recorder.Add(DenialsMetric, nil, 1)
			if err := c.s.reportDenial(ctx, rec, v, e, point); err != nil {
				return err
			}
		}
		c.mu.Lock()
		c.resume[rec.ID] = point
		c.mu.Unlock()
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
// parley.sandbox.deny event, and stores taken as how far the sandbox's log
// was taken, all in one transaction. Its texts pass through Scrub with the
// sandbox's values v, the stored destination's too, so that no exposed
// value is stored; a method and a path the entry does not have, as for a
// blocked connection, are null.
func (s *Service) reportDenial(ctx context.Context, rec Record, v Values, e runtime.LogEntry, taken logPoint) error {
	clean := func(text string) string { return Scrub(v, text) }
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
		if err := storePoint(ctx, tx, rec.ID, taken); err != nil {
			return err
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
