package access

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// RefusedEventType is the type of the event that reports a refusal of the
// binding guard.
const RefusedEventType = "parley.token.bound_refused"

// refusalWindow is how long after a token's refusal event further refusals
// of the token are counted instead of reported.
const refusalWindow = time.Minute

// errNoEmitter is a limiter the hub built without an event emitter.
var errNoEmitter = errors.New("the refusal limiter has no event emitter")

// RefusalLimiter is the binding guard's RefusalReporter in the hub: it
// reports a token's refusal as a parley.token.bound_refused event at most
// once a minute, and counts the refusals in between, which the token's next
// event carries as suppressed, so one looping agent cannot flood the event
// log. Its state is in memory only, so a restart starts every token afresh.
// It is safe for concurrent requests.
type RefusalLimiter struct {
	events EventEmitter
	now    func() time.Time
	logger *slog.Logger

	mu     sync.Mutex
	tokens map[string]*refusalState
}

// refusalState is what the limiter holds of one token: when its last event
// was emitted, and how many refusals it has not reported since.
type refusalState struct {
	last       time.Time
	suppressed int
}

// NewRefusalLimiter returns a limiter that emits through events, reads the
// time from now (nil is time.Now) and logs a failed emit to logger (nil
// discards).
func NewRefusalLimiter(events EventEmitter, now func() time.Time, logger *slog.Logger) *RefusalLimiter {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &RefusalLimiter{events: events, now: now, logger: logger, tokens: map[string]*refusalState{}}
}

// Refused reports the refusal f. When no event was emitted for f's token in
// the preceding minute it emits one, which carries the count of the
// refusals suppressed since the token's previous event, and starts the
// token's minute and count afresh; otherwise it only adds f to the count.
// The guard has decided the answer before it calls Refused, and nothing the
// emit does changes it: an error or a panic of the emitter is logged once
// and the event is not retried.
func (l *RefusalLimiter) Refused(ctx context.Context, f Refusal) {
	suppressed, emit := l.admit(f.TokenID)
	if !emit {
		return
	}
	// The event reports a refusal already answered, so it is written even
	// when the client has gone.
	if err := l.emit(context.WithoutCancel(ctx), refusedEvent(f, suppressed)); err != nil {
		l.logger.Warn("the refusal of a workspace-bound token was not recorded",
			"token_id", f.TokenID, "bound_workspace", f.BoundWorkspace, "error", err)
	}
}

// admit decides whether a refusal of tokenID is reported: it returns the
// count of suppressed refusals the event carries and true, or false when the
// refusal is counted instead. It drops the entries whose minute has passed
// with nothing counted, so that the limiter forgets the tokens that are no
// longer refused.
func (l *RefusalLimiter) admit(tokenID string) (int, bool) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, s := range l.tokens {
		if s.suppressed == 0 && now.Sub(s.last) >= refusalWindow {
			delete(l.tokens, id)
		}
	}
	s, ok := l.tokens[tokenID]
	if ok && now.Sub(s.last) < refusalWindow {
		s.suppressed++
		return 0, false
	}
	if !ok {
		s = &refusalState{}
		l.tokens[tokenID] = s
	}
	suppressed := s.suppressed
	s.last, s.suppressed = now, 0
	return suppressed, true
}

// emit hands ev to the event emitter now and returns its error, or the
// panic it raised as an error.
func (l *RefusalLimiter) emit(ctx context.Context, ev Event) (err error) {
	if l.events == nil {
		return errNoEmitter
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("the event emitter panicked: %v", p)
		}
	}()
	return l.events.EmitNow(ctx, ev)
}

// refusedEvent is the event that reports the refusal f, carrying the count
// of refusals suppressed before it.
func refusedEvent(f Refusal, suppressed int) Event {
	meta := map[string]any{
		"token_id":        f.TokenID,
		"bound_workspace": f.BoundWorkspace,
		"method":          f.Method,
		"route":           f.Route,
		"status":          f.Status,
		"surface":         f.Surface,
		"suppressed":      suppressed,
	}
	if f.RequestedWorkspace != nil {
		meta["requested_workspace"] = *f.RequestedWorkspace
	}
	return Event{
		Type: RefusedEventType, Workspace: f.BoundWorkspace, ActorType: "pat", ActorID: f.UserID,
		ResourceType: "token", ResourceID: f.TokenID, Metadata: meta,
	}
}
