package sandbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestRemoveWorkspaceRules_RemovesTheRulesOfTheWorkspaceInTheGivenTransaction(t *testing.T) {
	t.Parallel()
	h := startRecordHub(t)
	ctx := context.Background()
	for _, ws := range []string{"demo", "demo", "other"} {
		r := sandbox.SandboxRule{ID: sandbox.NewRuleID(), Workspace: ws, Rule: json.RawMessage(`{"id":"` + sandbox.NewRuleID() + `"}`), CreatedAt: recordStart}
		if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return sandbox.SaveRule(ctx, tx, r) }); err != nil {
			t.Fatal(err)
		}
	}
	count := func(ws string) int {
		t.Helper()
		n, err := sandbox.CountRules(ctx, h.Store.Main(), ws)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	remove := func(commit bool) {
		t.Helper()
		err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
			if err := sandbox.RemoveWorkspaceRules(ctx, tx, "demo"); err != nil {
				return err
			}
			if !commit {
				return errRollback
			}
			return nil
		})
		if err != nil && !errors.Is(err, errRollback) {
			t.Fatal(err)
		}
	}

	remove(false)
	if n := count("demo"); n != 2 {
		t.Errorf("rules of demo after a rolled back removal = %d; want 2", n)
	}
	remove(true)
	if n := count("demo"); n != 0 {
		t.Errorf("rules of demo = %d; want 0", n)
	}
	if n := count("other"); n != 1 {
		t.Errorf("rules of other = %d; want 1", n)
	}
	var events int
	if err := h.Store.Main().QueryRow(`SELECT COUNT(*) FROM hub_events WHERE type = ?`, sandbox.EventRuleRemove).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Errorf("rule_remove events = %d; want 0", events)
	}
}

// ruleIDPattern is the form of a workspace sandbox rule id.
var ruleIDPattern = regexp.MustCompile(`^sbr_[0-9a-f]{32}$`)

// promote is the body of an approval for the workspace.
var promote = map[string]any{"scope": "workspace"}

func TestPromote_SavesTheRuleOnTheHoldingWorkspaceAndRecordsBothEvents(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	h.fork(t, "demo--a", "demo", recordStart.Add(24*time.Hour), nil)
	ctx := context.Background()
	high := []runtime.Finding{{Severity: "low", Message: "plain HTTP"}, {Severity: "high", Message: "reaches any host"}}
	dana := sandbox.Creator{ActorType: "api_key", ActorID: h.dana, Username: "dana"}
	// In turn: both rows save their rule on demo.
	for _, ws := range []string{"demo", "demo--a"} {
		t.Run("a sandbox on "+ws, func(t *testing.T) {
			id := h.liveSandboxIn(t, ws, sandbox.StateReady, func(r *sandbox.Record) { r.SessionID = new(t1Session) })
			p := h.seedProposal(t, id, sandbox.ProposalOpen, proposedBefore, func(p *sandbox.Proposal) { p.Findings = high })
			got := decoded(t, h.call(t, http.MethodPost, approvePath(ws, id, p.ID), h.key,
				map[string]any{"scope": "workspace", "acknowledge_findings": true}), http.StatusOK)
			ruleID, _ := got["workspace_rule_id"].(string)
			if !ruleIDPattern.MatchString(ruleID) {
				t.Fatalf("workspace_rule_id = %v; want an id sbr_ and 32 lowercase hex characters", got["workspace_rule_id"])
			}
			want := p
			want.State, want.Scope, want.WorkspaceRuleID = sandbox.ProposalApproved, new("workspace"), &ruleID
			want.DecidedBy, want.DecidedAt = &dana, new(recordStart)
			if !reflect.DeepEqual(got, proposalMap(t, want)) {
				t.Errorf("answer\n%v\nwant\n%v", got, proposalMap(t, want))
			}

			wantRule := sandbox.SandboxRule{
				ID: ruleID, Workspace: "demo", Rule: p.Rule, Summary: p.Summary, Findings: p.Findings,
				FromProposal: &sandbox.RuleOrigin{SandboxID: id, ProposalID: p.ID, Workspace: ws}, CreatedBy: dana, CreatedAt: recordStart,
			}
			stored, err := sandbox.ListRules(ctx, h.Store.Main(), "demo")
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(stored, func(r sandbox.SandboxRule) bool { return r.ID == ruleID })
			if i < 0 || !reflect.DeepEqual(ruleMap(t, stored[i]), ruleMap(t, wantRule)) {
				t.Errorf("the rules of demo %+v; want them to hold %+v", stored, wantRule)
			}
			for _, slug := range []string{"demo", ws} {
				items, _ := listItems(t, h.call(t, http.MethodGet, rulesPath(slug), h.key, nil))
				if !slices.ContainsFunc(items, func(it any) bool { return reflect.DeepEqual(it, ruleMap(t, wantRule)) }) {
					t.Errorf("GET %s lists %v; want it to hold %v", rulesPath(slug), items, ruleMap(t, wantRule))
				}
			}
			if n := h.count(t, `SELECT COUNT(*) FROM sandbox_rules WHERE workspace = ?`, "demo--a"); n != 0 {
				t.Errorf("rules held by the fork = %d; want 0", n)
			}
			rules, calls := h.addedRules(t, id)
			if wantRules := []runtime.Rule{{Name: p.ID, Body: p.Rule}}; calls != 1 || !reflect.DeepEqual(decodedRules(t, rules), decodedRules(t, wantRules)) {
				t.Errorf("the runtime added %v in %d calls; want %v in 1", rules, calls, wantRules)
			}
			if n := storedRecord(t, h.Hub, id).PolicyRevision; n != 1 {
				t.Errorf("policy_revision = %d; want 1", n)
			}

			decisions := h.sandboxEvents(t, sandbox.EventProposalDecided, id)
			wantMeta := map[string]any{"proposal_id": p.ID, "decision": "approved", "acknowledged_findings": true, "scope": "workspace", "rule_id": ruleID}
			if len(decisions) != 1 || decisions[0].workspace == nil || *decisions[0].workspace != ws || decisions[0].session == nil ||
				*decisions[0].session != t1Session || !reflect.DeepEqual(decisions[0].metadata, wantMeta) {
				t.Errorf("decision events %+v; want one of %s in its session with metadata %v", decisions, ws, wantMeta)
			}
			decidedSeq := h.count(t, `SELECT seq FROM hub_events WHERE type = ? AND resource_id = ?`, sandbox.EventProposalDecided, id)
			addSeq := h.count(t, `SELECT seq FROM hub_events WHERE type = ? AND resource_id = ?`, sandbox.EventRuleAdd, ruleID)
			if decidedSeq >= addSeq {
				t.Errorf("proposal_decided has seq %d and rule_add seq %d; want the decision first", decidedSeq, addSeq)
			}
			adds := h.sandboxEvents(t, sandbox.EventRuleAdd, ruleID)
			wantMeta = map[string]any{
				"rule_id": ruleID, "summary": p.Summary, "max_severity": "high",
				"from_proposal": map[string]any{"sandbox_id": id, "proposal_id": p.ID, "workspace": ws},
			}
			if len(adds) != 1 {
				t.Fatalf("rule_add events %+v; want 1", adds)
			}
			if e := adds[0]; e.actorType != "api_key" || e.actorID == nil || *e.actorID != h.dana || e.resourceType != "sandbox_rule" ||
				e.workspace == nil || *e.workspace != "demo" || e.session == nil || *e.session != t1Session || !reflect.DeepEqual(e.metadata, wantMeta) {
				t.Errorf("rule_add event %+v (metadata %v); want one by dana about the sandbox_rule in demo, in its session, metadata %v", e, e.metadata, wantMeta)
			}
		})
	}
}

func TestPromote_NamesAnIdenticalRuleOfTheWorkspaceInsteadOfSavingASecond(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	ctx := context.Background()
	const proposed = `{"host":"same.test","port":443,"tls":{"sni":"same.test"}}`
	for i, tc := range []struct {
		name, held string
		// others is how many other rules the workspace holds.
		others    int
		identical bool
	}{
		{"the same rule object", proposed, 0, true},
		{"the same object with its keys in another order and other whitespace",
			"{ \"tls\" : {\"sni\": \"same.test\"},\n  \"port\": 443, \"host\": \"same.test\" }", 0, true},
		{"50 rules of which one is identical", proposed, 49, true},
		{"a rule object that differs in one port value", `{"host":"same.test","port":444,"tls":{"sni":"same.test"}}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ws := fmt.Sprintf("same%d", i)
			h.storedRepositories(t, ws)
			h.fillRules(t, ws, tc.others)
			held := h.saveRule(t, ws, "", tc.held, recordStart.Add(-time.Hour))
			id := h.liveSandboxIn(t, ws, sandbox.StateReady, nil)
			p := h.seedProposal(t, id, sandbox.ProposalOpen, proposedBefore, func(p *sandbox.Proposal) { p.Rule = json.RawMessage(proposed) })

			got := decoded(t, h.call(t, http.MethodPost, approvePath(ws, id, p.ID), h.key, promote), http.StatusOK)
			ruleID, _ := got["workspace_rule_id"].(string)
			count, err := sandbox.CountRules(ctx, h.Store.Main(), ws)
			if err != nil {
				t.Fatal(err)
			}
			adds := h.count(t, `SELECT COUNT(*) FROM hub_events WHERE type = ? AND workspace = ?`, sandbox.EventRuleAdd, ws)
			wantCount, wantAdds := tc.others+1, 0
			if tc.identical && ruleID != held.ID {
				t.Errorf("workspace_rule_id = %q; want the identical rule's %s", ruleID, held.ID)
			}
			if !tc.identical {
				wantCount, wantAdds = tc.others+2, 1
				if ruleID == held.ID || !ruleIDPattern.MatchString(ruleID) {
					t.Errorf("workspace_rule_id = %q; want a new rule's id, not %s", ruleID, held.ID)
				}
			}
			if count != wantCount || adds != wantAdds {
				t.Errorf("rules = %d and rule_add events = %d; want %d and %d", count, adds, wantCount, wantAdds)
			}
			if got["scope"] != "workspace" || got["state"] != sandbox.ProposalApproved {
				t.Errorf("answer %v; want it approved with scope workspace", got)
			}
			if e := h.sandboxEvents(t, sandbox.EventProposalDecided, id); len(e) != 1 || e[0].metadata["rule_id"] != ruleID {
				t.Errorf("decision events %+v; want one with rule_id %s", e, ruleID)
			}
			if n := storedRecord(t, h.Hub, id).PolicyRevision; n != 1 {
				t.Errorf("policy_revision = %d; want 1", n)
			}
		})
	}
}

func TestPromote_RefusesWithoutAuthorityOnTheParentOrWithTheParentArchivedAndDecidesNothing(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	omar := h.CreateUser(t, "omar")
	h.AddMember(t, h.org, omar)
	h.forkBy(t, "demo--a", "demo", omar, recordStart.Add(24*time.Hour), nil)
	// In turn: the last row archives demo.
	for _, tc := range []struct {
		name, cred string
		archive    bool
		status     int
		errorType  string
	}{
		{"the fork's owner with read access to the parent", h.IssueKey(t, omar), false, http.StatusForbidden, "forbidden"},
		{"the parent's owner with the parent archived", h.key, true, http.StatusConflict, "parent_archived"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := h.liveSandboxIn(t, "demo--a", sandbox.StateReady, nil)
			p := h.seedProposal(t, id, sandbox.ProposalOpen, proposedBefore, nil)
			if tc.archive {
				h.exec(t, `UPDATE workspaces SET status = ? WHERE slug = ?`, string(workspace.StatusArchived), "demo")
			}
			a := h.call(t, http.MethodPost, approvePath("demo--a", id, p.ID), tc.cred, promote)
			if a.status != tc.status || a.err.ErrorType != tc.errorType {
				t.Errorf("= %d %s; want %d %s", a.status, a.body, tc.status, tc.errorType)
			}
			h.undecided(t, p)
			if _, calls := h.addedRules(t, id); calls != 0 {
				t.Errorf("add-rule calls = %d; want 0", calls)
			}
			if n := h.count(t, `SELECT COUNT(*) FROM hub_events WHERE resource_id = ?`, id); n != 0 {
				t.Errorf("events of the sandbox = %d; want none", n)
			}
			again := decoded(t, h.call(t, http.MethodPost, approvePath("demo--a", id, p.ID), tc.cred, map[string]any{"scope": "sandbox"}), http.StatusOK)
			if again["scope"] != "sandbox" {
				t.Errorf("the approval for the sandbox answered scope %v; want sandbox", again["scope"])
			}
		})
	}
}

func TestPromote_RefusesARuleBeyondFiftyAndLetsOneOfTwoTakeTheLastPlace(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	ctx := context.Background()
	count := func(t *testing.T, ws string) int {
		t.Helper()
		n, err := sandbox.CountRules(ctx, h.Store.Main(), ws)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("a workspace with 50 rules", func(t *testing.T) {
		t.Parallel()
		h.fillRules(t, "other", 50)
		id := h.liveSandboxIn(t, "other", sandbox.StateReady, nil)
		p := h.seedProposal(t, id, sandbox.ProposalOpen, proposedBefore, nil)
		a := h.call(t, http.MethodPost, approvePath("other", id, p.ID), h.key, promote)
		if a.status != http.StatusTooManyRequests || a.err.ErrorType != "sandbox_rule_limit_reached" {
			t.Errorf("= %d %s; want 429 sandbox_rule_limit_reached", a.status, a.body)
		}
		if stored, err := sandbox.GetProposal(ctx, h.Store.Main(), id, p.ID); err != nil || stored.State != sandbox.ProposalOpen {
			t.Errorf("stored %+v, %v; want it open", stored, err)
		}
		if n := count(t, "other"); n != 50 {
			t.Errorf("rules = %d; want 50", n)
		}
		if _, calls := h.addedRules(t, id); calls != 0 {
			t.Errorf("add-rule calls = %d; want 0", calls)
		}
		if n := h.count(t, `SELECT COUNT(*) FROM hub_events WHERE resource_id = ? OR (type = ? AND workspace = ?)`, id, sandbox.EventRuleAdd, "other"); n != 0 {
			t.Errorf("events = %d; want none", n)
		}
	})

	t.Run("two promotions released together at 49 rules", func(t *testing.T) {
		t.Parallel()
		const rounds = 20
		h.fork(t, "demo--a", "demo", recordStart.Add(24*time.Hour), nil)
		h.fillRules(t, "demo", 49)
		slugs := [2]string{"demo", "demo--a"}
		ids := [2]string{h.liveSandboxIn(t, slugs[0], sandbox.StateReady, nil), h.liveSandboxIn(t, slugs[1], sandbox.StateReady, nil)}
		for round := range rounds {
			var ps [2]sandbox.Proposal
			for i, id := range ids {
				ps[i] = h.seedProposal(t, id, sandbox.ProposalOpen, proposedBefore, nil)
			}
			var (
				wg      sync.WaitGroup
				release = make(chan struct{})
				answers [2]answer
			)
			for i := range ps {
				wg.Go(func() {
					<-release
					answers[i] = h.call(t, http.MethodPost, approvePath(slugs[i], ids[i], ps[i].ID), h.key, promote)
				})
			}
			close(release)
			wg.Wait()
			statuses := []int{answers[0].status, answers[1].status}
			slices.Sort(statuses)
			if !slices.Equal(statuses, []int{http.StatusOK, http.StatusTooManyRequests}) {
				t.Fatalf("round %d: answers %d %s and %d %s; want one 200 and one 429", round, answers[0].status, answers[0].body, answers[1].status, answers[1].body)
			}
			won, lost := 0, 1
			if answers[0].status != http.StatusOK {
				won, lost = 1, 0
			}
			if answers[lost].err.ErrorType != "sandbox_rule_limit_reached" {
				t.Errorf("the refusal %s; want sandbox_rule_limit_reached", answers[lost].body)
			}
			if n := count(t, "demo"); n != 50 {
				t.Errorf("round %d: rules = %d; want 50", round, n)
			}
			for i, want := range map[int]string{won: sandbox.ProposalApproved, lost: sandbox.ProposalOpen} {
				if stored, err := sandbox.GetProposal(ctx, h.Store.Main(), ids[i], ps[i].ID); err != nil || stored.State != want {
					t.Errorf("round %d: proposal %s stored %+v, %v; want %s", round, ps[i].ID, stored, err, want)
				}
			}
			approved, err := sandbox.ApprovedRules(ctx, h.Store.Main(), ids[lost])
			if err != nil {
				t.Fatal(err)
			}
			rules, _ := h.addedRules(t, ids[lost])
			if slices.ContainsFunc(approved, func(a sandbox.ApprovedRule) bool { return a.ProposalID == ps[lost].ID }) ||
				slices.ContainsFunc(rules, func(r runtime.Rule) bool { return r.Name == ps[lost].ID }) {
				t.Errorf("round %d: the refused rule %s is among the sandbox's approved rules or the runtime's", round, ps[lost].ID)
			}
			// Room for the next round, and in the last for the refused
			// approval again.
			var winner map[string]any
			if err := json.Unmarshal(answers[won].body, &winner); err != nil {
				t.Fatal(err)
			}
			h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
				_, err := sandbox.DeleteRule(ctx, tx, "demo", winner["workspace_rule_id"].(string))
				return err
			})
			if round == rounds-1 {
				again := h.call(t, http.MethodPost, approvePath(slugs[lost], ids[lost], ps[lost].ID), h.key, promote)
				if again.status != http.StatusOK {
					t.Errorf("the refused approval again = %d %s; want 200", again.status, again.body)
				}
			}
		}
	})
}

func TestPromote_RefusedWhileWorkspaceRulesAreOffLeavesEarlierRulesInForce(t *testing.T) {
	t.Parallel()
	h := startCreateHub(t, "allow_workspace_rules = false\n"+agentProfile)
	h.AddMember(t, h.org, h.dana)
	h.repository(t, "demo", workspace.StatusReady)
	omar := h.CreateUser(t, "omar")
	h.AddMember(t, h.org, omar)
	h.forkBy(t, "demo--a", "demo", omar, recordStart.Add(24*time.Hour), nil)
	r1 := h.saveRule(t, "demo", "", `{"host":"r1.test"}`, recordStart.Add(-2*time.Hour))
	r2 := h.saveRule(t, "demo", "", `{"host":"r2.test"}`, recordStart.Add(-time.Hour))
	ctx := context.Background()

	id := h.liveSandbox(t, sandbox.StateReady)
	p := h.seedProposal(t, id, sandbox.ProposalOpen, proposedBefore, nil)
	forkID := h.liveSandboxIn(t, "demo--a", sandbox.StateReady, nil)
	fp := h.seedProposal(t, forkID, sandbox.ProposalOpen, proposedBefore, nil)
	for _, c := range []struct {
		name, slug, cred string
		p                sandbox.Proposal
	}{
		{"the workspace's owner", "demo", h.key, p},
		{"a fork's owner without write access to the parent", "demo--a", h.IssueKey(t, omar), fp},
	} {
		a := h.call(t, http.MethodPost, approvePath(c.slug, c.p.SandboxID, c.p.ID), c.cred, promote)
		if a.status != http.StatusForbidden || a.err.ErrorType != "workspace_rules_disabled" {
			t.Errorf("%s: = %d %s; want 403 workspace_rules_disabled", c.name, a.status, a.body)
		}
		if stored, err := sandbox.GetProposal(ctx, h.Store.Main(), c.p.SandboxID, c.p.ID); err != nil || stored.State != sandbox.ProposalOpen {
			t.Errorf("%s: stored %+v, %v; want it open", c.name, stored, err)
		}
		if _, calls := h.addedRules(t, c.p.SandboxID); calls != 0 {
			t.Errorf("%s: add-rule calls = %d; want 0", c.name, calls)
		}
	}
	if n := h.count(t, `SELECT COUNT(*) FROM hub_events WHERE type IN (?, ?)`, sandbox.EventProposalDecided, sandbox.EventRuleAdd); n != 0 {
		t.Errorf("decision and rule_add events = %d; want none", n)
	}

	items, _ := listItems(t, h.call(t, http.MethodGet, rulesPath("demo"), h.key, nil))
	wantRules(t, items, []sandbox.SandboxRule{r1, r2})
	if a := h.call(t, http.MethodDelete, rulePath("demo", r1.ID), h.key, nil); a.status != http.StatusNoContent {
		t.Errorf("DELETE = %d %s; want 204", a.status, a.body)
	}
	if got, want := sandboxObject(t, h.post(t, "demo", map[string]any{}))["workspace_rules"], nameList(r2.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("workspace_rules of a new sandbox = %v; want %v", got, want)
	}
	if o := decoded(t, h.call(t, http.MethodPost, approvePath("demo", id, p.ID), h.key, map[string]any{"scope": "sandbox"}), http.StatusOK); o["scope"] != "sandbox" {
		t.Errorf("the approval for the sandbox answered scope %v; want sandbox", o["scope"])
	}
}
