package land

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
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

// AutoLandStep creates the store schema of the automatic lander: one row
// per change whose auto_land is on, with the count of the retries of its
// automatic land and the time the next start of it is due, NULL while none
// is; and the position in the event log up to which the announcements of
// landable changes have been handled. Who set the flag is not kept here:
// the change record keeps it beside the flag.
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
// and the flag, which the lander clears.
type AutoLandChanges interface {
	ReadChange(ctx context.Context, q change.Querier, workspace string, number int64) (change.Change, error)
	AutoLandSetter(ctx context.Context, q change.Querier, workspace string, number int64) (change.AutoLandSet, bool, error)
	SetAutoLand(ctx context.Context, tx *sql.Tx, workspace string, number int64, value bool, caller change.Caller) error
	OpenAutoLand(ctx context.Context, q change.Querier) ([]change.Key, error)
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
// of the event log as the lander's position when none is stored yet, and
// runs the start procedure for every open change whose flag is on, so that
// a change that became landable while the hub was stopped, or was
// announced before the lander first ran, is not left waiting for an
// announcement that does not come.
func (l *AutoLander) Recover(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.position(ctx); err != nil {
		return err
	}
	return l.scan(ctx)
}

// Run runs the lander until ctx ends, every Interval: it follows the event
// log (Pass) and starts the lands that are due. It is the hub's background
// work, off the request path, and returns once ctx ended.
func (l *AutoLander) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.deps.After(l.deps.Interval):
		}
		err := errors.Join(l.Pass(ctx), l.runDue(ctx))
		if err != nil && ctx.Err() == nil {
			l.deps.Logger.Warn("automatic land: a run failed", "error", err)
		}
	}
}

// Pass reads the events after the stored position, runs the start
// procedure for the change of each parley.change.ready among them, and
// stores the position the read reached. The position of an event whose
// procedure wrote something is stored with what it wrote, or, for a start,
// right after it, so that an event starts at most one land. A position
// whose events have left the log unread is moved to where the log now
// begins, and every open change whose flag is on is looked at, since the
// lost events may have been about any of them.
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
			if err := l.storePosition(ctx, l.deps.Store.Main(), pos); err != nil {
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
			l.consider(ctx, *rec.Workspace, number, rec.Seq)
		}
		if examined > pos {
			if err := l.storePosition(ctx, l.deps.Store.Main(), examined); err != nil {
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
// now, each due time cleared first, so that a start that fails is not
// tried again at every run.
func (l *AutoLander) runDue(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
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
	for _, k := range due {
		if _, err := l.deps.Store.Main().ExecContext(ctx, `UPDATE land_auto_land SET due_at = NULL
WHERE workspace_slug = ? AND number = ? AND due_at <= ?`, k.WorkspaceSlug, k.Number, now); err != nil {
			return fmt.Errorf("clear the due time of %s: %w", change.Ref(k.WorkspaceSlug, k.Number), err)
		}
		l.consider(ctx, k.WorkspaceSlug, k.Number, 0)
	}
	return nil
}

// scan runs the start procedure for every open change whose flag is on.
func (l *AutoLander) scan(ctx context.Context) error {
	keys, err := l.deps.Changes.OpenAutoLand(ctx, nil)
	if err != nil {
		return err
	}
	for _, k := range keys {
		l.consider(ctx, k.WorkspaceSlug, k.Number, 0)
	}
	return nil
}

// consider runs the start procedure for the change number of the
// workspace, for the event at seq (0 for none), and logs what fails: one
// change the lander cannot judge holds up no other.
func (l *AutoLander) consider(ctx context.Context, workspace string, number int64, seq int64) {
	if err := l.startProcedure(ctx, workspace, number, seq); err != nil && ctx.Err() == nil {
		l.deps.Logger.Warn("automatic land: the start procedure failed", "change", change.Ref(workspace, number), "error", err)
	}
}

// startProcedure starts the automatic land of the change number of the
// workspace when it is to start now. Nothing happens, and the flag stays
// as it is, for a change that is not open, whose flag is off, that is not
// landable, that a running attempt covers, or whose depends_on names a
// change that is not landed: such a change is landed by a later
// announcement, if any. Then the setter of the flag must be allowed to
// land the change now, as the land route would allow them, and the files
// of its current version must be within the paths of the credential they
// set the flag with, whether or not that credential still exists: when
// either fails no land is started, the flag is cleared and the refusal is
// told as a parley.change.land_fail by system, with no attempt. Otherwise
// the land of the change alone is started, at its current version, with
// trigger auto_land, as the setter.
//
// seq, when not 0, is the position of the event the procedure runs for,
// stored with what the procedure writes.
func (l *AutoLander) startProcedure(ctx context.Context, workspace string, number int64, seq int64) error {
	ch, err := l.deps.Changes.ReadChange(ctx, nil, workspace, number)
	if errors.Is(err, change.ErrChangeNotFound) {
		return l.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return forgetAutoLand(ctx, tx, workspace, number) })
	}
	if err != nil {
		return err
	}
	if start, err := l.startable(ctx, ch); err != nil || !start {
		return err
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
	_, err = l.deps.Lands.Start(ctx, StartInput{
		Workspace: workspace, Number: number, ExpectedVersion: &ch.Version, Trigger: TriggerAutoLand,
		RequestedBy: actorOf(caller), Caller: caller,
	})
	var refusal *server.Error
	if errors.As(err, &refusal) {
		// A precondition of the land stopped holding since the change was
		// read, such as another attempt or a new version, which a later
		// announcement starts from.
		l.deps.Logger.Debug("automatic land: not started", "change", ch.Ref, "reason", refusal.Message())
		err = nil
	}
	if err != nil || seq == 0 {
		return err
	}
	return l.storePosition(ctx, l.deps.Store.Main(), seq)
}

// startable reports whether the change ch may have its automatic land
// started now: open, its flag on, landable, covered by no running attempt
// and depending on no change that is not landed. A depends_on that names
// no change counts as none, as the land does.
func (l *AutoLander) startable(ctx context.Context, ch change.Change) (bool, error) {
	if ch.State != change.StateOpen || !ch.AutoLand || ch.Landing.Status != change.LandingReady || ch.Mergeable != change.MergeableClean {
		return false, nil
	}
	running, err := l.deps.Lands.Running(ctx, ch.WorkspaceSlug, ch.Number)
	if err != nil || running {
		return false, err
	}
	if ch.DependsOn == nil {
		return true, nil
	}
	below, err := l.deps.Changes.ReadChange(ctx, nil, ch.WorkspaceSlug, *ch.DependsOn)
	switch {
	case errors.Is(err, change.ErrChangeNotFound):
		return true, nil
	case err != nil:
		return false, err
	}
	return below.State == change.StateLanded, nil
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
	if _, err := l.deps.Store.Main().ExecContext(ctx, `INSERT INTO land_auto_land_position (id, seq) VALUES (1, ?)
ON CONFLICT (id) DO NOTHING`, b.LastSeq); err != nil {
		return 0, fmt.Errorf("store the automatic lander's position: %w", err)
	}
	return b.LastSeq, nil
}

// execer is a store connection or a transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// storePosition stores seq as the lander's position through q, unless the
// position stored is past it already.
func (l *AutoLander) storePosition(ctx context.Context, q execer, seq int64) error {
	if _, err := q.ExecContext(ctx, `INSERT INTO land_auto_land_position (id, seq) VALUES (1, ?)
ON CONFLICT (id) DO UPDATE SET seq = excluded.seq WHERE excluded.seq > seq`, seq); err != nil {
		return fmt.Errorf("store the automatic lander's position: %w", err)
	}
	return nil
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
