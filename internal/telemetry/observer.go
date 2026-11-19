package telemetry

import "sync"

// RunEventObserver is told the event_type of a run event once it is stored.
type RunEventObserver func(eventType string)

// runEventObservers are the observers registered with OnRunEvent.
type runEventObservers struct {
	mu   sync.Mutex
	list []RunEventObserver
}

// OnRunEvent registers obs, which is called once for every run event
// stored from then on, by the single route, a batch or the recorder of
// sandbox use, after the write has committed. It is never called for a
// replay, a duplicate, an invalid item, a refused request, a write that
// failed or ran out of time, or a record of another kind, so that what it
// counts is what the store holds. It is safe to call at any time, also
// while requests are served.
func (c *Component) OnRunEvent(obs RunEventObserver) {
	c.observers.mu.Lock()
	defer c.observers.mu.Unlock()
	c.observers.list = append(c.observers.list, obs)
}

// stored tells the observers of rec, a record newly stored by a write that
// has committed, when it is a run event; a record of another kind tells
// them nothing.
func (c *Component) stored(rec record) {
	if e, ok := rec.(runEvent); ok {
		c.notifyRunEvent(e.EventType)
	}
}

// notifyRunEvent calls every registered observer with eventType.
func (c *Component) notifyRunEvent(eventType string) {
	c.observers.mu.Lock()
	list := c.observers.list
	c.observers.mu.Unlock()
	for _, obs := range list {
		obs(eventType)
	}
}
