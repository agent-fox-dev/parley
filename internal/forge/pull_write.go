package forge

import (
	"cmp"
	"context"
	"net/http"
	"strings"
)

// commentOnPull posts a comment in the conversation of a pull request.
func commentOnPull(ctx context.Context, call *Call) (written, error) {
	return postComment(ctx, call, true)
}

// pullOpen is the body of a pull request open: the title, the branch it
// merges and the branch it merges into, its body, nil when it is not set,
// and whether it is a draft.
type pullOpen struct {
	Title, Head, Base *string
	Body              *string
	Draft             bool
}

func (in pullOpen) texts() []text { return []text{{"body", in.Body}} }

// readPullOpen reads the body of a pull request open: a title, a head and
// a base, each required and not blank; a body, which is measured; and
// draft, true or false, false when it is not set.
func readPullOpen(r *http.Request, call *Call) error {
	m, err := readObject(r)
	if err != nil {
		return err
	}
	var in pullOpen
	var draft *bool
	if err := cmp.Or(m.required("title", &in.Title), m.required("head", &in.Head), m.required("base", &in.Base),
		m.text("body", &in.Body), m.flag("draft", &draft)); err != nil {
		return err
	}
	in.Draft = draft != nil && *draft
	call.Params = in
	return nil
}

// openPull opens the pull request the request's body describes and answers
// it as the forge returns it. Whether the branches exist and whether such a
// pull request is open already is the forge's to say: its refusal is the
// answer. Its event has the head, the base and the draft as asked, and on
// success the new number and the pull request's web URL.
func openPull(ctx context.Context, call *Call) (written, error) {
	in := call.Params.(pullOpen)
	ev := writeEvent{extra: map[string]any{"head": *in.Head, "base": *in.Base, "draft": in.Draft}}
	p, err := writerOf(call.Resolution.Kind).openPull(ctx, call, in)
	if err != nil {
		return written{event: ev}, err
	}
	ev.number, ev.url = p.Number, p.URL
	return written{status: http.StatusCreated, body: p, event: ev}, nil
}

// githubPullOpen is the body of a pull request open on GitHub. The head is
// sent as given, so that owner:branch names a branch of a fork.
type githubPullOpen struct {
	Title string  `json:"title"`
	Head  string  `json:"head"`
	Base  string  `json:"base"`
	Body  *string `json:"body,omitempty"`
	Draft bool    `json:"draft"`
}

func (github) openPull(ctx context.Context, call *Call, in pullOpen) (PullRequest, error) {
	var g githubPull
	body := githubPullOpen{Title: *in.Title, Head: *in.Head, Base: *in.Base, Body: in.Body, Draft: in.Draft}
	if err := call.sendJSON(ctx, http.MethodPost, githubRepo(call.Resolution.Repository)+"/pulls", body, &g); err != nil {
		return PullRequest{}, err
	}
	return g.pull(), nil
}

// gitlabDraftPrefix marks a GitLab merge request as a draft, which GitLab
// takes from its title.
const gitlabDraftPrefix = "Draft: "

// gitlabMROpen is the body of a merge request open on GitLab.
type gitlabMROpen struct {
	Title        string  `json:"title"`
	SourceBranch string  `json:"source_branch"`
	TargetBranch string  `json:"target_branch"`
	Description  *string `json:"description,omitempty"`
}

func (gitlab) openPull(ctx context.Context, call *Call, in pullOpen) (PullRequest, error) {
	title := *in.Title
	if in.Draft && !strings.HasPrefix(title, gitlabDraftPrefix) {
		title = gitlabDraftPrefix + title
	}
	var g gitlabMergeRequest
	body := gitlabMROpen{Title: title, SourceBranch: *in.Head, TargetBranch: *in.Base, Description: in.Body}
	if err := call.sendJSON(ctx, http.MethodPost, gitlabProject(call.Resolution.Repository)+"/merge_requests", body, &g); err != nil {
		return PullRequest{}, err
	}
	return g.pull(), nil
}
