package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/server"
)

// IssueFilter is what an issue list asks the forge for, in the hub's terms,
// which each forge's API translates into its own.
type IssueFilter struct {
	// State is open, closed or all.
	State string
	// Sort is created or updated, and Direction asc or desc.
	Sort      string
	Direction string
	// Labels are the names of the labels every issue carries, separated by
	// commas; "" for any.
	Labels string
	// Assignee is the login of the user the issues are assigned to; "" for
	// any.
	Assignee string
}

// issueList is what an issue list request asks for.
type issueList struct {
	filter IssueFilter
	paging Paging
}

// issueParams are the query parameters of an issue list that name one
// value, with the values they take (nil for any) and their defaults.
var issueParams = []struct {
	name   string
	values []string
	field  func(*IssueFilter) *string
	def    string
}{
	{"state", []string{"open", "closed", "all"}, func(f *IssueFilter) *string { return &f.State }, "open"},
	{"sort", []string{"created", "updated"}, func(f *IssueFilter) *string { return &f.Sort }, "created"},
	{"direction", []string{"asc", "desc"}, func(f *IssueFilter) *string { return &f.Direction }, "desc"},
	{"labels", nil, func(f *IssueFilter) *string { return &f.Labels }, ""},
	{"assignee", nil, func(f *IssueFilter) *string { return &f.Assignee }, ""},
}

// readIssueList checks the filters and the page of an issue list request,
// before the forge is resolved.
func readIssueList(r *http.Request, call *Call) error {
	query := r.URL.Query()
	var f IssueFilter
	for _, p := range issueParams {
		values, field := query[p.name], p.field(&f)
		switch {
		case len(values) == 0:
			*field = p.def
			continue
		case len(values) > 1:
			return server.InvalidRequest(p.name + " must be given at most once")
		case p.values != nil && !slices.Contains(p.values, values[0]):
			return server.InvalidRequest(fmt.Sprintf("%s must be one of %s", p.name, strings.Join(p.values, ", ")))
		}
		*field = values[0]
	}
	if query.Has("labels") && slices.Contains(strings.Split(f.Labels, ","), "") {
		return server.InvalidRequest("labels must be label names separated by commas, none of them empty")
	}
	if query.Has("assignee") && f.Assignee == "" {
		return server.InvalidRequest("assignee must not be empty")
	}
	paging, err := ReadPaging(r)
	if err != nil {
		return err
	}
	call.Params = issueList{filter: f, paging: paging}
	return nil
}

// serveIssues answers a page of the issues of the target's repository that
// match the request's filters, read from the forge anew.
func serveIssues(w http.ResponseWriter, r *http.Request, call *Call) error {
	l := call.Params.(issueList)
	issues, next, err := apiOf(call.Resolution.Kind).issues(r.Context(), call, l.filter, l.paging)
	if err != nil {
		return err
	}
	return WriteList(w, l.paging, issues, next)
}

// serveRepository answers the target's repository, read from the forge
// anew.
func serveRepository(w http.ResponseWriter, r *http.Request, call *Call) error {
	repo, err := apiOf(call.Resolution.Kind).repository(r.Context(), call)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, repo)
	return nil
}

// threadComments is the page size of the comments an issue thread reads,
// and of the comments route's pages that continue them.
const threadComments = 100

// commentsPattern is the route of an issue's comments, on which the
// comments of an issue thread continue.
const commentsPattern = "/workspaces/:slug/issues/:number/comments"

// issueRequest is what a request on one issue asks for: its number, and
// for its comments, their page.
type issueRequest struct {
	number int64
	paging Paging
}

// readIssueNumber checks the :number of a request on one issue.
func readIssueNumber(r *http.Request, call *Call) error {
	n, err := PathNumber(r)
	if err != nil {
		return err
	}
	call.Params = issueRequest{number: n}
	return nil
}

// readComments checks the :number and the page of a request for an
// issue's comments.
func readComments(r *http.Request, call *Call) error {
	n, err := PathNumber(r)
	if err != nil {
		return err
	}
	paging, err := ReadPaging(r)
	if err != nil {
		return err
	}
	call.Params = issueRequest{number: n, paging: paging}
	return nil
}

// serveIssueThread answers the issue :number with the first page of its
// comments, both read from the forge anew. A failed issue read is the
// route's error; a failed comments read still answers the issue, with no
// comments and the failure, mapped and scrubbed, in comments_error, so that
// a caller gets the issue when only its discussion cannot be read.
func (c *Component) serveIssueThread(w http.ResponseWriter, r *http.Request, call *Call) error {
	ctx, n, f := r.Context(), call.Params.(issueRequest).number, apiOf(call.Resolution.Kind)
	issue, err := f.issue(ctx, call, n)
	if err != nil {
		return err
	}
	thread := IssueThread{Issue: issue, Comments: []Comment{}}
	first := Paging{Position: Position{Page: 1, Size: threadComments}}
	comments, next, err := f.comments(ctx, call, n, first)
	switch e, failed := MapError(err, c.deps.Scrubber, call.Resolution.Credential.Token); {
	case failed:
		c.logFailure(r, call, e)
		thread.CommentsError = &CommentsError{Type: e.Type, Message: e.Message}
	case err != nil:
		return err
	default:
		thread.Comments = comments
		if next != nil {
			cursor, err := commentsCursor(r, call, *next)
			if err != nil {
				return err
			}
			thread.CommentsNextCursor = &cursor
		}
	}
	server.WriteJSON(w, http.StatusOK, thread)
	return nil
}

// commentsCursor is the cursor of the comments route that continues the
// comments of the request's issue at next: the route of the same slug,
// number and target, the target only when the request gave one, as the
// comments route's own pages issue it.
func commentsCursor(r *http.Request, call *Call, next Position) (string, error) {
	query := url.Values{}
	if target, ok := r.URL.Query()["target"]; ok {
		query["target"] = target
	}
	params := map[string]string{"slug": call.Slug, "number": server.Param(r, "number")}
	return server.CursorFor(http.MethodGet, commentsPattern, params, query, next)
}

// serveComments answers a page of the comments of the issue :number,
// oldest first, read from the forge anew.
func serveComments(w http.ResponseWriter, r *http.Request, call *Call) error {
	req := call.Params.(issueRequest)
	comments, next, err := apiOf(call.Resolution.Kind).comments(r.Context(), call, req.number, req.paging)
	if err != nil {
		return err
	}
	return WriteList(w, req.paging, comments, next)
}

// getJSON reads the forge's answer to GET path into v.
func getJSON(ctx context.Context, call *Call, path string, v any) error {
	_, err := getJSONResponse(ctx, call, path, v)
	return err
}

// getJSONResponse reads the forge's answer to GET path into v, and returns
// the answer for its header.
func getJSONResponse(ctx context.Context, call *Call, path string, v any) (*Response, error) {
	resp, err := call.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	return resp, Decode(resp, v)
}
