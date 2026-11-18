package forge

import (
	"cmp"
	"context"
	"net/http"
	"strings"
)

// issueInput is the body of an issue create: each field is nil when it is
// not set.
type issueInput struct {
	Title     *string
	Body      *string
	Labels    *[]string
	Assignees *[]string
}

func (in issueInput) texts() []text { return []text{{"body", in.Body}} }

// readIssueCreate reads the body of an issue create: a title, required and
// not blank; a body, which is measured; labels and assignees, lists of
// names.
func readIssueCreate(r *http.Request, call *Call) error {
	m, err := readObject(r)
	if err != nil {
		return err
	}
	var in issueInput
	if err := cmp.Or(m.required("title", &in.Title), m.text("body", &in.Body),
		m.names("labels", &in.Labels), m.names("assignees", &in.Assignees)); err != nil {
		return err
	}
	call.Params = in
	return nil
}

// createIssue creates the issue the request's body describes and answers it
// as the forge returns it; its event has the new number and the issue's
// web URL.
func createIssue(ctx context.Context, call *Call) (written, error) {
	issue, err := writerOf(call.Resolution.Kind).createIssue(ctx, call, call.Params.(issueInput))
	if err != nil {
		return written{}, err
	}
	return written{status: http.StatusCreated, body: issue,
		event: writeEvent{number: issue.Number, url: issue.URL}}, nil
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
