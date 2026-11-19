package sandbox

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// proposalStates are the values the state query of the proposal list
// takes, written exactly so.
var proposalStates = []string{ProposalOpen, ProposalApproved, ProposalRejected, ProposalWithdrawn}

// ProposalsRoute declares GET /workspaces/:slug/sandboxes/:id/proposals:
// the proposals of the sandbox id of the workspace, in every state of the
// sandbox, deleted included, that are in the state the query state names
// (open without one), oldest first by created_at and then id, paged (see
// listProposals). It needs the scope sandboxes:read of a PAT and read
// access to the workspace, which a token bound to the workspace has, and
// reads the store only, so it keeps answering while the runtime is
// unreachable.
func (s *Service) ProposalsRoute() server.Route {
	return s.WorkspaceRoute(KindRead, server.Route{
		Method:  http.MethodGet,
		Pattern: "/workspaces/:slug/sandboxes/:id/proposals",
		Handler: s.listProposals,
	})
}

// listProposals answers GET /workspaces/:slug/sandboxes/:id/proposals. The
// query is checked before the sandbox is read: a state other than exactly
// one of the four, or a bad limit or cursor, is 400 invalid_request; then
// a sandbox the workspace has no record of is 404 sandbox_not_found.
func (s *Service) listProposals(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	state, err := readProposalState(r.URL.Query())
	if err != nil {
		return err
	}
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the proposal list found no access decision")
	}
	ctx := r.Context()
	rec, err := s.readSandbox(ctx, d.Record.Slug, server.Param(r, "id"))
	if err != nil {
		return err
	}
	var pos Position
	after := &pos
	if ok, err := page.After(&pos); err != nil {
		return err
	} else if !ok {
		after = nil
	}
	ps, err := ListProposals(ctx, s.deps.Store.Main(), rec.ID, state, after, page.Limit+1)
	if err != nil {
		return err
	}
	items := make([]proposalJSON, len(ps))
	for i, p := range ps {
		items[i] = proposalObject(p)
	}
	return server.WritePage(w, page, items, func(o proposalJSON) any {
		return Position{CreatedAt: time.Time(o.CreatedAt), ID: o.ID}
	})
}

// readProposalState reads the state query of the proposal list: absent for
// open, or given once as exactly one of the four states.
func readProposalState(q url.Values) (string, error) {
	given, ok := q["state"]
	if !ok {
		return ProposalOpen, nil
	}
	if len(given) != 1 || !slices.Contains(proposalStates, given[0]) {
		return "", server.InvalidRequest("state must be one of " + strings.Join(proposalStates, ", "))
	}
	return given[0], nil
}

// ApproveRoute declares POST
// /workspaces/:slug/sandboxes/:id/proposals/:proposal_id/approve: it
// approves the open proposal proposal_id of the sandbox id for the caller,
// adding its rule to the sandbox's policy, and answers with the proposal
// (see serveApprove). It needs the scope sandboxes:write of a PAT and
// write access to the workspace, and refuses every workspace-bound token,
// so that a sandbox's own hub credential cannot approve what its workload
// asked for: the binding guard answers 403 token_bound before the scope
// check.
func (s *Service) ApproveRoute() server.Route {
	return s.WorkspaceRoute(KindWrite, server.Route{
		Method:  http.MethodPost,
		Pattern: "/workspaces/:slug/sandboxes/:id/proposals/:proposal_id/approve",
		Marks:   map[string]string{access.MarkRefusesBound: ""},
		Handler: s.serveApprove,
	})
}

// serveApprove answers the approval of a proposal: the body is read
// before anything else (see readDecision), then the decision runs (see
// decide) and the answer is the proposal as stored.
func (s *Service) serveApprove(w http.ResponseWriter, r *http.Request) error {
	return s.serveDecision(w, r, ProposalApproved)
}

// serveDecision answers a decision of the state, approved or rejected, of
// a proposal.
func (s *Service) serveDecision(w http.ResponseWriter, r *http.Request, state string) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return server.InvalidRequest("request body could not be read")
	}
	d, err := readDecision(raw, state)
	if err != nil {
		return err
	}
	ad, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the proposal decision found no access decision")
	}
	ctx := r.Context()
	caller, _ := identity.PrincipalOf(ctx)
	p, err := s.decide(ctx, caller, ad.Record.Slug, server.Param(r, "id"), server.Param(r, "proposal_id"), d)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, proposalObject(p))
	return nil
}

// readDecision reads the body of a decision of the state: a JSON object,
// none being the empty one, whose member comment, when given, is a string,
// and, for an approval, whose acknowledge_findings is a boolean and whose
// scope is sandbox or workspace, sandbox when absent. A member that is
// null counts as absent. It answers the first member it refuses with 400
// invalid_request.
func readDecision(raw []byte, state string) (decision, error) {
	body, err := readBody(raw)
	if err != nil {
		return decision{}, err
	}
	d := decision{State: state}
	if c, ok := body["comment"]; ok {
		var comment string
		if json.Unmarshal(c, &comment) != nil {
			return decision{}, server.InvalidRequest("comment: must be a string")
		}
		d.Comment = &comment
	}
	if state != ProposalApproved {
		return d, nil
	}
	if a, ok := body["acknowledge_findings"]; ok && json.Unmarshal(a, &d.Acknowledge) != nil {
		return decision{}, server.InvalidRequest("acknowledge_findings: must be a boolean")
	}
	d.Scope = ScopeSandbox
	if sc, ok := body["scope"]; ok {
		if json.Unmarshal(sc, &d.Scope) != nil || (d.Scope != ScopeSandbox && d.Scope != ScopeWorkspace) {
			return decision{}, server.InvalidRequest("scope: must be sandbox or workspace")
		}
	}
	return d, nil
}
