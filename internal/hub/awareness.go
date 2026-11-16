package hub

import (
	"context"
	"errors"
	"time"

	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// awarenessHook is the name of the start hook that binds the awareness
// component.
const awarenessHook = "awareness"

// awarenessIndexWork is the name of the background work that keeps the
// index of the changes by touched file; awarenessIndexGrace is how long
// the stop waits for it, which ends the git process of a running pass at
// once and leaves the event it was on for the next start.
const (
	awarenessIndexWork  = "awareness_index"
	awarenessIndexGrace = 5 * time.Second
)

// composeAwareness adds the awareness: the schema of the index of the
// changes by touched file and of its worker's position, its routes, the
// overlaps a read of a change may include, and the component,
// bound in a start hook to the store, to the hub's event log, which the
// index worker follows and writes the overlap events to, to the change
// records, which it reads through the change component, and to the place
// of each repository on disk, opened with the hub's git, where it lists
// the files of a change's version.
//
// The index worker runs as background work, a pass every
// awareness.Deps.IndexInterval, so that no request that records a version
// or changes a change's state waits for the index.
//
// It registers the keys of [awareness] and checks, before any store is
// opened, the rule that spans two of them: a default claim lifetime longer
// than the maximum stops the start. No session interface is registered:
// without the session feature every claim that names a session is refused.
//
// The component exists from the composition on (Awareness); tests change
// its dependencies with AdjustAwareness.
func composeAwareness(c *Composition) {
	comp := &awareness.Component{}
	c.awareness = comp
	c.AddSchemaStep(awareness.SchemaStep)
	c.AddSchemaStep(awareness.ClaimSchemaStep)
	for _, k := range awareness.ConfigKeys() {
		c.RegisterKey(k)
	}
	var cfg awareness.Config
	c.addConfigCheck(func(loaded *config.Config) (err error) {
		cfg, err = awareness.ConfigFrom(loaded)
		return err
	})
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	// The overlaps a read of a change includes are the entries its own
	// overlaps route lists, in the same order and all of them.
	c.changes.RegisterExpander("overlaps", func(ctx context.Context, ch change.Change) (any, error) {
		return comp.Overlaps(ctx, nil, ch.WorkspaceSlug, ch.Number)
	})
	c.AddStartHook(awarenessHook, func(_ context.Context, rt *Runtime) error {
		git := gitFor(c.env)
		d := awareness.Deps{
			Store:   rt.Store,
			Events:  c.events,
			Changes: awarenessChanges{c.changes},
			Emitter: c.events,
			Repos: func(ctx context.Context, slug string) (*gitrepo.Repo, error) {
				return git.Open(ctx, c.workspaces.RepoPath(slug))
			},
			DefaultBranch: func(ctx context.Context, slug string) (string, error) {
				w, found, err := workspace.BySlug(ctx, rt.Store.Main(), slug)
				if err != nil || !found || w.DefaultBranch == nil {
					return "", err
				}
				return *w.DefaultBranch, nil
			},
			Owns:   c.access.CanWrite,
			Now:    rt.Now,
			Logger: rt.Logger,
			Config: cfg,
		}
		for _, adjust := range c.awarenessAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
	c.AddBackgroundWork(awarenessIndexWork, awarenessIndexGrace, func(ctx context.Context, _ *Runtime) { comp.Run(ctx) })
}

// awarenessChanges is the awareness component's change reader: the change
// component's read of one change, in the caller's transaction or the
// store.
type awarenessChanges struct{ changes *change.Component }

func (r awarenessChanges) ReadChange(ctx context.Context, q awareness.Querier, workspace string, number int64) (awareness.Change, error) {
	ch, err := r.changes.ReadChange(ctx, q, workspace, number)
	if errors.Is(err, change.ErrChangeNotFound) {
		return awareness.Change{}, awareness.ErrChangeNotFound
	}
	if err != nil {
		return awareness.Change{}, err
	}
	out := awareness.Change{
		Workspace: ch.WorkspaceSlug, Number: ch.Number, Ref: ch.Ref, Title: ch.Title, Intent: ch.Intent, State: ch.State,
		TargetBranch: ch.TargetBranch, Version: ch.Version, HeadSHA: ch.HeadSHA, BaseSHA: ch.BaseSHA, DependsOn: ch.DependsOn,
		Author:    awareness.Actor{ActorType: ch.Author.ActorType, ActorID: ch.Author.ActorID, Username: ch.Author.Username},
		SessionID: ch.SessionID, Mergeable: ch.Mergeable, LandingStatus: ch.Landing.Status, UpdatedAt: ch.UpdatedAt,
	}
	if ch.Landed != nil {
		out.Landed = &awareness.Landed{SHA: ch.Landed.SHA, At: ch.Landed.At}
	}
	return out, nil
}

// Awareness returns the hub's awareness component, which serves once its
// start hook has run.
func (c *Composition) Awareness() *awareness.Component { return c.awareness }

// AdjustAwareness adds fn to the functions that change the awareness
// component's dependencies after the start hook has built them and before
// it binds them, in the order they were added.
func (c *Composition) AdjustAwareness(fn func(*awareness.Deps)) {
	c.awarenessAdjusters = append(c.awarenessAdjusters, fn)
}
