package land

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// DefaultAutoLandInterval is how often the automatic lander reads the
// event log and starts the lands that are due, when the hub names no
// interval.
const DefaultAutoLandInterval = time.Second

// autoLandBatch is how many events one read of the event log takes.
const autoLandBatch = 500

// An automatic land that ends failed is tried again, up to autoLandRetries
// times after the last attempt that did not end failed: the first retry
// firstRetryDelay after the failure, each further one after twice the
// delay before it, at most lastRetryDelay. A transient failure is retried
// without anyone watching, and not forever.
const (
	autoLandRetries = 10
	firstRetryDelay = 2 * time.Second
	lastRetryDelay  = 300 * time.Second
)

// AutoLandStep creates the store schema of the automatic lander: one row
// per change whose auto_land is on, with the count of the retries of its
// automatic land, the time the next start of it is due, NULL while none
// is, and the number of the change below it that held its start back
// while it was not landed, NULL while none did; and the position in the
// event log up to which the announcements of landable changes have been
// handled. Who set the flag is not kept here: the change record keeps it
// beside the flag.
var AutoLandStep = store.Step{
	Name:   "land_auto_land",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		for _, ddl := range []string{
			`CREATE TABLE land_auto_land (
	workspace_slug TEXT NOT NULL,
	number INTEGER NOT NULL,
	retries INTEGER NOT NULL DEFAULT 0,
	due_at INTEGER,
	held_by INTEGER,
	PRIMARY KEY (workspace_slug, number)
)`,
			`CREATE INDEX land_auto_land_due ON land_auto_land (due_at) WHERE due_at IS NOT NULL`,
			`CREATE TABLE land_auto_land_position (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	seq INTEGER NOT NULL
)`,
		} {
			if _, err := tx.ExecContext(ctx, ddl); err != nil {
				return err
			}
		}
		return nil
	},
}

// Starter is the land component as the automatic lander uses it: it starts
// a land no request asks for, and says whether a running attempt covers a
// change.
type Starter interface {
	Start(ctx context.Context, in StartInput) (Attempt, error)
	Running(ctx context.Context, workspace string, number int64) (bool, error)
}

// AutoLandChanges are the change records as the automatic lander reads
// them, the setter of a change's auto_land, which the change record keeps,
// the flag, which the lander clears, and the announcement of a landable
// change, which the lander has made again for a change it held back.
type AutoLandChanges interface {
	ReadChange(ctx context.Context, q change.Querier, workspace string, number int64) (change.Change, error)
	AutoLandSetter(ctx context.Context, q change.Querier, workspace string, number int64) (change.AutoLandSet, bool, error)
	SetAutoLand(ctx context.Context, tx *sql.Tx, workspace string, number int64, value bool, caller change.Caller) error
	OpenAutoLand(ctx context.Context, q change.Querier) ([]change.Key, error)
	ForgetLandable(ctx context.Context, tx *sql.Tx, workspace string, number int64) error
	AnnounceLandable(ctx context.Context, tx *sql.Tx, workspace string, number int64) error
}

// EventLog is the hub's event log, which the automatic lander follows by
// position (docs/adr/05-deliver-events-by-reading-the-log-by-position.md) and writes its
// refusals to.
type EventLog interface {
	ReadAfter(ctx context.Context, after int64, limit int) ([]event.Record, int64, error)
	Bounds(ctx context.Context) (event.Bounds, error)
	Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error)
}

// Users says who a user is: their username, and whether they may act at
// all, false for a user who was deleted or is blocked.
type Users interface {
	User(ctx context.Context, id string) (username string, active bool, err error)
}

// AutoLandDeps are what the automatic lander needs. Every member but the
// clock, the interval and the logger is required.
type AutoLandDeps struct {
	// Store holds the lander's rows, and the transactions of its refusals.
	Store *store.Store
	// Lands starts the lands; in the hub, the land component.
	Lands Starter
	// Changes reads the changes and clears their flag.
	Changes AutoLandChanges
	// Events is the event log followed and written to.
	Events EventLog
	// Users, Access and Workspaces decide whether the setter of a flag may
	// land its change now, as the land route decides for its caller.
	Users      Users
	Access     AccessRule
	Workspaces WorkspaceReader
	// Repos opens the repository of a workspace, which holds every
	// version of its changes, for the path limits of a flag.
	Repos func(ctx context.Context, slug string) (*gitrepo.Repo, error)
	// Now is the clock of the due times; nil is time.Now.
	Now func() time.Time
	// After is the clock the lander waits on between two runs; nil is
	// time.After.
	After func(time.Duration) <-chan time.Time
	// Interval is the time between two runs; 0 is
	// DefaultAutoLandInterval.
	Interval time.Duration
	// Logger is told of what fails; nil discards it.
	Logger *slog.Logger
}

// AutoLander starts an automatic land, an attempt with trigger auto_land,
// for an open change whose auto_land is on once it is landable: when the
// change component announces it landable (parley.change.ready, the one
// signal of it, read from the event log after a stored position, so that
// every announcement is handled once and none is lost over a restart),
// when the flag is turned on for a change that is landable already, and at
// the start of the hub. It starts the land as the user who set the flag,
// held to the path limits of the credential they set it with. Its zero
// value is ready to be registered as the change component's auto_land
// guard; it runs once Bind has given it its dependencies.
type AutoLander struct {
	deps AutoLandDeps
	// bound is set once Bind has given the lander its dependencies: the
	// land component may tell its final observers of an attempt it
	// recovered before the lander is bound.
	bound atomic.Bool
	// mu makes one run of the lander at a time, so that an announcement or
	// a due start is handled once, whoever asks for the run.
	mu sync.Mutex
}

// Bind gives the lander its dependencies, the defaults in place of the
// members d leaves zero.
func (l *AutoLander) Bind(d AutoLandDeps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.After == nil {
		d.After = time.After
	}
	if d.Interval <= 0 {
		d.Interval = DefaultAutoLandInterval
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	l.deps = d
	l.bound.Store(true)
}

// CheckAutoLand is the change component's auto_land guard, asked in tx,
// the transaction of the open or the edit, before the flag is stored. A
// path-scoped caller may turn the flag on only for a change whose current
// version has every file within the caller's paths, since the land it
// asks for writes those files to the target; otherwise the request is
// refused 403 path_not_allowed and nothing of it is applied. Turning the
// flag off is never refused and forgets the change's retries.
//
// Turning it on for a change whose flag was off makes a start of its land
// due now, in tx, so that a change that is landable already, and will not
// be announced again, has its land started by the next run once the
// request committed; the start is gone with tx when the request fails.
func (l *AutoLander) CheckAutoLand(ctx context.Context, tx *sql.Tx, caller change.Caller, ch change.Change, value bool) error {
	if !value {
		return forgetAutoLand(ctx, tx, ch.WorkspaceSlug, ch.Number)
	}
	if caller.Paths != nil {
		repo, err := l.deps.Repos(ctx, ch.WorkspaceSlug)
		if err != nil {
			return err
		}
		refused, err := CheckPaths(ctx, repo, caller.Paths, baseOf(ch), ch.HeadSHA)
		if err != nil {
			return err
		}
		if refused != "" {
			return pathNotAllowed(refused)
		}
	}
	if ch.AutoLand {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO land_auto_land (workspace_slug, number, retries, due_at) VALUES (?, ?, 0, ?)
ON CONFLICT (workspace_slug, number) DO UPDATE SET retries = 0, due_at = excluded.due_at`,
		ch.WorkspaceSlug, ch.Number, l.now().UnixNano())
	if err != nil {
		return fmt.Errorf("make the automatic land of %s due: %w", ch.Ref, err)
	}
	return nil
}

// Recover runs at the start of the hub, once the land component has
// handed the attempts a stop left running to its pool: it stores the end
// of the event log as the lander's position when none is stored yet,
// makes the starts that came due while the hub was stopped, retries
// among them, with the retry counts stored, and runs the start procedure
// for every other open change whose flag is on, so that a change that
// became landable while the hub was stopped, or was announced before the
// lander first ran, is not left waiting for an announcement that does not
// come. A start due later is left to the loop (Run), at its time.
func (l *AutoLander) Recover(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.position(ctx); err != nil {
		return err
	}
	if err := l.startDue(ctx); err != nil {
		return err
	}
	return l.scan(ctx)
}

// Run runs the lander until ctx ends: it follows the event log (Pass) and
// starts the lands that are due, every Interval, and at the time the
// earliest start is due when that comes first, so that a retry starts at
// its due time. It is the hub's one loop of pending starts, its
// background work, off the request path; it returns once ctx ended and
// leaves the due times stored, for the next start of the hub.
func (l *AutoLander) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.deps.After(l.wait(ctx)):
		}
		err := errors.Join(l.Pass(ctx), l.runDue(ctx))
		if err != nil && ctx.Err() == nil {
			l.deps.Logger.Warn("automatic land: a run failed", "error", err)
		}
	}
}

// wait is how long the loop waits before its next run: Interval, or the
// time until the earliest start is due when that is sooner, none for a
// start due already. A start made due while it waits, by a failed
// attempt, is seen at the latest an Interval later, and then waited for
// to its time.
func (l *AutoLander) wait(ctx context.Context) time.Duration {
	var next sql.NullInt64
	err := l.deps.Store.Main().QueryRowContext(ctx, `SELECT MIN(due_at) FROM land_auto_land`).Scan(&next)
	if err != nil {
		if ctx.Err() == nil {
			l.deps.Logger.Warn("automatic land: the due starts were not read", "error", err)
		}
		return l.deps.Interval
	}
	if !next.Valid {
		return l.deps.Interval
	}
	return min(l.deps.Interval, max(time.Duration(next.Int64-l.now().UnixNano()), 0))
}

// Pass reads the events after the stored position, runs the start
// procedure for the change of each parley.change.ready among them, and
// stores the position the read reached. The position of an event whose
// procedure wrote something is stored with what it wrote, or, for a start,
// right after it, so that an event starts at most one land. An event whose
// procedure failed is passed once its start is made due again (consider),
// since the change is announced once per landable head and would not be
// again; when not even that could be stored, the pass stops before the
// event, to read it again. A position whose events have left the log
// unread is moved to where the log now begins, and every open change whose
// flag is on is looked at, since the lost events may have been about any
// of them.
func (l *AutoLander) Pass(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	pos, err := l.position(ctx)
	if err != nil {
		return err
	}
	for {
		recs, examined, err := l.deps.Events.ReadAfter(ctx, pos, autoLandBatch)
		var expired *event.PositionExpiredError
		if errors.As(err, &expired) {
			pos = expired.OldestSeq - 1
			if err := l.storePositionTx(ctx, pos); err != nil {
				return err
			}
			if err := l.scan(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		for _, rec := range recs {
			if rec.Type != change.EventReady || rec.Workspace == nil {
				continue
			}
			number, err := strconv.ParseInt(rec.ResourceID, 10, 64)
			if err != nil {
				continue
			}
			if err := l.consider(ctx, *rec.Workspace, number, rec.Seq, false); err != nil {
				return errors.Join(err, l.storePositionTx(ctx, rec.Seq-1))
			}
		}
		if examined > pos {
			if err := l.storePositionTx(ctx, examined); err != nil {
				return err
			}
			pos = examined
		}
		if len(recs) < autoLandBatch {
			return nil
		}
	}
}

// runDue runs the start procedure for every change whose start is due by
// now (startDue).
func (l *AutoLander) runDue(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.startDue(ctx)
}

// startDue runs the start procedure for every change whose start is due
// by now, each due time cleared first, so that a start that fails is not
// tried again at every run. The caller holds mu.
func (l *AutoLander) startDue(ctx context.Context) error {
	now := l.now().UnixNano()
	rows, err := l.deps.Store.Main().QueryContext(ctx, `SELECT workspace_slug, number FROM land_auto_land
WHERE due_at IS NOT NULL AND due_at <= ? ORDER BY due_at, workspace_slug, number`, now)
	if err != nil {
		return fmt.Errorf("read the due automatic lands: %w", err)
	}
	var due []change.Key
	for rows.Next() {
		var k change.Key
		if err := rows.Scan(&k.WorkspaceSlug, &k.Number); err != nil {
			rows.Close()
			return err
		}
		due = append(due, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, k := range due {
		err := l.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE land_auto_land SET due_at = NULL
WHERE workspace_slug = ? AND number = ? AND due_at <= ?`, k.WorkspaceSlug, k.Number, now)
			return err
		})
		if err != nil {
			return fmt.Errorf("clear the due time of %s: %w", change.Ref(k.WorkspaceSlug, k.Number), err)
		}
		errs = append(errs, l.consider(ctx, k.WorkspaceSlug, k.Number, 0, true))
	}
	return errors.Join(errs...)
}

// scan runs the start procedure for every open change whose flag is on.
func (l *AutoLander) scan(ctx context.Context) error {
	keys, err := l.deps.Changes.OpenAutoLand(ctx, nil)
	if err != nil {
		return err
	}
	var errs []error
	for _, k := range keys {
		errs = append(errs, l.consider(ctx, k.WorkspaceSlug, k.Number, 0, false))
	}
	return errors.Join(errs...)
}

// consider runs the start procedure for the change number of the
// workspace, for the event at seq (0 for none) or a start that came due.
// When the procedure fails, or the land component refuses the start for a
// reason that may pass, such as a busy workspace, the start is made due
// again an Interval later (retrySoon) and what failed is logged: one
// change the lander cannot judge holds up no other, and the announcement
// it ran for, made once per landable head, is not lost. The error returned
// is that the start could not even be made due again.
func (l *AutoLander) consider(ctx context.Context, workspace string, number int64, seq int64, due bool) error {
	err := l.startProcedure(ctx, workspace, number, seq, due)
	if err == nil || ctx.Err() != nil {
		return nil
	}
	ref := change.Ref(workspace, number)
	if refusal := (*server.Error)(nil); errors.As(err, &refusal) {
		l.deps.Logger.Debug("automatic land: not started, tried again later", "change", ref, "reason", refusal.Message())
	} else {
		l.deps.Logger.Warn("automatic land: the start procedure failed, tried again later", "change", ref, "error", err)
	}
	if err := l.retrySoon(ctx, workspace, number); err != nil {
		return fmt.Errorf("make the start of %s due again: %w", ref, err)
	}
	return nil
}

// retrySoon makes the start of the change number of the workspace due an
// Interval from now, unless one is due sooner, while the change is open
// with its flag on. It is no retry of a failed attempt: the count of those
// stays as it is.
func (l *AutoLander) retrySoon(ctx context.Context, workspace string, number int64) error {
	return l.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		ch, err := l.deps.Changes.ReadChange(ctx, tx, workspace, number)
		switch {
		case errors.Is(err, change.ErrChangeNotFound):
			return forgetAutoLand(ctx, tx, workspace, number)
		case err != nil:
			return err
		case ch.State != change.StateOpen || !ch.AutoLand:
			return nil
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO land_auto_land (workspace_slug, number, retries, due_at) VALUES (?, ?, 0, ?)
ON CONFLICT (workspace_slug, number) DO UPDATE SET due_at = COALESCE(MIN(due_at, excluded.due_at), excluded.due_at)`,
			workspace, number, l.now().Add(l.deps.Interval).UnixNano())
		return err
	})
}

// startProcedure starts the automatic land of the change number of the
// workspace when it is to start now. Nothing happens, and the flag stays
// as it is, for a change that is not open, whose flag is off, that is not
// landable, that a running attempt covers, or whose depends_on names a
// change that is not landed: such a change is landed by a later
// announcement, if any; the change below that holds it back is noted
// (hold), so that its landing has the change announced again (release).
// A start that came due (due) for a change that is
// not open, whose flag is off or that is not landable is dropped, and its
// retries are counted from zero again. A change whose mergeable is unknown
// while it is otherwise landable has its start made due again an Interval
// later, by an announcement and a due start alike: the change component
// does not announce it again once it is clean at the head it was
// announced at. An announcement, or the start-up,
// starts nothing for a change whose retry is due later: the retry is made
// at its time. Then the setter of the flag must be allowed to
// land the change now, as the land route would allow them, and the files
// of its current version must be within the paths of the credential they
// set the flag with, whether or not that credential still exists: when
// either fails no land is started, the flag is cleared and the refusal is
// told as a parley.change.land_fail by system, with no attempt. Otherwise
// the land of the change alone is started, at its current version, with
// trigger auto_land, as the setter.
//
// seq, when not 0, is the position of the event the procedure runs for,
// stored with what the procedure writes. Once a land is started, a start
// of the change due by then is cleared: it is the one just made; and so
// is the change below that held it back. The land component's refusal of
// the start is returned, for consider to make the start due again, unless
// it is land_in_progress: the running attempt's end decides what follows.
func (l *AutoLander) startProcedure(ctx context.Context, workspace string, number int64, seq int64, due bool) error {
	ch, err := l.deps.Changes.ReadChange(ctx, nil, workspace, number)
	if errors.Is(err, change.ErrChangeNotFound) {
		return l.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return forgetAutoLand(ctx, tx, workspace, number) })
	}
	if err != nil {
		return err
	}
	switch verdict, err := l.startable(ctx, ch, due); {
	case err != nil:
		return err
	case verdict == notLandable && due:
		return l.dropRetry(ctx, workspace, number)
	case verdict == unsettled:
		return l.retrySoon(ctx, workspace, number)
	case verdict == heldBelow:
		held, err := l.hold(ctx, ch)
		if err != nil || held {
			return err
		}
	case verdict != startNow:
		return nil
	}
	setter, found, err := l.deps.Changes.AutoLandSetter(ctx, nil, workspace, number)
	if err != nil || !found {
		return err
	}
	caller, permitted, err := l.permitted(ctx, setter, ch)
	if err != nil {
		return err
	}
	if permitted && setter.Paths != nil {
		repo, err := l.deps.Repos(ctx, workspace)
		if err != nil {
			return err
		}
		refused, err := CheckPaths(ctx, repo, setter.Paths, baseOf(ch), ch.HeadSHA)
		if err != nil {
			return err
		}
		permitted = refused == ""
	}
	if !permitted {
		return l.refuse(ctx, ch, seq)
	}
	now := l.now().UnixNano()
	_, err = l.deps.Lands.Start(ctx, StartInput{
		Workspace: workspace, Number: number, ExpectedVersion: &ch.Version, Trigger: TriggerAutoLand,
		RequestedBy: actorOf(caller), Caller: caller,
	})
	var refusal *server.Error
	switch {
	case errors.As(err, &refusal) && refusal.Type() == "land_in_progress":
		// An attempt started since the change was read: its end decides
		// what follows.
		l.deps.Logger.Debug("automatic land: not started", "change", ch.Ref, "reason", refusal.Message())
	case err != nil:
		// Any other refusal, such as a busy workspace, may pass: consider
		// makes the start due again.
		return err
	}
	return l.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err == nil {
			// The retry the attempt's end may have made due is later than
			// now, and no change below holds the change back any longer.
			if _, err := tx.ExecContext(ctx, `UPDATE land_auto_land
SET due_at = CASE WHEN due_at <= ? THEN NULL ELSE due_at END, held_by = NULL
WHERE workspace_slug = ? AND number = ?`, now, workspace, number); err != nil {
				return fmt.Errorf("clear the due start of %s: %w", ch.Ref, err)
			}
		}
		if seq == 0 {
			return nil
		}
		return l.storePosition(ctx, tx, seq)
	})
}

// dropRetry drops the start of the change number of the workspace that
// came due while it was not to be landed, and counts its retries from
// zero again: the change is landed again from its next announcement.
func (l *AutoLander) dropRetry(ctx context.Context, workspace string, number int64) error {
	err := l.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE land_auto_land SET retries = 0, due_at = NULL
WHERE workspace_slug = ? AND number = ?`, workspace, number)
		return err
	})
	if err != nil {
		return fmt.Errorf("drop the retry of %s: %w", change.Ref(workspace, number), err)
	}
	return nil
}

// A verdict is whether the start procedure starts a change's land now.
type verdict int

const (
	// startNow: the change is landable, and nothing holds its land back.
	startNow verdict = iota
	// notLandable: the change is not open, its flag is off, its landing
	// is not ready or its mergeable is conflict. The change component
	// forgets its announcement then, and announces it again once it is
	// landable.
	notLandable
	// unsettled: the change is open, its flag on and its landing ready,
	// but its mergeable is unknown while a check runs. The change
	// component neither announces nor forgets a change then, so one
	// announced landable before is not announced again once the check
	// finds it clean: its start is tried again an Interval later instead.
	unsettled
	// notNow: a running attempt covers the change, or its retry is due
	// later.
	notNow
	// heldBelow: the change below it, which its depends_on names, is not
	// landed.
	heldBelow
)

// startable judges whether the change ch may have its automatic land
// started now: open, its flag on, landable, covered by no running attempt,
// depending on no change that is not landed and, unless the start is the
// one that came due (due), with no retry due later. A mergeable that is
// unknown is not yet known to be clean (unsettled). A depends_on that
// names no change counts as none, as the land does.
func (l *AutoLander) startable(ctx context.Context, ch change.Change, due bool) (verdict, error) {
	if ch.State != change.StateOpen || !ch.AutoLand || ch.Landing.Status != change.LandingReady || ch.Mergeable == change.MergeableConflict {
		return notLandable, nil
	}
	running, err := l.deps.Lands.Running(ctx, ch.WorkspaceSlug, ch.Number)
	if err != nil || running {
		return notNow, err
	}
	if !due {
		var pending bool
		if err := l.deps.Store.Main().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM land_auto_land
WHERE workspace_slug = ? AND number = ? AND due_at > ?)`, ch.WorkspaceSlug, ch.Number, l.now().UnixNano()).Scan(&pending); err != nil || pending {
			return notNow, err
		}
	}
	if ch.Mergeable != change.MergeableClean {
		return unsettled, nil
	}
	if ch.DependsOn == nil {
		return startNow, nil
	}
	below, err := l.deps.Changes.ReadChange(ctx, nil, ch.WorkspaceSlug, *ch.DependsOn)
	switch {
	case errors.Is(err, change.ErrChangeNotFound):
		return startNow, nil
	case err != nil:
		return notNow, err
	case below.State != change.StateLanded:
		return heldBelow, nil
	}
	return startNow, nil
}

// hold notes, for the change ch, whose depends_on names a change that was
// not landed when its start was judged, that this change below holds its
// start back, and reports true; once that change has landed, release has
// ch announced landable again, since the change component announces a
// change once per head and ch, landable at its head all along, would not
// be announced again. The change below is read in the transaction that
// notes it, so that a land that ends in between is not missed: a change
// below found landed holds nothing back, and hold reports false.
func (l *AutoLander) hold(ctx context.Context, ch change.Change) (held bool, err error) {
	err = l.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		below, err := l.deps.Changes.ReadChange(ctx, tx, ch.WorkspaceSlug, *ch.DependsOn)
		switch {
		case errors.Is(err, change.ErrChangeNotFound):
			return nil
		case err != nil:
			return err
		case below.State == change.StateLanded:
			return nil
		}
		held = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO land_auto_land (workspace_slug, number, held_by) VALUES (?, ?, ?)
ON CONFLICT (workspace_slug, number) DO UPDATE SET held_by = excluded.held_by`, ch.WorkspaceSlug, ch.Number, below.Number); err != nil {
			return fmt.Errorf("note what holds %s back: %w", ch.Ref, err)
		}
		return nil
	})
	return held, err
}

// release is told of the attempt a that landed, whatever its trigger.
// Each change whose start a change a landed held back (hold) is held no
// longer, and when it already sits on the commit that change landed as,
// so that no restack will give it a version to announce, it is announced
// landable again in the same transaction: forgotten, then announced if it
// is landable now, else once its mergeability, which the land made
// unknown, is known again. Its land then starts from that announcement,
// after a ended. A change that does not sit on the landed commit is left
// to the version the hub's restack records, announced in its turn.
func (l *AutoLander) release(ctx context.Context, a Attempt) error {
	var errs []error
	for _, landed := range afterLanding(a).Changes {
		held, err := l.heldBy(ctx, a.Workspace, landed.Number)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, n := range held {
			errs = append(errs, l.announceAgain(ctx, a.Workspace, n, landed))
		}
	}
	return errors.Join(errs...)
}

// heldBy are the numbers of the changes of the workspace whose start the
// change below numbered below holds back.
func (l *AutoLander) heldBy(ctx context.Context, workspace string, below int64) ([]int64, error) {
	rows, err := l.deps.Store.Main().QueryContext(ctx, `SELECT number FROM land_auto_land
WHERE workspace_slug = ? AND held_by = ? ORDER BY number`, workspace, below)
	if err != nil {
		return nil, fmt.Errorf("read the changes %s holds back: %w", change.Ref(workspace, below), err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// announceAgain releases the change number of the workspace, which the
// change landed held back (release).
func (l *AutoLander) announceAgain(ctx context.Context, workspace string, number int64, landed LandedChange) error {
	ch, err := l.deps.Changes.ReadChange(ctx, nil, workspace, number)
	if errors.Is(err, change.ErrChangeNotFound) {
		return l.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return forgetAutoLand(ctx, tx, workspace, number) })
	}
	if err != nil {
		return err
	}
	repo, err := l.deps.Repos(ctx, workspace)
	if err != nil {
		return err
	}
	sits, err := repo.IsAncestor(ctx, landed.LandedSHA, ch.HeadSHA)
	if err != nil {
		return err
	}
	return l.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE land_auto_land SET held_by = NULL
WHERE workspace_slug = ? AND number = ? AND held_by = ?`, workspace, number, landed.Number)
		if err != nil {
			return fmt.Errorf("release %s: %w", ch.Ref, err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 || !sits {
			return err
		}
		now, err := l.deps.Changes.ReadChange(ctx, tx, workspace, number)
		if err != nil || now.HeadSHA != ch.HeadSHA {
			// A version recorded since is announced in its turn.
			return err
		}
		if err := l.deps.Changes.ForgetLandable(ctx, tx, workspace, number); err != nil {
			return err
		}
		return l.deps.Changes.AnnounceLandable(ctx, tx, workspace, number)
	})
}

// Finished is the land component's final observer. For an automatic land
// it keeps, makes or ends the retries of each change the attempt covered
// (settle); and for every land that landed, whatever its trigger, it
// releases the changes above those it landed that they held back
// (release).
func (l *AutoLander) Finished(ctx context.Context, f Final) {
	if !l.bound.Load() {
		return
	}
	if f.Result == StatusLanded {
		if err := l.release(ctx, f.Attempt); err != nil {
			l.deps.Logger.Warn("automatic land: the changes held back by a land were not released", "workspace",
				f.Attempt.Workspace, "land_id", f.Attempt.ID, "error", err)
		}
	}
	if f.Attempt.Trigger != TriggerAutoLand {
		return
	}
	for _, cov := range f.Attempt.Covered {
		if err := l.settle(ctx, f.Attempt.Workspace, cov.Number, f.Result); err != nil {
			l.deps.Logger.Warn("automatic land: the end of an attempt was not followed up", "change",
				change.Ref(f.Attempt.Workspace, cov.Number), "land_id", f.Attempt.ID, "error", err)
		}
	}
}

// settle follows up the end, with the status result, of an automatic
// land of the change number of the workspace, in one transaction. A
// landed change is forgotten: its flag no longer matters. Any other end
// that is not failed counts the retries from zero again and makes no start
// due: a conflict or a block waits for the change to be announced landable
// again, and the flag stays as it is. A failed end of a change
// whose flag is still on makes its next retry due, the count one more,
// after a delay that doubles with each retry already made (retryDelay);
// once autoLandRetries were made the flag is cleared instead, and the
// change keeps its state.
func (l *AutoLander) settle(ctx context.Context, workspace string, number int64, result string) error {
	return l.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if result == StatusLanded {
			return forgetAutoLand(ctx, tx, workspace, number)
		}
		if result != StatusFailed {
			_, err := tx.ExecContext(ctx, `UPDATE land_auto_land SET retries = 0, due_at = NULL
WHERE workspace_slug = ? AND number = ?`, workspace, number)
			return err
		}
		ch, err := l.deps.Changes.ReadChange(ctx, tx, workspace, number)
		switch {
		case errors.Is(err, change.ErrChangeNotFound):
			return forgetAutoLand(ctx, tx, workspace, number)
		case err != nil:
			return err
		case !ch.AutoLand:
			return forgetAutoLand(ctx, tx, workspace, number)
		}
		var retries int
		err = tx.QueryRowContext(ctx, `SELECT retries FROM land_auto_land WHERE workspace_slug = ? AND number = ?`,
			workspace, number).Scan(&retries)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if retries >= autoLandRetries {
			if err := l.deps.Changes.SetAutoLand(ctx, tx, workspace, number, false, change.Caller{}); err != nil {
				return err
			}
			return forgetAutoLand(ctx, tx, workspace, number)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO land_auto_land (workspace_slug, number, retries, due_at) VALUES (?, ?, ?, ?)
ON CONFLICT (workspace_slug, number) DO UPDATE SET retries = excluded.retries, due_at = excluded.due_at`,
			workspace, number, retries+1, l.now().Add(retryDelay(retries)).UnixNano())
		return err
	})
}

// retryDelay is the delay before the retry that follows the given number
// of retries already made: firstRetryDelay, doubled for each, at most
// lastRetryDelay.
func retryDelay(retries int) time.Duration {
	d := firstRetryDelay
	for range retries {
		if d >= lastRetryDelay {
			break
		}
		d *= 2
	}
	return min(d, lastRetryDelay)
}

// permitted reports whether the setter s may land ch now, and returns
// them as the caller the land runs as. The admin token may; a user must
// exist, not be blocked, and be allowed to land ch as the land route
// allows its caller: a writer of the workspace, or its author while
// land.by_author is on. The scopes of the credential are not looked at:
// it had changes:write when it set the flag.
func (l *AutoLander) permitted(ctx context.Context, s change.AutoLandSet, ch change.Change) (change.Caller, bool, error) {
	caller := change.Caller{Credential: change.Credential{Type: s.CredentialType}, Paths: s.Paths}
	if s.CredentialType == change.CredentialAdminToken {
		return caller, true, nil
	}
	if s.UserID == nil {
		return caller, false, nil
	}
	username, active, err := l.deps.Users.User(ctx, *s.UserID)
	if err != nil || !active {
		return caller, false, err
	}
	caller.UserID, caller.Username = *s.UserID, username
	ws, found, err := l.deps.Workspaces.Workspace(ctx, l.deps.Store.Main(), ch.WorkspaceSlug)
	if err != nil || !found {
		return caller, false, err
	}
	may, err := mayLand(ctx, l.deps.Access, caller, []change.Change{ch}, ws)
	return caller, may, err
}

// refuse clears the flag of ch, with its setter and its retries, and tells
// of the refusal of its automatic land: a parley.change.land_fail by
// system on the change, blocked, not_permitted, of no attempt; in one
// transaction with the position seq (0 for none).
func (l *AutoLander) refuse(ctx context.Context, ch change.Change, seq int64) error {
	return l.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if err := l.deps.Changes.SetAutoLand(ctx, tx, ch.WorkspaceSlug, ch.Number, false, change.Caller{}); err != nil {
			return err
		}
		if err := forgetAutoLand(ctx, tx, ch.WorkspaceSlug, ch.Number); err != nil {
			return err
		}
		if _, err := l.deps.Events.Emit(ctx, tx, event.Input{
			Type: EventLandFail, Workspace: &ch.WorkspaceSlug, ActorType: event.ActorSystem,
			ResourceType: "change", ResourceID: strconv.FormatInt(ch.Number, 10),
			Metadata: map[string]any{"land_id": nil, "status": StatusBlocked, "reason": "not_permitted",
				"number": ch.Number, "ref": ch.Ref},
		}); err != nil {
			return err
		}
		if seq == 0 {
			return nil
		}
		return l.storePosition(ctx, tx, seq)
	})
}

// position is the stored position of the lander in the event log. With
// none stored it stores the end of the log: the announcements before it
// are about changes the start-up looks at anyway.
func (l *AutoLander) position(ctx context.Context) (int64, error) {
	var seq int64
	err := l.deps.Store.Main().QueryRowContext(ctx, `SELECT seq FROM land_auto_land_position WHERE id = 1`).Scan(&seq)
	if !errors.Is(err, sql.ErrNoRows) {
		return seq, err
	}
	b, err := l.deps.Events.Bounds(ctx)
	if err != nil {
		return 0, err
	}
	err = l.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO land_auto_land_position (id, seq) VALUES (1, ?)
ON CONFLICT (id) DO NOTHING`, b.LastSeq)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store the automatic lander's position: %w", err)
	}
	return b.LastSeq, nil
}

// storePosition stores seq as the lander's position in tx, unless the
// position stored is past it already.
func (l *AutoLander) storePosition(ctx context.Context, tx *sql.Tx, seq int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO land_auto_land_position (id, seq) VALUES (1, ?)
ON CONFLICT (id) DO UPDATE SET seq = excluded.seq WHERE excluded.seq > seq`, seq); err != nil {
		return fmt.Errorf("store the automatic lander's position: %w", err)
	}
	return nil
}

// storePositionTx is storePosition in a store transaction of its own: the
// store writes one transaction at a time, and a write outside one would
// not wait its turn.
func (l *AutoLander) storePositionTx(ctx context.Context, seq int64) error {
	return l.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return l.storePosition(ctx, tx, seq) })
}

// now is the lander's clock, in UTC.
func (l *AutoLander) now() time.Time { return l.deps.Now().UTC() }

// forgetAutoLand deletes, in tx, the row of the change number of the
// workspace: its flag is off, or it is gone.
func forgetAutoLand(ctx context.Context, tx *sql.Tx, workspace string, number int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM land_auto_land WHERE workspace_slug = ? AND number = ?`, workspace, number); err != nil {
		return fmt.Errorf("forget the automatic land of %s: %w", change.Ref(workspace, number), err)
	}
	return nil
}

// baseOf is the base of the current version of ch, "" for the empty tree.
func baseOf(ch change.Change) string {
	if ch.BaseSHA == nil {
		return ""
	}
	return *ch.BaseSHA
}

// Running reports whether a running attempt covers the change number of
// the workspace.
func (c *Component) Running(ctx context.Context, workspace string, number int64) (bool, error) {
	_, found, err := runningCovering(ctx, c.deps.Store.Main(), workspace, []int64{number}, "")
	return found, err
}
