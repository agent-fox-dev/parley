package forge

import (
	"context"
	"net/http"
	"strings"
	"unicode"

	"github.com/agent-fox-dev/parley/internal/server"
)

// issueInput is the body of an issue create: each field is nil when it is
// not set.
type issueInput struct {
	Title     *string   `json:"title"`
	Body      *string   `json:"body"`
	Labels    *[]string `json:"labels"`
	Assignees *[]string `json:"assignees"`
}

// readIssueCreate reads the body of an issue create, whose title is
// required and not blank.
func readIssueCreate(r *http.Request, call *Call) error {
	var in issueInput
	if err := server.DecodeJSON(r, &in); err != nil {
		return err
	}
	if in.Title == nil {
		return server.InvalidRequest("title is required")
	}
	if blank(*in.Title) {
		return server.InvalidRequest("title must not be blank")
	}
	call.Params = in
	return nil
}

// blank reports whether s is empty or made of white space only.
func blank(s string) bool {
	return strings.TrimFunc(s, unicode.IsSpace) == ""
}

// createIssue creates the issue the request's body describes and answers it
// as the forge returns it.
func createIssue(ctx context.Context, call *Call) (written, error) {
	issue, err := writerOf(call.Resolution.Kind).createIssue(ctx, call, call.Params.(issueInput))
	if err != nil {
		return written{}, err
	}
	return written{status: http.StatusCreated, body: issue}, nil
}

// githubIssueWrite is the body of an issue create on GitHub: the fields
// that are set.
type githubIssueWrite struct {
	Title     *string   `json:"title,omitempty"`
	Body      *string   `json:"body,omitempty"`
	Labels    *[]string `json:"labels,omitempty"`
	Assignees *[]string `json:"assignees,omitempty"`
}

func (github) createIssue(ctx context.Context, call *Call, in issueInput) (Issue, error) {
	var g githubIssue
	body := githubIssueWrite(in)
	if err := call.sendJSON(ctx, http.MethodPost, githubRepo(call.Resolution.Repository)+"/issues", body, &g); err != nil {
		return Issue{}, err
	}
	return g.issue(), nil
}

// gitlabIssueWrite is the body of an issue create on GitLab: the fields
// that are set, the labels as one string of names separated by commas,
// which is how GitLab takes them. GitLab assigns by user id, not by name,
// so the assignees are not sent.
type gitlabIssueWrite struct {
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	Labels      *string `json:"labels,omitempty"`
}

// gitlabLabels is the label names in GitLab's form; nil when they are not
// set.
func gitlabLabels(labels *[]string) *string {
	if labels == nil {
		return nil
	}
	return new(strings.Join(*labels, ","))
}

func (gitlab) createIssue(ctx context.Context, call *Call, in issueInput) (Issue, error) {
	var g gitlabIssue
	body := gitlabIssueWrite{Title: in.Title, Description: in.Body, Labels: gitlabLabels(in.Labels)}
	if err := call.sendJSON(ctx, http.MethodPost, gitlabProject(call.Resolution.Repository)+"/issues", body, &g); err != nil {
		return Issue{}, err
	}
	return g.issue(), nil
}
