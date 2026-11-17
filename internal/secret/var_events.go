package secret

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/event"
)

// The event types of the variable routes. An event names the variable by its
// stored key and, like a secret's, never carries its value: a record of a
// change to configuration is read by everyone who sees the scope, and a value
// may be one the owner would not show them.
const (
	varEventCreate = "parley.variable.create"
	varEventUpdate = "parley.variable.update"
	varEventDelete = "parley.variable.delete"
)

// varEventInput is the event of a change to t's variables by the request's
// caller, without its type and its variable: the event of a secret's change,
// about a variable.
func varEventInput(ctx context.Context, t target) (event.Input, error) {
	in, err := eventInput(ctx, t)
	in.ResourceType = "variable"
	return in, err
}
