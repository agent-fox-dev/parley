package forge

import (
	"context"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// pullPattern is the route of a pull request, below which its state,
// files, checks and reviews are read.
const pullPattern = "/workspaces/:slug/pulls/:number"

// pullRoutes are the routes of a pull request: each checks the :number,
// and a list its page, and reads the forge anew.
func (c *Component) pullRoutes() []server.Route {
	read := func(pattern string, params func(*http.Request, *Call) error, serve func(http.ResponseWriter, *http.Request, *Call) error) server.Route {
		return c.Route(Route{Method: http.MethodGet, Pattern: pullPattern + pattern, Scope: ScopeRead, Need: access.NeedRead, Params: params, Serve: serve})
	}
	return []server.Route{
		read("", readIssueNumber, servePull),
		read("/state", readIssueNumber, servePullState),
		read("/files", readComments, servePullList(api.files)),
		c.Route(Route{Method: http.MethodGet, Pattern: pullPattern + "/checks", Scope: ScopeRead, Need: access.NeedRead,
			Params: readComments, Check: checkAnchor, Serve: servePullList(api.checks)}),
		read("/reviews", readReviews, servePullList(api.reviews)),
	}
}

// readReviews checks the :number and the page of a request for a pull
// request's reviews, whose cursor may name the phase of GitLab's notes.
func readReviews(r *http.Request, call *Call) error {
	if err := readComments(r, call); err != nil {
		return err
	}
	if phase := call.Params.(issueRequest).paging.Phase; phase != "" && phase != phaseNotes {
		return server.InvalidRequest("cursor is malformed or belongs to another query")
	}
	return nil
}

// checkAnchor refuses a check run cursor whose anchor is not what the
// forge's first page can have found: the anchor goes into the path of the
// forge request, so a forged one must not steer it elsewhere.
func checkAnchor(_ *http.Request, call *Call) error {
	if p := call.Params.(issueRequest).paging; p.Continued && !apiOf(call.Resolution.Kind).checkAnchor(p.Anchor) {
		return server.InvalidRequest("cursor is malformed or belongs to another query")
	}
	return nil
}

// servePull answers the pull request :number.
func servePull(w http.ResponseWriter, r *http.Request, call *Call) error {
	pull, err := apiOf(call.Resolution.Kind).pull(r.Context(), call, call.Params.(issueRequest).number)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, pull)
	return nil
}

// servePullState answers where the pull request :number stands, as its
// read gives it.
func servePullState(w http.ResponseWriter, r *http.Request, call *Call) error {
	pull, err := apiOf(call.Resolution.Kind).pull(r.Context(), call, call.Params.(issueRequest).number)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, PullRequestState{Number: pull.Number, State: pull.State, Merged: pull.Merged, HeadSHA: pull.HeadSHA})
	return nil
}

// servePullList is the handler of a list of the pull request :number that
// read reads a page of.
func servePullList[T any](read func(api, context.Context, *Call, int64, Paging) ([]T, *Position, error)) func(http.ResponseWriter, *http.Request, *Call) error {
	return func(w http.ResponseWriter, r *http.Request, call *Call) error {
		req := call.Params.(issueRequest)
		items, next, err := read(apiOf(call.Resolution.Kind), r.Context(), call, req.number, req.paging)
		if err != nil {
			return err
		}
		return WriteList(w, req.paging, items, next)
	}
}
