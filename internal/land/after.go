package land

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// concluded is told that the final status of the attempt a, of the target
// workspace ws, is committed. A landed attempt's follow-up runs first
// (follow), then every final observer is told of the attempt (report).
// Nothing of it changes the attempt or what the caller is answered, and
// none of it contacts origin or a forge: a land changes the hub's own
// repositories alone, and publishing a landed branch is a push to origin
// of its own.
//
// In the pool it runs once the attempt's worker is free (later), so that a
// hook or an observer that takes long holds no worker.
func (c *Component) concluded(ctx context.Context, ws Workspace, a Attempt) {
	later(ctx, func() {
		ctx := context.WithoutCancel(ctx)
		if a.Status == StatusLanded {
			c.follow(ctx, ws, a)
		}
		c.report(ctx, a)
	})
}

// follow does the work a land leaves, once it is recorded, each step on
// its own: the source branch of each landed change is deleted where
// nothing else needs it (deleteSource), the mergeability of every other
// change into the target is checked again (scheduleOthers), and each
// after-landing hook is called, once.
func (c *Component) follow(ctx context.Context, ws Workspace, a Attempt) {
	for _, cov := range a.Covered {
		c.step(a, "delete the source branch", func() error { return c.deleteSource(ctx, ws, cov) })
	}
	c.step(a, "schedule the mergeability checks", func() error { return c.scheduleOthers(ctx, a) })
	in := afterLanding(a)
	for _, h := range c.registrations().afterLanding {
		c.step(a, "after-landing hook", func() error { return h(ctx, in) })
	}
}

// report tells every final observer of the final attempt a.
func (c *Component) report(ctx context.Context, a Attempt) {
	f := Final{Attempt: a, Result: a.Status, Attempts: a.Attempts}
	if a.FinishedAt != nil {
		f.Duration = a.FinishedAt.Sub(a.CreatedAt)
	}
	for _, o := range c.registrations().observers {
		c.step(a, "final observer", func() error {
			o(ctx, f)
			return nil
		})
	}
}

// step runs fn, one step of what follows the end of the attempt a, and
// logs its error or its panic: a step that fails stops neither the land
// nor the steps after it.
func (c *Component) step(a Attempt, name string, fn func() error) {
	defer func() {
		if p := recover(); p != nil {
			c.deps.Logger.Error("land attempt: a step after its end panicked", "step", name, "workspace", a.Workspace,
				"land_id", a.ID, "panic", p)
		}
	}()
	if err := fn(); err != nil {
		c.deps.Logger.Error("land attempt: a step after its end failed", "step", name, "workspace", a.Workspace,
			"land_id", a.ID, "error", err)
	}
}

// deleteSource deletes the source branch of the landed change cov when
// the target workspace ws has land.delete_source on, no draft or open
// change has that branch as its source and the branch is still at the head
// of the version that landed: a branch moved since holds work that did not
// land and is kept. The default branch of the source workspace is never
// deleted, since the repository's HEAD names it.
//
// The check and the delete run in one store transaction, the ref moved
// before it commits, as the ref writer does: a change opens in a store
// transaction of its own, which reads the branch there, and the store
// runs its transactions one at a time, so a change opened from the branch
// is either seen by the check and keeps it, or opens after the delete and
// finds no branch.
//
// The branch is deleted directly with a compare-and-swap, not through the
// ref writer: the push rules judge what a caller writes, and the hub
// writes this. So no push event is written, and the mergeability of the
// changes into the branch, if any, is scheduled here.
func (c *Component) deleteSource(ctx context.Context, ws Workspace, cov CoveredChange) error {
	if !ws.DeleteSource || cov.SourceBranch == "" || cov.Head == "" {
		return nil
	}
	src, found, err := c.deps.Workspaces.Workspace(ctx, c.deps.Store.Main(), cov.SourceWorkspace)
	if err != nil || !found || src.DefaultBranch == cov.SourceBranch {
		return err
	}
	repo, err := c.deps.Repos(ctx, cov.SourceWorkspace)
	if err != nil {
		return err
	}
	ref := "refs/heads/" + cov.SourceBranch
	return c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, used, err := c.deps.Changes.OpenChangeOfBranch(ctx, tx, cov.SourceWorkspace, cov.SourceBranch)
		if err != nil || used {
			return err
		}
		if c.seams.sourceChecked != nil {
			c.seams.sourceChecked()
		}
		rtx, err := repo.PrepareRefs(ctx, []gitrepo.RefUpdate{{Ref: ref, Old: cov.Head}})
		if errors.Is(err, gitrepo.ErrRefMoved) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("delete %s of %s: %w", ref, cov.SourceWorkspace, err)
		}
		if err := c.deps.Changes.ScheduleTargetMergeable(ctx, tx, cov.SourceWorkspace, cov.SourceBranch); err != nil {
			_ = rtx.Abort()
			return err
		}
		if err := rtx.Commit(); err != nil {
			return fmt.Errorf("delete %s of %s: %w", ref, cov.SourceWorkspace, err)
		}
		return nil
	})
}

// scheduleOthers schedules the mergeability check of every draft and open
// change into the target of the attempt a other than those it landed: each
// was computed against a head the target no longer has. A land that moved
// nothing schedules them too.
func (c *Component) scheduleOthers(ctx context.Context, a Attempt) error {
	numbers, err := c.deps.Changes.ChangesInto(ctx, nil, a.Workspace, a.TargetBranch)
	if err != nil {
		return err
	}
	numbers = slices.DeleteFunc(numbers, func(n int64) bool { return slices.Contains(a.Numbers(), n) })
	if len(numbers) == 0 {
		return nil
	}
	return c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, n := range numbers {
			if err := c.deps.Mergeability.ScheduleMergeable(ctx, tx, a.Workspace, n); err != nil {
				return err
			}
		}
		return nil
	})
}

// afterLanding is what the after-landing hooks are told of the landed
// attempt a. Each change's landed commit is its last commit on the target,
// or, for a change that brought none, the commit the target held after the
// change below it, or before the land for the first.
func afterLanding(a Attempt) AfterLanding {
	in := AfterLanding{Attempt: a, Workspace: a.Workspace, SessionID: a.SessionID,
		Update: RefUpdate{Ref: "refs/heads/" + a.TargetBranch}}
	if a.OntoSHA != nil && a.LandedSHA != nil {
		in.Update.Before, in.Update.After = *a.OntoSHA, *a.LandedSHA
	}
	at := in.Update.Before
	for _, cov := range a.Covered {
		for _, cm := range a.Commits {
			if cm.Change == cov.Number {
				at = cm.LandedSHA
			}
		}
		in.Changes = append(in.Changes, LandedChange{Number: cov.Number, SourceWorkspace: cov.SourceWorkspace,
			SourceBranch: cov.SourceBranch, LandedSHA: at})
	}
	return in
}
