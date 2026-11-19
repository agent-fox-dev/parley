package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// rulesPath is the path of the workspace sandbox rules of the workspace
// slug, and rulePath that of its rule id.
func rulesPath(slug string) string    { return "/workspaces/" + slug + "/sandbox-rules" }
func rulePath(slug, id string) string { return rulesPath(slug) + "/" + id }

// unknownRule is a well-formed rule id no rule has.
const unknownRule = "sbr_00000000000000000000000000000000"

// ruleIDs returns the ids of the rules the rule list of the workspace slug
// answers on its first page, in order.
func (h *createHub) ruleIDs(t *testing.T, slug string) []string {
	t.Helper()
	items, _ := listItems(t, h.call(t, http.MethodGet, rulesPath(slug), h.key, nil))
	ids := []string{}
	for _, it := range items {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	return ids
}

// seedRules saves the rules r1, r2 and r3 on the workspace slug, in that
// order of creation; r2 and r3 share a second, r2 having the smaller id, and
// r3 was promoted from a proposal. They are stored newest first, so that the
// order is the list's own.
func (h *createHub) seedRules(t *testing.T, slug string) (r1, r2, r3 sandbox.SandboxRule) {
	t.Helper()
	low, high := sandbox.NewRuleID(), sandbox.NewRuleID()
	if low > high {
		low, high = high, low
	}
	r3 = sandbox.SandboxRule{
		ID: high, Workspace: slug, Rule: json.RawMessage(`{"host":"r3.test","port":443}`), Summary: "allow r3.test",
		FromProposal: &sandbox.RuleOrigin{SandboxID: unknownID, ProposalID: unknownProposal, Workspace: slug + "--a"},
		CreatedBy:    sandbox.Creator{ActorType: "pat", ActorID: h.dana, Username: "dana"}, CreatedAt: recordStart.Add(-time.Hour),
	}
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error { return sandbox.SaveRule(ctx, tx, r3) })
	r2 = h.saveRule(t, slug, low, `{"host":"r2.test"}`, recordStart.Add(-time.Hour))
	r1 = h.saveRule(t, slug, "", `{"host":"r1.test"}`, recordStart.Add(-2*time.Hour))
	return r1, r2, r3
}

func TestListRules_AnswersTheHoldingWorkspacesRulesOldestFirst(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	h.fork(t, "demo--a", "demo", recordStart.Add(24*time.Hour), nil)
	r1, r2, r3 := h.seedRules(t, "demo")
	forkBound := h.IssuePAT(t, identity.PATSpec{UserID: h.dana, Permissions: []string{"sandboxes:read"}, Workspace: "demo--a"})
	mia := h.CreateUser(t, "mia")
	h.AddMember(t, h.org, mia)
	member := h.IssueKey(t, mia)
	all := []sandbox.SandboxRule{r1, r2, r3}

	for _, tc := range []struct {
		name, slug, query, cred string
		want                    []sandbox.SandboxRule
		// next is the second page the first page's next_cursor leads to,
		// nil when the first page has no next_cursor.
		next []sandbox.SandboxRule
	}{
		{name: "a repository", slug: "demo", cred: h.key, want: all},
		{name: "a limit of 2", slug: "demo", query: "?limit=2", cred: h.key, want: all[:2], next: all[2:]},
		{name: "a fork, as its owner", slug: "demo--a", cred: h.key, want: all},
		{name: "a fork, with a PAT bound to it", slug: "demo--a", cred: forkBound, want: all},
		{name: "a workspace without rules", slug: "other", cred: h.key, want: nil},
		{name: "an organization member with read access", slug: "demo", cred: member, want: all},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := rulesPath(tc.slug) + tc.query
			items, next := listItems(t, h.call(t, http.MethodGet, path, tc.cred, nil))
			wantRules(t, items, tc.want)
			if (next != "") != (tc.next != nil) {
				t.Fatalf("next_cursor %q; want one only when a second page follows", next)
			}
			if next == "" {
				return
			}
			items, next = listItems(t, h.call(t, http.MethodGet, path+"&cursor="+url.QueryEscape(next), tc.cred, nil))
			wantRules(t, items, tc.next)
			if next != "" {
				t.Errorf("the second page's next_cursor = %q; want null", next)
			}
		})
	}
}

// wantRules checks that the decoded list items are the rule objects of
// want, in order.
func wantRules(t *testing.T, items []any, want []sandbox.SandboxRule) {
	t.Helper()
	wantMaps := make([]any, len(want))
	for i, r := range want {
		wantMaps[i] = ruleMap(t, r)
	}
	if !reflect.DeepEqual(items, wantMaps) {
		t.Errorf("items\n%v\nwant\n%v", items, wantMaps)
	}
}

func TestDeleteRule_RemovesTheRuleAndKeepsTheCopiesOfEarlierSandboxes(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, agentProfile)
	h.AddMember(t, h.org, h.dana)
	created := func(t *testing.T, slug string) map[string]any {
		t.Helper()
		return sandboxObject(t, h.post(t, slug, map[string]any{}))
	}
	for _, tc := range []struct {
		name, slug string
		archived   bool
	}{
		{"an active workspace", "demo", false},
		{"an archived workspace", "kept", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h.repository(t, tc.slug, workspace.StatusReady)
			r1, r2, r3 := h.seedRules(t, tc.slug)
			s := created(t, tc.slug)["id"].(string)
			if tc.archived {
				h.exec(t, `UPDATE workspaces SET status = ? WHERE slug = ?`, string(workspace.StatusArchived), tc.slug)
			}

			a := h.call(t, http.MethodDelete, rulePath(tc.slug, r2.ID), h.key, nil)
			if a.status != http.StatusNoContent || len(a.body) != 0 {
				t.Fatalf("DELETE = %d %q; want 204 with no body", a.status, a.body)
			}
			if got, want := h.ruleIDs(t, tc.slug), []string{r1.ID, r3.ID}; !reflect.DeepEqual(got, want) {
				t.Errorf("rules after the delete = %v; want %v", got, want)
			}
			ev := h.sandboxEvents(t, sandbox.EventRuleRemove, r2.ID)
			wantMeta := map[string]any{"rule_id": r2.ID, "summary": r2.Summary}
			if len(ev) != 1 || ev[0].actorType != "api_key" || ev[0].actorID == nil || *ev[0].actorID != h.dana ||
				ev[0].resourceType != "sandbox_rule" || ev[0].workspace == nil || *ev[0].workspace != tc.slug ||
				ev[0].session != nil || !reflect.DeepEqual(ev[0].metadata, wantMeta) {
				t.Errorf("rule_remove events %+v; want one by dana about the sandbox_rule in %s, no session, metadata %v", ev, tc.slug, wantMeta)
			}
			got := decoded(t, h.call(t, http.MethodGet, sandboxPath(tc.slug, s), h.key, nil), http.StatusOK)
			if want := nameList(r1.ID, r2.ID, r3.ID); !reflect.DeepEqual(got["workspace_rules"], want) {
				t.Errorf("workspace_rules of the earlier sandbox = %v; want %v", got["workspace_rules"], want)
			}

			if tc.archived {
				h.exec(t, `UPDATE workspaces SET status = ? WHERE slug = ?`, string(workspace.StatusReady), tc.slug)
			}
			if got, want := created(t, tc.slug)["workspace_rules"], nameList(r1.ID, r3.ID); !reflect.DeepEqual(got, want) {
				t.Errorf("workspace_rules of a later sandbox = %v; want %v", got, want)
			}
		})
	}
}

func TestRuleRoutes_RefuseAtTheFirstFailingCheckAndRemoveNothing(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	h.fork(t, "demo--a", "demo", recordStart.Add(24*time.Hour), nil)
	r1, _, _ := h.seedRules(t, "demo")
	foreign := h.saveRule(t, "other", "", `{"host":"other.test"}`, recordStart)
	gitRead := h.pat(t, h.dana, "git:read")
	readOnly := h.pat(t, h.dana, "sandboxes:read")
	bound := h.IssuePAT(t, identity.PATSpec{UserID: h.dana, Permissions: []string{"sandboxes:write"}, Workspace: "demo"})
	stranger := h.IssueKey(t, h.CreateUser(t, "stranger"))
	omar := h.CreateUser(t, "omar")
	h.AddMember(t, h.org, omar)
	member := h.IssueKey(t, omar)
	removes := func() int {
		return h.count(t, `SELECT COUNT(*) FROM hub_events WHERE type = ?`, sandbox.EventRuleRemove)
	}
	rules := func() int { return h.count(t, `SELECT COUNT(*) FROM sandbox_rules`) }
	wantRules, wantRemoves := rules(), 0

	// In order: the last two rows delete r1 and then find it gone.
	for _, tc := range []struct {
		name, method, path, cred string
		status                   int
		errorType                string
	}{
		{"list with a PAT holding git:read only", http.MethodGet, rulesPath("demo"), gitRead, http.StatusForbidden, "insufficient_scope"},
		{"list as a user outside the organization", http.MethodGet, rulesPath("demo"), stranger, http.StatusNotFound, "not_found"},
		{"delete as a user outside the organization", http.MethodDelete, rulePath("demo", r1.ID), stranger, http.StatusNotFound, "not_found"},
		{"delete with a PAT bound to the workspace with sandboxes:write", http.MethodDelete, rulePath("demo", r1.ID), bound, http.StatusForbidden, "token_bound"},
		{"delete with a PAT holding sandboxes:read only", http.MethodDelete, rulePath("demo", r1.ID), readOnly, http.StatusForbidden, "insufficient_scope"},
		{"delete as an organization member with read access", http.MethodDelete, rulePath("demo", r1.ID), member, http.StatusForbidden, "forbidden"},
		{"delete under a fork's slug", http.MethodDelete, rulePath("demo--a", r1.ID), h.key, http.StatusBadRequest, "not_supported_on_fork"},
		{"delete an unknown rule id", http.MethodDelete, rulePath("demo", unknownRule), h.key, http.StatusNotFound, "sandbox_rule_not_found"},
		{"delete a rule of another workspace", http.MethodDelete, rulePath("demo", foreign.ID), h.key, http.StatusNotFound, "sandbox_rule_not_found"},
		{"delete a rule", http.MethodDelete, rulePath("demo", r1.ID), h.key, http.StatusNoContent, ""},
		{"delete a rule already deleted", http.MethodDelete, rulePath("demo", r1.ID), h.key, http.StatusNotFound, "sandbox_rule_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := h.call(t, tc.method, tc.path, tc.cred, nil)
			if a.status != tc.status || a.err.ErrorType != tc.errorType {
				t.Errorf("= %d %s; want %d %s", a.status, a.body, tc.status, tc.errorType)
			}
			if tc.status == http.StatusNoContent {
				wantRules, wantRemoves = wantRules-1, wantRemoves+1
			}
			if n := rules(); n != wantRules {
				t.Errorf("rules = %d; want %d", n, wantRules)
			}
			if n := removes(); n != wantRemoves {
				t.Errorf("rule_remove events = %d; want %d", n, wantRemoves)
			}
		})
	}
}
