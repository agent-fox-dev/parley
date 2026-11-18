package forge

import (
	"cmp"
	"context"
	"net/http"
	"strings"

	"github.com/agent-fox-dev/parley/internal/server"
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

// issueUpdate is the body of an issue update, and the number of the issue
// it updates: each field is nil when it is not set.
type issueUpdate struct {
	number int64
	issueInput
	State *string
}

// readIssueUpdate reads the number and the body of an issue update: a
// title, not blank when it is set; a body, which may be empty to clear the
// issue's, and is measured; a state, open or closed; labels and assignees,
// lists of names, which may be empty to clear the issue's. An update must
// set at least one of them.
func readIssueUpdate(r *http.Request, call *Call) error {
	n, err := PathNumber(r)
	if err != nil {
		return err
	}
	m, err := readObject(r)
	if err != nil {
		return err
	}
	u := issueUpdate{number: n}
	if err := cmp.Or(m.filled("title", &u.Title), m.text("body", &u.Body), m.text("state", &u.State),
		m.names("labels", &u.Labels), m.names("assignees", &u.Assignees)); err != nil {
		return err
	}
	if u.State != nil && *u.State != IssueOpen && *u.State != IssueClosed {
		return server.InvalidRequest("state must be open or closed")
	}
	if len(u.fields(KindGitHub)) == 0 {
		return server.InvalidRequest("an update must set at least one of title, body, state, labels and assignees")
	}
	call.Params = u
	return nil
}

// fields are the names of the fields of u that are sent to a forge of
// kind, sorted: on GitLab never assignees, which it does not take.
func (u issueUpdate) fields(kind ForgeKind) []string {
	var out []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"assignees", u.Assignees != nil && kind != KindGitLab}, {"body", u.Body != nil}, {"labels", u.Labels != nil},
		{"state", u.State != nil}, {"title", u.Title != nil},
	} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// refuseAssigneesAlone refuses an update of a GitLab issue that sets only
// assignees: GitLab does not take them, which would leave nothing to send.
func refuseAssigneesAlone(_ *http.Request, call *Call) error {
	if call.Resolution.Kind == KindGitLab && len(call.Params.(issueUpdate).fields(KindGitLab)) == 0 {
		return server.InvalidRequest("GitLab assigns issues by user id, not by name: an update that sets only assignees has nothing to send")
	}
	return nil
}

// updateIssue updates the issue the request names with the fields its body
// sets and answers the issue as the forge returns it; its event has the
// number, the issue's web URL and the fields sent.
func updateIssue(ctx context.Context, call *Call) (written, error) {
	u := call.Params.(issueUpdate)
	ev := writeEvent{number: u.number, extra: map[string]any{"fields": u.fields(call.Resolution.Kind)}}
	issue, err := writerOf(call.Resolution.Kind).updateIssue(ctx, call, u)
	if err != nil {
		return written{event: ev}, err
	}
	ev.url = issue.URL
	return written{status: http.StatusOK, body: issue, event: ev}, nil
}

// githubIssueWrite is the body of an issue create or update on GitHub: the
// fields that are set.
type githubIssueWrite struct {
	Title     *string   `json:"title,omitempty"`
	Body      *string   `json:"body,omitempty"`
	State     *string   `json:"state,omitempty"`
	Labels    *[]string `json:"labels,omitempty"`
	Assignees *[]string `json:"assignees,omitempty"`
}

func (github) createIssue(ctx context.Context, call *Call, in issueInput) (Issue, error) {
	var g githubIssue
	body := githubIssueWrite{Title: in.Title, Body: in.Body, Labels: in.Labels, Assignees: in.Assignees}
	if err := call.sendJSON(ctx, http.MethodPost, githubRepo(call.Resolution.Repository)+"/issues", body, &g); err != nil {
		return Issue{}, err
	}
	return g.issue(), nil
}

func (github) updateIssue(ctx context.Context, call *Call, in issueUpdate) (Issue, error) {
	var g githubIssue
	body := githubIssueWrite{Title: in.Title, Body: in.Body, State: in.State, Labels: in.Labels, Assignees: in.Assignees}
	if err := call.sendJSON(ctx, http.MethodPatch, githubIssuePath(call.Resolution.Repository, in.number), body, &g); err != nil {
		return Issue{}, err
	}
	return g.issue(), nil
}

// gitlabIssueWrite is the body of an issue create or update on GitLab: the
// fields that are set, the labels as one string of names separated by
// commas, which is how GitLab takes them, and a state as the event that
// leads to it. GitLab assigns by user id, not by name, so the assignees are
// not sent.
type gitlabIssueWrite struct {
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	StateEvent  *string `json:"state_event,omitempty"`
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

func (gitlab) updateIssue(ctx context.Context, call *Call, in issueUpdate) (Issue, error) {
	var g gitlabIssue
	body := gitlabIssueWrite{Title: in.Title, Description: in.Body, Labels: gitlabLabels(in.Labels)}
	if in.State != nil {
		body.StateEvent = new("reopen")
		if *in.State == IssueClosed {
			body.StateEvent = new("close")
		}
	}
	if err := call.sendJSON(ctx, http.MethodPut, gitlabIssuePath(call.Resolution.Repository, in.number), body, &g); err != nil {
		return Issue{}, err
	}
	return g.issue(), nil
}
