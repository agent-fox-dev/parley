package sandbox_test

import (
	"cmp"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// proposalsPath is the path of the proposal list of the sandbox id of the
// workspace slug.
func proposalsPath(slug, id string) string { return sandboxPath(slug, id) + "/proposals" }

func TestListProposals_AnswersTheProposalsInTheStateAskedForOldestFirst(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	s := h.seedRecord(t, "demo", sandbox.StateReady, nil)
	at := func(sec int) time.Time { return recordStart.Add(time.Duration(sec) * time.Second) }
	// Stored newest first, so that the order is the list's own.
	o3 := h.seedProposal(t, s, sandbox.ProposalOpen, at(3), nil)
	o2 := h.seedProposal(t, s, sandbox.ProposalOpen, at(2), nil)
	o1 := h.seedProposal(t, s, sandbox.ProposalOpen, at(1), func(p *sandbox.Proposal) {
		p.Findings = []runtime.Finding{{Severity: "high", Message: "reaches any host"}, {Severity: "low", Message: "plain HTTP"}}
	})
	a1 := h.seedProposal(t, s, sandbox.ProposalApproved, at(4), func(p *sandbox.Proposal) {
		p.Scope, p.WorkspaceRuleID, p.Comment = new("workspace"), new(sandbox.NewRuleID()), new("needed for the build")
		p.DecidedBy = &sandbox.Creator{ActorType: "api_key", ActorID: h.dana, Username: "dana"}
		p.DecidedAt = new(at(60))
	})
	r1 := h.seedProposal(t, s, sandbox.ProposalRejected, at(5), func(p *sandbox.Proposal) {
		p.DecidedBy = &sandbox.Creator{ActorType: "pat", ActorID: h.dana, Username: "dana"}
		p.DecidedAt = new(at(61))
	})
	// Another sandbox's proposal, which no list of s shows.
	h.seedProposal(t, h.seedRecord(t, "demo", sandbox.StateReady, nil), sandbox.ProposalOpen, at(0), nil)
	gone := h.seedRecord(t, "demo", sandbox.StateDeleted, nil)
	w1 := h.seedProposal(t, gone, sandbox.ProposalWithdrawn, at(1), nil)
	bound := h.IssuePAT(t, identity.PATSpec{UserID: h.dana, Permissions: []string{"sandboxes:read"}, Workspace: "demo"})

	for _, tc := range []struct {
		name, id, query, cred string
		want                  []sandbox.Proposal
		// next is the second page the first page's next_cursor leads to,
		// nil when the first page has no next_cursor.
		next []sandbox.Proposal
	}{
		{name: "no state", id: s, cred: h.key, want: []sandbox.Proposal{o1, o2, o3}},
		{name: "state open", id: s, query: "state=open", cred: h.key, want: []sandbox.Proposal{o1, o2, o3}},
		{name: "state approved", id: s, query: "state=approved", cred: h.key, want: []sandbox.Proposal{a1}},
		{name: "state rejected", id: s, query: "state=rejected", cred: h.key, want: []sandbox.Proposal{r1}},
		{name: "state withdrawn", id: s, query: "state=withdrawn", cred: h.key, want: []sandbox.Proposal{}},
		{name: "a limit of 2", id: s, query: "limit=2", cred: h.key, want: []sandbox.Proposal{o1, o2}, next: []sandbox.Proposal{o3}},
		{name: "a token bound to the workspace", id: s, cred: bound, want: []sandbox.Proposal{o1, o2, o3}},
		{name: "a deleted sandbox", id: gone, query: "state=withdrawn", cred: h.key, want: []sandbox.Proposal{w1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := proposalsPath("demo", tc.id)
			if tc.query != "" {
				path += "?" + tc.query
			}
			items, next := listItems(t, h.call(t, http.MethodGet, path, tc.cred, nil))
			wantItems(t, items, tc.want)
			if (next != "") != (tc.next != nil) {
				t.Fatalf("next_cursor %q; want one only when a second page follows", next)
			}
			if next == "" {
				return
			}
			sep := "&"
			if !strings.Contains(path, "?") {
				sep = "?"
			}
			items, next = listItems(t, h.call(t, http.MethodGet, path+sep+"cursor="+url.QueryEscape(next), tc.cred, nil))
			wantItems(t, items, tc.next)
			if next != "" {
				t.Errorf("the second page's next_cursor = %q; want null", next)
			}
		})
	}
}

// wantItems checks that the decoded list items are the proposal objects
// of want, in order.
func wantItems(t *testing.T, items []any, want []sandbox.Proposal) {
	t.Helper()
	wantMaps := make([]any, len(want))
	for i, p := range want {
		wantMaps[i] = proposalMap(t, p)
	}
	if !reflect.DeepEqual(items, wantMaps) {
		t.Errorf("items\n%v\nwant\n%v", items, wantMaps)
	}
}

func TestListProposals_RefusesAStateOtherThanOneOfTheFour(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	s := h.seedRecord(t, "demo", sandbox.StateReady, nil)
	for _, q := range []string{"state=", "state=OPEN", "state=pending", "state=" + url.QueryEscape("open,approved"), "state=open&state=approved"} {
		a := h.call(t, http.MethodGet, proposalsPath("demo", s)+"?"+q, h.key, nil)
		if a.status != http.StatusBadRequest || a.err.ErrorType != "invalid_request" {
			t.Errorf("GET ?%s = %d %s; want 400 invalid_request", q, a.status, a.body)
		}
	}
}

func TestListProposals_RefusesTheScopeThenTheAccessThenTheSandbox(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	s := h.seedRecord(t, "demo", sandbox.StateReady, nil)
	foreign := h.seedRecord(t, "other", sandbox.StateReady, nil)
	removed := h.seedRecord(t, "demo", sandbox.StateDeleted, nil)
	h.seedProposal(t, removed, sandbox.ProposalWithdrawn, recordStart, nil)
	h.insert(t, func(ctx context.Context, tx *sql.Tx) error {
		if err := sandbox.PurgeNetworkPolicy(ctx, tx, removed); err != nil {
			return err
		}
		return sandbox.Remove(ctx, tx, removed)
	})
	gitRead := h.pat(t, h.dana, "git:read")
	stranger := h.IssueKey(t, h.CreateUser(t, "stranger"))
	for _, tc := range []struct {
		name, slug, id, cred string
		status               int
		errorType            string
	}{
		{"a PAT with git:read only", "demo", s, gitRead, http.StatusForbidden, "insufficient_scope"},
		{"the same PAT on a workspace that does not exist", "nope", s, gitRead, http.StatusForbidden, "insufficient_scope"},
		{"the API key of a user outside the organization", "demo", s, stranger, http.StatusNotFound, "not_found"},
		{"an unknown id", "demo", unknownID, h.key, http.StatusNotFound, "sandbox_not_found"},
		{"the id of another workspace's sandbox", "demo", foreign, h.key, http.StatusNotFound, "sandbox_not_found"},
		{"the id of a removed record", "demo", removed, h.key, http.StatusNotFound, "sandbox_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := h.call(t, http.MethodGet, proposalsPath(tc.slug, tc.id), tc.cred, nil)
			if a.status != tc.status || a.err.ErrorType != tc.errorType {
				t.Errorf("= %d %s; want %d %s", a.status, a.body, tc.status, tc.errorType)
			}
		})
	}
}

// policyRoute is a route of the network-policy feature, with a body its
// handler would accept.
type policyRoute struct {
	method, path string
	body         any
}

func (r policyRoute) String() string { return r.method + " " + r.path }

// policyRoutes are the routes of the network-policy feature, on the
// workspace ws, naming ids no sandbox, proposal or rule has.
var policyRoutes = []policyRoute{
	{http.MethodGet, proposalsPath("ws", unknownID), nil},
	{http.MethodPost, approvePath("ws", unknownID, unknownProposal), map[string]any{}},
	{http.MethodPost, rejectPath("ws", unknownID, unknownProposal), map[string]any{}},
}

func TestPolicyRoutes_AnswerDisabledWhileSandboxesAreOffBeforeAnyOtherCheck(t *testing.T) {
	t.Parallel()
	h := startProbeHub(t, false)
	org := h.CreateOrg(t, "elsewhere")
	h.AddWorkspace(t, access.Record{Slug: "ws2", OwnerID: h.owner, OrgID: org, Kind: access.KindRepository, Status: access.StatusReady})
	callers := []struct{ name, cred string }{
		{"an API key", h.IssueKey(t, h.owner)},
		{"a PAT without sandbox scopes", h.pat(t, h.owner, "git:read")},
		{"a PAT bound to another workspace", h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"sandboxes:write"}, Workspace: "ws2"})},
	}
	for _, r := range policyRoutes {
		for _, c := range callers {
			t.Run(r.String()+"/"+c.name, func(t *testing.T) {
				a := h.call(t, r.method, r.path, c.cred, r.body)
				if a.status != http.StatusNotFound || a.err.ErrorType != "sandboxes_disabled" {
					t.Errorf("= %d %s; want 404 sandboxes_disabled", a.status, a.body)
				}
			})
		}
		t.Run(r.String()+"/without a credential", func(t *testing.T) {
			a := h.call(t, r.method, r.path, "", r.body)
			if a.status != http.StatusUnauthorized || a.err.ErrorType != "unauthenticated" {
				t.Errorf("= %d %s; want 401 unauthenticated", a.status, a.body)
			}
		})
	}
}

// unknownProposal is a well-formed proposal id no proposal has.
const unknownProposal = "sbp_00000000000000000000000000000000"

// approvePath is the path of the approval of the proposal pid of the
// sandbox id of the workspace slug.
func approvePath(slug, id, pid string) string {
	return proposalsPath(slug, id) + "/" + pid + "/approve"
}

// rejectPath is the path of the rejection of the proposal pid of the
// sandbox id of the workspace slug.
func rejectPath(slug, id, pid string) string {
	return proposalsPath(slug, id) + "/" + pid + "/reject"
}

// decisionPaths are the paths of the two decisions of a proposal, by the
// decision's name.
var decisionPaths = []struct {
	name string
	path func(slug, id, pid string) string
}{{"approve", approvePath}, {"reject", rejectPath}}

// proposedBefore is when the proposals of the decision tests were made: a
// minute before the hub's clock, which decides them at recordStart.
var proposedBefore = recordStart.Add(-time.Minute)

func TestApprove_AddsTheRuleToTheSandboxAndRecordsTheDecision(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	high := []runtime.Finding{{Severity: "high", Message: "reaches any host"}}
	medium := []runtime.Finding{{Severity: "medium", Message: "plain HTTP"}}
	for _, tc := range []struct {
		name        string
		body        map[string]any
		findings    []runtime.Finding
		comment     *string
		acknowledge bool
	}{
		{name: "an empty body", body: map[string]any{}},
		{name: "scope sandbox with a comment", body: map[string]any{"scope": "sandbox", "comment": "ok"}, comment: new("ok")},
		{name: "a high finding acknowledged", body: map[string]any{"acknowledge_findings": true}, findings: high, acknowledge: true},
		{name: "a medium finding", body: map[string]any{}, findings: medium},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id := h.liveSandbox(t, sandbox.StateReady)
			p := h.seedProposal(t, id, sandbox.ProposalOpen, proposedBefore, func(p *sandbox.Proposal) { p.Findings = tc.findings })
			a := h.call(t, http.MethodPost, approvePath("demo", id, p.ID), h.key, tc.body)

			want := p
			want.State, want.Scope, want.Comment = sandbox.ProposalApproved, new("sandbox"), tc.comment
			want.DecidedBy = &sandbox.Creator{ActorType: "api_key", ActorID: h.dana, Username: "dana"}
			want.DecidedAt = new(recordStart)
			if got := decoded(t, a, http.StatusOK); !reflect.DeepEqual(got, proposalMap(t, want)) {
				t.Errorf("answer\n%v\nwant\n%v", got, proposalMap(t, want))
			}
			stored, err := sandbox.GetProposal(context.Background(), h.Store.Main(), id, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(proposalMap(t, stored), proposalMap(t, want)) {
				t.Errorf("stored %+v; want %+v", stored, want)
			}
			rules, calls := h.addedRules(t, id)
			if wantRules := []runtime.Rule{{Name: p.ID, Body: p.Rule}}; calls != 1 || !reflect.DeepEqual(decodedRules(t, rules), decodedRules(t, wantRules)) {
				t.Errorf("the runtime added %v in %d calls; want %v in 1", rules, calls, wantRules)
			}
			if n := storedRecord(t, h.Hub, id).PolicyRevision; n != 1 {
				t.Errorf("policy_revision = %d; want 1", n)
			}
			events := h.sandboxEvents(t, sandbox.EventProposalDecided, id)
			wantMeta := map[string]any{"proposal_id": p.ID, "decision": "approved", "acknowledged_findings": tc.acknowledge, "scope": "sandbox", "rule_id": nil}
			if len(events) != 1 {
				t.Fatalf("decision events %v; want 1", events)
			}
			if e := events[0]; e.actorType != "api_key" || e.actorID == nil || *e.actorID != h.dana || e.resourceType != "sandbox" ||
				e.workspace == nil || *e.workspace != "demo" || e.session != nil || !reflect.DeepEqual(e.metadata, wantMeta) {
				t.Errorf("decision event %+v (metadata %v); want actor api_key %s on sandbox %s of demo in no session, metadata %v", e, e.metadata, h.dana, id, wantMeta)
			}
			if n := h.count(t, `SELECT COUNT(*) FROM sandbox_rules`); n != 0 {
				t.Errorf("workspace sandbox rules = %d; want 0", n)
			}
		})
	}
}

func TestApprove_StoresTheRuleOfAnEndedSandboxForItsNextStartWithoutTheRuntime(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	for _, state := range []string{sandbox.StateStopped, sandbox.StateCompleted, sandbox.StateFailed} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			id := h.seedRecord(t, "demo", state, nil)
			p := h.seedProposal(t, id, sandbox.ProposalOpen, proposedBefore, nil)
			if o := decoded(t, h.call(t, http.MethodPost, approvePath("demo", id, p.ID), h.key, nil), http.StatusOK); o["state"] != sandbox.ProposalApproved {
				t.Errorf("answer state = %v; want approved", o["state"])
			}
			if _, calls := h.addedRules(t, id); calls != 0 {
				t.Errorf("add-rule calls = %d; want 0", calls)
			}
			rec := storedRecord(t, h.Hub, id)
			if rec.PolicyRevision != 1 {
				t.Errorf("policy_revision = %d; want 1", rec.PolicyRevision)
			}
			policy, err := h.svc.ComposePolicy(context.Background(), rec)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(policy.Added); n == 0 || !reflect.DeepEqual(decodedRules(t, policy.Added[n-1:]), decodedRules(t, []runtime.Rule{{Name: p.ID, Body: p.Rule}})) {
				t.Errorf("the next start's added rules %v; want them to end with %s", decodedRules(t, policy.Added), p.Rule)
			}
		})
	}
}

func TestApprove_RefusesAHighFindingThatIsNotAcknowledged(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	for _, scope := range []string{"sandbox", "workspace"} {
		for _, ack := range []struct {
			name string
			body map[string]any
		}{
			{"absent", map[string]any{"scope": scope}},
			{"false", map[string]any{"scope": scope, "acknowledge_findings": false}},
		} {
			t.Run("scope "+scope+"/acknowledge_findings "+ack.name, func(t *testing.T) {
				t.Parallel()
				id := h.liveSandbox(t, sandbox.StateReady)
				p := h.seedProposal(t, id, sandbox.ProposalOpen, proposedBefore, func(p *sandbox.Proposal) {
					p.Findings = []runtime.Finding{{Severity: "low", Message: "plain HTTP"}, {Severity: "high", Message: "reaches any host"}}
				})
				a := h.call(t, http.MethodPost, approvePath("demo", id, p.ID), h.key, ack.body)
				if a.status != http.StatusConflict || a.err.ErrorType != "findings_not_acknowledged" {
					t.Errorf("= %d %s; want 409 findings_not_acknowledged", a.status, a.body)
				}
				h.undecided(t, p)
			})
		}
	}
}

func TestApprove_RefusesWhatTheRuntimeRejectsOrCannotBeAskedAndDecidesNothing(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	long := strings.Repeat("x", 5000)
	for _, tc := range []struct {
		name      string
		fail      func(p sandbox.Proposal)
		status    int
		errorType string
		// says is in the message, which the runtime's reason makes.
		says string
	}{
		{"a rejection", func(p sandbox.Proposal) { h.rt.RejectPolicies(adds(p.ID), "destination denied by base policy") },
			http.StatusConflict, "conflict", "destination denied by base policy"},
		{"a rejection with a reason of 5,000 bytes", func(p sandbox.Proposal) { h.rt.RejectPolicies(adds(p.ID), long) },
			http.StatusConflict, "conflict", "xxxxxxxx"},
		{"a runtime that cannot be reached", func(sandbox.Proposal) {
			h.rt.FailOperation(runtime.OpAddPolicyRule, runtime.ErrUnavailable, "connection refused")
		}, http.StatusBadGateway, "sandbox_runtime_error", "connection refused"},
	} {
		// In turn: the runtime's failure is the fake's, shared by the rows.
		t.Run(tc.name, func(t *testing.T) {
			id := h.liveSandbox(t, sandbox.StateReady)
			p := h.seedProposal(t, id, sandbox.ProposalOpen, proposedBefore, nil)
			tc.fail(p)
			a := h.call(t, http.MethodPost, approvePath("demo", id, p.ID), h.key, map[string]any{"scope": "sandbox"})
			h.rt.RejectPolicies(nil, "")
			h.rt.FailOperation(runtime.OpAddPolicyRule, nil, "")
			if a.status != tc.status || a.err.ErrorType != tc.errorType {
				t.Fatalf("= %d %s; want %d %s", a.status, a.body, tc.status, tc.errorType)
			}
			if !strings.Contains(a.err.Message, tc.says) || len(a.err.Message) > 1024 {
				t.Errorf("message %q (%d bytes); want one of at most 1,024 bytes that says %q", a.err.Message, len(a.err.Message), tc.says)
			}
			h.undecided(t, p)
			if again := h.call(t, http.MethodPost, approvePath("demo", id, p.ID), h.key, map[string]any{"scope": "sandbox"}); again.status != http.StatusOK {
				t.Errorf("the approval again = %d %s; want 200", again.status, again.body)
			}
		})
	}
}

// adds matches a policy value that adds the rule named name.
func adds(name string) func(runtime.Policy) bool {
	return func(p runtime.Policy) bool {
		return slices.ContainsFunc(p.Added, func(r runtime.Rule) bool { return r.Name == name })
	}
}

func TestReject_RecordsTheDecisionAndThenTellsTheRuntime(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	for _, tc := range []struct {
		name     string
		state    string
		body     map[string]any
		findings []runtime.Finding
		comment  *string
	}{
		{name: "an empty body", state: sandbox.StateReady, body: map[string]any{}},
		{name: "a comment", state: sandbox.StateReady, body: map[string]any{"comment": "not this host"}, comment: new("not this host")},
		{name: "a high finding", state: sandbox.StateReady, body: map[string]any{},
			findings: []runtime.Finding{{Severity: "high", Message: "reaches any host"}}},
		{name: "a stopped sandbox", state: sandbox.StateStopped, body: map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id := h.liveSandbox(t, tc.state)
			p := h.pendingProposal(t, id, func(p *sandbox.Proposal) { p.Findings = tc.findings })
			a := h.call(t, http.MethodPost, rejectPath("demo", id, p.ID), h.key, tc.body)

			want := p
			want.State, want.Comment = sandbox.ProposalRejected, tc.comment
			want.DecidedBy = &sandbox.Creator{ActorType: "api_key", ActorID: h.dana, Username: "dana"}
			want.DecidedAt = new(recordStart)
			if got := decoded(t, a, http.StatusOK); !reflect.DeepEqual(got, proposalMap(t, want)) {
				t.Errorf("answer\n%v\nwant\n%v", got, proposalMap(t, want))
			}
			stored, err := sandbox.GetProposal(context.Background(), h.Store.Main(), id, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(proposalMap(t, stored), proposalMap(t, want)) {
				t.Errorf("stored %+v; want %+v", stored, want)
			}
			if calls, gone := h.rejected(t, id, p); calls != 1 || !slices.Equal(gone, []string{p.RuntimeID}) {
				t.Errorf("the runtime got %d reject calls and no longer lists %v; want 1 call that rejected %s", calls, gone, p.RuntimeID)
			}
			if _, calls := h.addedRules(t, id); calls != 0 {
				t.Errorf("add-rule calls = %d; want 0", calls)
			}
			if n := storedRecord(t, h.Hub, id).PolicyRevision; n != 0 {
				t.Errorf("policy_revision = %d; want 0", n)
			}
			events := h.sandboxEvents(t, sandbox.EventProposalDecided, id)
			wantMeta := map[string]any{"proposal_id": p.ID, "decision": "rejected", "acknowledged_findings": false, "scope": nil, "rule_id": nil}
			if len(events) != 1 {
				t.Fatalf("decision events %v; want 1", events)
			}
			if e := events[0]; e.actorType != "api_key" || e.actorID == nil || *e.actorID != h.dana || e.resourceType != "sandbox" ||
				e.workspace == nil || *e.workspace != "demo" || !reflect.DeepEqual(e.metadata, wantMeta) {
				t.Errorf("decision event %+v (metadata %v); want actor api_key %s on sandbox %s of demo, metadata %v", e, e.metadata, h.dana, id, wantMeta)
			}
		})
	}
}

func TestReject_KeepsTheDecisionAndLogsWhenTheRuntimeCannotBeTold(t *testing.T) {
	t.Parallel()
	const exposed = "supersecretvalue"
	logs := &hubtest.LogBuffer{}
	h := startProposalHub(t, func(d *sandbox.Deps) { d.Logger = slog.New(slog.NewJSONHandler(logs, nil)) })
	for _, tc := range []struct {
		name string
		kind error
	}{
		{"a runtime that cannot be reached", runtime.ErrUnavailable},
		{"a runtime that answers an error", runtime.ErrInvalid},
	} {
		// In turn: the runtime's failure is the fake's, shared by the rows.
		t.Run(tc.name, func(t *testing.T) {
			id := h.liveSandbox(t, sandbox.StateReady)
			h.svc.SetValues(id, sandbox.Values{Exposed: []string{exposed}})
			p := h.pendingProposal(t, id, nil)
			h.rt.FailOperation(runtime.OpRejectProposal, tc.kind, "the gateway refused token "+exposed)
			a := h.call(t, http.MethodPost, rejectPath("demo", id, p.ID), h.key, nil)
			h.rt.FailOperation(runtime.OpRejectProposal, nil, "")
			if o := decoded(t, a, http.StatusOK); o["state"] != sandbox.ProposalRejected {
				t.Errorf("answer state = %v; want rejected", o["state"])
			}
			if stored, err := sandbox.GetProposal(context.Background(), h.Store.Main(), id, p.ID); err != nil || stored.State != sandbox.ProposalRejected {
				t.Errorf("stored %+v, %v; want it rejected", stored, err)
			}
			if events := h.sandboxEvents(t, sandbox.EventProposalDecided, id); len(events) != 1 {
				t.Errorf("decision events %v; want 1", events)
			}
			var lines []string
			for line := range strings.Lines(logs.String()) {
				if strings.Contains(line, id) {
					lines = append(lines, line)
				}
			}
			if len(lines) != 1 || !strings.Contains(lines[0], p.ID) || strings.Contains(lines[0], exposed) {
				t.Errorf("the log lines of the sandbox %q; want one that names the proposal %s and not the exposed value", lines, p.ID)
			}
		})
	}
}

func TestDecisions_RefuseAProposalThatIsDecidedAndChangeNothing(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	for _, d := range decisionPaths {
		for _, state := range []string{sandbox.ProposalApproved, sandbox.ProposalRejected, sandbox.ProposalWithdrawn} {
			t.Run(d.name+"/"+state, func(t *testing.T) {
				t.Parallel()
				id := h.liveSandbox(t, sandbox.StateReady)
				p := h.seedProposal(t, id, state, proposedBefore, func(p *sandbox.Proposal) {
					if state == sandbox.ProposalWithdrawn {
						return
					}
					p.DecidedBy = &sandbox.Creator{ActorType: "pat", ActorID: h.dana, Username: "dana"}
					p.DecidedAt, p.Comment = new(proposedBefore), new("decided before")
					if state == sandbox.ProposalApproved {
						p.Scope = new(sandbox.ScopeSandbox)
					}
				})
				before, err := sandbox.GetProposal(context.Background(), h.Store.Main(), id, p.ID)
				if err != nil {
					t.Fatal(err)
				}
				a := h.call(t, http.MethodPost, d.path("demo", id, p.ID), h.key, map[string]any{"comment": "again"})
				if a.status != http.StatusConflict || a.err.ErrorType != "proposal_decided" {
					t.Errorf("= %d %s; want 409 proposal_decided", a.status, a.body)
				}
				after, err := sandbox.GetProposal(context.Background(), h.Store.Main(), id, p.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(after, before) {
					t.Errorf("the proposal became %+v; want %+v", after, before)
				}
				if n := h.count(t, `SELECT COUNT(*) FROM hub_events WHERE resource_id = ?`, id); n != 0 {
					t.Errorf("events of the sandbox = %d; want none", n)
				}
				if calls, _ := h.rejected(t, id); calls != 0 {
					t.Errorf("reject calls = %d; want 0", calls)
				}
			})
		}
	}
}

func TestDecisions_RefuseAtTheFirstFailingCheckAndDecideNothing(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	s := h.seedRecord(t, "demo", sandbox.StateReady, func(r *sandbox.Record) {
		r.HubAccess = &sandbox.HubAccess{Scopes: []string{"git:read", "git:write"}, ExpiresAt: r.ExpiresAt}
	})
	ownCredential := h.hubCredential(t, s)
	other := h.seedProposal(t, h.seedRecord(t, "demo", sandbox.StateReady, nil), sandbox.ProposalOpen, proposedBefore, nil)
	bound := h.IssuePAT(t, identity.PATSpec{UserID: h.dana, Permissions: []string{"sandboxes:write"}, Workspace: "demo"})
	readOnly := h.pat(t, h.dana, "sandboxes:read")
	omar := h.CreateUser(t, "omar")
	h.AddMember(t, h.org, omar)
	member := h.IssueKey(t, omar)
	type row struct {
		name, cred string
		body       any
		// pid is the proposal id the request names; "" for the row's own.
		pid       string
		status    int
		errorType string
	}
	rows := []row{
		{name: "a PAT bound to the workspace with sandboxes:write", cred: bound, body: map[string]any{}, status: 403, errorType: "token_bound"},
		{name: "a bound PAT with a body that is not an object", cred: bound, body: []any{}, status: 403, errorType: "token_bound"},
		{name: "the hub credential of the sandbox", cred: ownCredential, body: map[string]any{}, status: 403, errorType: "token_bound"},
		{name: "a PAT with sandboxes:read only", cred: readOnly, body: map[string]any{}, status: 403, errorType: "insufficient_scope"},
		{name: "an organization member with read access", cred: member, body: map[string]any{}, status: 403, errorType: "forbidden"},
		{name: "a body that is not an object", cred: h.key, body: []any{}, status: 400, errorType: "invalid_request"},
		{name: "a comment that is not a string", cred: h.key, body: map[string]any{"comment": 5}, status: 400, errorType: "invalid_request"},
		{name: "an unknown proposal id", cred: h.key, body: map[string]any{}, pid: unknownProposal, status: 404, errorType: "not_found"},
		{name: "a proposal id of another sandbox", cred: h.key, body: map[string]any{}, pid: other.ID, status: 404, errorType: "not_found"},
	}
	approveOnly := []row{
		{name: "a scope other than sandbox or workspace", cred: h.key, body: map[string]any{"scope": "org"}, status: 400, errorType: "invalid_request"},
		{name: "an acknowledge_findings that is not a boolean", cred: h.key, body: map[string]any{"acknowledge_findings": "yes"}, status: 400, errorType: "invalid_request"},
	}
	for _, d := range decisionPaths {
		table := rows
		if d.name == "approve" {
			table = append(slices.Clone(rows), approveOnly...)
		}
		for _, tc := range table {
			t.Run(d.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				p := h.seedProposal(t, s, sandbox.ProposalOpen, proposedBefore, nil)
				pid := cmp.Or(tc.pid, p.ID)
				a := h.call(t, http.MethodPost, d.path("demo", s, pid), tc.cred, tc.body)
				if a.status != tc.status || a.err.ErrorType != tc.errorType {
					t.Errorf("= %d %s; want %d %s", a.status, a.body, tc.status, tc.errorType)
				}
				// The proposal's own 404, not a route's.
				if tc.status == http.StatusNotFound && !strings.Contains(a.err.Message, pid) {
					t.Errorf("message %q; want one that names the proposal %s", a.err.Message, pid)
				}
				h.undecided(t, p)
				h.undecided(t, other)
				if calls, _ := h.rejected(t, s); calls != 0 {
					t.Errorf("reject calls = %d; want 0", calls)
				}
			})
		}
	}
}

func TestDecisions_ApplyExactlyOneOfTwoReleasedTogether(t *testing.T) {
	t.Parallel()
	h := startProposalHub(t)
	const rounds = 20
	for _, pair := range [][2]int{{0, 0}, {0, 1}, {1, 1}} {
		first, second := decisionPaths[pair[0]], decisionPaths[pair[1]]
		t.Run(first.name+" and "+second.name, func(t *testing.T) {
			t.Parallel()
			id := h.liveSandbox(t, sandbox.StateReady)
			for range rounds {
				p := h.pendingProposal(t, id, nil)
				revision := storedRecord(t, h.Hub, id).PolicyRevision
				var (
					wg      sync.WaitGroup
					release = make(chan struct{})
					answers [2]answer
				)
				for i, d := range []func(slug, id, pid string) string{first.path, second.path} {
					wg.Go(func() {
						<-release
						answers[i] = h.call(t, http.MethodPost, d("demo", id, p.ID), h.key, nil)
					})
				}
				close(release)
				wg.Wait()
				statuses := []int{answers[0].status, answers[1].status}
				slices.Sort(statuses)
				if !slices.Equal(statuses, []int{http.StatusOK, http.StatusConflict}) {
					t.Fatalf("answers %d %s and %d %s; want one 200 and one 409", answers[0].status, answers[0].body, answers[1].status, answers[1].body)
				}
				for _, a := range answers {
					if a.status == http.StatusConflict && a.err.ErrorType != "proposal_decided" {
						t.Errorf("the refusal %s; want proposal_decided", a.body)
					}
				}
				stored, err := sandbox.GetProposal(context.Background(), h.Store.Main(), id, p.ID)
				if err != nil {
					t.Fatal(err)
				}
				var decisions []any
				for _, e := range h.sandboxEvents(t, sandbox.EventProposalDecided, id) {
					if e.metadata["proposal_id"] == p.ID {
						decisions = append(decisions, e.metadata["decision"])
					}
				}
				if !slices.Equal(decisions, []any{stored.State}) {
					t.Errorf("decision events %v of a proposal stored %s; want one that says so", decisions, stored.State)
				}
				rose := storedRecord(t, h.Hub, id).PolicyRevision - revision
				if want := map[string]int{sandbox.ProposalApproved: 1, sandbox.ProposalRejected: 0}[stored.State]; rose != want {
					t.Errorf("policy_revision rose by %d for a proposal stored %s; want %d", rose, stored.State, want)
				}
			}
		})
	}
}
