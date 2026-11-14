package fork

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The events a fork's removal writes.
const (
	// EventDelete is the type of the event of a fork deleted by request or
	// with its parent.
	EventDelete = "parley.workspace.delete"
	// EventExpire is the type of the event of a fork the sweep removed.
	EventExpire = "parley.fork.expire"
)

// sandboxReason is the reason a removal gives the sandbox lifecycle; no
// checkpoint is kept, since nothing could resume it.
const sandboxReason = "workspace_deleted"

// DiscardStep creates the table of the pending discards: the forks whose
// record is removed but whose repository could not be, by slug, with the
// parent and the time the discard failed, so that a later pass removes the
// repository. It applies after SchemaStep.
var DiscardStep = store.Step{
	Name:   "fork_discards",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE fork_discards (
			slug        TEXT PRIMARY KEY,
			parent_slug TEXT NOT NULL,
			recorded_at INTEGER NOT NULL
		)`)
		return err
	},
}

// errGone is the result of a removal of a fork that no longer exists: it
// never existed, or another removal took it first.
var errGone = errors.New("fork not found")

// errNotDue is the result of an expiry of a fork that is no longer due,
// such as one given a new lifetime since the sweep read it.
var errNotDue = errors.New("fork not due for expiry")

// removalCause is why a fork is removed. It decides the event the removal
// writes and the reason the session closer is given.
type removalCause string

// The causes of a removal.
const (
	causeDeleted       removalCause = "deleted"
	causeExpired       removalCause = "expired"
	causeParentDeleted removalCause = "parent_deleted"
)

// sessionReason is the reason the session closer gives the sessions of a
// fork removed for cause c.
func (c removalCause) sessionReason() string {
	if c == causeExpired {
		return "fork expired"
	}
	return "workspace deleted"
}

// removal is one removal of a fork: its cause, the caller who asked for it
// (for a delete, alone or with the parent), for an expiry the reason its
// event gives, ttl or landed, and the rule that says whether the fork, as
// read, is still due, nil for always.
type removal struct {
	cause  removalCause
	actor  identity.Principal
	expiry string
	due    func(Fork) bool
}

// notDue is errNotDue when rm has a rule by which the fork f is not due.
func (rm removal) notDue(f Fork) error {
	if rm.due != nil && !rm.due(f) {
		return errNotDue
	}
	return nil
}

// deleteEventMeta is the metadata of a fork's parley.workspace.delete
// event, as package workspace writes it for a fork removed without this
// component; a fork's event never holds deleted_forks.
type deleteEventMeta struct {
	Kind          string   `json:"kind"`
	ParentSlug    string   `json:"parent_slug"`
	RevokedTokens []string `json:"revoked_tokens"`
}

// expireEventMeta is the metadata of a parley.fork.expire event.
type expireEventMeta struct {
	ParentSlug    string   `json:"parent_slug"`
	Reason        string   `json:"reason"`
	ExpiresAt     *string  `json:"expires_at"`
	RevokedTokens []string `json:"revoked_tokens"`
}

// DeleteFork serves DELETE /workspaces/:slug for the fork slug, asked by
// caller, as the fork remover of package workspace, whose route has checked
// the scope and the caller's write access already. A fork whose parent is
// pending, cloning or under a maintenance operation is refused 409
// workspace_busy; any other parent lets the fork go, so that a fork stays
// removable whatever became of its parent. It returns nil once the fork is
// removed, 404 not_found when no fork has the slug, among them one another
// removal took first, and 500 internal for any other failure, on which the
// fork is whole.
func (c *Component) DeleteFork(ctx context.Context, slug string, caller identity.Principal) error {
	err := c.remove(ctx, slug, removal{cause: causeDeleted, actor: caller})
	var refusal *server.Error
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errGone):
		return server.NotFound(workspace.ErrNotFound.Error())
	case errors.As(err, &refusal) && refusal.Status() == http.StatusConflict:
		return refusal
	}
	c.deps.Logger.Error("fork not removed", "slug", slug, "error", err)
	return server.Internal("internal error")
}

// RemoveForksOf removes every fork of the workspace parent in tx, the
// transaction of the parent's delete by actor, in ascending slug order, each
// with what it owns and its parley.workspace.delete event, and returns
// their slugs, empty and not nil when there are none, and the function that
// discards their repositories once tx has committed. It asks no sandbox
// lifecycle, since the parent's delete has deleted the sandboxes of the
// parent and its forks first, and opens no transaction of its own.
func (c *Component) RemoveForksOf(ctx context.Context, tx *sql.Tx, parent string, actor identity.Principal) ([]string, func(), error) {
	rows, err := tx.QueryContext(ctx, `SELECT slug FROM workspaces WHERE kind = ? AND parent_slug = ? ORDER BY slug`,
		access.KindFork, parent)
	if err != nil {
		return nil, nil, fmt.Errorf("list the forks of %q: %w", parent, err)
	}
	slugs := []string{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("list the forks of %q: %w", parent, err)
		}
		slugs = append(slugs, slug)
	}
	// Read in full before the forks are removed, since tx holds one
	// connection.
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("list the forks of %q: %w", parent, err)
	}
	removed := make([]Fork, 0, len(slugs))
	for _, slug := range slugs {
		f, err := c.removeIn(ctx, tx, slug, removal{cause: causeParentDeleted, actor: actor})
		if err != nil {
			return nil, nil, err
		}
		removed = append(removed, f)
	}
	return slugs, func() {
		for _, f := range removed {
			c.discard(ctx, f)
		}
	}, nil
}

// CountTaskForks is the number of task forks that exist; a removed fork no
// longer counts.
func (c *Component) CountTaskForks(ctx context.Context) (int, error) {
	var n int
	if err := c.deps.Store.Main().QueryRowContext(ctx, `SELECT count(*) FROM workspaces WHERE kind = ?`,
		access.KindFork).Scan(&n); err != nil {
		return 0, fmt.Errorf("count the task forks: %w", err)
	}
	return n, nil
}

// remove removes the fork slug alone, for rm: it deletes the fork's
// sandboxes first, which live outside the store's transaction, so that no
// sandbox outlives its fork, and stops on their error; then it removes the
// fork in one transaction; and once that has committed, never before, it
// discards the fork's repository, so that a removal rolled back has lost
// nothing. A parent that refuses the removal is checked before the
// sandboxes and again in the transaction. The parent's create lock is held
// from the transaction to the discard, so that a create of a fork of the
// same name never has its new repository discarded.
func (c *Component) remove(ctx context.Context, slug string, rm removal) error {
	f, found, err := BySlug(ctx, c.deps.Store.Main(), slug)
	switch {
	case err != nil:
		return err
	case !found:
		return errGone
	}
	if err := rm.notDue(f); err != nil {
		return err
	}
	if err := removalRefusal(ctx, c.deps.Store.Main(), f); err != nil {
		return err
	}
	if c.sandboxes != nil {
		if err := c.sandboxes.DeleteSandboxes(ctx, slug, sandboxReason, false); err != nil {
			return fmt.Errorf("remove fork %q: delete its sandboxes: %w", slug, err)
		}
	}
	unlock := c.lockParent(*f.ParentSlug)
	defer unlock()
	err = c.deps.Transact(ctx, func(tx *sql.Tx) error {
		var err error
		f, err = c.removeIn(ctx, tx, slug, rm)
		return err
	})
	if err != nil {
		return err
	}
	c.discard(ctx, f)
	return nil
}

// removeIn removes the fork slug in tx for rm and returns its record as it
// was: what it owns, through RemoveOwned of the workspace service, which
// revokes the tokens bound to it, runs every remover of owned data and ends
// its sessions; then its record; then its event. A fork with no record is
// errGone. The record's deletion decides between two removals of one fork:
// the one that deletes no row is errGone and writes nothing. An expiry of a
// fork that, read through tx, is no longer due is errNotDue. A removal with
// its parent does not judge the parent, whose delete is running.
func (c *Component) removeIn(ctx context.Context, tx *sql.Tx, slug string, rm removal) (Fork, error) {
	f, found, err := BySlug(ctx, tx, slug)
	switch {
	case err != nil:
		return Fork{}, err
	case !found:
		return Fork{}, errGone
	}
	if err := rm.notDue(f); err != nil {
		return Fork{}, err
	}
	if rm.cause != causeParentDeleted {
		if err := removalRefusal(ctx, tx, f); err != nil {
			return Fork{}, err
		}
	}
	revoked, err := c.deps.Workspaces.RemoveOwned(ctx, tx, slug, rm.cause.sessionReason())
	if err != nil {
		return Fork{}, fmt.Errorf("remove fork %q: %w", slug, err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE slug = ? AND kind = ?`, slug, access.KindFork)
	if err != nil {
		return Fork{}, fmt.Errorf("remove the record of fork %q: %w", slug, err)
	}
	n, err := res.RowsAffected()
	switch {
	case err != nil:
		return Fork{}, fmt.Errorf("remove the record of fork %q: %w", slug, err)
	case n == 0:
		return Fork{}, errGone
	}
	in, err := rm.event(f, revoked)
	if err != nil {
		return Fork{}, err
	}
	if _, err := c.deps.Events.Emit(ctx, tx, in); err != nil {
		return Fork{}, fmt.Errorf("write the event of the removal of fork %q: %w", slug, err)
	}
	return f, nil
}

// event is the event of the removal rm of the fork f, which revoked the
// tokens revoked: parley.fork.expire by the system for an expiry, and
// parley.workspace.delete by the caller otherwise. Parent is set, since
// the fork's record is gone and its event must stay visible to the
// parent's readers.
func (rm removal) event(f Fork, revoked []string) (event.Input, error) {
	slug, parent := f.Slug, *f.ParentSlug
	in := event.Input{Workspace: &slug, ResourceType: "workspace", ResourceID: slug, SessionID: f.SessionID, Parent: parent}
	if rm.cause == causeExpired {
		in.Type, in.ActorType = EventExpire, event.ActorSystem
		meta := expireEventMeta{ParentSlug: parent, Reason: rm.expiry, RevokedTokens: revoked}
		if f.ExpiresAt != nil {
			meta.ExpiresAt = new(timefmt.Format(*f.ExpiresAt))
		}
		in.Metadata = meta
		return in, nil
	}
	in.Type = EventDelete
	in.Metadata = deleteEventMeta{Kind: access.KindFork, ParentSlug: parent, RevokedTokens: revoked}
	switch p := rm.actor; {
	case p.Type == identity.TypeAdmin:
		in.ActorType = event.ActorAdminToken
	case p.User != nil:
		in.ActorType, in.ActorID = p.Type, new(p.User.ID)
	default:
		return event.Input{}, fmt.Errorf("remove fork %q: the removal names no caller", slug)
	}
	return in, nil
}

// removalRefusal is nil when a removal may take the fork f, and otherwise
// the 409 workspace_busy of the parent, read through q: a parent that is
// pending or cloning, or that a maintenance operation runs on, will be able
// to let the fork go once that work ends. Every other parent, ready,
// archived or failed, lets it go.
func removalRefusal(ctx context.Context, q access.Querier, f Fork) error {
	parent, found, err := workspace.BySlug(ctx, q, *f.ParentSlug)
	switch {
	case err != nil:
		return err
	case !found:
		return nil
	case parent.Marker == workspace.MarkerNone && parent.Status != workspace.StatusPending &&
		parent.Status != workspace.StatusCloning:
		return nil
	}
	return workspace.RequireReady(parent)
}

// discard removes the repository of the removed fork f, which holds its
// own branches. A removal that fails is recorded as a pending discard and
// logged, and changes nothing about the removal, which has committed.
func (c *Component) discard(ctx context.Context, f Fork) {
	dir := c.deps.Workspaces.RepoPath(f.Slug)
	err := c.deps.RemoveRepo(dir)
	if err == nil {
		return
	}
	c.deps.Logger.Error("fork repository not removed after its removal; recorded for a later pass",
		"slug", f.Slug, "path", dir, "error", err)
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO fork_discards (slug, parent_slug, recorded_at) VALUES (?, ?, ?)
			ON CONFLICT (slug) DO UPDATE SET parent_slug = excluded.parent_slug, recorded_at = excluded.recorded_at`,
			f.Slug, *f.ParentSlug, c.deps.Now().UnixNano())
		return err
	})
	if err != nil {
		c.deps.Logger.Error("pending discard of a fork not recorded", "slug", f.Slug, "path", dir, "error", err)
	}
}
