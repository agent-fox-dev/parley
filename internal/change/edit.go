package change

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/server"
)

// editRoute edits the change :number of the workspace :slug with the
// members the PATCH body names, through modify; members the body does not
// name, and members it does not know, are left as they are.
func (c *Component) editRoute(w http.ResponseWriter, r *http.Request) error {
	return c.modify(w, r, c.parseEdit)
}

// edit is a PATCH body as parseEdit reads it: a member the body does not
// name is nil, so that an absent member is not taken for an empty one.
type edit struct {
	title, intent, state, targetBranch *string
	labels                             []string
	autoLand                           *bool
	// dependsOn is the depends_on member, read only while a stack linker is
	// registered: hasDependsOn says the body names it, dependsOn is nil
	// for null.
	hasDependsOn bool
	dependsOn    *int64
}

// edits says whether e names a member the edit itself stores. depends_on is
// not one: it is the stack linker's.
func (e edit) edits() bool {
	return e.title != nil || e.intent != nil || e.state != nil || e.targetBranch != nil || e.labels != nil || e.autoLand != nil
}

// labelsOnly says whether labels is the only member e edits.
func (e edit) labelsOnly() bool {
	return e.labels != nil && e.title == nil && e.intent == nil && e.state == nil && e.targetBranch == nil && e.autoLand == nil
}

// parseEdit decodes the PATCH body, keeping apart a member that is absent
// and one that is null, and checks every member it names before anything
// is written, so that one invalid member refuses the whole body.
func (c *Component) parseEdit(r *http.Request) (operation, error) {
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, server.InvalidRequest("the body must be a JSON object")
	}
	var e edit
	var err error
	if e.title, err = member[string](body, "title", "a string"); err != nil {
		return nil, err
	}
	if e.intent, err = member[string](body, "intent", "a string"); err != nil {
		return nil, err
	}
	if e.state, err = member[string](body, "state", "a string"); err != nil {
		return nil, err
	}
	if e.targetBranch, err = member[string](body, "target_branch", "a string"); err != nil {
		return nil, err
	}
	if e.autoLand, err = member[bool](body, "auto_land", "true or false"); err != nil {
		return nil, err
	}
	labels, err := member[[]string](body, "labels", "an array of strings")
	if err != nil {
		return nil, err
	}
	if labels != nil {
		e.labels = []string{}
		for _, l := range *labels {
			if !slices.Contains(e.labels, l) {
				e.labels = append(e.labels, l)
			}
		}
	}
	if err := e.validate(); err != nil {
		return nil, err
	}
	co := c.collaborators()
	if raw, ok := body["depends_on"]; ok && co.stack != nil {
		e.hasDependsOn = true
		if !isNull(raw) {
			var n int64
			if err := json.Unmarshal(raw, &n); err != nil {
				return nil, server.InvalidRequest("depends_on: must be a change number or null")
			}
			e.dependsOn = &n
		}
	}
	return func(ctx context.Context, tx *sql.Tx, ch Change, caller Caller) error {
		return c.applyEdit(ctx, tx, co, ch, caller, e)
	}, nil
}

// member decodes the member name of body as a T, nil when the body does not
// name it; null or another JSON type is 400 invalid_request saying it must
// be want.
func member[T any](body map[string]json.RawMessage, name, want string) (*T, error) {
	raw, ok := body[name]
	if !ok {
		return nil, nil
	}
	var v T
	if isNull(raw) || json.Unmarshal(raw, &v) != nil {
		return nil, server.InvalidRequest(fmt.Sprintf("%s: must be %s", name, want))
	}
	return &v, nil
}

// isNull says whether raw is the JSON null.
func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// validate checks every member e names against its limits, counting
// characters as code points, the labels after their duplicates are gone.
func (e edit) validate() error {
	if e.title != nil {
		switch t := *e.title; {
		case strings.TrimSpace(t) == "":
			return server.InvalidRequest("title: must not be empty")
		case utf8.RuneCountInString(t) > MaxTitle:
			return server.InvalidRequest(fmt.Sprintf("title: at most %d characters", MaxTitle))
		case strings.ContainsAny(t, "\r\n"):
			return server.InvalidRequest("title: must be one line")
		}
	}
	if e.intent != nil && utf8.RuneCountInString(*e.intent) > MaxIntent {
		return server.InvalidRequest(fmt.Sprintf("intent: at most %d characters", MaxIntent))
	}
	if len(e.labels) > MaxLabels {
		return server.InvalidRequest(fmt.Sprintf("labels: at most %d", MaxLabels))
	}
	for _, l := range e.labels {
		if !labelPattern.MatchString(l) {
			return server.InvalidRequest(fmt.Sprintf("labels: %q must match %s", l, labelPattern))
		}
	}
	if e.state != nil && *e.state != StateDraft && *e.state != StateOpen {
		return server.InvalidRequest(fmt.Sprintf("state: must be %q or %q", StateDraft, StateOpen))
	}
	return nil
}

// changeClosed is the refusal of an edit a closed change does not take.
func changeClosed(ch Change, takes string) error {
	return server.NewError("change_closed", http.StatusConflict, fmt.Sprintf("%s is %s and takes %s", ch.Ref, ch.State, takes))
}

// applyEdit applies e to the change ch, as caller, in tx: the state rules
// of a closed change, the intent rule on the change as the edit would leave
// it, the auto_land guard, then the writes, so that a refusal leaves every
// member as it was. updated_at advances only when a stored value changes,
// and no event is written for these members.
func (c *Component) applyEdit(ctx context.Context, tx *sql.Tx, co collaborators, ch Change, caller Caller, e edit) error {
	switch {
	case !e.edits() && !e.hasDependsOn:
		return nil
	case ch.State == StateAbandoned && e.edits():
		return changeClosed(ch, "no edit")
	case ch.State == StateLanded && e.edits() && !e.labelsOnly():
		return changeClosed(ch, "no edit but of its labels")
	}
	closed := ch.State != StateDraft && ch.State != StateOpen
	state, intent := ch.State, ch.Intent
	if e.state != nil {
		state = *e.state
	}
	if e.intent != nil {
		intent = *e.intent
	}
	if state == StateOpen && strings.TrimSpace(intent) == "" {
		target, _, err := c.deps.Workspaces.Workspace(ctx, tx, ch.WorkspaceSlug)
		if err != nil {
			return err
		}
		if target.RequireIntent {
			return server.NewError("intent_required", http.StatusBadRequest,
				fmt.Sprintf("intent: %s requires one unless the change is a draft; say why the change exists", ch.WorkspaceSlug))
		}
	}
	if e.targetBranch != nil && *e.targetBranch != ch.TargetBranch {
		return server.NewError("not_implemented", http.StatusNotImplemented, "target_branch: a change cannot be retargeted yet")
	}
	if e.autoLand != nil && co.guard != nil {
		if err := co.guard.CheckAutoLand(ctx, tx, caller, ch, *e.autoLand); err != nil {
			return err
		}
	}
	var set []string
	var args []any
	if e.title != nil && *e.title != ch.Title {
		set, args = append(set, "title = ?"), append(args, *e.title)
	}
	if e.intent != nil && *e.intent != ch.Intent {
		set, args = append(set, "intent = ?"), append(args, *e.intent)
	}
	if e.state != nil && *e.state != ch.State {
		set, args = append(set, "state = ?"), append(args, *e.state)
	}
	if e.labels != nil && !slices.Equal(e.labels, ch.Labels) {
		b, err := json.Marshal(e.labels)
		if err != nil {
			return err
		}
		set, args = append(set, "labels = ?"), append(args, string(b))
	}
	if len(set) > 0 {
		if err := update(ctx, tx, ch.WorkspaceSlug, ch.Number, c.now(), strings.Join(set, ", "), args...); err != nil {
			return err
		}
	}
	if e.autoLand != nil {
		if err := c.SetAutoLand(ctx, tx, ch.WorkspaceSlug, ch.Number, *e.autoLand, caller); err != nil {
			return err
		}
	}
	if e.hasDependsOn && !closed {
		return co.stack.SetDependsOn(ctx, tx, ch, e.dependsOn)
	}
	return nil
}
