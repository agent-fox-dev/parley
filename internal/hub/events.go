package hub

import (
	"context"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
)

// eventsHook is the name of the start hook that binds the event component,
// and of the background work that runs it.
const eventsHook = "events"

// eventsGrace is how long the stop waits for the event component's Run,
// which returns as soon as its context ends unless a read or an OnCommit
// callback is under way.
const eventsGrace = time.Second

// composeEvents registers the keys of [events], adds the hub events to the
// transactional store and binds the event component in a start hook to the
// stores, the hub's clock and scrubber, the fork-parent lookup over the
// workspace records, the access rule and memberships composeAccess binds,
// from which it decides who sees an event, the hub's shutdown notification,
// which releases the requests the component holds, the identity service's
// authenticator, with which a stream checks its credential at every
// heartbeat, and the loaded [events] settings: the limit of open streams and
// the retention. It registers the event routes. It runs the component as
// background work: Run follows the commits for the waiters and the OnCommit
// callbacks until the stop, and the stop waits for it before the stores close
// (ADR 05). The component exists from the composition on, so a later part
// keeps it through Composition.Events; the hooks that run after this one,
// those of the later parts and of Options.Compose, may emit, before the hub
// listens.
func composeEvents(c *Composition) {
	c.events = &event.Component{}
	if err := event.Register(c.registry); err != nil {
		c.errs = append(c.errs, err)
	}
	c.AddSchemaStep(event.SchemaStep)
	c.AddStartHook(eventsHook, func(_ context.Context, rt *Runtime) error {
		settings := event.SettingsFrom(rt.Config)
		return c.events.Bind(event.Deps{
			Store:        rt.Store,
			Clock:        event.SystemClock(rt.Now),
			Scrubber:     rt.Scrubber,
			Parents:      event.ForkParentsFunc(forkParent),
			Access:       c.access,
			Memberships:  activeOrgs(rt.Store.Main()),
			Shutdown:     c.ShuttingDown(),
			Authenticate: c.identity.Authenticate,
			MaxStreams:   settings.MaxStreams,
			Retention:    settings.Retention,
		})
	})
	for _, r := range c.events.Routes() {
		c.AddRoute(r)
	}
	c.AddBackgroundWork(eventsHook, eventsGrace, func(ctx context.Context, _ *Runtime) { c.events.Run(ctx) })
}

// forkParent is the parent's slug when slug names a task fork, read in q.
func forkParent(ctx context.Context, q event.Querier, slug string) (string, error) {
	rec, found, err := access.RecordBySlug(ctx, q, slug)
	if err != nil || !found || rec.Kind != access.KindFork {
		return "", err
	}
	return rec.ParentSlug, nil
}

// Events returns the hub's event component, through which every part that
// changes state emits its events. A part keeps it when it is composed; the
// component accepts emits once its start hook has run, so from the start
// hooks of the parts composed after it on.
func (c *Composition) Events() *event.Component { return c.events }
