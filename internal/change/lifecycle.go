package change

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/server"
)

// EventRestore is the event of an abandoned change brought back to open.
const EventRestore = "parley.change.restore"

// What an abandon does with the draft and open changes that depend on the
// change, as the dependants member of its body names it.
const (
	// DependantsReparent makes each direct dependant depend on what the
	// abandoned change depended on, and has it restacked.
	DependantsReparent = "reparent"
	// DependantsAbandon abandons every change above the abandoned one in
	// its stack, direct or not, for the same reason.
	DependantsAbandon = "abandon"
)

// TriggerAbandon is the trigger of the restack an abandon asks for a
// reparented dependant.
const TriggerAbandon = "abandon"

// Restacker restacks changes, which the stack feature does. Without one an
// abandon that reparents a change's dependants schedules their
// mergeability checks instead.
type Restacker interface {
	// RequestRestack asks for the restack of the change ch, as it stands
	// in tx, for the trigger, such as TriggerAbandon, inside tx, the
	// transaction of what caused it. Its error undoes that transaction.
	RequestRestack(ctx context.Context, tx *sql.Tx, ch Change, trigger string) error
}

// RegisterRestacker makes r the restacker; nil removes it.
func (c *Component) RegisterRestacker(r Restacker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restacker = r
}

// abandonRoute abandons the change :number of the workspace :slug, through
// modify, as its body says. A check the abandon dropped from the waiting
// mergeability checks is scheduled again when the transaction does not
// commit, since the change then is still draft or open.
func (c *Component) abandonRoute(w http.ResponseWriter, r *http.Request) error {
	var dropped []mergeKey
	err := c.modify(w, r, func(r *http.Request) (operation, error) { return c.parseAbandon(r, &dropped) })
	if err != nil {
		for _, k := range dropped {
			c.merge.pend(k)
		}
	}
	return err
}

// parseAbandon decodes the abandon's body, which may be absent: none, or
// one of white space only, is the same as an empty object. reason must be
// a string, "" when absent, and dependants DependantsReparent or
// DependantsAbandon. The operation adds to dropped the changes whose
// waiting check it dropped.
func (c *Component) parseAbandon(r *http.Request, dropped *[]mergeKey) (operation, error) {
	var body map[string]json.RawMessage
	if err := decodeOptional(r, &body); err != nil {
		return nil, err
	}
	reason, err := member[string](body, "reason", "a string")
	if err != nil {
		return nil, err
	}
	choice, err := member[string](body, "dependants", fmt.Sprintf("%q or %q", DependantsReparent, DependantsAbandon))
	if err != nil {
		return nil, err
	}
	if choice != nil && *choice != DependantsReparent && *choice != DependantsAbandon {
		return nil, server.InvalidRequest(fmt.Sprintf("dependants: must be %q or %q", DependantsReparent, DependantsAbandon))
	}
	a := abandoning{co: c.collaborators(), dropped: dropped}
	if reason != nil {
		a.reason = *reason
	}
	if choice != nil {
		a.choice = *choice
	}
	return func(ctx context.Context, tx *sql.Tx, ch Change, caller Caller) error {
		return c.abandonWith(ctx, tx, ch, caller, a)
	}, nil
}

// abandoning is an abandon as its body asks it.
type abandoning struct {
	reason, choice string
	co             collaborators
	dropped        *[]mergeKey
}

// abandonWith abandons the change ch as caller, in tx, and settles its
// draft and open dependants as a.choice says: a change that is neither
// draft nor open is 409 change_closed, and one with dependants and no
// choice 409 has_dependants, with nothing written.
func (c *Component) abandonWith(ctx context.Context, tx *sql.Tx, ch Change, caller Caller, a abandoning) error {
	if ch.State != StateDraft && ch.State != StateOpen {
		return server.NewError("change_closed", http.StatusConflict, fmt.Sprintf("%s is %s and cannot be abandoned", ch.Ref, ch.State))
	}
	if len(ch.Dependants) > 0 && a.choice == "" {
		return server.NewError("has_dependants", http.StatusConflict,
			fmt.Sprintf("%s has dependants %v; name dependants %q or %q", ch.Ref, ch.Dependants, DependantsReparent, DependantsAbandon)).
			With("dependants", ch.Dependants)
	}
	// The stack above is collected first, though abandoning a change
	// leaves its dependants' depends_on as it is.
	var above []int64
	if a.choice == DependantsAbandon {
		var err error
		if above, err = stackAbove(ctx, tx, ch.WorkspaceSlug, ch.Number); err != nil {
			return err
		}
	}
	for _, n := range append([]int64{ch.Number}, above...) {
		dropped, err := c.abandon(ctx, tx, ch.WorkspaceSlug, n, a.reason, caller)
		if err != nil {
			return err
		}
		if dropped {
			*a.dropped = append(*a.dropped, mergeKey{ch.WorkspaceSlug, n})
		}
	}
	if a.choice == DependantsReparent {
		return c.reparent(ctx, tx, a.co, ch)
	}
	return nil
}

// stackAbove are the draft and open changes of the workspace above the
// change number in its stack, the direct dependants first.
func stackAbove(ctx context.Context, q Querier, workspace string, number int64) ([]int64, error) {
	var out []int64
	for next := []int64{number}; len(next) > 0; {
		n := next[0]
		next = next[1:]
		deps, err := dependants(ctx, q, workspace, n)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if d != number && !slices.Contains(out, d) {
				out = append(out, d)
				next = append(next, d)
			}
		}
	}
	return out, nil
}

// reparent makes each direct dependant of the abandoned change ch depend
// on what ch depended on, then asks the restacker to restack each one, or,
// without a restacker, schedules its mergeability check, since what it sits
// on changed.
func (c *Component) reparent(ctx context.Context, tx *sql.Tx, co collaborators, ch Change) error {
	for _, d := range ch.Dependants {
		if err := c.SetDependsOn(ctx, tx, ch.WorkspaceSlug, d, ch.DependsOn); err != nil {
			return err
		}
	}
	for _, d := range ch.Dependants {
		if co.restacker == nil {
			if err := c.ScheduleMergeable(ctx, tx, ch.WorkspaceSlug, d); err != nil {
				return err
			}
			continue
		}
		dep, err := readChange(ctx, tx, ch.WorkspaceSlug, d)
		if err != nil {
			return err
		}
		if err := co.restacker.RequestRestack(ctx, tx, dep, TriggerAbandon); err != nil {
			return fmt.Errorf("restack %s: %w", dep.Ref, err)
		}
	}
	return nil
}

// Abandon moves the change number of the workspace to abandoned for
// reason, as actor did, in tx, and writes parley.change.abandon with the
// reason as given. The move goes through SetAbandoned, so a fork source's
// activity recorder is told; it forgets the change's announcement as
// landable, so that a restore announces it again, and drops its waiting
// mergeability check. Its source branch and its version refs stay as they
// are, and its dependants as they were.
func (c *Component) Abandon(ctx context.Context, tx *sql.Tx, workspace string, number int64, reason string, actor Caller) error {
	_, err := c.abandon(ctx, tx, workspace, number, reason, actor)
	return err
}

// abandon is Abandon, which also reports whether it dropped a waiting
// check, for a caller that schedules it again when tx does not commit.
func (c *Component) abandon(ctx context.Context, tx *sql.Tx, workspace string, number int64, reason string, actor Caller) (dropped bool, err error) {
	if err := c.SetAbandoned(ctx, tx, workspace, number, reason); err != nil {
		return false, err
	}
	if err := c.ForgetLandable(ctx, tx, workspace, number); err != nil {
		return false, err
	}
	if err := c.emitAbandon(ctx, tx, actor, workspace, number, reason); err != nil {
		return false, err
	}
	return c.merge.drop(mergeKey{workspace, number}), nil
}

// restoreRoute brings the change :number of the workspace :slug back to
// open, through modify. The restore takes no body; one that is sent is
// ignored.
func (c *Component) restoreRoute(w http.ResponseWriter, r *http.Request) error {
	return c.modify(w, r, func(*http.Request) (operation, error) { return c.restore, nil })
}

// restore moves the change ch back to open at its last version, with
// mergeable unknown and its check scheduled, and writes
// parley.change.restore by caller.
func (c *Component) restore(ctx context.Context, tx *sql.Tx, ch Change, caller Caller) error {
	if err := update(ctx, tx, ch.WorkspaceSlug, ch.Number, c.now(), `state = 'open', abandon_reason = NULL`); err != nil {
		return err
	}
	if err := c.ScheduleMergeable(ctx, tx, ch.WorkspaceSlug, ch.Number); err != nil {
		return err
	}
	return c.emit(ctx, tx, caller, ch, EventRestore, map[string]any{"version": ch.Version, "head_sha": ch.HeadSHA})
}

// decodeOptional is server.DecodeJSON for a body that may be absent: no
// body, or one of white space only, leaves v as it is.
func decodeOptional(r *http.Request, v any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return server.InvalidRequest("request body could not be read")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return server.DecodeJSON(r, v)
}
