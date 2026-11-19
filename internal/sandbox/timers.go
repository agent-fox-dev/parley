package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// LifecycleInterval is the time between the lifecycle loop's idle and
// expiry passes, on the hub's clock.
const LifecycleInterval = time.Minute

// liveStates are the states of a sandbox that has not been deleted.
var liveStates = []string{StateProvisioning, StateReady, StateStopped, StateCompleted, StateFailed}

// IdlePass stops, with reason StopIdle by the system, every ready sandbox
// with an idle timeout whose last activity is longer ago than that timeout,
// counted in whole seconds on the hub's clock. A stop the runtime fails
// leaves the sandbox ready for the next pass, and the pass goes on with
// the others; it returns an error only when the records cannot be read,
// and stops between two sandboxes once ctx ends.
func (s *Service) IdlePass(ctx context.Context) error {
	recs, err := ListByState(ctx, s.deps.Store.Main(), StateReady)
	if err != nil {
		return err
	}
	now := s.deps.Now().Truncate(time.Second)
	for _, rec := range recs {
		if ctx.Err() != nil {
			return nil
		}
		if rec.IdleTimeout <= 0 || now.Sub(rec.LastActivityAt) <= rec.IdleTimeout {
			continue
		}
		if _, err := s.Stop(ctx, rec, StopIdle, SystemActor); err != nil && ctx.Err() == nil {
			// The error is scrubbed of the sandbox's values by Stop.
			s.logger().Warn("the idle stop of a sandbox failed; the next pass tries again", "sandbox", rec.ID, "error", err.Error())
		}
	}
	return nil
}

// ExpiryPass deletes by the system every sandbox not deleted whose
// expires_at is at or before the hub's clock. One on a fork whose own
// expiry has come too, or on a workspace already removed, is deleted with
// reason DeleteWorkspaceDeleted and no checkpoint, since the workspace and
// its refs are going; every other with
// reason DeleteExpired and a checkpoint first when its profile asked for
// one. The deletion is recorded also when the runtime cannot be reached,
// which leaves the runtime's remains to the reconciliation. It returns an
// error only when the records cannot be read or a deletion cannot be
// recorded, after trying the others, and stops between two sandboxes once
// ctx ends.
func (s *Service) ExpiryPass(ctx context.Context) error {
	db := s.deps.Store.Main()
	recs, err := ListByState(ctx, db, liveStates...)
	if err != nil {
		return err
	}
	now := s.deps.Now()
	var errs []error
	for _, rec := range recs {
		if ctx.Err() != nil {
			return nil
		}
		if rec.ExpiresAt.After(now) {
			continue
		}
		reason, checkpoint := DeleteExpired, rec.Snapshot.Checkpoint
		gone, err := workspaceGone(ctx, db, rec.Workspace, now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if gone {
			reason, checkpoint = DeleteWorkspaceDeleted, false
		}
		if err := s.Delete(ctx, rec, reason, SystemActor, checkpoint); err != nil {
			errs = append(errs, fmt.Errorf("delete the expired sandbox %s: %w", rec.ID, err))
		}
	}
	return errors.Join(errs...)
}

// workspaceGone reports whether the workspace slug is removed, or is a fork
// whose expiry is at or before now.
func workspaceGone(ctx context.Context, q access.Querier, slug string, now time.Time) (bool, error) {
	w, found, err := workspace.BySlug(ctx, q, slug)
	if err != nil {
		return false, fmt.Errorf("read workspace %s: %w", slug, err)
	}
	return !found || (w.Kind == access.KindFork && w.ExpiresAt != nil && !w.ExpiresAt.After(now)), nil
}

// RunLifecycle runs the idle and expiry passes until ctx ends: both every
// LifecycleInterval, and the expiry pass also when a timer set to the
// earliest expires_at of a sandbox not deleted fires, all on the hub's
// clock. The timer is set again whenever the service tells of a change,
// which every transition does (a create's sandbox becoming ready, a start,
// a deletion) and a change of a ttl, so that a sandbox is deleted at its
// expires_at rather than up to a minute later. A sandbox whose expiry has
// come and that a pass could not delete is left to the next minute's pass,
// so that a failing deletion is not tried without pause. It returns at once
// while sandboxes are off, and when ctx ends, between two runtime calls of
// a pass at the latest.
func (s *Service) RunLifecycle(ctx context.Context) {
	if !s.Enabled() || s.client == nil || s.deps.Store == nil {
		return
	}
	tick := s.newTimer(LifecycleInterval)
	var (
		expiry Timer
		// armed is the expires_at expiry is set to; passed is the clock's
		// time at the last expiry pass, whose expired sandboxes the timer
		// leaves to the minute's pass.
		armed, passed time.Time
	)
	defer func() {
		tick.Stop()
		if expiry != nil {
			expiry.Stop()
		}
	}()
	pass := func(idle bool) {
		if idle {
			if err := s.IdlePass(ctx); err != nil && ctx.Err() == nil {
				s.logger().Warn("the idle pass could not read the sandboxes", "error", err.Error())
			}
		}
		passed = s.deps.Now()
		if err := s.ExpiryPass(ctx); err != nil && ctx.Err() == nil {
			s.logger().Warn("the expiry pass did not delete every expired sandbox", "error", err.Error())
		}
	}
	for {
		changed := s.changes.next()
		next, ok, err := earliestExpiry(ctx, s.deps.Store.Main(), passed)
		if err != nil && ctx.Err() == nil {
			s.logger().Warn("the lifecycle loop could not read the next expiry; the minute's pass covers it", "error", err.Error())
		}
		if expiry != nil && (!ok || !next.Equal(armed)) {
			expiry.Stop()
			expiry = nil
		}
		var due <-chan time.Time
		switch now := s.deps.Now(); {
		case ok && !next.After(now):
			// Already expired: a timer of the hub's clock would wait for
			// the clock to move on.
			pass(false)
			continue
		case ok && expiry == nil:
			expiry, armed = s.newTimer(next.Sub(now)), next
		}
		if expiry != nil {
			due = expiry.C()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C():
			tick = s.newTimer(LifecycleInterval)
			pass(true)
		case <-due:
			expiry = nil
			pass(false)
		case <-changed:
		}
	}
}

// earliestExpiry reads through q the earliest expires_at after after of a
// sandbox not deleted; ok is false when there is none.
func earliestExpiry(ctx context.Context, q access.Querier, after time.Time) (at time.Time, ok bool, err error) {
	var unix sql.NullInt64
	err = q.QueryRowContext(ctx, `SELECT min(expires_at) FROM sandboxes WHERE state <> ? AND expires_at > ?`,
		StateDeleted, after.Unix()).Scan(&unix)
	if err != nil || !unix.Valid {
		return time.Time{}, false, err
	}
	return time.Unix(unix.Int64, 0).UTC(), true, nil
}
