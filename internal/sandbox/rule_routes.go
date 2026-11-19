package sandbox

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// EventRuleRemove is the type of the event a workspace sandbox rule's
// delete writes.
const EventRuleRemove = "parley.sandbox.rule_remove"

// errorTypeRuleNotFound answers a rule delete that names no rule of the
// workspace, with 404.
const errorTypeRuleNotFound = "sandbox_rule_not_found"

// ruleJSON is the workspace sandbox rule object of the rule routes: its
// seven members, each always present, findings [] without findings,
// from_proposal null for a rule promoted from no proposal, and created_at
// in RFC 3339, UTC, with whole seconds, as in the sandbox object.
type ruleJSON struct {
	ID           string          `json:"id"`
	Rule         json.RawMessage `json:"rule"`
	Summary      string          `json:"summary"`
	Findings     []findingJSON   `json:"findings"`
	FromProposal *RuleOrigin     `json:"from_proposal"`
	CreatedBy    Creator         `json:"created_by"`
	CreatedAt    timefmt.Time    `json:"created_at"`
}

// ruleObject returns the workspace sandbox rule object of r.
func ruleObject(r SandboxRule) ruleJSON {
	o := ruleJSON{
		ID: r.ID, Rule: r.Rule, Summary: r.Summary, Findings: make([]findingJSON, len(r.Findings)),
		FromProposal: r.FromProposal, CreatedBy: r.CreatedBy, CreatedAt: timefmt.Time(r.CreatedAt),
	}
	for i, f := range r.Findings {
		o.Findings[i] = findingJSON(f)
	}
	return o
}

// RulesRoute declares GET /workspaces/:slug/sandbox-rules: the workspace
// sandbox rules of the workspace that holds those of slug, which is the
// parent when slug names a task fork, oldest first by created_at and then
// id, paged. It needs the scope sandboxes:read of a PAT and read access to
// the workspace slug, which a token bound to it has, and reads the store
// only.
func (s *Service) RulesRoute() server.Route {
	return s.WorkspaceRoute(KindRead, server.Route{
		Method:  http.MethodGet,
		Pattern: "/workspaces/:slug/sandbox-rules",
		Handler: s.listRules,
	})
}

// listRules answers GET /workspaces/:slug/sandbox-rules.
func (s *Service) listRules(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the rule list found no access decision")
	}
	var pos Position
	after := &pos
	if ok, err := page.After(&pos); err != nil {
		return err
	} else if !ok {
		after = nil
	}
	rules, err := rulesAfter(r.Context(), s.deps.Store.Main(), holdingSlug(d.Record), after, page.Limit+1)
	if err != nil {
		return err
	}
	items := make([]ruleJSON, len(rules))
	for i, rule := range rules {
		items[i] = ruleObject(rule)
	}
	return server.WritePage(w, page, items, func(o ruleJSON) any {
		return Position{CreatedAt: time.Time(o.CreatedAt), ID: o.ID}
	})
}

// RuleDeleteRoute declares DELETE /workspaces/:slug/sandbox-rules/:rule_id:
// it deletes the workspace sandbox rule rule_id of the repository workspace
// slug and answers 204 (see deleteRule). The sandboxes that recorded the
// rule keep it. It needs the scope sandboxes:write of a PAT and write
// access to the workspace, and refuses every workspace-bound token: the
// binding guard answers 403 token_bound before the scope check. No
// workspace state refuses it, so a rule of an archived workspace can be
// deleted too.
func (s *Service) RuleDeleteRoute() server.Route {
	return s.WorkspaceRoute(KindWrite, server.Route{
		Method:  http.MethodDelete,
		Pattern: "/workspaces/:slug/sandbox-rules/:rule_id",
		Marks:   map[string]string{access.MarkRefusesBound: ""},
		Handler: s.deleteRule,
	})
}

// deleteRule answers the delete of a workspace sandbox rule: 400
// not_supported_on_fork under a task fork's slug, whose rules its parent
// holds, then 404 sandbox_rule_not_found when the workspace holds no rule
// of the id. The rule's removal and its parley.sandbox.rule_remove event,
// by the caller, are one transaction.
func (s *Service) deleteRule(w http.ResponseWriter, r *http.Request) error {
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the rule delete found no access decision")
	}
	if d.Record.Kind == access.KindFork {
		return server.NewError("not_supported_on_fork", http.StatusBadRequest,
			"a task fork holds no sandbox rules; delete the rule on its parent "+d.Record.ParentSlug)
	}
	caller, ok := identity.PrincipalOf(r.Context())
	if !ok {
		return server.Internal("the rule delete found no caller")
	}
	ctx, slug, id := r.Context(), d.Record.Slug, server.Param(r, "rule_id")
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		rule, err := DeleteRule(ctx, tx, slug, id)
		if err != nil {
			return err
		}
		actor := actorFor(caller)
		_, err = s.deps.Events.Emit(ctx, tx, event.Input{
			Type: EventRuleRemove, ActorType: actor.Type, ActorID: actor.ID,
			ResourceType: "sandbox_rule", ResourceID: rule.ID, Workspace: &slug,
			Metadata: map[string]any{"rule_id": rule.ID, "summary": rule.Summary},
		})
		return err
	})
	if errors.Is(err, ErrRuleNotFound) {
		return server.NewError(errorTypeRuleNotFound, http.StatusNotFound,
			"workspace "+slug+" holds no sandbox rule "+id)
	}
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
