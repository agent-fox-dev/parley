package fork

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/store"
)

// landedGrace is how long a fork whose work has landed or been abandoned
// stays after its last activity before the sweep removes it early.
const landedGrace = 24 * time.Hour

// expiryLanded is the reason of the early expiry of a fork whose work has
// landed.
const expiryLanded = "landed"

// The states of a change that leave nothing for its source fork to do.
const (
	changeLanded    = "landed"
	changeAbandoned = "abandoned"
)

// ActivityStep gives the fork records the instant of their last activity,
// last_activity_at in UnixNano. It is NULL until an activity is recorded,
// and the fork's created_at stands for it then, so that the create need not
// write it and a fork created before the step counts as last active at its
// creation. It applies after SchemaStep.
var ActivityStep = store.Step{
	Name:   "fork_activity",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE workspaces ADD COLUMN last_activity_at INTEGER`)
		return err
	},
}

// RecordActivity records, at the hub's time, an activity of the fork slug:
// a push or an API write to it, or a change whose source it is opened,
// versioned, landed, abandoned or restored. The day after which a fork
// whose work has landed is removed is counted from its last activity, and
// since every event that can start or end the conditions of that removal
// is an activity, the one instant is all the sweep needs to know how long
// they have held. A slug that names no fork changes nothing. It opens a
// transaction of the store, so it must not be called inside one.
func (c *Component) RecordActivity(ctx context.Context, slug string) error {
	return c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET last_activity_at = ? WHERE slug = ? AND kind = ?`,
			c.deps.Now().UnixNano(), slug, access.KindFork); err != nil {
			return fmt.Errorf("record an activity of fork %q: %w", slug, err)
		}
		return nil
	})
}

// expireLanded is the early expiry of a pass at now: it judges, in
// ascending slug order, every fork whose expires_at is after now and
// removes with the reason landed each one whose work has landed
// (workLanded), the way the pass removes an expired fork. A fork it cannot
// judge, because the change reader or the sandbox collaborator fails, is
// logged and kept, and the pass goes on with the next fork. The context is
// checked before each fork, as for the expired forks.
func (c *Component) expireLanded(ctx context.Context, now time.Time, res *PassResult) {
	slugs, err := c.liveForks(ctx, now)
	if err != nil {
		c.deps.Logger.Error("fork sweep: forks not read for their early expiry", "error", err)
		return
	}
	due := func(ctx context.Context, q access.Querier, f Fork) (bool, error) { return landedStill(ctx, q, f, now) }
	for _, slug := range slugs {
		if ctx.Err() != nil {
			res.Stopped = true
			return
		}
		landed, err := c.workLanded(ctx, slug, now)
		switch {
		case errors.Is(err, errGone), err == nil && !landed:
			continue
		case err != nil:
			c.deps.Logger.Error("fork sweep: early expiry of the fork not judged; the fork is kept", "slug", slug, "error", err)
			continue
		}
		c.expire(ctx, slug, removal{cause: causeExpired, expiry: expiryLanded, due: due}, res)
	}
}

// liveForks are the slugs of the forks whose expires_at is after now, in
// ascending order.
func (c *Component) liveForks(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := c.deps.Store.Main().QueryContext(ctx,
		`SELECT slug FROM workspaces WHERE kind = ? AND (expires_at IS NULL OR expires_at > ?) ORDER BY slug`,
		access.KindFork, now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	return slugs, rows.Err()
}

// workLanded reports whether the fork slug meets at now every condition of
// its early expiry: its last activity is at least landedGrace before now;
// it is the source of at least one change, and every such change is landed
// or abandoned; each of its own branches is the source branch of one of
// them, so that no work of it is left unproposed; and it has no sandbox in
// state provisioning or ready, which may still be working. Without a change
// reader a fork has no changes, and without a sandbox collaborator no
// sandboxes. A fork with no record is errGone.
func (c *Component) workLanded(ctx context.Context, slug string, now time.Time) (bool, error) {
	f, found, err := BySlug(ctx, c.deps.Store.Main(), slug)
	switch {
	case err != nil:
		return false, err
	case !found:
		return false, errGone
	}
	if idle, err := idleSince(ctx, c.deps.Store.Main(), slug, now); err != nil || !idle || c.changes == nil {
		return false, err
	}
	changes, err := c.changes.ChangesFrom(ctx, slug)
	if err != nil {
		return false, fmt.Errorf("read the changes of fork %q: %w", slug, err)
	}
	sources := make(map[string]bool, len(changes))
	for _, ch := range changes {
		if ch.State != changeLanded && ch.State != changeAbandoned {
			return false, nil
		}
		sources[ch.Branch] = true
	}
	if len(sources) == 0 {
		return false, nil
	}
	own, err := c.ownBranches(ctx, f)
	if err != nil {
		return false, err
	}
	for name := range own {
		if !sources[name] {
			return false, nil
		}
	}
	if c.sandboxes == nil {
		return true, nil
	}
	live, err := c.sandboxes.HasLiveSandbox(ctx, slug)
	if err != nil {
		return false, fmt.Errorf("read the sandboxes of fork %q: %w", slug, err)
	}
	return !live, nil
}

// landedStill is the rule by which the fork f, judged at now to have
// landed, is still due when its removal reads it again through q: its
// expires_at is still after now, since a fork past it goes with the reason
// ttl, and its last activity is still at least landedGrace before now.
// Every event that changes the other conditions is an activity, so a fork
// with no activity since it was judged meets them still.
func landedStill(ctx context.Context, q access.Querier, f Fork, now time.Time) (bool, error) {
	if f.ExpiresAt != nil && !f.ExpiresAt.After(now) {
		return false, nil
	}
	return idleSince(ctx, q, f.Slug, now)
}

// idleSince reports whether the last activity of the fork slug, read
// through q, is at least landedGrace before now: its last recorded
// activity, or its creation while none is recorded. A fork with no record
// is errGone.
func idleSince(ctx context.Context, q access.Querier, slug string, now time.Time) (bool, error) {
	var at int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(last_activity_at, created_at) FROM workspaces WHERE slug = ? AND kind = ?`,
		slug, access.KindFork).Scan(&at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, errGone
	case err != nil:
		return false, fmt.Errorf("read the last activity of fork %q: %w", slug, err)
	}
	return !time.Unix(0, at).Add(landedGrace).After(now), nil
}
