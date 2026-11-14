package sandbox

import (
	"context"
	"encoding/json"
	"time"
)

// Source is where the hub-wide sandbox list (GET /sandboxes) reads the
// sandboxes from: the package that stores them registers one with
// RegisterSource. The list never asks the runtime, so it keeps answering
// while the runtime is unreachable.
type Source interface {
	// List returns, from the stored sandboxes of every workspace that match
	// filter, those after the position after (all of them when after is
	// nil), ordered by created_at and then by id, both descending, at most
	// limit of them. Positions are compared by instant (time.Time.Equal and
	// Before), not by location. Each item carries its created_at and id,
	// from which the next page continues.
	List(ctx context.Context, filter Filter, after *Position, limit int) ([]Item, error)
}

// Filter selects sandboxes; an empty member selects every value. Both
// members together select the sandboxes that match both.
type Filter struct {
	// State is one of the sandbox states: provisioning, ready, stopped,
	// completed, failed or deleted.
	State string
	// User is the id of the user who created the sandbox.
	User string
}

// Position is a sandbox's place in the list's order: its created_at, and
// its id among those created at the same instant.
type Position struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

// Item is one sandbox of a Source's answer: its position in the list and
// the sandbox object, which the list writes as it is, member for member.
type Item struct {
	CreatedAt time.Time
	ID        string
	// Object is the sandbox object; it must encode as JSON.
	Object any
}

// MarshalJSON writes the item as its object.
func (i Item) MarshalJSON() ([]byte, error) { return json.Marshal(i.Object) }

// RegisterSource makes src the source of the hub-wide sandbox list,
// replacing any registered before. It is called while the hub composes or
// in a start hook, before the hub serves; until a source is registered the
// list is empty.
func (s *Service) RegisterSource(src Source) { s.source = src }
