package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/server"
)

// withMarker runs fn while the workspace slug holds the maintenance marker
// m, which RequireReady reads, so that no other request that needs the
// workspace's repository runs beside the operation. The marker is the
// record's maintenance column rather than a lock in memory, so that the
// state guard sees it in the record it is given: setting it is one
// conditional write, so that of two operations that ask at once exactly one
// holds it; the other is answered 409 workspace_busy, or 404 when the
// workspace is gone, without fn running. The marker is released when fn
// returns or panics, on a context the request's end does not cancel, and
// only while it is still m. Neither write changes updated_at, since the
// marker is the hub's own and no response shows it. Once the marker is set,
// fn waits for the syncs and pushes of the workspace that passed the state
// guard before it to end (see admitWork); the ones that come after it are
// refused by the marker.
func (s *Service) withMarker(ctx context.Context, slug string, m Marker, fn func() error) error {
	db := s.deps.Store.Main()
	res, err := db.ExecContext(ctx, `UPDATE workspaces SET maintenance = ? WHERE slug = ? AND maintenance = ''`, string(m), slug)
	if err != nil {
		return fmt.Errorf("set the %s marker of workspace %q: %w", m, slug, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set the %s marker of workspace %q: %w", m, slug, err)
	}
	if n == 0 {
		w, found, err := BySlug(ctx, db, slug)
		switch {
		case err != nil:
			return err
		case !found:
			return server.NotFound(ErrNotFound.Error())
		case w.Marker == MarkerNone:
			// Released between the two reads: the operation that held it
			// has just ended.
			return busy(fmt.Sprintf("workspace %q is busy; try again later", slug))
		}
		return RequireReady(w)
	}
	defer func() {
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`UPDATE workspaces SET maintenance = '' WHERE slug = ? AND maintenance = ?`, slug, string(m)); err != nil {
			s.deps.Logger.Error("workspace maintenance marker not released", "slug", slug, "marker", string(m), "error", err)
		}
	}()
	defer s.work.lock(slug)()
	return fn()
}

// admitWork admits a sync or a push of the workspace slug, whose route
// judges the record with check, the route's own refusals with the state
// guard among them: it reads the record and judges it, then holds the
// workspace's work lock shared, reads and judges the record again under
// it, and returns that record and the function that releases the lock,
// which the caller calls when its work has ended. A slug no record has is
// answered 404. The first read answers a request that comes while a
// maintenance operation runs at once, without waiting for the operation; the
// second refuses one whose first read came just before the operation set
// its marker and which waited for it to end, since it changed the record.
func (s *Service) admitWork(ctx context.Context, slug string, check func(Workspace) error) (Workspace, func(), error) {
	read := func() (Workspace, error) {
		rec, found, err := BySlug(ctx, s.deps.Store.Main(), slug)
		switch {
		case err != nil:
			return Workspace{}, err
		case !found:
			// Deleted between the access check and this read: the answer is
			// the one the check gives for a slug no workspace has.
			return Workspace{}, server.NotFound(ErrNotFound.Error())
		}
		return rec, check(rec)
	}
	if _, err := read(); err != nil {
		return Workspace{}, nil, err
	}
	release := s.work.rlock(slug)
	rec, err := read()
	if err != nil {
		release()
		return Workspace{}, nil, err
	}
	return rec, release, nil
}

// ClearMarkers releases through ex the maintenance marker of every
// workspace. The hub calls it at start, before it serves or runs clone
// work: a marker is held only while an operation runs in the process, and
// each operation changes the store in one transaction, so a marker a hub
// left when it stopped in the middle of one guards nothing.
func ClearMarkers(ctx context.Context, ex Execer) error {
	if _, err := ex.ExecContext(ctx, `UPDATE workspaces SET maintenance = '' WHERE maintenance <> ''`); err != nil {
		return fmt.Errorf("release the workspaces' maintenance markers: %w", err)
	}
	return nil
}

// reread returns the record of the workspace rec names as an operation
// whose transaction has committed left it. The change has stuck by then,
// so a record that cannot be read again is not a failure of the operation:
// the failure is logged, and the answer is rec with the committed change,
// which apply makes, rather than a 500 for a change that was made.
func (s *Service) reread(ctx context.Context, op string, rec Workspace, apply func(*Workspace)) Workspace {
	got, found, err := BySlug(ctx, s.deps.Store.Main(), rec.Slug)
	if err == nil && found {
		return got
	}
	if err == nil {
		err = errors.New("the record is gone")
	}
	s.deps.Logger.Error("workspace record not read again after the "+op, "slug", rec.Slug, "error", err)
	apply(&rec)
	return rec
}

// guarded runs fn and returns its error, or the panic it raised as an
// error, so that a collaborator called after an operation committed fails
// as one that returns an error does: logged, without changing the answer
// or skipping what comes after it.
func guarded(what string, fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%s panicked: %v", what, p)
		}
	}()
	return fn()
}
