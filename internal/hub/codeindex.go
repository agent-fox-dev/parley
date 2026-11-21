package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/codeindex"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// codeIndexHook is the start hook that binds the code index component and,
// on a hub with [code_index], refuses a platform the index cannot run on.
const codeIndexHook = "code-index"

// codeIndexWork is the background work that closes the loaded indexes at
// the hub's stop, within codeIndexGrace.
const (
	codeIndexWork  = "code-index"
	codeIndexGrace = 10 * time.Second
)

// codeIndexTask is the maintenance task that queues a build for every
// workspace whose index is not at its head and removes the trees and
// indexes no workspace needs.
const codeIndexTask = "code_index"

// composeCodeIndex composes the code index: the trees and indexes of the
// workspaces' default branches (package codeindex), built by jobs of the
// type codeindex.JobType in the hub's queue, at most [code_index].workers
// at once. A hub whose configuration has no [code_index] gets the
// component and nothing it does: no handler is registered, so nothing
// builds.
//
// The component exists from the composition on (CodeIndex); tests change
// its dependencies with AdjustCodeIndex.
func composeCodeIndex(c *Composition) {
	comp := &codeindex.Component{}
	c.codeIndex = comp
	c.AddSchemaStep(codeindex.SchemaStep)
	// A build is queued wherever the head of a default branch moves: an
	// accepted write of refs (a land, a push, the source control API), a
	// clone that made a workspace ready (the first one and a reclone), a
	// sync, and a rebuild's publish; and when a workspace's setting is
	// turned on. Each notice is the component's to judge: it queues nothing
	// for a fork, for another branch, or on a hub without [code_index].
	c.RefWriter().RegisterPostPushListener(func(ctx context.Context, p pushrule.PostPush) error {
		ctx, cancel := untilShutdown(ctx, c.ShuttingDown())
		defer cancel()
		var failures []error
		for _, u := range p.Updates {
			if branch, isBranch := strings.CutPrefix(u.Ref, "refs/heads/"); isBranch && !u.Deleted {
				failures = append(failures, comp.BranchMoved(ctx, p.Workspace, branch))
			}
		}
		return errors.Join(failures...)
	})
	c.workspaces.RegisterCloneObserver(codeIndexCloned{comp})
	c.workspaces.RegisterBranchMoveObserver(codeIndexMoved{comp})
	c.AdjustCarryPatch(func(d *carrypatch.Deps) {
		d.AfterRebuildPublish = func(ctx context.Context, slug, branch string) {
			if err := comp.BranchMoved(ctx, slug, branch); err != nil {
				c.Runtime().Logger.Error("code index build not queued after a rebuild", "workspace", slug, "error", err)
			}
		}
	})
	c.workspaces.RegisterUpdateObserver(func(ctx context.Context, before, after workspace.Workspace) error {
		settings := c.workspaces.Settings()
		was, _ := settings.EffectiveSettings(before.Settings, before.DefaultBranch)[codeindex.SettingName].(bool)
		is, _ := settings.EffectiveSettings(after.Settings, after.DefaultBranch)[codeindex.SettingName].(bool)
		switch {
		case !was && is:
			return comp.Queue(ctx, after.Slug)
		case was && !is:
			return comp.Stop(ctx, after.Slug)
		}
		return nil
	})
	c.AddStartHook(codeIndexHook, func(ctx context.Context, rt *Runtime) error {
		git := gitFor(c.env)
		settings := c.workspaces.Settings()
		d := codeindex.Deps{
			Settings: rt.Config.CodeIndex,
			DataDir:  rt.Config.DataDir(),
			Store:    rt.Store,
			Jobs:     c.Jobs(),
			Workspace: func(ctx context.Context, slug string) (codeindex.Workspace, bool, error) {
				w, found, err := workspace.BySlug(ctx, rt.Store.Main(), slug)
				if err != nil || !found {
					return codeindex.Workspace{}, false, err
				}
				return codeIndexWorkspace(w, settings), true, nil
			},
			Workspaces: func(ctx context.Context) ([]codeindex.Workspace, error) {
				return codeIndexWorkspaces(ctx, rt.Store.Main(), settings)
			},
			Repos: func(ctx context.Context, slug string) (codeindex.Repository, error) {
				return git.Open(ctx, c.workspaces.RepoPath(slug))
			},
			Metrics: c.metrics,
			Scrub:   func(text string) string { return rt.Scrubber.Scrub(text) },
			Now:     rt.Now,
		}
		for _, adjust := range c.codeIndexAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		if !comp.Enabled() {
			return nil
		}
		if err := comp.CheckPlatform(); err != nil {
			return err
		}
		// The builds are bounded apart from whatever else the queue runs.
		c.Jobs().Limit(codeindex.JobType, rt.Config.CodeIndex.Workers)
		c.Jobs().Register(codeindex.JobType, comp.Handler())
		// The queue has recovered what a stop left running; what moved
		// while the hub was down is queued here.
		comp.Measure()
		return comp.Reconcile(ctx)
	})
	c.AddBackgroundWork(codeIndexWork, codeIndexGrace, func(ctx context.Context, _ *Runtime) { comp.Run(ctx) })
	// The record goes in the delete's transaction; the queue's own remover
	// deletes the builds and ends the one that runs, and the maintenance
	// pass removes the directory.
	c.workspaces.Teardown().RegisterRemover(codeindex.RemoveWorkspace)
	c.AddMaintenanceTask(codeIndexTask, func(ctx context.Context, _ *Runtime) error {
		return errors.Join(comp.Reconcile(ctx), comp.Sweep(ctx))
	})
}

// codeIndexWorkspaces lists every repository workspace as the code index
// component sees it.
func codeIndexWorkspaces(ctx context.Context, q access.Querier, settings *workspace.SettingsRegistry) ([]codeindex.Workspace, error) {
	rows, err := q.QueryContext(ctx, `SELECT slug FROM workspaces WHERE kind = ? ORDER BY slug`, access.KindRepository)
	if err != nil {
		return nil, fmt.Errorf("list the repository workspaces: %w", err)
	}
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			_ = rows.Close()
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var all []codeindex.Workspace
	for _, slug := range slugs {
		w, found, err := workspace.BySlug(ctx, q, slug)
		if err != nil {
			return nil, err
		}
		if found {
			all = append(all, codeIndexWorkspace(w, settings))
		}
	}
	return all, nil
}

// codeIndexWorkspace is w as the code index component sees it.
func codeIndexWorkspace(w workspace.Workspace, settings *workspace.SettingsRegistry) codeindex.Workspace {
	ws := codeindex.Workspace{Slug: w.Slug, Repository: w.Kind == access.KindRepository}
	if w.DefaultBranch != nil {
		ws.DefaultBranch = *w.DefaultBranch
	}
	ws.Indexed, _ = settings.EffectiveSettings(w.Settings, w.DefaultBranch)[codeindex.SettingName].(bool)
	return ws
}

// codeIndexCloned queues a build of a workspace in the transaction that
// makes it ready, after its first clone and after a reclone.
type codeIndexCloned struct{ comp *codeindex.Component }

func (o codeIndexCloned) Cloned(ctx context.Context, tx *sql.Tx, slug string) error {
	return o.comp.QueueIn(ctx, tx, slug)
}

// codeIndexMoved queues a build of a workspace whose default branch a sync
// moved.
type codeIndexMoved struct{ comp *codeindex.Component }

func (o codeIndexMoved) BranchMoved(ctx context.Context, m workspace.BranchMove) error {
	return o.comp.BranchMoved(ctx, m.Workspace, m.Branch)
}

// CodeIndex returns the hub's code index component, which works once its
// start hook has bound it.
func (c *Composition) CodeIndex() *codeindex.Component { return c.codeIndex }

// AdjustCodeIndex adds fn to the functions that change the code index
// component's dependencies after the start hook has built them and before
// it binds them, in the order they were added.
func (c *Composition) AdjustCodeIndex(fn func(*codeindex.Deps)) {
	c.codeIndexAdjusters = append(c.codeIndexAdjusters, fn)
}
