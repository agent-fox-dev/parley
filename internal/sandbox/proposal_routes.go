package sandbox

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
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
