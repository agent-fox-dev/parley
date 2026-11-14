package hub

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
)

// eventsHook is the name of the start hook that binds the event component.
const eventsHook = "events"

// composeEvents adds the hub events to the transactional store and binds
// the event component in a start hook to the stores, the hub's clock and
// scrubber, and the fork-parent lookup over the workspace records. The
// component exists from the composition on, so a later part keeps it
// through Composition.Events; the hooks that run after this one, those of
// the later parts and of Options.Compose, may emit, before the hub listens.
func composeEvents(c *Composition) {
	c.events = &event.Component{}
	c.AddSchemaStep(event.SchemaStep)
	c.AddStartHook(eventsHook, func(_ context.Context, rt *Runtime) error {
		c.events.Bind(event.Deps{
			Store:    rt.Store,
			Clock:    event.SystemClock(rt.Now),
			Scrubber: rt.Scrubber,
			Parents:  event.ForkParentsFunc(forkParent),
		})
		return nil
	})
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
