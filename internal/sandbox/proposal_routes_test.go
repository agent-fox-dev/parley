package sandbox_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
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
