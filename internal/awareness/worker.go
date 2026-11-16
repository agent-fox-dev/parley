package awareness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
)

// batchSize is how many events the index worker reads at a time.
const batchSize = 100

// Run is the index worker: a pass every Deps.IndexInterval until ctx is
// done. A pass that fails is logged and its events are tried again by the
// next one.
func (c *Component) Run(ctx context.Context) {
	tick := time.NewTicker(c.deps.IndexInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := c.Pass(ctx); err != nil && ctx.Err() == nil {
			c.deps.Logger.Warn("the awareness index pass failed", "error", err)
		}
	}
}

// Pass applies every event the log holds after the worker's position and
// returns: each event the index follows in a transaction of its own that
// also stores the event's seq as the position, and, after each batch, the
// position the read examined, past the events the index skips. A pass
// whose transaction fails returns its error and leaves the position at the
// event before, so that the next pass applies that event again; one pass
// runs at a time.
func (c *Component) Pass(ctx context.Context) error {
	c.pass.Lock()
	defer c.pass.Unlock()
	if c.deps.Store == nil {
		return errNotBound
	}
	pos, err := c.Position(ctx)
	if err != nil {
		return err
	}
	for {
		recs, examined, err := c.deps.Events.ReadAfter(ctx, pos, batchSize)
		if expired, ok := errors.AsType[*event.PositionExpiredError](err); ok && expired.OldestSeq-1 > pos {
			// The events up to oldest_seq left the log before the worker
			// read them; it cannot get them back, and goes on with the
			// first event the log still holds rather than stall.
			c.deps.Logger.Warn("the event log no longer holds the events after the awareness index's position; going on from its oldest event",
				"position", pos, "oldest_seq", expired.OldestSeq)
			pos = expired.OldestSeq - 1
			continue
		}
		if err != nil {
			return err
		}
		for _, rec := range recs {
			done, err := c.process(ctx, rec)
			if err != nil {
				return err
			}
			if done {
				pos = rec.Seq
			}
		}
		if examined > pos {
			if err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return setPosition(ctx, tx, examined) }); err != nil {
				return err
			}
			pos = examined
		}
		if len(recs) < batchSize {
			return nil
		}
	}
}

// process applies the event rec, in a transaction that also stores its seq
// as the position, and reports whether it did; an event of a type the
// index does not follow, or of a change the reader no longer finds, such
// as one of a deleted workspace, is left for the position after the batch.
func (c *Component) process(ctx context.Context, rec event.Record) (bool, error) {
	u, ok, err := c.prepare(ctx, rec)
	if err != nil || !ok {
		return false, err
	}
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := c.apply(ctx, tx, u); err != nil {
			return err
		}
		if err := setPosition(ctx, tx, rec.Seq); err != nil {
			return err
		}
		if c.beforeCommit != nil {
			return c.beforeCommit()
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// prepare reads what the event rec does to the index, outside any
// transaction: the change it names, and the files of its current version
// for a version or a restore. A version whose files cannot be listed, its
// commit missing or git failing, is indexed with none, and the failure
// logged with the workspace and the number.
func (c *Component) prepare(ctx context.Context, rec event.Record) (update, bool, error) {
	if !follows(rec.Type) || rec.Workspace == nil || rec.ResourceType != "change" {
		return update{}, false, nil
	}
	number, err := strconv.ParseInt(rec.ResourceID, 10, 64)
	if err != nil {
		return update{}, false, nil
	}
	workspace := *rec.Workspace
	ch, err := c.deps.Changes.ReadChange(ctx, nil, workspace, number)
	if errors.Is(err, ErrChangeNotFound) {
		return update{}, false, nil
	}
	if err != nil {
		return update{}, false, err
	}
	ch.Workspace, ch.Number = workspace, number
	u := update{seq: rec.Seq, typ: rec.Type, change: ch}
	switch rec.Type {
	case eventVersion, eventRestore:
		u.files, err = c.listFiles(ctx, ch)
		if err != nil {
			if ctx.Err() != nil {
				return update{}, false, ctx.Err()
			}
			c.deps.Logger.Warn("cannot list the files of a change's version; the awareness index records it with none",
				"workspace", workspace, "number", number, "error", err)
			u.files = nil
		}
	case eventLand:
		u.landed = landedOf(ch, rec)
	}
	return u, true, nil
}

// landedOf is how the change ch of the land event rec landed: as the
// change record says, or, when it says nothing, as the event does.
func landedOf(ch Change, rec event.Record) Landed {
	if ch.Landed != nil {
		return *ch.Landed
	}
	var meta struct {
		LandedSHA string `json:"landed_sha"`
	}
	_ = json.Unmarshal(rec.Metadata, &meta)
	return Landed{SHA: meta.LandedSHA, At: rec.Time}
}
