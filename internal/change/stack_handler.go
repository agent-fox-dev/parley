package change

import (
	"context"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/server"
)

// Stack is the stack a change is part of, as GET …/changes/:number/stack
// and include=stack show it: the change's target branch and the summaries
// of the chain of changes below it, the change itself and every draft or
// open change above it, bottom first.
type Stack struct {
	TargetBranch string         `json:"target_branch"`
	Changes      []StackSummary `json:"changes"`
}

// StackSummary is one change of a stack, in the members a stack shows.
type StackSummary struct {
	Number        int64        `json:"number"`
	Title         string       `json:"title"`
	State         string       `json:"state"`
	Version       int64        `json:"version"`
	HeadSHA       string       `json:"head_sha"`
	Mergeable     string       `json:"mergeable"`
	NeedsRestack  bool         `json:"needs_restack"`
	RestackReason *string      `json:"restack_reason"`
	Landing       StackLanding `json:"landing"`
}

// StackLanding is the landing of a change of a stack: its status alone.
type StackLanding struct {
	Status string `json:"status"`
}

// stackRoute answers the stack of the change :number of the workspace
// :slug. changeOf resolves the change, so the route refuses alike with 404
// change_not_found what every read of one change refuses.
func (c *Component) stackRoute(w http.ResponseWriter, r *http.Request) error {
	ch, err := c.changeOf(r, c.deps.Store.Main())
	if err != nil {
		return err
	}
	s, err := c.Stack(r.Context(), ch)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, s)
	return nil
}

// Stack is the stack of the change ch: down its depends_on links to the
// change that depends on nothing, then ch, then every draft or open change
// reachable upward through the dependants, ordered by their depth from the
// bottom and then by number, so that where a change has several dependants
// all are listed, and abandoned and landed changes above are left out. In
// a request of a token bound to a fork, made on the fork's parent, only the
// changes whose source is the fork are listed, as every read of changes
// confines it. It is the answer of the stack route and the member
// include=stack adds; the stack_of filter of the list keeps its own walk,
// which lists every state.
func (c *Component) Stack(ctx context.Context, ch Change) (Stack, error) {
	q := c.deps.Store.Main()
	var below []int64
	seen := map[int64]bool{ch.Number: true}
	// A cycle, which the stack linker never stores, ends the walk where it
	// would repeat a change.
	for link := ch.DependsOn; link != nil && !seen[*link]; {
		seen[*link] = true
		below = append(below, *link)
		var err error
		if link, err = linkOf(ctx, q, ch.WorkspaceSlug, *link); err != nil {
			return Stack{}, err
		}
	}
	slices.Reverse(below)
	numbers := append(below, ch.Number)
	// Level by level upward: every change of a level is one deeper than the
	// changes of the level before.
	for level := []int64{ch.Number}; len(level) > 0; {
		var next []int64
		for _, n := range level {
			deps, err := dependants(ctx, q, ch.WorkspaceSlug, n)
			if err != nil {
				return Stack{}, err
			}
			for _, d := range deps {
				if !seen[d] {
					seen[d] = true
					next = append(next, d)
				}
			}
		}
		slices.Sort(next)
		numbers, level = append(numbers, next...), next
	}
	// The token's binding is attached to the request's context, which is
	// all an expander is given.
	fork, confined := confinedTo((&http.Request{}).WithContext(ctx))
	s := Stack{TargetBranch: ch.TargetBranch, Changes: []StackSummary{}}
	for _, n := range numbers {
		each := ch
		if n != ch.Number {
			var err error
			if each, err = readChange(ctx, q, ch.WorkspaceSlug, n); err != nil {
				return Stack{}, err
			}
		}
		if confined && each.Source.WorkspaceSlug != fork {
			continue
		}
		s.Changes = append(s.Changes, StackSummary{
			Number: each.Number, Title: each.Title, State: each.State, Version: each.Version, HeadSHA: each.HeadSHA,
			Mergeable: each.Mergeable, NeedsRestack: each.NeedsRestack, RestackReason: each.RestackReason,
			Landing: StackLanding{Status: each.Landing.Status},
		})
	}
	return s, nil
}
